package server

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNativePHPIniValidationAndRollback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "php.ini")
	if err := os.WriteFile(path, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	read := func() string {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	_, err := saveNativePHPIniWithOps(path, "bad", nativePHPIniOps{test: func(p string) (string, error) { return "PHP:  syntax error", nil }, running: func() bool { return true }, restart: func() error { return nil }})
	if err == nil || read() != "old" {
		t.Fatalf("invalid configuration changed file: %v", err)
	}
	restarts := 0
	_, err = saveNativePHPIniWithOps(path, "new", nativePHPIniOps{test: func(p string) (string, error) {
		b, e := os.ReadFile(p)
		if e != nil || string(b) != "new" {
			t.Fatal("candidate not written")
		}
		return "", nil
	}, running: func() bool { return true }, restart: func() error {
		restarts++
		if restarts == 1 {
			return errors.New("failed")
		}
		return nil
	}})
	if err == nil || read() != "old" || restarts != 2 {
		t.Fatalf("rollback failed: %v, restarts=%d", err, restarts)
	}
	msg, err := saveNativePHPIniWithOps(path, "new", nativePHPIniOps{test: func(string) (string, error) { return "", nil }, running: func() bool { return false }, restart: func() error { t.Fatal("stopped service restarted"); return nil }})
	if err != nil || read() != "new" || !strings.Contains(msg, "未运行") {
		t.Fatalf("save stopped: %q %v", msg, err)
	}
}

func TestNativePHPVersionBranches(t *testing.T) {
	for _, version := range []string{"8.3.37", "8.4.28", "8.5.13", "8.6.0"} {
		if !nativePHPVersion.MatchString(version) {
			t.Errorf("valid release rejected: %s", version)
		}
	}
	for _, version := range []string{"8.2.30", "8.7.0", "8.4", "8.4.1-rc1", "8.4.1/../../x"} {
		if nativePHPVersion.MatchString(version) {
			t.Errorf("invalid release accepted: %s", version)
		}
	}
}
