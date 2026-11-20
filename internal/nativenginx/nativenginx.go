// Package nativenginx installs and manages an NGINX instance built from official sources.
package nativenginx

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"kunpanel/internal/config"
)

const ServiceName = "kunpanel-nginx"
const unitPath = "/etc/systemd/system/kunpanel-nginx.service"
const releasePage = "https://nginx.org/en/download.html"

// Version is a currently published official release.
type Version struct {
	Version string `json:"version"`
	Channel string `json:"channel"`
}

var (
	installMu    sync.Mutex
	validVersion = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
	htmlVersion  = regexp.MustCompile(`(?is)nginx-([0-9]+\.[0-9]+\.[0-9]+)\.tar\.gz`)
	nextHeading  = regexp.MustCompile(`(?i)<h[1-6](?:\s|>)`)
	webClient    = &http.Client{Timeout: 45 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 || req.URL.Scheme != "https" || req.URL.Hostname() != "nginx.org" {
			return fmt.Errorf("拒绝非 nginx.org 的下载重定向")
		}
		return nil
	}}
)

func root() string       { return filepath.Join(config.DataDir(), "native", "nginx") }
func Prefix() string     { return filepath.Join(root(), "current") }
func Binary() string     { return filepath.Join(Prefix(), "sbin", "nginx") }
func ConfigPath() string { return filepath.Join(root(), "nginx.conf") }
func Installed() bool    { info, err := os.Stat(Binary()); return err == nil && !info.IsDir() }

// Versions reads release channels from nginx.org rather than a bundled version list.
func Versions() ([]Version, error) {
	b, err := fetch(releasePage, 2<<20)
	if err != nil {
		return nil, fmt.Errorf("获取 Nginx 官方版本失败: %w", err)
	}
	return parseVersions(string(b))
}

func parseVersions(page string) ([]Version, error) {
	low := strings.ToLower(page)
	mainIdx := strings.Index(low, "mainline version")
	stableIdx := strings.Index(low, "stable version")
	if mainIdx < 0 || stableIdx <= mainIdx {
		return nil, fmt.Errorf("无法识别 Nginx 官方版本页面")
	}
	stableEnd := len(page)
	if loc := nextHeading.FindStringIndex(page[stableIdx:]); loc != nil {
		stableEnd = stableIdx + loc[0]
	}
	sections := []struct{ channel, body string }{{"mainline", page[mainIdx:stableIdx]}, {"stable", page[stableIdx:stableEnd]}}
	result := make([]Version, 0, 2)
	for _, section := range sections {
		m := htmlVersion.FindStringSubmatch(section.body)
		if len(m) != 2 {
			return nil, fmt.Errorf("无法识别官方 %s 版本", section.channel)
		}
		result = append(result, Version{Version: m[1], Channel: section.channel})
	}
	return result, nil
}

func fetch(url string, limit int64) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "kunpanel-native-nginx")
	resp, err := webClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("下载文件过大")
	}
	return b, nil
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

// Record service diagnostics before cleanup removes the unit or changes its state.
// Both commands are best-effort and time-limited so diagnostics cannot block rollback.
func logServiceDiagnostics(w io.Writer) {
	commands := []struct {
		name string
		args []string
	}{
		{"systemctl", []string{"status", "--no-pager", "--full", "--lines=30", ServiceName}},
		{"journalctl", []string{"-u", ServiceName, "-n", "50", "--no-pager", "-o", "short"}},
	}
	for _, c := range commands {
		fmt.Fprintf(w, "\n---- %s %s ----\n", c.name, strings.Join(c.args, " "))
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		cmd := exec.CommandContext(ctx, c.name, c.args...)
		// A child retaining a redirected output pipe must not stall cleanup.
		cmd.WaitDelay = time.Second
		cmd.Stdout, cmd.Stderr = w, w
		err := cmd.Run()
		if ctx.Err() != nil {
			fmt.Fprintf(w, "%s 诊断超时\n", c.name)
		} else if err != nil {
			fmt.Fprintf(w, "%s 诊断命令结束: %v\n", c.name, err)
		}
		cancel()
	}
}

func systemd() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	_, err := os.Stat("/run/systemd/system")
	return err == nil
}
func action(action string) error {
	if !systemd() {
		return fmt.Errorf("原生 Nginx 当前仅支持 Linux systemd")
	}
	out, err := exec.Command("systemctl", action, ServiceName).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl %s: %s (%v)", action, strings.TrimSpace(string(out)), err)
	}
	return nil
}

// Action controls the panel-owned service.
func Action(name string) error {
	if !Installed() {
		return fmt.Errorf("尚未安装原生 Nginx")
	}
	switch name {
	case "start", "stop", "restart":
		return action(name)
	case "reload":
		if err := run(io.Discard, 30*time.Second, "", Binary(), "-p", Prefix()+"/", "-c", ConfigPath(), "-t"); err != nil {
			return err
		}
		return action("reload")
	default:
		return fmt.Errorf("不支持的操作: %s", name)
	}
}
func Status() string {
	if !Installed() {
		return "missing"
	}
	if systemd() && exec.Command("systemctl", "is-active", "--quiet", ServiceName).Run() == nil {
		return "running"
	}
	return "exited"
}

func dependencies(w io.Writer) error {
	// Packages are build prerequisites; the Nginx program itself is compiled from nginx.org.
	if exec.Command("sh", "-c", "command -v gcc >/dev/null && command -v make >/dev/null && command -v gpg >/dev/null && command -v tar >/dev/null && command -v pkg-config >/dev/null && pkg-config --exists openssl zlib libpcre2-8").Run() == nil {
		return nil
	}
	fmt.Fprintln(w, "安装编译依赖（Nginx 本体使用官方源码）...")
	var name string
	var args []string
	switch {
	case exists("apt-get"):
		if err := run(w, 10*time.Minute, "", "apt-get", "update"); err != nil {
			return err
		}
		name = "apt-get"
		args = []string{"install", "-y", "--no-install-recommends", "build-essential", "libpcre2-dev", "zlib1g-dev", "libssl-dev", "pkg-config", "gnupg", "tar", "ca-certificates"}
	case exists("dnf"):
		name = "dnf"
		args = []string{"install", "-y", "gcc", "make", "pcre2-devel", "zlib-devel", "openssl-devel", "pkgconf-pkg-config", "gnupg2", "tar", "ca-certificates"}
	case exists("yum"):
		name = "yum"
		args = []string{"install", "-y", "gcc", "make", "pcre2-devel", "zlib-devel", "openssl-devel", "pkgconfig", "gnupg2", "tar", "ca-certificates"}
	case exists("apk"):
		name = "apk"
		args = []string{"add", "build-base", "pcre2-dev", "zlib-dev", "openssl-dev", "pkgconf", "gnupg", "tar", "ca-certificates"}
	default:
		return fmt.Errorf("缺少 gcc、make、gpg 或 OpenSSL/PCRE2/zlib 开发依赖，请先安装")
	}
	return run(w, 20*time.Minute, "", name, args...)
}
func exists(name string) bool { _, err := exec.LookPath(name); return err == nil }

// verifySource imports nginx.org release keys into an isolated keyring, checks their
// published fingerprints and verifies the detached signature before extraction.
func verifySource(dir, archive, signature string, w io.Writer) error {
	keys := []struct{ file, fingerprint string }{
		{"arut.key", "43387825DDB1BB97EC36BA5D007C8D7C15D87369"},
		{"pluknet.key", "D6786CE303D9A9022998DC6CC8464D549AF75C0A"},
	}
	home := filepath.Join(dir, "gnupg")
	if err := os.Mkdir(home, 0o700); err != nil {
		return err
	}
	imported := 0
	for _, k := range keys {
		b, err := fetch("https://nginx.org/keys/"+k.file, 1<<20)
		if err != nil {
			fmt.Fprintf(w, "无法获取签名公钥 %s: %v\n", k.file, err)
			continue
		}
		path := filepath.Join(dir, k.file)
		if err = os.WriteFile(path, b, 0o600); err != nil {
			return err
		}
		out, err := exec.Command("gpg", "--batch", "--homedir", home, "--with-colons", "--import-options", "show-only", "--import", path).Output()
		if err != nil {
			fmt.Fprintf(w, "无法读取签名公钥 %s: %v\n", k.file, err)
			continue
		}
		expected := false
		for _, line := range strings.Split(string(out), "\n") {
			fields := strings.Split(line, ":")
			if len(fields) > 9 && fields[0] == "fpr" && strings.EqualFold(fields[9], k.fingerprint) {
				expected = true
				break
			}
		}
		if !expected {
			fmt.Fprintf(w, "签名公钥 %s 指纹不匹配\n", k.file)
			continue
		}
		if err = run(w, 30*time.Second, "", "gpg", "--batch", "--homedir", home, "--import", path); err != nil {
			fmt.Fprintf(w, "无法导入签名公钥 %s: %v\n", k.file, err)
			continue
		}
		imported++
	}
	if imported == 0 {
		return fmt.Errorf("没有可用的官方 Nginx 签名公钥")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gpg", "--batch", "--homedir", home, "--status-fd", "1", "--verify", signature, archive)
	cmd.Stderr = w
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("签名验证失败: %w", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 || fields[0] != "[GNUPG:]" || fields[1] != "VALIDSIG" {
			continue
		}
		// GPG reports the signing key fingerprint first and the primary key last.
		for _, key := range keys {
			if fields[2] == key.fingerprint || fields[len(fields)-1] == key.fingerprint {
				return nil
			}
		}
	}
	return fmt.Errorf("签名密钥不在 Nginx 官方指纹名单中")
}

func rejectWAF() error {
	dir := filepath.Join(config.DataDir(), "websites", "conf.d")
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".conf") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return err
		}
		s := strings.ToLower(string(b))
		for _, bad := range []string{"access_by_lua", "lua_shared_dict", "more_set_headers", "lua_package_path", "init_by_lua", "header_filter_by_lua", "root /var/www/", "/etc/nginx/ssl/"} {
			if strings.Contains(s, bad) {
				return fmt.Errorf("现有站点配置 %s 含 WAF 指令或容器路径，暂不能直接迁移；请先备份并调整配置", entry.Name())
			}
		}
	}
	return nil
}
func checkPorts() error {
	for _, p := range []string{"80", "443"} {
		l, err := net.Listen("tcp", ":"+p)
		if err != nil {
			return fmt.Errorf("端口 %s 已被占用，请先处理现有网站服务: %w", p, err)
		}
		_ = l.Close()
	}
	return nil
}
func nginxQuote(s string) (string, error) {
	if strings.ContainsAny(s, "\r\n\x00") {
		return "", fmt.Errorf("目录路径包含非法字符")
	}
	return strconv.Quote(s), nil
}
func serviceQuote(s string) (string, error) {
	if strings.ContainsAny(s, "\r\n\x00%") {
		return "", fmt.Errorf("目录路径不能包含换行或 %%")
	}
	return strconv.Quote(strings.ReplaceAll(s, "$", "$$")), nil
}
func writeInitialConfig() error {
	if _, err := os.Stat(ConfigPath()); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	logs := filepath.Join(config.DataDir(), "websites", "logs")
	sites := filepath.Join(config.DataDir(), "websites", "conf.d")
	for _, d := range []string{root(), logs, sites} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	pid, e := nginxQuote(filepath.Join(root(), "nginx.pid"))
	if e != nil {
		return e
	}
	el, e := nginxQuote(filepath.Join(logs, "error.log"))
	if e != nil {
		return e
	}
	al, e := nginxQuote(filepath.Join(logs, "access.log"))
	if e != nil {
		return e
	}
	mime, e := nginxQuote(filepath.Join(Prefix(), "conf", "mime.types"))
	if e != nil {
		return e
	}
	include, e := nginxQuote(filepath.Join(sites, "*.conf"))
	if e != nil {
		return e
	}
	text := fmt.Sprintf("worker_processes auto;\npid %s;\nerror_log %s;\nevents { worker_connections 1024; }\nhttp {\n    include %s;\n    default_type application/octet-stream;\n    access_log %s;\n    sendfile on;\n    keepalive_timeout 65;\n    include %s;\n}\n", pid, el, mime, al, include)
	return os.WriteFile(ConfigPath(), []byte(text), 0o644)
}
func serviceUnit(binary, prefixPath, configPath, pidPath string) (string, error) {
	bin, e := serviceQuote(binary)
	if e != nil {
		return "", e
	}
	prefix, e := serviceQuote(prefixPath)
	if e != nil {
		return "", e
	}
	conf, e := serviceQuote(configPath)
	if e != nil {
		return "", e
	}
	// PIDFile is a single path, not an ExecStart-style argument. systemd
	// treats quotes and dollar signs here as literal path characters.
	if !filepath.IsAbs(pidPath) || strings.ContainsAny(pidPath, "\r\n\x00%") {
		return "", fmt.Errorf("PID 文件路径必须是绝对路径且不能包含换行或 %%")
	}
	return fmt.Sprintf("[Unit]\nDescription=KunPanel native Nginx\nAfter=network.target\n\n[Service]\nType=forking\nPIDFile=%s\nExecStartPre=%s -p %s -c %s -t\nExecStart=%s -p %s -c %s\nExecReload=%s -p %s -c %s -s reload\nExecStop=%s -p %s -c %s -s quit\nTimeoutStopSec=30\n\n[Install]\nWantedBy=multi-user.target\n", pidPath, bin, prefix, conf, bin, prefix, conf, bin, prefix, conf, bin, prefix, conf), nil
}
func writeService() error {
	text, err := serviceUnit(Binary(), Prefix()+"/", ConfigPath(), filepath.Join(root(), "nginx.pid"))
	if err != nil {
		return err
	}
	// Do not replace a service not installed by this module.
	if b, err := os.ReadFile(unitPath); err == nil && !strings.Contains(string(b), "Description=KunPanel native Nginx") {
		return fmt.Errorf("服务文件 %s 已存在且不属于面板", unitPath)
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	return os.WriteFile(unitPath, []byte(text), 0o644)
}

// buildManifest allows a previously verified and built release to be selected
// again without treating an arbitrary pre-existing directory as panel-owned.
type buildManifest struct {
	Version      string `json:"version"`
	BinarySHA256 string `json:"binary_sha256"`
	MimeSHA256   string `json:"mime_sha256"`
}

func regularFileHash(path string) (string, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("不是普通文件: %s", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func versionFiles(target string) (string, string, error) {
	for _, dir := range []string{target, filepath.Join(target, "sbin"), filepath.Join(target, "conf")} {
		fi, err := os.Lstat(dir)
		if err != nil {
			return "", "", err
		}
		if !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
			return "", "", fmt.Errorf("不是面板版本目录: %s", dir)
		}
	}
	bin, err := regularFileHash(filepath.Join(target, "sbin", "nginx"))
	if err != nil {
		return "", "", err
	}
	mime, err := regularFileHash(filepath.Join(target, "conf", "mime.types"))
	if err != nil {
		return "", "", err
	}
	return bin, mime, nil
}

func saveBuildManifest(target, version string) error {
	bin, mime, err := versionFiles(target)
	if err != nil {
		return err
	}
	b, err := json.Marshal(buildManifest{Version: version, BinarySHA256: bin, MimeSHA256: mime})
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(target, ".kunpanel-build.json"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, writeErr := f.Write(b)
	closeErr := f.Close()
	if writeErr != nil {
		return writeErr
	}
	return closeErr
}

func verifyBuildManifest(target, version string) error {
	bin, mime, err := versionFiles(target)
	if err != nil {
		return err
	}
	path := filepath.Join(target, ".kunpanel-build.json")
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() || fi.Size() > 4096 {
		return fmt.Errorf("版本记录不合法")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var m buildManifest
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	if m.Version != version || m.BinarySHA256 != bin || m.MimeSHA256 != mime {
		return fmt.Errorf("版本文件与面板构建记录不一致")
	}
	return nil
}

// Install downloads and verifies an officially published version, builds into a
// versioned directory, tests its configuration and starts the dedicated service.
func Install(version string, w io.Writer) error {
	installMu.Lock()
	defer installMu.Unlock()
	if w == nil {
		w = io.Discard
	}
	if !systemd() {
		return fmt.Errorf("原生 Nginx 当前仅支持 Linux systemd")
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("安装原生 Nginx 需要 root 权限")
	}
	if !validVersion.MatchString(version) {
		return fmt.Errorf("非法版本号")
	}
	versions, err := Versions()
	if err != nil {
		return err
	}
	allowed := false
	for _, v := range versions {
		if v.Version == version {
			allowed = true
		}
	}
	if !allowed {
		return fmt.Errorf("版本 %s 不在官方当前 Stable/Mainline 列表中，请刷新", version)
	}
	if err = rejectWAF(); err != nil {
		return err
	}
	wasInstalled := Installed()
	wasRunning := Status() == "running"
	if !wasInstalled {
		if exec.Command("systemctl", "is-active", "--quiet", "nginx").Run() == nil {
			return fmt.Errorf("检测到宿主机 Nginx 服务正在运行，请先处理现有服务")
		}
		for _, name := range []string{"kun-nginx", "kun-openresty"} {
			out, _ := exec.Command("docker", "ps", "--filter", "name=^/"+name+"$", "--format", "{{.ID}}").Output()
			if strings.TrimSpace(string(out)) != "" {
				return fmt.Errorf("检测到正在运行的容器 %s，请先处理现有网关", name)
			}
		}
	}
	if !wasInstalled {
		if err = checkPorts(); err != nil {
			return err
		}
	}
	if wasInstalled {
		if old, _ := os.Readlink(Prefix()); filepath.Base(old) == version {
			return fmt.Errorf("版本 %s 已安装", version)
		}
	}
	target := filepath.Join(root(), "versions", version)
	reused := false
	if _, err = os.Lstat(target); err == nil {
		if err = verifyBuildManifest(target, version); err != nil {
			return fmt.Errorf("无法安全复用已有版本目录 %s: %w", target, err)
		}
		reused = true
		fmt.Fprintln(w, "复用已验证构建的 Nginx 版本:", version)
	} else if !os.IsNotExist(err) {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	installed := false
	defer func() {
		if !installed && !reused {
			// Never delete a candidate that is still selected if rollback failed.
			selected, _ := os.Readlink(Prefix())
			if selected != target {
				_ = os.RemoveAll(target)
			}
		}
	}()
	if !reused {
		if err = dependencies(w); err != nil {
			return err
		}
		work, err := os.MkdirTemp(root(), ".build-")
		if err != nil {
			return fmt.Errorf("创建 Nginx 编译目录失败: %w", err)
		}
		defer os.RemoveAll(work)
		fmt.Fprintln(w, "Nginx 编译目录:", work)
		base := "https://nginx.org/download/nginx-" + version + ".tar.gz"
		fmt.Fprintln(w, "下载官方 Nginx", version, "...")
		src, err := fetch(base, 16<<20)
		if err != nil {
			return err
		}
		sig, err := fetch(base+".asc", 64<<10)
		if err != nil {
			return err
		}
		archive := filepath.Join(work, "nginx.tar.gz")
		signature := archive + ".asc"
		if err = os.WriteFile(archive, src, 0o600); err != nil {
			return err
		}
		if err = os.WriteFile(signature, sig, 0o600); err != nil {
			return err
		}
		fmt.Fprintln(w, "验证官方源码签名...")
		if err = verifySource(work, archive, signature, w); err != nil {
			return fmt.Errorf("官方签名验证失败: %w", err)
		}
		if err = run(w, 2*time.Minute, work, "tar", "-xzf", archive, "--no-same-owner"); err != nil {
			return err
		}
		srcDir := filepath.Join(work, "nginx-"+version)
		fmt.Fprintln(w, "编译 Nginx，可能需要几分钟...")
		args := []string{"--prefix=" + target, "--with-http_ssl_module", "--with-http_v2_module", "--with-http_realip_module", "--with-http_stub_status_module", "--with-http_gzip_static_module", "--with-threads"}
		if err = run(w, 5*time.Minute, srcDir, "./configure", args...); err != nil {
			return err
		}
		jobs := runtime.NumCPU()
		if jobs > 4 {
			jobs = 4
		}
		if jobs < 1 {
			jobs = 1
		}
		if err = run(w, 30*time.Minute, srcDir, "make", "-j", strconv.Itoa(jobs)); err != nil {
			return err
		}
		if err = run(w, 5*time.Minute, srcDir, "make", "install"); err != nil {
			return err
		}
		if err = saveBuildManifest(target, version); err != nil {
			return fmt.Errorf("保存版本构建记录失败: %w", err)
		}
	}
	if err = writeInitialConfig(); err != nil {
		return err
	}
	old, _ := os.Readlink(Prefix())
	next := filepath.Join(root(), ".current-new")
	_ = os.Remove(next)
	if err = os.Symlink(target, next); err != nil {
		return err
	}
	if err = os.Rename(next, Prefix()); err != nil {
		_ = os.Remove(next)
		return err
	}
	rollback := func() error {
		if old == "" {
			if err := os.Remove(Prefix()); err != nil && !os.IsNotExist(err) {
				return err
			}
			return nil
		}
		tmp := filepath.Join(root(), ".current-rollback")
		if err := os.Remove(tmp); err != nil && !os.IsNotExist(err) {
			return err
		}
		if err := os.Symlink(old, tmp); err != nil {
			return err
		}
		if err := os.Rename(tmp, Prefix()); err != nil {
			_ = os.Remove(tmp)
			return err
		}
		return nil
	}
	if err = run(w, 30*time.Second, "", Binary(), "-p", Prefix()+"/", "-c", ConfigPath(), "-t"); err != nil {
		if rollbackErr := rollback(); rollbackErr != nil {
			return fmt.Errorf("配置检查失败: %w；恢复版本指针失败: %v", err, rollbackErr)
		}
		return fmt.Errorf("配置检查失败，保留原服务: %w", err)
	}
	if err = writeService(); err != nil {
		if rollbackErr := rollback(); rollbackErr != nil {
			return fmt.Errorf("写入服务失败: %w；恢复版本指针失败: %v", err, rollbackErr)
		}
		return err
	}
	cleanupNewService := func() error {
		if !wasInstalled {
			if err := run(w, 60*time.Second, "", "systemctl", "disable", "--now", ServiceName); err != nil {
				return err
			}
			if err := os.Remove(unitPath); err != nil && !os.IsNotExist(err) {
				return err
			}
			return run(w, 30*time.Second, "", "systemctl", "daemon-reload")
		}
		return nil
	}
	if err = run(w, 30*time.Second, "", "systemctl", "daemon-reload"); err != nil {
		if cleanupErr := cleanupNewService(); cleanupErr != nil {
			return fmt.Errorf("加载服务失败: %w；清理失败: %v", err, cleanupErr)
		}
		if rollbackErr := rollback(); rollbackErr != nil {
			return fmt.Errorf("加载服务失败: %w；恢复版本指针失败: %v", err, rollbackErr)
		}
		return err
	}
	if !wasInstalled {
		if err = run(w, 30*time.Second, "", "systemctl", "enable", ServiceName); err != nil {
			if cleanupErr := cleanupNewService(); cleanupErr != nil {
				return fmt.Errorf("启用服务失败: %w；清理失败: %v", err, cleanupErr)
			}
			if rollbackErr := rollback(); rollbackErr != nil {
				return fmt.Errorf("启用服务失败: %w；恢复版本指针失败: %v", err, rollbackErr)
			}
			return err
		}
	} else if !wasRunning {
		installed = true
		fmt.Fprintln(w, "原生 Nginx 升级完成，服务保持停止:", version)
		return nil
	}
	operation := "start"
	if wasRunning {
		operation = "restart"
	}
	if err = run(w, 60*time.Second, "", "systemctl", operation, ServiceName); err != nil {
		logServiceDiagnostics(w)
		if cleanupErr := cleanupNewService(); cleanupErr != nil {
			return fmt.Errorf("启动失败: %w；清理新服务失败，已保留版本文件: %v", err, cleanupErr)
		}
		if rollbackErr := rollback(); rollbackErr != nil {
			return fmt.Errorf("启动失败: %w；恢复版本指针失败: %v", err, rollbackErr)
		}
		if wasRunning {
			if restoreErr := run(w, 60*time.Second, "", "systemctl", "restart", ServiceName); restoreErr != nil {
				return fmt.Errorf("启动失败，已尝试恢复旧版本，但重启旧服务失败: %w；恢复错误: %v", err, restoreErr)
			}
		}
		if !wasInstalled {
			return fmt.Errorf("启动失败，已尝试清理新安装: %w", err)
		}
		return fmt.Errorf("启动失败，已尝试恢复旧版本: %w", err)
	}
	fmt.Fprintln(w, "原生 Nginx 安装完成:", version)
	installed = true
	return nil
}

// Uninstall removes only the panel-owned service and Nginx binaries, preserving
// configuration, certificates, logs and website data for recovery.
func Uninstall(w io.Writer) error {
	installMu.Lock()
	defer installMu.Unlock()
	if w == nil {
		w = io.Discard
	}
	if !Installed() {
		return fmt.Errorf("未安装原生 Nginx")
	}
	if !systemd() {
		return fmt.Errorf("原生 Nginx 当前仅支持 Linux systemd")
	}
	if b, err := os.ReadFile(unitPath); err != nil || !strings.Contains(string(b), "Description=KunPanel native Nginx") {
		return fmt.Errorf("无法确认面板服务归属，拒绝卸载")
	}
	if err := run(w, 60*time.Second, "", "systemctl", "disable", "--now", ServiceName); err != nil {
		return err
	}
	if err := os.Remove(unitPath); err != nil {
		return err
	}
	if err := run(w, 30*time.Second, "", "systemctl", "daemon-reload"); err != nil {
		return err
	}
	if err := os.Remove(Prefix()); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.RemoveAll(filepath.Join(root(), "versions")); err != nil {
		return err
	}
	fmt.Fprintln(w, "已卸载原生 Nginx；网站、证书、配置及日志已保留")
	return nil
}
