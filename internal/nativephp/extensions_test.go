package nativephp

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestSelectExtensionReleaseUsesCompatibleStableTags(t *testing.T) {
	commit := strings.Repeat("a", 40)
	body := []byte(fmt.Sprintf(`[
		{"name":"3.6.0alpha1","commit":{"sha":%q}},
		{"name":"v3.5.2","commit":{"sha":%q}},
		{"name":"v3.5.10","commit":{"sha":%q}},
		{"name":"3.6.0","commit":{"sha":"bad"}}]`, commit, commit, commit))
	xdebug, _ := findExtension("xdebug")
	selected, err := selectExtensionRelease("8.5", xdebug, body)
	if err != nil || selected.version != "3.5.10" || selected.commit != commit {
		t.Fatalf("selected %+v: %v", selected, err)
	}
	if _, err := selectExtensionRelease("8.6", xdebug, body); err == nil {
		t.Fatal("accepted an Xdebug release without PHP 8.6 stable support")
	}
}

func TestSelectExtensionReleaseUnderstandsMessagePackTags(t *testing.T) {
	commit := strings.Repeat("b", 40)
	body := []byte(fmt.Sprintf(`[{"name":"msgpack-3.0.0","commit":{"sha":%q}},
		{"name":"msgpack-3.1.0RC1","commit":{"sha":%q}}]`, commit, commit))
	msgpack, _ := findExtension("msgpack")
	got, err := selectExtensionRelease("8.4", msgpack, body)
	if err != nil || got.version != "3.0.0" || got.commit != commit {
		t.Fatalf("selected %+v: %v", got, err)
	}
}

func TestSelectExtensionReleaseSkipsKnownIncompatibleRedis(t *testing.T) {
	commit := strings.Repeat("c", 40)
	redis, _ := findExtension("redis")
	old := fmt.Sprintf(`{"name":"6.3.0","commit":{"sha":%q}}`, commit)
	if _, err := selectExtensionRelease("8.6", redis, []byte("["+old+"]")); err == nil {
		t.Fatal("selected Redis 6.3.0 for PHP 8.6")
	}
	newTag := fmt.Sprintf(`{"name":"6.3.1","commit":{"sha":%q}}`, commit)
	got, err := selectExtensionRelease("8.6", redis, []byte("["+old+","+newTag+"]"))
	if err != nil || got.version != "6.3.1" {
		t.Fatalf("new upstream release was not picked up: %+v (%v)", got, err)
	}
}

func TestManagedExtensionDoesNotBlockPHPUpgrade(t *testing.T) {
	t.Setenv("KP_DATA_DIR", t.TempDir())
	minor := "8.4"
	ext, _ := findExtension("redis")
	ini := extensionIniPath(minor, ext.name)
	release := extensionRelease{"6.3.0", strings.Repeat("a", 40)}
	if err := os.MkdirAll(ScanDir(minor), 0o755); err != nil {
		t.Fatal(err)
	}
	managed := extensionIni(minor, ext, release)
	if err := os.WriteFile(ini, []byte(managed), 0o644); err != nil {
		t.Fatal(err)
	}
	if dynamic, err := containsDynamicExtensions(minor); dynamic || err != nil {
		t.Fatalf("managed extension blocked upgrade: %v %v", dynamic, err)
	}
	if err := os.WriteFile(ini, []byte(managed+"extension=manual.so\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if dynamic, err := containsDynamicExtensions(minor); !dynamic || err != nil {
		t.Fatalf("manual extension was accepted as managed: %v %v", dynamic, err)
	}
}

func TestExtensionActivationRestoresPreviousConfiguration(t *testing.T) {
	t.Setenv("KP_DATA_DIR", t.TempDir())
	const minor = "8.4"
	ext, _ := findExtension("apcu")
	ini := extensionIniPath(minor, ext.name)
	oldIni := []byte(extensionIni(minor, ext, extensionRelease{"5.1.27", strings.Repeat("a", 40)}))
	newIni := []byte(extensionIni(minor, ext, extensionRelease{"5.1.28", strings.Repeat("b", 40)}))
	if err := writeExtensionFile(ini, oldIni); err != nil {
		t.Fatal(err)
	}
	// This installation has no runnable PHP binary, so validation must fail.
	if err := activateExtension(minor, "", ini, oldIni, newIni); err == nil {
		t.Fatal("accepted an extension without a working PHP runtime")
	}
	data, err := os.ReadFile(ini)
	if err != nil || string(data) != string(oldIni) {
		t.Fatalf("old configuration was not restored: %q (%v)", data, err)
	}
}
