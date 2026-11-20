package webstat

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGetNativeLogContent(t *testing.T) {
	dir := t.TempDir()
	for name, data := range map[string]string{"access.log": "global\n", "example.access.log": "site1\nsite2\n", "example.error.log": "ignored\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	got, err := GetNativeLogContent(dir, 2)
	if err != nil || got != "site1\nsite2" {
		t.Fatalf("got %q, %v", got, err)
	}
	if strings.Contains(got, "ignored") {
		t.Fatal("included error log")
	}
}
