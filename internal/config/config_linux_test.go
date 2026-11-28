package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadExistingConfigDoesNotTouchFirewall(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("KP_DATA_DIR", dir)
	t.Setenv("PATH", dir)
	firewallCalls := filepath.Join(dir, "firewall-calls")
	t.Setenv("TEST_FIREWALL_CALLS", firewallCalls)
	if err := os.WriteFile(filepath.Join(dir, "iptables"), []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$TEST_FIREWALL_CALLS\"\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"port":54744,"entrance":"panel"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	old, oldInitialFile := M, initialFile
	t.Cleanup(func() { M, initialFile = old, oldInitialFile })
	m, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := m.Get().Port; got != 54744 {
		t.Fatalf("loaded port = %d, want 54744", got)
	}
	if _, err := os.Stat(firewallCalls); !os.IsNotExist(err) {
		t.Fatalf("loading an existing panel called the firewall: %v", err)
	}
}
