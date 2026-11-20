package websites

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"kunpanel/internal/nativephp"
)

func TestNativePHPConfigUsesFPMForSiteAndSubdomain(t *testing.T) {
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
	socket := "unix:" + nativephp.Socket("8.5")
	site := &Site{ID: "site", Domain: "example.com", Port: 80, Enabled: true,
		Type: "php", PHPMode: "native", PhpKey: "php85", FpmPass: socket,
		Root: filepath.Join(dir, "site with spaces"), Subs: []SiteSub{{SubDomain: "m.example.com", Dir: "mobile app"}}}
	if err := writeConf(site); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(confDir(), "site.conf"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	if strings.Count(got, "fastcgi_pass "+strconv.Quote(socket)+";") != 2 {
		t.Fatalf("site and subdomain must use the selected socket: %s", got)
	}
	for _, want := range []string{"index.php index.html index.htm", "try_files $uri =404;", "fastcgi_param SCRIPT_FILENAME $document_root$fastcgi_script_name;", `fastcgi_param HTTP_PROXY "";`} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q: %s", want, got)
		}
	}
	if strings.Contains(got, "proxy_pass") {
		t.Fatalf("native PHP must not use container HTTP proxy: %s", got)
	}
	for _, want := range []string{strconv.Quote(site.Root), strconv.Quote(strings.TrimSuffix(site.Root, "/") + "/mobile app")} {
		if !strings.Contains(got, "root "+want+";") {
			t.Errorf("native paths must be quoted: missing %q in %s", want, got)
		}
	}
}

func TestNativePHPBindingRollbackPreservesUnrelatedEdits(t *testing.T) {
	t.Setenv("KP_DATA_DIR", t.TempDir())
	InvalidateNginxCache()
	t.Cleanup(InvalidateNginxCache)
	old := Site{ID: "site", Domain: "example.com", Port: 80, Enabled: true,
		Type: "php", PHPMode: "native", PhpKey: "php84", FpmPass: "unix:/tmp/old.sock", Remark: "old"}
	attempted := old
	attempted.PhpKey, attempted.FpmPass = "php85", "unix:/tmp/new.sock"
	current := attempted
	current.Remark = "concurrent edit"
	mu.Lock()
	list = []Site{current}
	err := save()
	mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	_ = restoreSiteAfterPHPChange(old, attempted, errors.New("reload failed"))
	mu.Lock()
	load()
	got := list[0]
	mu.Unlock()
	if got.PhpKey != "php84" || got.FpmPass != "unix:/tmp/old.sock" || got.Remark != "concurrent edit" {
		t.Fatalf("rollback changed unrelated data: %+v", got)
	}
}

func TestNativePHPLocationsRejectInvalidSocket(t *testing.T) {
	for _, socket := range []string{"127.0.0.1:9000", "unix:relative", "unix:/tmp/x;\n}", "unix:/tmp/$name"} {
		var b strings.Builder
		if err := writeNativePHPLocations(&b, socket, false); err == nil {
			t.Errorf("accepted unsafe socket %q", socket)
		}
	}
}

func TestNativePHPSiteDoesNotFallBackToDocker(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("KP_DATA_DIR", dir)
	InvalidateNginxCache()
	t.Cleanup(InvalidateNginxCache)
	site := &Site{ID: "site", Domain: "example.com", Port: 80, Enabled: true, Type: "php", PHPMode: "native", PhpKey: "php85", FpmPass: "unix:" + nativephp.Socket("8.5"), Root: filepath.Join(dir, "site")}
	if err := writeConf(site); err == nil {
		t.Fatal("missing native nginx must not generate a Docker proxy")
	}
}
