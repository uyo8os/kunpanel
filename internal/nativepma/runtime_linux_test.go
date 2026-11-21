package nativepma

import (
	"context"
	"io"
	"net"
	"os"
	"os/exec"
	"os/user"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestFPMStartsWithJournalStream(t *testing.T) {
	binary := os.Getenv("KP_TEST_PHP_FPM")
	if binary == "" {
		t.Skip("set KP_TEST_PHP_FPM to a PHP-FPM binary in an isolated Linux environment")
	}
	worker, err := user.Lookup("nobody")
	if err != nil {
		t.Fatal(err)
	}
	group, err := user.LookupGroupId(worker.Gid)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("KP_DATA_DIR", t.TempDir())
	if err := os.MkdirAll(root(), 0o755); err != nil {
		t.Fatal(err)
	}

	start := func(ctx context.Context) (*exec.Cmd, *os.File) {
		t.Helper()
		// Match systemd's journal output: fd 2 is an AF_UNIX stream, not an os.Pipe.
		pair, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
		if err != nil {
			t.Fatal(err)
		}
		stderr, journal := os.NewFile(uintptr(pair[0]), "stderr"), os.NewFile(uintptr(pair[1]), "journal")
		defer stderr.Close()
		t.Cleanup(func() { journal.Close() })
		cmd := exec.CommandContext(ctx, binary, "-n", "-F", "-y", fpmConfig())
		cmd.Stdout, cmd.Stderr = stderr, stderr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		return cmd, journal
	}

	generated := fpmConfigContents(worker.Username, group.Name, worker.Username)
	legacy := strings.Replace(generated, fpmLogging, "error_log = /proc/self/fd/2", 1)
	if err := os.WriteFile(fpmConfig(), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	old, journal := start(ctx)
	exitErr := old.Wait()
	output, err := io.ReadAll(journal)
	if err != nil || exitErr == nil || !strings.Contains(string(output), "failed to open error_log (/proc/self/fd/2)") {
		t.Fatalf("expected legacy FPM to fail reopening stderr: %v, %v, %s", exitErr, err, output)
	}
	t.Logf("legacy startup: %s", strings.TrimSpace(string(output)))
	if _, err := os.Stat(socket()); !os.IsNotExist(err) {
		t.Fatalf("legacy startup should leave no FPM socket: %v", err)
	}

	if err := ensureFPMLogging(); err != nil {
		t.Fatal(err)
	}
	conf, err := os.ReadFile(fpmConfig())
	if err != nil || string(conf) != generated {
		t.Fatalf("expected migration to match the generated configuration: %v", err)
	}
	ctx, cancelFixed := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelFixed()
	cmd, _ := start(ctx)
	done := make(chan error, 1)
	go func() {
		defer close(done)
		done <- cmd.Wait()
	}()
	defer func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		<-done
	}()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		conn, err := net.DialTimeout("unix", socket(), 100*time.Millisecond)
		if err == nil {
			conn.Close()
			t.Log("fixed PHP-FPM accepted a connection on its Unix socket")
			return
		}
		select {
		case exitErr := <-done:
			t.Fatalf("fixed PHP-FPM exited before creating its socket: %v", exitErr)
		case <-ctx.Done():
			t.Fatalf("fixed PHP-FPM socket not ready: %v", err)
		case <-ticker.C:
		}
	}
}
