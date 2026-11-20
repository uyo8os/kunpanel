package server

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNativeNginxConfigSave(t *testing.T) {
	cases := []struct {
		name, content              string
		valid, running, failReload bool
		want, errorPart            string
		reloads                    int
	}{
		{name: "valid running", content: "new config", valid: true, running: true, want: "new config", reloads: 1},
		{name: "valid stopped", content: "new config", valid: true, running: false, want: "new config", reloads: 0},
		{name: "invalid", content: "bad config", valid: false, running: true, want: "old config", errorPart: "配置校验失败", reloads: 0},
		{name: "failed reload", content: "new config", valid: true, running: true, failReload: true, want: "old config", errorPart: "已恢复原配置", reloads: 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "nginx.conf")
			if err := os.WriteFile(path, []byte("old config"), 0o640); err != nil {
				t.Fatal(err)
			}
			checks, reloads := 0, 0
			ops := nativeNginxConfOps{
				test: func(candidate string) (string, error) {
					checks++
					b, err := os.ReadFile(candidate)
					if err != nil {
						return "", err
					}
					if string(b) != tc.content {
						t.Fatalf("candidate %q", b)
					}
					original, err := os.ReadFile(path)
					if err != nil || string(original) != "old config" {
						t.Fatalf("original changed before test: %q, %v", original, err)
					}
					if !tc.valid {
						return "syntax error", fmt.Errorf("invalid")
					}
					return "ok", nil
				},
				running: func() bool { return tc.running },
				reload: func() error {
					reloads++
					if tc.failReload && reloads == 1 {
						return fmt.Errorf("cannot reload")
					}
					return nil
				},
			}
			_, err := saveNativeNginxConfig(path, tc.content, ops)
			if tc.errorPart == "" && err != nil {
				t.Fatal(err)
			}
			if tc.errorPart != "" && (err == nil || !strings.Contains(err.Error(), tc.errorPart)) {
				t.Fatalf("expected %q: %v", tc.errorPart, err)
			}
			got, err := os.ReadFile(path)
			if err != nil || string(got) != tc.want {
				t.Fatalf("content %q: %v", got, err)
			}
			if checks != 1 || reloads != tc.reloads {
				t.Fatalf("checks=%d reloads=%d", checks, reloads)
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 1 {
				t.Fatalf("temporary config left behind: %v %v", entries, err)
			}
		})
	}
}

func TestNativeNginxConfigRejectsInvalidContent(t *testing.T) {
	for _, content := range []string{"", "a\x00b", strings.Repeat("a", 512*1024+1)} {
		_, err := saveNativeNginxConfig("unused", content, nativeNginxConfOps{})
		if err == nil {
			t.Fatal("invalid config accepted")
		}
	}
}

func TestNativeNginxTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "error.log")
	var b strings.Builder
	for i := 0; i < 130; i++ {
		fmt.Fprintf(&b, "line %03d\n", i)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := nativeNginxTail(path, 120)
	if err != nil || strings.HasPrefix(got, "line 009") || !strings.HasPrefix(got, "line 010\n") || !strings.HasSuffix(got, "line 129") {
		t.Fatalf("tail: %q, %v", got, err)
	}
}
