package websites

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestManagedNativeSitePathsAndWAF(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("KP_DATA_DIR", dir)
	InvalidateNginxCache()
	oldWAF := WafEnabled
	WafEnabled = func() bool { return true }
	t.Cleanup(func() { WafEnabled = oldWAF; InvalidateNginxCache() })
	binary := ManagedNginxBinary()
	if err := os.MkdirAll(filepath.Dir(binary), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binary, []byte("fixture"), 0755); err != nil {
		t.Fatal(err)
	}
	if mode := DetectNginx(); mode.Mode != "host" || mode.Target != binary {
		t.Fatalf("mode: %+v", mode)
	}
	if err := os.MkdirAll(sslDir(), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(certMetaPath("testcert"), []byte(`{"id":"testcert"}`), 0600); err != nil {
		t.Fatal(err)
	}
	crt, key, err := CertFilePaths("testcert")
	if err != nil || crt != certCrtPath("testcert") || key != certKeyPath("testcert") {
		t.Fatalf("certificate paths %q %q: %v", crt, key, err)
	}
	site := &Site{ID: "site", Domain: "example.com", Enabled: true, Port: 80, Type: "static", Root: filepath.Join(dir, "site"), SslCertID: "testcert"}
	if err := writeConf(site); err != nil {
		t.Fatal(err)
	}
	conf, err := os.ReadFile(filepath.Join(confDir(), "site.conf"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(conf)
	for _, want := range []string{crt, key, filepath.Join(dir, "websites", "logs")} {
		if !strings.Contains(got, want) {
			t.Errorf("missing native path %q in %s", want, got)
		}
	}
	if strings.Contains(got, "access_by_lua_file") {
		t.Fatal("native config includes container WAF directive")
	}
}
