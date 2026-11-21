package nativephp

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// Run only in an isolated disposable Linux environment with a systemctl stub.
func TestNativePHPIntegration(t *testing.T) {
	if os.Getenv("KP_NATIVE_PHP_E2E") != "1" {
		t.Skip("set KP_NATIVE_PHP_E2E=1 in isolated Linux test container")
	}
	if !systemd() || os.Geteuid() != 0 {
		t.Fatal("requires Linux root and systemctl stub")
	}
	m := os.Getenv("KP_NATIVE_PHP_BRANCH")
	if m == "" {
		m = "8.4"
	}
	if _, err := os.Lstat(unitPath(m)); !os.IsNotExist(err) {
		t.Fatalf("service already exists: %v", err)
	}
	dir, err := os.MkdirTemp("/tmp", "kunpanel-php-e2e-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err = os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KP_DATA_DIR", dir)
	rel, err := Latest(m)
	if err != nil {
		t.Fatal(err)
	}
	if err = Install(m, rel.Version, os.Stdout); err != nil {
		t.Fatal(err)
	}
	installed := true
	t.Cleanup(func() {
		if installed {
			if e := Uninstall(m, os.Stdout); e != nil {
				t.Errorf("cleanup: %v", e)
			}
		}
	})
	modules, err := commandOutput(30*time.Second, Binary(m), "-m")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Zend OPcache", "mysqli", "pdo_mysql", "pgsql", "pdo_pgsql", "mbstring", "curl", "gd", "intl", "zip"} {
		if !strings.Contains(strings.ToLower(modules), strings.ToLower(want)) {
			t.Errorf("missing %s in %s", want, modules)
		}
	}
	if names := os.Getenv("KP_NATIVE_PHP_E2E_EXTENSIONS"); names != "" {
		testNativePHPExtensions(t, m, dir, strings.Split(names, ","))
	}
	testNativePHPDisabledFunctions(t, m, dir)
	script := filepath.Join(dir, "index.php")
	if err = os.WriteFile(script, []byte(`<?php echo "kunpanel-php-fpm:" . PHP_VERSION;`), 0644); err != nil {
		t.Fatal(err)
	}
	body, err := fpmRequest(Socket(m), script)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "kunpanel-php-fpm:"+rel.Version) {
		t.Fatalf("unexpected FastCGI output: %s", body)
	}
	if err = Action(m, "restart"); err != nil {
		t.Fatal(err)
	}
	if err = health(m); err != nil {
		t.Fatal(err)
	}
	body, err = fpmRequest(Socket(m), script)
	if err != nil || !strings.Contains(body, "kunpanel-php-fpm:") {
		t.Fatalf("post restart request %s %v", body, err)
	}
	if err = Uninstall(m, os.Stdout); err != nil {
		t.Fatal(err)
	}
	installed = false
	if _, err = os.Stat(script); err != nil {
		t.Fatalf("site removed: %v", err)
	}
	if _, err = os.Stat(IniPath(m)); err != nil {
		t.Fatalf("config removed: %v", err)
	}
}

func testNativePHPDisabledFunctions(t *testing.T, minor, dir string) {
	t.Helper()
	ini := IniPath(minor)
	old, err := os.ReadFile(ini)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ini, append(slices.Clone(old), "\n[PHP]\ndisable_functions = strrev\n"...), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.WriteFile(ini, old, 0o644) })
	got, err := DisabledFunctions(minor)
	if err != nil || !slices.Equal(got, []string{"strrev"}) {
		t.Fatalf("existing php.ini functions: %v (%v)", got, err)
	}
	got, restarted, err := SetDisabledFunctions(minor, "STRREV,phpinfo")
	if err != nil || !restarted || !slices.Equal(got, []string{"strrev", "phpinfo"}) {
		t.Fatalf("disable functions: %v, restart %v (%v)", got, restarted, err)
	}
	script := filepath.Join(dir, "functions.php")
	if err := os.WriteFile(script, []byte(`<?php echo function_exists('strrev') ? 'function-available' : 'function-disabled';`), 0o644); err != nil {
		t.Fatal(err)
	}
	body, err := fpmRequest(Socket(minor), script)
	if err != nil || !strings.Contains(body, "function-disabled") {
		t.Fatalf("expected an unavailable function: %s (%v)", body, err)
	}
	managed := filepath.Join(ScanDir(minor), disabledFunctionsINI)
	before, err := os.ReadFile(managed)
	if err != nil {
		t.Fatal(err)
	}
	override := filepath.Join(ScanDir(minor), "zz-test-disabled-functions.ini")
	if err := os.WriteFile(override, []byte("disable_functions = str_rot13\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := SetDisabledFunctions(minor, "str_shuffle"); err == nil {
		t.Fatal("saved a list overridden by another configuration file")
	}
	after, err := os.ReadFile(managed)
	if err != nil || !slices.Equal(before, after) {
		t.Fatalf("could not restore original disabled functions: %v", err)
	}
	if err := os.Remove(override); err != nil {
		t.Fatal(err)
	}
	if _, restarted, err = SetDisabledFunctions(minor, ""); err != nil || !restarted {
		t.Fatalf("clear disabled functions: restart %v (%v)", restarted, err)
	}
	body, err = fpmRequest(Socket(minor), script)
	if err != nil || !strings.Contains(body, "function-available") {
		t.Fatalf("expected a restored function: %s (%v)", body, err)
	}
}

func testNativePHPExtensions(t *testing.T, minor, dir string, names []string) {
	t.Helper()
	for _, name := range names {
		if err := ManageExtension(minor, name, false, os.Stdout); err != nil {
			t.Fatalf("install %s: %v", name, err)
		}
	}
	script := filepath.Join(dir, "extensions.php")
	if err := os.WriteFile(script, []byte(`<?php echo implode(',', get_loaded_extensions());`), 0o644); err != nil {
		t.Fatal(err)
	}
	body, err := fpmRequest(Socket(minor), script)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		if !strings.Contains(strings.ToLower(body), ","+name) {
			t.Fatalf("FPM did not load %s: %s", name, body)
		}
	}
	if err := ManageExtension(minor, names[0], false, os.Stdout); err != nil {
		t.Fatalf("check update %s: %v", names[0], err)
	}
	if err := ManageExtension(minor, names[0], true, os.Stdout); err != nil {
		t.Fatalf("remove %s: %v", names[0], err)
	}
	body, err = fpmRequest(Socket(minor), script)
	if err != nil || strings.Contains(strings.ToLower(body), ","+names[0]) {
		t.Fatalf("FPM still loads %s after removal: %s (%v)", names[0], body, err)
	}
}
func fpmRecord(c net.Conn, typ byte, payload []byte) error {
	h := make([]byte, 8)
	h[0] = 1
	h[1] = typ
	binary.BigEndian.PutUint16(h[2:], 1)
	binary.BigEndian.PutUint16(h[4:], uint16(len(payload)))
	if _, e := c.Write(h); e != nil {
		return e
	}
	if len(payload) > 0 {
		_, e := c.Write(payload)
		return e
	}
	return nil
}
func fpmPair(name, value string) []byte {
	var b []byte
	put := func(n int) {
		if n < 128 {
			b = append(b, byte(n))
		} else {
			b = append(b, byte(n>>24)|128, byte(n>>16), byte(n>>8), byte(n))
		}
	}
	put(len(name))
	put(len(value))
	b = append(b, name...)
	b = append(b, value...)
	return b
}
func fpmRequest(socket, script string) (string, error) {
	c, e := net.DialTimeout("unix", socket, 5*time.Second)
	if e != nil {
		return "", e
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	begin := []byte{0, 1, 0, 0, 0, 0, 0, 0}
	if e = fpmRecord(c, 1, begin); e != nil {
		return "", e
	}
	var params []byte
	for k, v := range map[string]string{"SCRIPT_FILENAME": script, "SCRIPT_NAME": "/index.php", "REQUEST_METHOD": "GET", "SERVER_PROTOCOL": "HTTP/1.1", "GATEWAY_INTERFACE": "CGI/1.1", "SERVER_NAME": "localhost", "SERVER_PORT": "80", "REMOTE_ADDR": "127.0.0.1", "QUERY_STRING": ""} {
		params = append(params, fpmPair(k, v)...)
	}
	for _, rec := range []struct {
		t byte
		p []byte
	}{{4, params}, {4, nil}, {5, nil}} {
		if e = fpmRecord(c, rec.t, rec.p); e != nil {
			return "", e
		}
	}
	var out strings.Builder
	for {
		h := make([]byte, 8)
		if _, e = io.ReadFull(c, h); e != nil {
			return out.String(), e
		}
		n := int(binary.BigEndian.Uint16(h[4:]))
		b := make([]byte, n+int(h[6]))
		if _, e = io.ReadFull(c, b); e != nil {
			return out.String(), e
		}
		switch h[1] {
		case 6:
			out.Write(b[:n])
		case 7:
			if n > 0 {
				out.Write(b[:n])
			}
		case 3:
			return out.String(), nil
		default:
			if h[1] == 0 {
				return out.String(), fmt.Errorf("bad FastCGI response")
			}
		}
	}
}
