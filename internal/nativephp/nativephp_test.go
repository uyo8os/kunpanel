package nativephp

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestBuildDoesNotUseSystemTemp(t *testing.T) {
	dataDir := t.TempDir()
	systemTemp := filepath.Join(dataDir, "unavailable-system-temp")
	t.Setenv("KP_DATA_DIR", dataDir)
	t.Setenv("TMPDIR", systemTemp)
	t.Setenv("TEMP", systemTemp)
	t.Setenv("TMP", systemTemp)
	work, err := createBuildDir("8.4")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dataDir, "native", "php", "8.4"); filepath.Dir(work) != want {
		t.Fatalf("build directory = %s, want a child of %s", work, want)
	}
	if runtime.GOOS != "linux" {
		return
	}
	source := filepath.Join(work, "source")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := run(&out, 5*time.Second, source, "sh", "-c", `printf '%s' "$TMPDIR"`); err != nil {
		t.Fatalf("run build command: %v: %s", err, out.String())
	}
	if out.String() != source {
		t.Fatalf("build command TMPDIR = %q, want %q", out.String(), source)
	}
}

type releaseRoundTripper func(*http.Request) (*http.Response, error)

func (f releaseRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestVersionsFetchesBranchesConcurrentlyAndOmitsFailures(t *testing.T) {
	previous := client
	defer func() { client = previous }()
	var requests atomic.Int32
	ready := make(chan struct{})
	client = &http.Client{Transport: releaseRoundTripper(func(r *http.Request) (*http.Response, error) {
		if requests.Add(1) == int32(len(minors)) {
			close(ready)
		}
		select {
		case <-ready:
		case <-time.After(time.Second):
			return nil, fmt.Errorf("branch requests did not run concurrently")
		}
		minor := r.URL.Query().Get("version")
		status := http.StatusOK
		if minor == "8.5" {
			status = http.StatusServiceUnavailable
		}
		version := minor + ".2"
		body := fmt.Sprintf(`{"version":%q,"source":[{"filename":%q,"sha256":%q}]}`, version, "php-"+version+".tar.gz", strings.Repeat("a", 64))
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}
	got, err := Versions()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].Minor != "8.6" || got[1].Minor != "8.4" || got[2].Minor != "8.3" {
		t.Fatalf("unexpected partial release list: %+v", got)
	}
}

func TestVersionsFailsWhenAllBranchesUnavailable(t *testing.T) {
	previous := client
	defer func() { client = previous }()
	client = &http.Client{Transport: releaseRoundTripper(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusServiceUnavailable, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
	})}
	versions, err := Versions()
	if err == nil || len(versions) != 0 {
		t.Fatalf("expected failure with no official versions, got %+v, %v", versions, err)
	}
}

func TestReleaseValidation(t *testing.T) {
	checksum := strings.Repeat("a", 64)
	data := []byte(fmt.Sprintf(`{"version":"8.5.13","source":[{"filename":"php-8.5.13.tar.gz","sha256":"%s"}]}`, checksum))
	rel, err := parseRelease("8.5", data)
	if err != nil || rel.Version != "8.5.13" || rel.SHA256 != checksum {
		t.Fatalf("bad release: %+v %v", rel, err)
	}
	if _, err = parseRelease("8.4", data); err == nil {
		t.Fatal("accepted wrong branch")
	}
	if _, err = parseRelease("8.5", []byte(`{"version":"8.5.13","source":[{"filename":"../../x","sha256":"a"}]}`)); err == nil {
		t.Fatal("accepted invalid source")
	}
	for _, bad := range []string{"8.2", "8.7", "8.5/../../etc", "8.5.1RC1"} {
		if _, err = Minor(bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}
func TestDynamicExtensionCheckAndConfig(t *testing.T) {
	t.Setenv("KP_DATA_DIR", t.TempDir())
	m := "8.4"
	if err := os.MkdirAll(ScanDir(m), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(IniPath(m)), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(IniPath(m), []byte(";extension=redis\n memory_limit=128M\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := ensureOpcache(m); err != nil {
		t.Fatal(err)
	}
	dynamic, err := containsDynamicExtensions(m)
	if err != nil || dynamic {
		t.Fatalf("comment incorrectly treated as extension: %v %v", dynamic, err)
	}
	if err := os.WriteFile(filepath.Join(ScanDir(m), "redis.ini"), []byte("extension=redis.so\n"), 0644); err != nil {
		t.Fatal(err)
	}
	dynamic, err = containsDynamicExtensions(m)
	if err != nil || !dynamic {
		t.Fatalf("extension not detected: %v %v", dynamic, err)
	}
}

func TestBuiltInOpcacheDoesNotLoadSharedExtension(t *testing.T) {
	t.Setenv("KP_DATA_DIR", t.TempDir())
	for _, m := range []string{"8.5", "8.6"} {
		if usesSharedOpcache(m) {
			t.Fatalf("PHP %s should not build shared OPcache", m)
		}
		if err := os.MkdirAll(ScanDir(m), 0755); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(ScanDir(m), opcacheIniName)
		if err := os.WriteFile(path, []byte(opcacheIniContent), 0644); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 2; i++ {
			if err := ensureOpcache(m); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("PHP %s retained shared OPcache configuration: %v", m, err)
			}
		}
	}
}
func TestConfigIsPrivate(t *testing.T) {
	t.Setenv("KP_DATA_DIR", t.TempDir())
	conf, err := configContents("8.4", "nobody", "nogroup")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"listen = " + Socket("8.4"), "listen.mode = 0660", "user = nobody"} {
		if !strings.Contains(conf, s) {
			t.Fatalf("missing %q", s)
		}
	}
	if strings.Contains(conf, "0.0.0.0") {
		t.Fatal("FPM is public")
	}
}

func TestValidateModulesRequiresActualLoadedModule(t *testing.T) {
	modules := "[PHP Modules]\nmysqli\npdo_mysql\npgsql\npdo_pgsql\ncurl\nmbstring\nxml\nzip\ngd\nintl\nbcmath\nsockets\n[Zend Modules]\nZend OPcache\n"
	if err := validateModules(modules); err != nil {
		t.Fatal(err)
	}
	if err := validateModules(strings.Replace(modules, "mbstring\n", "warning: unable to load mbstring\n", 1)); err == nil {
		t.Fatal("startup warning was mistaken for a loaded module")
	}
}
