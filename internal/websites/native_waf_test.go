package websites

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kunpanel/internal/nativenginx"
)

func TestNativeWAFConfigFailureRestoresOriginalFiles(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("KP_DATA_DIR", dir)
	InvalidateNginxCache()
	t.Cleanup(InvalidateNginxCache)
	write := func(path, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(nativenginx.Binary(), "invalid executable")
	write(nativenginx.ConfigPath(), "# original nginx config\n")
	write(storeFile(), `[{"id":"site_fixture","enabled":false}]`)
	siteConf := filepath.Join(confDir(), "site_fixture.conf")
	write(siteConf, "# original site config\n")

	if err := ApplyNativeWAF("# new nginx config\n", []byte(NativeWAFSnippetMarker)); err == nil {
		t.Fatal("invalid gateway executable passed configuration verification")
	}
	for path, original := range map[string]string{nativenginx.ConfigPath(): "# original nginx config", siteConf: "# original site config"} {
		got, err := os.ReadFile(path)
		if err != nil || strings.TrimSpace(string(got)) != original {
			t.Fatalf("configuration %s was not restored: %v", path, err)
		}
	}
	if _, err := os.Stat(filepath.Join(confDir(), "kunwaf.conf")); !os.IsNotExist(err) {
		t.Fatalf("failed activation left a WAF snippet: %v", err)
	}
}
