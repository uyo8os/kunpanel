package nativenginx

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// This test performs real downloads, compilation and service operations. Run
// only in an isolated disposable Linux container with a systemctl test stub.
func TestNativeNginxEndToEnd(t *testing.T) {
	if os.Getenv("KP_NATIVE_NGINX_E2E") != "1" {
		t.Skip("set KP_NATIVE_NGINX_E2E=1 in an isolated Linux container")
	}
	if runtime.GOOS != "linux" || os.Geteuid() != 0 || !systemd() {
		t.Fatal("requires root, Linux and a systemd test stub")
	}
	if _, err := os.Lstat(unitPath); err == nil || !os.IsNotExist(err) {
		t.Fatalf("service unit already exists or cannot be checked: %v", err)
	}
	// t.TempDir uses mode 0700, preventing the unprivileged Nginx worker
	// from traversing it to serve files. /tmp is traversable in the isolated
	// Linux test environment; only this test's own directory is removed.
	dir, err := os.MkdirTemp("/tmp", "kunpanel-native-nginx-e2e-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KP_DATA_DIR", dir)
	confDir := filepath.Join(dir, "websites", "conf.d")
	if err := os.MkdirAll(confDir, 0o755); err != nil {
		t.Fatal(err)
	}
	confPath := filepath.Join(confDir, "test.conf")
	webRoot := filepath.Join(dir, "webroot")
	if err := os.MkdirAll(webRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	indexPath := filepath.Join(webRoot, "index.html")
	if err := os.WriteFile(indexPath, []byte("kunpanel-native-e2e"), 0o644); err != nil {
		t.Fatal(err)
	}
	certPath, keyPath, roots := nativeE2ECertificate(t, dir)
	site := fmt.Sprintf(`server {
    listen 127.0.0.1:80;
    listen 127.0.0.1:443 ssl;
    server_name localhost;
    ssl_certificate "%s";
    ssl_certificate_key "%s";
    root "%s";
    location / { try_files $uri $uri/ =404; }
}`, certPath, keyPath, webRoot)
	httpsClient := &http.Client{Timeout: time.Second, Transport: &http.Transport{
		TLSClientConfig:   &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
		DisableKeepAlives: true,
	}}
	t.Cleanup(httpsClient.CloseIdleConnections)
	if err := os.WriteFile(confPath, []byte(site), 0o644); err != nil {
		t.Fatal(err)
	}

	versions, err := Versions()
	if err != nil {
		t.Fatal(err)
	}
	var stable, mainline string
	for _, v := range versions {
		if v.Channel == "stable" {
			stable = v.Version
		}
		if v.Channel == "mainline" {
			mainline = v.Version
		}
	}
	if stable == "" || mainline == "" || stable == mainline {
		t.Fatalf("unexpected official channels: %+v", versions)
	}
	installed := false
	t.Cleanup(func() {
		if installed && Installed() {
			if err := Uninstall(os.Stdout); err != nil {
				t.Errorf("cleanup failed: %v", err)
			}
		}
	})
	if err := Install(stable, os.Stdout); err != nil {
		t.Fatalf("install %s: %v", stable, err)
	}
	installed = true
	checkNativeHTTP(t)
	checkNativeHTTPS(t, httpsClient)
	if err := Action("restart"); err != nil {
		t.Fatalf("restart: %v", err)
	}
	checkNativeHTTP(t)
	checkNativeHTTPS(t, httpsClient)
	if err := Install(mainline, os.Stdout); err != nil {
		t.Fatalf("upgrade %s: %v", mainline, err)
	}
	checkNativeHTTP(t)
	checkNativeHTTPS(t, httpsClient)
	if err := Install(stable, os.Stdout); err != nil {
		t.Fatalf("reuse previous build %s: %v", stable, err)
	}
	checkNativeHTTP(t)
	checkNativeHTTPS(t, httpsClient)

	before, err := os.Readlink(Prefix())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(confPath, []byte("invalid_nginx_directive;"), 0o644); err != nil {
		t.Fatal(err)
	}
	err = Install(mainline, io.Discard)
	if err == nil {
		t.Fatal("invalid config accepted during update")
	}
	after, readErr := os.Readlink(Prefix())
	if readErr != nil || after != before {
		t.Fatalf("rollback: before %q, after %q (%v)", before, after, readErr)
	}
	checkNativeHTTP(t) // old running workers continue serving the last valid configuration
	checkNativeHTTPS(t, httpsClient)
	if err := os.WriteFile(confPath, []byte(site), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Action("reload"); err != nil {
		t.Fatal(err)
	}
	checkNativeHTTP(t)
	checkNativeHTTPS(t, httpsClient)
	if err := Uninstall(os.Stdout); err != nil {
		t.Fatal(err)
	}
	installed = false
	if _, err := os.Stat(confPath); err != nil {
		t.Fatalf("site configuration removed: %v", err)
	}
	if _, err := os.Stat(indexPath); err != nil {
		t.Fatalf("site content removed: %v", err)
	}
}

func checkNativeHTTP(t *testing.T) {
	t.Helper()
	checkNativeResponse(t, &http.Client{Timeout: time.Second, Transport: &http.Transport{DisableKeepAlives: true}}, "http://127.0.0.1/")
}

func checkNativeHTTPS(t *testing.T, client *http.Client) {
	t.Helper()
	checkNativeResponse(t, client, "https://127.0.0.1/")
}

func checkNativeResponse(t *testing.T, client *http.Client, url string) {
	t.Helper()
	var last error
	for i := 0; i < 30; i++ {
		resp, err := client.Get(url)
		if err == nil {
			b, readErr := io.ReadAll(io.LimitReader(resp.Body, 1024))
			resp.Body.Close()
			if readErr == nil && resp.StatusCode == http.StatusOK && strings.Contains(string(b), "kunpanel-native-e2e") {
				return
			}
			last = fmt.Errorf("HTTP %d: %q (%v)", resp.StatusCode, b, readErr)
		} else {
			last = err
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("native Nginx did not serve %s: %v", url, last)
}

func nativeE2ECertificate(t *testing.T, dir string) (string, string, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "kunpanel-native-e2e"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPath := filepath.Join(dir, "e2e.crt")
	keyPath := filepath.Join(dir, "e2e.key")
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(certPEM) {
		t.Fatal("cannot add test certificate to trust store")
	}
	return certPath, keyPath, roots
}
