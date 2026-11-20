//go:build linux

package websites

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kunpanel/internal/appstore"
	"kunpanel/internal/nativenginx"
	"kunpanel/internal/nativephp"
)

// This test installs and starts real PHP-FPM and Nginx. It is only for a
// disposable root container with nginx, network access and a systemctl stub.
func TestNativePHPSiteEndToEnd(t *testing.T) {
	if os.Getenv("KP_NATIVE_PHP_SITE_E2E") != "1" {
		t.Skip("requires explicit opt-in and a disposable Linux container")
	}
	if os.Geteuid() != 0 {
		t.Fatal("requires root in a disposable container")
	}
	nginx, err := exec.LookPath("nginx")
	if err != nil {
		t.Fatal("test container needs nginx: ", err)
	}
	if _, err := os.Stat("/run/systemd/system"); err != nil {
		t.Fatal("test container needs systemctl stub")
	}
	dir, err := os.MkdirTemp("/tmp", "kunpanel-site-php-e2e-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KP_DATA_DIR", dir)
	minor := os.Getenv("KP_NATIVE_PHP_BRANCH")
	if minor == "" {
		minor = "8.4"
	}
	if _, err := os.Lstat(filepath.Join("/etc/systemd/system", nativephp.ServiceName(minor)+".service")); !os.IsNotExist(err) {
		t.Fatalf("PHP service already exists: %v", err)
	}
	release, err := nativephp.Latest(minor)
	if err != nil {
		t.Fatal(err)
	}
	if err := nativephp.Install(minor, release.Version, os.Stdout); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := nativephp.Uninstall(minor, os.Stdout); err != nil {
			t.Errorf("PHP cleanup: %v", err)
		}
	})
	if err := appstore.RegisterNativePHP(minor, release.Version); err != nil {
		t.Fatal(err)
	}

	// The test uses distro Nginx as the actual executable, but the panel's
	// configuration generator and native gateway detection are exercised.
	if err := os.MkdirAll(filepath.Dir(ManagedNginxBinary()), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(nginx, ManagedNginxBinary()); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(confDir(), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(siteNginxLogDir(ReloadMode{Target: ManagedNginxBinary()}), 0755); err != nil {
		t.Fatal(err)
	}
	worker, err := user.Lookup(nativephp.WorkerUser())
	if err != nil {
		t.Fatal(err)
	}
	group, err := user.LookupGroupId(worker.Gid)
	if err != nil {
		t.Fatal(err)
	}
	nginxConfig := fmt.Sprintf("user %s %s;\nworker_processes 1;\npid %s;\nerror_log %s;\nevents { worker_connections 128; }\nhttp { include %s; }\n", worker.Username, group.Name, filepath.Join(dir, "nginx.pid"), filepath.Join(dir, "nginx-error.log"), filepath.Join(confDir(), "*.conf"))
	if err := os.WriteFile(nativenginx.ConfigPath(), []byte(nginxConfig), 0644); err != nil {
		t.Fatal(err)
	}
	cmd := func(args ...string) error {
		out, err := exec.Command(ManagedNginxBinary(), append([]string{"-p", nativenginx.Prefix() + "/", "-c", nativenginx.ConfigPath()}, args...)...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("nginx: %s: %w", out, err)
		}
		return nil
	}
	if err := cmd(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := cmd("-s", "quit"); err != nil {
			t.Errorf("nginx cleanup: %v", err)
		}
	})
	InvalidateNginxCache()
	t.Cleanup(InvalidateNginxCache)
	site, err := Create(CreateOpts{Domain: "site.test", SiteType: "php", PhpKey: appstore.NativePHPKey(minor), Port: 80})
	if err != nil {
		t.Fatal(err)
	}
	script := `<?php header('Content-Type: application/json'); echo json_encode(['query'=>$_GET['q']??'', 'post'=>$_POST['value']??'', 'proxy'=>getenv('HTTP_PROXY') ?: '', 'written'=>file_put_contents(__DIR__.'/upload.txt', 'ok')]);`
	if err := os.WriteFile(filepath.Join(site.Root, "index.php"), []byte(script), 0644); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest("POST", "http://127.0.0.1/index.php?q=hello", strings.NewReader(url.Values{"value": {"world"}}.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "site.test"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Proxy", "http://attacker.invalid")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("PHP request HTTP %d: %s (%v)", resp.StatusCode, data, err)
	}
	var got struct {
		Query   string `json:"query"`
		Post    string `json:"post"`
		Proxy   string `json:"proxy"`
		Written int    `json:"written"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("PHP output %q: %v", data, err)
	}
	if got.Query != "hello" || got.Post != "world" || got.Proxy != "" || got.Written != 2 {
		t.Fatalf("unexpected PHP output: %+v", got)
	}
	missing, err := http.NewRequest("GET", "http://127.0.0.1/not-found.php", nil)
	if err != nil {
		t.Fatal(err)
	}
	missing.Host = "site.test"
	missingResp, err := client.Do(missing)
	if err != nil {
		t.Fatal(err)
	}
	missingResp.Body.Close()
	if missingResp.StatusCode != http.StatusNotFound {
		t.Fatalf("missing PHP file: HTTP %d", missingResp.StatusCode)
	}
}
