package nativepma

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
	"kunpanel/internal/config"
	"kunpanel/internal/nativenginx"
	"kunpanel/internal/util"
)

const AccessUser = "phpmyadmin"

type Settings struct {
	PHP        string `json:"php"`
	Host       string `json:"host"`
	Port       int    `json:"port"`
	SelfSigned bool   `json:"self_signed"`
}

type Database struct {
	Key  string
	Name string
	Port int
}

// ServerID stays stable when another database is installed or uninstalled.
func (d Database) ServerID() int {
	switch d.Key {
	case "mysql-native":
		return 2
	case "mariadb":
		return 3
	default:
		return 1
	}
}

func (s Settings) URL() string {
	return "https://" + net.JoinHostPort(s.Host, strconv.Itoa(s.Port)) + "/"
}

var hostPattern = regexp.MustCompile(`(?i)^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)*$`)

func (s Settings) Validate() error {
	if s.Port < 1 || s.Port > 65535 {
		return fmt.Errorf("访问端口必须在 1 到 65535 之间")
	}
	if len(s.Host) > 253 || (net.ParseIP(s.Host) == nil && !hostPattern.MatchString(s.Host)) {
		return fmt.Errorf("请输入访问 phpMyAdmin 的域名或 IP 地址")
	}
	return nil
}

func ValidateAccessPassword(password string) error {
	if len(password) < 12 || len(password) > 72 || strings.ContainsAny(password, "\x00\r\n") {
		return fmt.Errorf("访问密码须为 12 到 72 个字节")
	}
	return nil
}

var nginxUser = regexp.MustCompile(`(?m)^\s*user\s+([\w-]+)(?:\s+([\w-]+))?\s*;`)

func writeAccessPassword(password string) error {
	conf, err := os.ReadFile(nativenginx.ConfigPath())
	if err != nil {
		return err
	}
	name, group := "nobody", ""
	if match := nginxUser.FindSubmatch(conf); len(match) == 3 {
		name, group = string(match[1]), string(match[2])
	}
	u, err := user.Lookup(name)
	if err != nil {
		return err
	}
	gid := u.Gid
	if group != "" {
		g, err := user.LookupGroup(group)
		if err != nil {
			return err
		}
		gid = g.Gid
	}
	n, err := strconv.Atoi(gid)
	if err != nil {
		return err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	return writeGroupFile(accessFile(), AccessUser+":"+string(hash)+"\n", n)
}

func certificatePaths(selfSigned bool) (string, string) {
	dir := filepath.Join(config.DataDir(), "panelssl")
	if selfSigned {
		dir = root()
	}
	return filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
}

func certificateMatches(crt, key, host string) bool {
	pair, err := tls.LoadX509KeyPair(crt, key)
	if err != nil {
		return false
	}
	now := time.Now()
	return pair.Leaf != nil && !pair.Leaf.NotBefore.After(now) && pair.Leaf.NotAfter.After(now.Add(7*24*time.Hour)) && pair.Leaf.VerifyHostname(host) == nil
}

// Reuse a panel certificate when it covers the selected hostname.
func prepareCertificate(s *Settings) error {
	crt, key := certificatePaths(false)
	if certificateMatches(crt, key, s.Host) {
		s.SelfSigned = false
		return nil
	}
	s.SelfSigned = true
	crt, key = certificatePaths(true)
	if certificateMatches(crt, key, s.Host) {
		return nil
	}
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return err
	}
	now := time.Now()
	cert := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: s.Host},
		NotBefore: now.Add(-time.Minute), NotAfter: now.AddDate(1, 0, 0),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if ip := net.ParseIP(s.Host); ip != nil {
		cert.IPAddresses = []net.IP{ip}
	} else {
		cert.DNSNames = []string{s.Host}
	}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &priv.PublicKey, priv)
	if err != nil {
		return err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return err
	}
	if err := util.AtomicWrite(key, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return err
	}
	return util.AtomicWrite(crt, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
}

func phpLiteral(value string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(value) + "'"
}

func phpConfig(s Settings, dbs []Database, secret []byte) string {
	var b strings.Builder
	fmt.Fprintf(&b, "<?php\n$cfg['blowfish_secret'] = hex2bin('%s');\n", hex.EncodeToString(secret))
	fmt.Fprintf(&b, "$cfg['PmaAbsoluteUri'] = %s;\n", phpLiteral(s.URL()))
	fmt.Fprintf(&b, "$cfg['TempDir'] = %s;\n", phpLiteral(tempPath()))
	b.WriteString("$cfg['AllowArbitraryServer'] = false;\n$cfg['VersionCheck'] = false;\n$cfg['LoginCookieStore'] = 0;\n")
	if len(dbs) != 0 {
		fmt.Fprintf(&b, "$cfg['ServerDefault'] = %d;\n", dbs[0].ServerID())
	}
	for _, db := range dbs {
		key := fmt.Sprintf("$cfg['Servers'][%d]", db.ServerID())
		fmt.Fprintf(&b, "%s['host'] = '127.0.0.1';\n%s['port'] = %d;\n%s['verbose'] = %s;\n%s['auth_type'] = 'cookie';\n%s['AllowNoPassword'] = false;\n", key, key, db.Port, key, phpLiteral(db.Name), key, key)
	}
	return b.String()
}

func listenAddress(s Settings) (string, string) {
	if ip := net.ParseIP(s.Host); ip != nil && ip.To4() == nil {
		return net.JoinHostPort("::", strconv.Itoa(s.Port)), net.JoinHostPort("::1", strconv.Itoa(s.Port))
	}
	return net.JoinHostPort("0.0.0.0", strconv.Itoa(s.Port)), net.JoinHostPort("127.0.0.1", strconv.Itoa(s.Port))
}

func nginxConfig(s Settings) string {
	crt, key := certificatePaths(s.SelfSigned)
	listen, _ := listenAddress(s)
	return fmt.Sprintf(`# Managed by KunPanel phpMyAdmin
server {
    listen %s ssl;
    server_name _;
    ssl_certificate %q;
    ssl_certificate_key %q;
    ssl_protocols TLSv1.2 TLSv1.3;
    root %q;
    index index.php;
    client_max_body_size 128m;
    auth_basic "KunPanel phpMyAdmin";
    auth_basic_user_file %q;
    add_header X-Frame-Options DENY always;
    add_header X-Content-Type-Options nosniff always;
    add_header Referrer-Policy same-origin always;

    location ~ ^/(?:setup|test|doc|sql|templates|vendor|libraries|src)(?:/|$) { return 404; }
    location ~ (^|/)\. { return 404; }
    location ~ \.php(?:/|$) { return 404; }
    location / { try_files $uri $uri/ =404; }
    location = /index.php {
        include %q;
        fastcgi_param SCRIPT_FILENAME $document_root/index.php;
        fastcgi_param HTTPS on;
        fastcgi_param HTTP_PROXY "";
        fastcgi_param HTTP_AUTHORIZATION "";
        fastcgi_read_timeout 300s;
        fastcgi_pass %q;
    }
    location = /url.php {
        try_files $uri =404;
        include %q;
        fastcgi_param SCRIPT_FILENAME $document_root/url.php;
        fastcgi_param HTTPS on;
        fastcgi_param HTTP_PROXY "";
        fastcgi_param HTTP_AUTHORIZATION "";
        fastcgi_pass %q;
    }
}
`, listen, crt, key, current(), accessFile(), fastcgiParams(), "unix:"+socket(), fastcgiParams(), "unix:"+socket())
}

func fastcgiParams() string { return filepath.Join(nativenginx.Prefix(), "conf", "fastcgi_params") }
