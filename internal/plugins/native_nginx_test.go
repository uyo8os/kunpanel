package plugins

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"kunpanel/internal/nativenginx"
)

func TestNativeNginxDoesNotRestoreContainerWAF(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("KP_DATA_DIR", dir)
	binary := nativenginx.Binary()
	if err := os.MkdirAll(filepath.Dir(binary), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binary, []byte("fixture"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(wafDir(), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wafDir(), "waf.lua"), []byte("legacy"), 0644); err != nil {
		t.Fatal(err)
	}
	syncWafLua()
	if _, err := os.Stat(wafConfSnippet()); !os.IsNotExist(err) {
		t.Fatalf("WAF configuration created: %v", err)
	}
	if err := InstallWaf(io.Discard); err == nil {
		t.Fatal("native Nginx accepted container WAF")
	}
}
