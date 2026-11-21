// Package nativephp manages isolated PHP-FPM installations built from official releases.
package nativephp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"kunpanel/internal/config"
)

var minors = []string{"8.6", "8.5", "8.4", "8.3"}
var versionPattern = regexp.MustCompile(`^8\.[3-6]\.[0-9]+$`)
var digestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
var mu sync.Mutex
var client = &http.Client{Timeout: 3 * time.Minute, CheckRedirect: func(req *http.Request, via []*http.Request) error {
	if len(via) >= 5 || req.URL.Scheme != "https" || (req.URL.Hostname() != "www.php.net" && req.URL.Hostname() != "php.net") {
		return fmt.Errorf("拒绝非 PHP 官方站点的重定向")
	}
	return nil
}}

type Release struct {
	Minor    string `json:"minor"`
	Version  string `json:"version"`
	Filename string `json:"filename"`
	SHA256   string `json:"sha256"`
}
type releaseResponse struct {
	Version string `json:"version"`
	Source  []struct {
		Filename string `json:"filename"`
		SHA256   string `json:"sha256"`
	} `json:"source"`
}

// Minor validates and extracts the supported branch from a release or branch string.
func Minor(v string) (string, error) {
	v = strings.TrimSpace(v)
	for _, m := range minors {
		if v == m || (strings.HasPrefix(v, m+".") && versionPattern.MatchString(v)) {
			return m, nil
		}
	}
	return "", fmt.Errorf("仅支持 PHP 8.3、8.4、8.5 和 8.6")
}
func root(m string) string        { return filepath.Join(config.DataDir(), "native", "php", m) }
func Prefix(m string) string      { return filepath.Join(root(m), "current") }
func Binary(m string) string      { return filepath.Join(Prefix(m), "bin", "php") }
func FpmBinary(m string) string   { return filepath.Join(Prefix(m), "sbin", "php-fpm") }
func ConfigPath(m string) string  { return filepath.Join(root(m), "etc", "php-fpm.conf") }
func IniPath(m string) string     { return filepath.Join(root(m), "etc", "php.ini") }
func ScanDir(m string) string     { return filepath.Join(root(m), "conf.d") }
func Socket(m string) string      { return filepath.Join(root(m), "php-fpm.sock") }
func ServiceName(m string) string { return "kunpanel-php" + strings.ReplaceAll(m, ".", "") }

// WorkerUser is the unprivileged account used for panel-managed FPM pools.
func WorkerUser() string { return "nobody" }
func unitPath(m string) string {
	return filepath.Join("/etc/systemd/system", ServiceName(m)+".service")
}
func Installed(m string) bool {
	if _, e := Minor(m); e != nil {
		return false
	}
	fi, e := os.Stat(FpmBinary(m))
	return e == nil && fi.Mode().IsRegular()
}

// CurrentVersion reads the active version from the managed install path.
func CurrentVersion(m string) string {
	target, err := os.Readlink(Prefix(m))
	if err != nil || filepath.Dir(target) != filepath.Join(root(m), "versions") {
		return ""
	}
	version := filepath.Base(target)
	if !versionPattern.MatchString(version) || !strings.HasPrefix(version, m+".") {
		return ""
	}
	return version
}

func systemd() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	st, e := os.Stat("/run/systemd/system")
	return e == nil && st.IsDir()
}
func Status(m string) string {
	if !Installed(m) {
		return "missing"
	}
	if systemd() {
		if _, err := commandOutput(5*time.Second, "systemctl", "is-active", "--quiet", ServiceName(m)); err == nil {
			return "running"
		}
	}
	return "exited"
}
func run(w io.Writer, timeout time.Duration, dir, name string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	if dir != "" {
		// Keep compiler temporary files on the same filesystem as the build.
		cmd.Env = append(cmd.Environ(), "TMPDIR="+dir)
	}
	cmd.Stdout = w
	cmd.Stderr = w
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("%s 执行超时", name)
		}
		return fmt.Errorf("%s 执行失败: %w", name, err)
	}
	return nil
}
func commandOutput(timeout time.Duration, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if ctx.Err() != nil {
		return string(out), ctx.Err()
	}
	return string(out), err
}
func service(m, verb string) error {
	out, err := commandOutput(60*time.Second, "systemctl", verb, ServiceName(m))
	if err != nil {
		return fmt.Errorf("systemctl %s %s: %s (%w)", verb, ServiceName(m), strings.TrimSpace(out), err)
	}
	return nil
}
func Action(m, verb string) error {
	if _, err := Minor(m); err != nil {
		return err
	}
	if !systemd() || !Installed(m) {
		return fmt.Errorf("原生 PHP-FPM 未安装或不支持当前系统")
	}
	switch verb {
	case "start", "restart":
		if err := service(m, verb); err != nil {
			return err
		}
		return health(m)
	case "stop":
		return service(m, verb)
	case "reload":
		if err := run(io.Discard, 30*time.Second, "", FpmBinary(m), "-t", "-y", ConfigPath(m)); err != nil {
			return err
		}
		return service(m, "reload")
	default:
		return fmt.Errorf("不支持的操作")
	}
}

func fetch(url string, limit int64) ([]byte, error) {
	return fetchContext(context.Background(), url, limit)
}

func fetchContext(ctx context.Context, url string, limit int64) ([]byte, error) {
	return fetchWithClient(ctx, client, url, limit)
}

func fetchWithClient(ctx context.Context, hc *http.Client, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "kunpanel-native-php")
	res, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d", res.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("响应过大")
	}
	return b, nil
}
func parseRelease(minor string, body []byte) (Release, error) {
	var obj releaseResponse
	if err := json.Unmarshal(body, &obj); err != nil {
		return Release{}, err
	}
	if !versionPattern.MatchString(obj.Version) || !strings.HasPrefix(obj.Version, minor+".") {
		return Release{}, fmt.Errorf("官方版本不属于请求分支")
	}
	expected := "php-" + obj.Version + ".tar.gz"
	for _, s := range obj.Source {
		if s.Filename == expected && digestPattern.MatchString(strings.ToLower(s.SHA256)) {
			return Release{Minor: minor, Version: obj.Version, Filename: expected, SHA256: strings.ToLower(s.SHA256)}, nil
		}
	}
	return Release{}, fmt.Errorf("官方信息缺少源码 SHA-256")
}
func Latest(minor string) (Release, error) {
	m, err := Minor(minor)
	if err != nil || m != minor {
		return Release{}, fmt.Errorf("无效 PHP 分支")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	b, err := fetchContext(ctx, "https://www.php.net/releases/index.php?json&version="+url.QueryEscape(m), 2<<20)
	if err != nil {
		return Release{}, fmt.Errorf("获取 PHP %s 官方版本失败: %w", m, err)
	}
	return parseRelease(m, b)
}
func Versions() ([]Release, error) {
	type result struct {
		release Release
		err     error
	}
	results := make([]result, len(minors))
	var wg sync.WaitGroup
	for i, m := range minors {
		wg.Add(1)
		go func(index int, minor string) {
			defer wg.Done()
			results[index].release, results[index].err = Latest(minor)
		}(i, m)
	}
	wg.Wait()
	versions := make([]Release, 0, len(results))
	var failures []string
	for _, r := range results {
		if r.err != nil {
			failures = append(failures, r.err.Error())
			continue
		}
		versions = append(versions, r.release)
	}
	if len(versions) == 0 {
		return nil, fmt.Errorf("所有 PHP 官方版本查询失败: %s", strings.Join(failures, "; "))
	}
	return versions, nil
}
func dependencies(w io.Writer) error {
	if exec.Command("sh", "-c", "command -v gcc >/dev/null && command -v make >/dev/null && command -v pkg-config >/dev/null && command -v tar >/dev/null && pkg-config --exists libxml-2.0 openssl libcurl oniguruma libzip zlib libpng libjpeg freetype2 icu-uc libpq sqlite3").Run() == nil {
		return nil
	}
	fmt.Fprintln(w, "安装 PHP 编译所需工具和开发库（PHP 本体来自官方源码）...")
	return installDevPackages(w,
		[]string{"build-essential", "pkg-config", "ca-certificates", "tar", "libxml2-dev", "libssl-dev", "libcurl4-openssl-dev", "libonig-dev", "libzip-dev", "zlib1g-dev", "libpng-dev", "libjpeg-dev", "libfreetype6-dev", "libicu-dev", "libpq-dev", "libsqlite3-dev"},
		[]string{"gcc", "gcc-c++", "make", "pkgconf-pkg-config", "ca-certificates", "tar", "libxml2-devel", "openssl-devel", "libcurl-devel", "oniguruma-devel", "libzip-devel", "zlib-devel", "libpng-devel", "libjpeg-turbo-devel", "freetype-devel", "libicu-devel", "postgresql-devel", "sqlite-devel"})
}

func installDevPackages(w io.Writer, deb, rpm []string) error {
	var name string
	var args []string
	switch {
	case exists("apt-get"):
		if err := run(w, 10*time.Minute, "", "apt-get", "update"); err != nil {
			return err
		}
		name = "apt-get"
		args = append([]string{"install", "-y", "--no-install-recommends"}, deb...)
	case exists("dnf") || exists("yum"):
		name = "dnf"
		if !exists(name) {
			name = "yum"
		}
		args = append([]string{"install", "-y"}, rpm...)
	default:
		return fmt.Errorf("当前仅支持 Debian/Ubuntu、RHEL 系列自动准备 PHP 编译依赖")
	}
	return run(w, 25*time.Minute, "", name, args...)
}
func exists(name string) bool { _, err := exec.LookPath(name); return err == nil }
func checkedPath(s string) error {
	if !filepath.IsAbs(s) || strings.ContainsAny(s, "\r\n\x00\";[]%$") {
		return fmt.Errorf("PHP 路径包含不支持的字符: %s", s)
	}
	return nil
}
func phpUser() (string, string, error) {
	u, e := user.Lookup(WorkerUser())
	if e != nil {
		return "", "", fmt.Errorf("找不到 nobody 用户: %w", e)
	}
	g, e := user.LookupGroupId(u.Gid)
	if e != nil {
		return "", "", e
	}
	return u.Username, g.Name, nil
}
func configContents(m, usr, group string) (string, error) {
	for _, s := range []string{root(m), Socket(m)} {
		if e := checkedPath(s); e != nil {
			return "", e
		}
	}
	if len(Socket(m)) > 100 {
		return "", fmt.Errorf("PHP-FPM socket 路径过长")
	}
	return fmt.Sprintf("[global]\ndaemonize = no\nerror_log = %s\n[www]\nuser = %s\ngroup = %s\nlisten = %s\nlisten.owner = %s\nlisten.group = %s\nlisten.mode = 0660\npm = dynamic\npm.max_children = 10\npm.start_servers = 2\npm.min_spare_servers = 1\npm.max_spare_servers = 3\nclear_env = yes\n", filepath.Join(root(m), "php-fpm.log"), usr, group, Socket(m), usr, group), nil
}
func writeConfig(m, sourceDir string) error {
	for _, d := range []string{root(m), filepath.Dir(ConfigPath(m)), ScanDir(m)} {
		if e := os.MkdirAll(d, 0755); e != nil {
			return e
		}
	}
	if _, e := os.Stat(ConfigPath(m)); os.IsNotExist(e) {
		u, g, e := phpUser()
		if e != nil {
			return e
		}
		v, e := configContents(m, u, g)
		if e != nil {
			return e
		}
		if e = os.WriteFile(ConfigPath(m), []byte(v), 0644); e != nil {
			return e
		}
	} else if e != nil {
		return e
	}
	if _, e := os.Stat(IniPath(m)); os.IsNotExist(e) {
		b, e := os.ReadFile(filepath.Join(sourceDir, "php.ini-production"))
		if e != nil {
			return e
		}
		if e = os.WriteFile(IniPath(m), b, 0644); e != nil {
			return e
		}
	} else if e != nil {
		return e
	}
	return nil
}
func writeUnit(m string) error {
	for _, p := range []string{FpmBinary(m), ConfigPath(m)} {
		if e := checkedPath(p); e != nil {
			return e
		}
	}
	path := unitPath(m)
	marker := "Description=KunPanel native PHP-FPM " + m
	if b, e := os.ReadFile(path); e == nil && !strings.Contains(string(b), marker) {
		return fmt.Errorf("服务文件已存在且不属于面板: %s", path)
	} else if e != nil && !os.IsNotExist(e) {
		return e
	}
	content := fmt.Sprintf("[Unit]\n%s\nAfter=network.target\n\n[Service]\nType=simple\nExecStartPre=%s -t -y %s\nExecStart=%s -F -y %s\nExecReload=/bin/kill -USR2 $MAINPID\nRestart=on-failure\n\n[Install]\nWantedBy=multi-user.target\n", marker, strconv.Quote(FpmBinary(m)), strconv.Quote(ConfigPath(m)), strconv.Quote(FpmBinary(m)), strconv.Quote(ConfigPath(m)))
	return os.WriteFile(path, []byte(content), 0644)
}

var extensionLine = regexp.MustCompile(`(?im)^\s*(zend_extension|extension)\s*=`)

const opcacheIniName = "00-kunpanel-opcache.ini"
const opcacheIniContent = "; Managed by KunPanel; extension is resolved from this PHP build.\nzend_extension=opcache.so\n"

func containsDynamicExtensions(m string) (bool, error) {
	paths := []string{IniPath(m)}
	entries, err := os.ReadDir(ScanDir(m))
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".ini") {
			paths = append(paths, filepath.Join(ScanDir(m), e.Name()))
		}
	}
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return false, err
		}
		if filepath.Base(p) == opcacheIniName && string(b) == opcacheIniContent {
			continue
		}
		if _, ok := parseManagedExtension(m, p, b); ok {
			continue
		}
		if extensionLine.Match(b) {
			return true, nil
		}
	}
	return false, nil
}

func usesSharedOpcache(m string) bool {
	return m == "8.3" || m == "8.4"
}

func ensureOpcache(m string) error {
	path := filepath.Join(ScanDir(m), opcacheIniName)
	if b, err := os.ReadFile(path); err == nil {
		if string(b) != opcacheIniContent {
			return fmt.Errorf("面板 OPcache 配置已被修改，请检查 %s", path)
		}
		if !usesSharedOpcache(m) {
			return os.Remove(path)
		}
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	if !usesSharedOpcache(m) {
		return nil
	}
	return os.WriteFile(path, []byte(opcacheIniContent), 0o644)
}
func symlinkSwap(path, target string) error {
	temp := path + ".new"
	_ = os.Remove(temp)
	if err := os.Symlink(target, temp); err != nil {
		return err
	}
	if err := os.Rename(temp, path); err != nil {
		_ = os.Remove(temp)
		return err
	}
	return nil
}
func health(m string) error {
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if Status(m) != "running" {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		st, err := os.Stat(Socket(m))
		if err == nil && st.Mode()&os.ModeSocket != 0 {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("PHP-FPM 未正常启动或 socket 未就绪")
}

// Validate real runtime output, not merely configure flags: failed shared
// extension loading can otherwise leave PHP running with missing functionality.
func validateModules(output string) error {
	loaded := make(map[string]bool)
	for _, line := range strings.Split(output, "\n") {
		loaded[strings.ToLower(strings.TrimSpace(line))] = true
	}
	for _, name := range []string{"zend opcache", "mysqli", "pdo_mysql", "pgsql", "pdo_pgsql", "curl", "mbstring", "xml", "zip", "gd", "intl", "bcmath", "sockets"} {
		if !loaded[name] {
			return fmt.Errorf("PHP 扩展未加载: %s", name)
		}
	}
	return nil
}

func createBuildDir(minor string) (string, error) {
	dir := root(minor)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return os.MkdirTemp(dir, ".build-")
}

// Install verifies selected official release and compiles a private PHP-FPM service.
func Install(minor, version string, w io.Writer) error {
	mu.Lock()
	defer mu.Unlock()
	if w == nil {
		w = io.Discard
	}
	m, err := Minor(minor)
	if err != nil || m != minor {
		return fmt.Errorf("无效 PHP 版本分支")
	}
	if !systemd() {
		return fmt.Errorf("原生 PHP 当前仅支持 Linux systemd")
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("原生 PHP 安装需要 root 权限")
	}
	if !versionPattern.MatchString(version) || !strings.HasPrefix(version, m+".") {
		return fmt.Errorf("无效 PHP 版本")
	}
	rel, err := Latest(m)
	if err != nil {
		return err
	}
	if rel.Version != version {
		return fmt.Errorf("版本不是官方当前版本，请刷新后重试")
	}
	for _, p := range []string{root(m), ConfigPath(m), Socket(m), IniPath(m), ScanDir(m)} {
		if err := checkedPath(p); err != nil {
			return err
		}
	}
	if len(Socket(m)) > 100 {
		return fmt.Errorf("PHP-FPM socket 路径过长")
	}
	if _, _, err := phpUser(); err != nil {
		return err
	}
	if unit, err := os.ReadFile(unitPath(m)); err == nil {
		if !strings.Contains(string(unit), "Description=KunPanel native PHP-FPM "+m) {
			return fmt.Errorf("服务文件已存在且不属于面板: %s", unitPath(m))
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	installed := Installed(m)
	running := Status(m) == "running"
	old, readErr := os.Readlink(Prefix(m))
	if installed && readErr != nil {
		return fmt.Errorf("现有 PHP 版本路径不属于面板管理")
	}
	if installed && filepath.Base(old) == version {
		return fmt.Errorf("PHP %s 已安装", version)
	}
	if installed {
		dynamic, e := containsDynamicExtensions(m)
		if e != nil {
			return e
		}
		if dynamic {
			return fmt.Errorf("现有 PHP 配置含动态扩展，请先确认扩展与新版本兼容并完成迁移，当前不支持自动升级")
		}
	}
	if !installed {
		if _, e := os.Lstat(Prefix(m)); e == nil {
			return fmt.Errorf("PHP 目录已存在，请先检查: %s", Prefix(m))
		} else if !os.IsNotExist(e) {
			return e
		}
	}
	target := filepath.Join(root(m), "versions", version)
	if _, e := os.Lstat(target); e == nil {
		return fmt.Errorf("PHP 目标版本目录已存在: %s", target)
	} else if !os.IsNotExist(e) {
		return e
	}
	if err := dependencies(w); err != nil {
		return err
	}
	work, err := createBuildDir(m)
	if err != nil {
		return fmt.Errorf("创建 PHP 编译目录失败: %w", err)
	}
	defer os.RemoveAll(work)
	fmt.Fprintln(w, "PHP 编译目录:", work)
	fmt.Fprintf(w, "下载 PHP 官方源码 %s ...\n", version)
	src, err := fetch("https://www.php.net/distributions/"+rel.Filename, 100<<20)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(src)
	if hex.EncodeToString(sum[:]) != rel.SHA256 {
		return fmt.Errorf("PHP 官方源码 SHA-256 校验失败")
	}
	archive := filepath.Join(work, rel.Filename)
	if err = os.WriteFile(archive, src, 0600); err != nil {
		return err
	}
	fmt.Fprintln(w, "官方源码 SHA-256 校验通过")
	if err = run(w, 3*time.Minute, work, "tar", "-xzf", archive, "--no-same-owner"); err != nil {
		return err
	}
	srcDir := filepath.Join(work, "php-"+version)
	if err = os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		return err
	}
	success := false
	defer func() {
		if !success {
			selected, _ := os.Readlink(Prefix(m))
			if selected != target {
				_ = os.RemoveAll(target)
			}
		}
	}()
	fmt.Fprintln(w, "编译 PHP-FPM 和常用扩展，可能需要数分钟...")
	args := []string{
		"--prefix=" + target,
		"--with-config-file-path=" + filepath.Dir(IniPath(m)),
		"--with-config-file-scan-dir=" + ScanDir(m),
		"--enable-fpm", "--enable-mbstring", "--enable-bcmath", "--enable-exif",
		"--enable-intl", "--enable-soap", "--enable-sockets", "--enable-gd",
		"--with-jpeg", "--with-freetype", "--with-curl", "--with-openssl",
		"--with-zlib", "--with-zip", "--with-mysqli=mysqlnd", "--with-pdo-mysql=mysqlnd",
		"--with-pgsql", "--with-pdo-pgsql",
	}
	if usesSharedOpcache(m) {
		args = append(args, "--enable-opcache")
	}
	if err = run(w, 10*time.Minute, srcDir, "./configure", args...); err != nil {
		return err
	}
	jobs := runtime.NumCPU()
	if jobs > 2 {
		jobs = 2
	}
	if jobs < 1 {
		jobs = 1
	}
	if err = run(w, 60*time.Minute, srcDir, "make", "-j", strconv.Itoa(jobs)); err != nil {
		return err
	}
	if err = run(w, 10*time.Minute, srcDir, "make", "install"); err != nil {
		return err
	}
	if err = rebuildManagedExtensions(m, target, w); err != nil {
		return fmt.Errorf("重编 PHP 扩展失败: %w", err)
	}
	if err = writeConfig(m, srcDir); err != nil {
		return err
	}
	if err = ensureOpcache(m); err != nil {
		return err
	}
	if err = symlinkSwap(Prefix(m), target); err != nil {
		return err
	}
	rollback := func() error {
		if old == "" {
			return os.Remove(Prefix(m))
		}
		return symlinkSwap(Prefix(m), old)
	}
	failed := func(e error) error {
		if re := rollback(); re != nil {
			return fmt.Errorf("%v；恢复旧版本失败: %w", e, re)
		}
		return e
	}
	modules, err := commandOutput(30*time.Second, Binary(m), "-m")
	if err != nil {
		return failed(fmt.Errorf("检查 PHP 扩展失败: %s (%w)", strings.TrimSpace(modules), err))
	}
	if err = validateModules(modules); err != nil {
		return failed(fmt.Errorf("检查 PHP 扩展失败: %w\n%s", err, strings.TrimSpace(modules)))
	}
	if err = verifyManagedExtensions(m); err != nil {
		return failed(err)
	}
	if err = run(w, 30*time.Second, "", FpmBinary(m), "-t", "-y", ConfigPath(m)); err != nil {
		return failed(fmt.Errorf("PHP-FPM 配置校验失败: %w", err))
	}
	if err = writeUnit(m); err != nil {
		return failed(err)
	}
	cleanup := func() {
		if !installed {
			_ = exec.Command("systemctl", "disable", "--now", ServiceName(m)).Run()
			_ = os.Remove(unitPath(m))
			_ = exec.Command("systemctl", "daemon-reload").Run()
		}
	}
	if err = run(w, 30*time.Second, "", "systemctl", "daemon-reload"); err != nil {
		cleanup()
		return failed(err)
	}
	if !installed {
		if err = run(w, 30*time.Second, "", "systemctl", "enable", ServiceName(m)); err != nil {
			cleanup()
			return failed(err)
		}
	}
	if installed && !running {
		success = true
		fmt.Fprintln(w, "PHP 升级完成，服务保持停止:", version)
		return nil
	}
	verb := "start"
	if running {
		verb = "restart"
	}
	if err = service(m, verb); err == nil {
		err = health(m)
	}
	if err != nil {
		cleanup()
		res := failed(fmt.Errorf("PHP-FPM 启动失败: %w", err))
		if running {
			if e := service(m, "restart"); e != nil {
				return fmt.Errorf("%v；旧服务恢复失败: %w", res, e)
			}
			if e := health(m); e != nil {
				return fmt.Errorf("%v；旧服务重启后健康检查失败: %w", res, e)
			}
		}
		return res
	}
	success = true
	fmt.Fprintln(w, "原生 PHP-FPM 安装完成:", version)
	return nil
}

// Uninstall only stops and removes the panel-owned service and program. Config/data are preserved.
func Uninstall(minor string, w io.Writer) error {
	mu.Lock()
	defer mu.Unlock()
	m, err := Minor(minor)
	if err != nil || m != minor {
		return fmt.Errorf("无效 PHP 分支")
	}
	if w == nil {
		w = io.Discard
	}
	if !systemd() {
		return fmt.Errorf("原生 PHP 当前仅支持 Linux systemd")
	}
	if !Installed(m) {
		return fmt.Errorf("未安装原生 PHP")
	}
	unit, err := os.ReadFile(unitPath(m))
	if err != nil || !strings.Contains(string(unit), "Description=KunPanel native PHP-FPM "+m) {
		return fmt.Errorf("无法确认服务归属，拒绝卸载")
	}
	if err = run(w, 60*time.Second, "", "systemctl", "disable", "--now", ServiceName(m)); err != nil {
		return err
	}
	if err = os.Remove(unitPath(m)); err != nil {
		return err
	}
	if err = run(w, 30*time.Second, "", "systemctl", "daemon-reload"); err != nil {
		return err
	}
	if err = os.Remove(Prefix(m)); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err = os.RemoveAll(filepath.Join(root(m), "versions")); err != nil {
		return err
	}
	fmt.Fprintln(w, "已卸载原生 PHP-FPM，配置和网站文件已保留")
	return nil
}
