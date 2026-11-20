package nativenginx

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseVersionsUsesOfficialChannels(t *testing.T) {
	html := `<h4>Mainline version</h4><a href="/download/nginx-1.31.9.tar.gz">nginx-1.31.9</a><h4>Stable version</h4><a href="/download/nginx-1.30.7.tar.gz">nginx-1.30.7</a><h4>Legacy versions</h4><a href="/download/nginx-1.28.3.tar.gz">old</a>`
	got, err := parseVersions(html)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != (Version{Version: "1.31.9", Channel: "mainline"}) || got[1] != (Version{Version: "1.30.7", Channel: "stable"}) {
		t.Fatalf("versions: %+v", got)
	}
}

func TestConfigAndLegacyWAFProtection(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("KP_DATA_DIR", dir)
	if err := writeInitialConfig(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{filepath.Join(dir, "websites", "conf.d", "*.conf"), filepath.Join(Prefix(), "conf", "mime.types"), filepath.Join(dir, "websites", "logs", "access.log")} {
		if !strings.Contains(string(b), strings.ReplaceAll(expected, `\`, `\\`)) {
			t.Fatalf("config missing %q: %s", expected, b)
		}
	}
	if err := rejectWAF(); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(dir, "websites", "conf.d", "old.conf")
	if err := os.WriteFile(old, []byte("server { access_by_lua_file /usr/local/kunwaf/waf.lua; }"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := rejectWAF(); err == nil {
		t.Fatal("legacy WAF configuration accepted")
	}
	if err := os.WriteFile(old, []byte("server { root /var/www/example; }"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := rejectWAF(); err == nil {
		t.Fatal("legacy container path accepted")
	}
}

func TestParseVersionsRejectsMissingChannel(t *testing.T) {
	_, err := parseVersions(`<h4>Mainline version</h4><a href="nginx-1.31.9.tar.gz">x</a>`)
	if err == nil {
		t.Fatal("missing stable section accepted")
	}
}

func TestParseVersionsDoesNotUseLegacyAsStable(t *testing.T) {
	page := `<h4>Mainline version</h4><a href="/download/nginx-1.31.9.tar.gz">mainline</a><h4>Stable version</h4><h4>Legacy versions</h4><a href="/download/nginx-1.28.3.tar.gz">legacy</a>`
	if _, err := parseVersions(page); err == nil {
		t.Fatal("legacy version accepted as stable")
	}
}

func TestNginxConfigQuoteRejectsNewline(t *testing.T) {
	if _, err := nginxQuote("/tmp/foo\ninjected"); err == nil {
		t.Fatal("newline accepted")
	}
	got, err := nginxQuote("/tmp/with space")
	if err != nil || !strings.HasPrefix(got, `"`) {
		t.Fatalf("quote: %q %v", got, err)
	}
}

func TestServiceUnitPIDFileMatchesNginxConfig(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data with $ space")
	t.Setenv("KP_DATA_DIR", dir)
	if err := writeInitialConfig(); err != nil {
		t.Fatal(err)
	}
	pid := filepath.Join(root(), "nginx.pid")
	unit, err := serviceUnit(Binary(), Prefix()+"/", ConfigPath(), pid)
	if err != nil {
		t.Fatal(err)
	}
	// PIDFile has no argument quoting or dollar expansion. ExecStart does.
	if !strings.Contains(unit, "\nPIDFile="+pid+"\n") {
		t.Fatalf("unit PIDFile is not the literal configured path: %s", unit)
	}
	if !strings.Contains(unit, "ExecStart=\"") || !strings.Contains(unit, "$$") {
		t.Fatalf("ExecStart did not quote/escape its arguments: %s", unit)
	}
	conf, err := os.ReadFile(ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	quotedPID, err := nginxQuote(pid)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(conf), "\npid "+quotedPID+";\n") {
		t.Fatalf("nginx pid does not match systemd PIDFile: %s", conf)
	}
}

func TestServiceUnitRejectsInvalidPIDPaths(t *testing.T) {
	base := filepath.Join(t.TempDir(), "nginx.pid")
	for _, path := range []string{"relative.pid", base + "%n", base + "\nInjected=yes", base + "\r", base + "\x00"} {
		if _, err := serviceUnit("nginx", "prefix", "config", path); err == nil {
			t.Errorf("accepted invalid PID path %q", path)
		}
	}
}

func TestReusingBuildRequiresMatchingManifest(t *testing.T) {
	target := filepath.Join(t.TempDir(), "1.30.7")
	for _, dir := range []string{filepath.Join(target, "sbin"), filepath.Join(target, "conf")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	bin := filepath.Join(target, "sbin", "nginx")
	if err := os.WriteFile(bin, []byte("verified nginx"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "conf", "mime.types"), []byte("mime"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := verifyBuildManifest(target, "1.30.7"); err == nil {
		t.Fatal("unrecorded directory accepted")
	}
	if err := saveBuildManifest(target, "1.30.7"); err != nil {
		t.Fatal(err)
	}
	if err := verifyBuildManifest(target, "1.30.7"); err != nil {
		t.Fatal(err)
	}
	if err := verifyBuildManifest(target, "1.31.0"); err == nil {
		t.Fatal("wrong version accepted")
	}
	if err := os.WriteFile(bin, []byte("modified nginx"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := verifyBuildManifest(target, "1.30.7"); err == nil {
		t.Fatal("modified binary accepted")
	}
}
