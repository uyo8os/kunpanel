//go:build linux

package websites

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"kunpanel/internal/appstore"
	"kunpanel/internal/nativephp"
)

func TestNginxWorkerSocketPermissions(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("socket ownership test requires root")
	}
	dir, err := os.MkdirTemp("/tmp", "kunpanel-nginx-socket-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	worker, err := nativePHPWorkerIdentity()
	if err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(dir, "php.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := os.Chown(socket, worker.uid, worker.gid); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(socket, 0660); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(socket)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkNginxUserSocketPermissions(socket, nativephp.WorkerUser(), "", fi); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(socket, 0000); err != nil {
		t.Fatal(err)
	}
	fi, _ = os.Stat(socket)
	if err := checkNginxUserSocketPermissions(socket, nativephp.WorkerUser(), "", fi); err == nil {
		t.Fatal("inaccessible socket accepted")
	}
	if err := os.Chmod(socket, 0660); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	fi, _ = os.Stat(socket)
	if err := checkNginxUserSocketPermissions(socket, nativephp.WorkerUser(), "", fi); err == nil {
		t.Fatal("inaccessible socket parent accepted")
	}
}

func TestNativePHPSiteDirectoryOwnershipAndExistingProtection(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("testing chown requires root")
	}
	parent, err := os.MkdirTemp("", "kunpanel-php-permissions-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(parent) })
	if err := os.Chmod(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	identity, err := nativePHPWorkerIdentity()
	if err != nil {
		t.Fatal(err)
	}
	newSite := filepath.Join(parent, "new")
	if err := prepareNativePHPSiteDirectory(newSite); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(newSite)
	if err != nil {
		t.Fatal(err)
	}
	if st := info.Sys().(*syscall.Stat_t); int(st.Uid) != identity.uid || int(st.Gid) != identity.gid {
		t.Fatalf("new directory owner = %d:%d, want %d:%d", st.Uid, st.Gid, identity.uid, identity.gid)
	}
	existing := filepath.Join(parent, "existing")
	if err := os.Mkdir(existing, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := prepareNativePHPSiteDirectory(existing); err != nil {
		t.Fatalf("existing readable site root was rejected: %v", err)
	}
	info, err = os.Stat(existing)
	if err != nil {
		t.Fatal(err)
	}
	if st := info.Sys().(*syscall.Stat_t); st.Uid != 0 || info.Mode().Perm() != 0o755 {
		t.Fatalf("existing directory changed: %d / %s", st.Uid, info.Mode())
	}
	if err := os.Chmod(existing, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := checkExistingNativePHPSiteDirectory(existing); err == nil {
		t.Fatal("inaccessible site root was accepted")
	}
}

func TestBindNativePHPVersion(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("KP_DATA_DIR", dir)
	InvalidateNginxCache()
	t.Cleanup(InvalidateNginxCache)
	binary := ManagedNginxBinary()
	if err := os.MkdirAll(filepath.Dir(binary), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binary, []byte("fixture"), 0o755); err != nil {
		t.Fatal(err)
	}
	insts := []appstore.Instance{{Key: "php83", InstallMode: "native", Params: map[string]string{"Version": "8.3.37"}}, {Key: "php85", InstallMode: "native", Params: map[string]string{"Version": "8.5.13"}}}
	data, err := json.Marshal(insts)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "apps"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "apps", "instances.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	site := &Site{Root: filepath.Join(dir, "not-yet-created")}
	if err := bindSitePHP(site, "php85"); err != nil {
		t.Fatal(err)
	}
	if site.PHPMode != "native" || site.PhpKey != "php85" || site.FpmPass != "unix:"+nativephp.Socket("8.5") || site.ProxyTo != "" {
		t.Fatalf("unexpected binding: %+v", site)
	}
	if err := bindSitePHP(site, "php83"); err != nil {
		t.Fatal(err)
	}
	if site.PhpKey != "php83" || !strings.HasSuffix(site.FpmPass, "/8.3/php-fpm.sock") {
		t.Fatalf("version change did not use 8.3 socket: %+v", site)
	}
	if err := bindSitePHP(site, "php86"); err == nil {
		t.Fatal("missing target must not silently become a Docker PHP upstream")
	}
	if site.PHPMode != "native" || site.PhpKey != "php83" {
		t.Fatalf("failed binding changed the site: %+v", site)
	}
}
