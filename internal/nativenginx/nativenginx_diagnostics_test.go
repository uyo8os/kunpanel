package nativenginx

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestServiceDiagnosticsContinuesAfterStatusFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("diagnostic command stubs require a POSIX shell")
	}
	dir := t.TempDir()
	for name, script := range map[string]string{
		"systemctl":  "#!/bin/sh\necho 'unit failed'\nexit 3\n",
		"journalctl": "#!/bin/sh\necho 'journal diagnostic'\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	var out bytes.Buffer
	logServiceDiagnostics(&out)
	for _, expected := range []string{"systemctl status", "unit failed", "journalctl -u " + ServiceName, "journal diagnostic"} {
		if !strings.Contains(out.String(), expected) {
			t.Fatalf("diagnostic output missing %q: %s", expected, out.String())
		}
	}
}
