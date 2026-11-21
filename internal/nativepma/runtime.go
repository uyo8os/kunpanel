package nativepma

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"kunpanel/internal/config"
	"kunpanel/internal/nativenginx"
	"kunpanel/internal/nativephp"
	"kunpanel/internal/util"
)

const service = "kunpanel-phpmyadmin"
const serviceMarker = "Description=KunPanel phpMyAdmin PHP-FPM"
const fpmLogging = "error_log = syslog\nsyslog.ident = " + service
const nginxMarker = "# Managed by KunPanel phpMyAdmin\n"
const NginxConfigName = "kunpanel-phpmyadmin.conf"

var mu sync.Mutex

func root() string          { return filepath.Join(config.DataDir(), "native", "phpmyadmin") }
func current() string       { return filepath.Join(root(), "current") }
func socket() string        { return filepath.Join(root(), "fpm.sock") }
func tempPath() string      { return filepath.Join(root(), "tmp") }
func sessionPath() string   { return filepath.Join(root(), "sessions") }
func accessFile() string    { return filepath.Join(root(), "access.htpasswd") }
func phpConfigPath() string { return filepath.Join(root(), "config.inc.php") }
func fpmConfig() string     { return filepath.Join(root(), "php-fpm.conf") }
func settingsFile() string  { return filepath.Join(root(), "settings.json") }
func unitPath() string      { return filepath.Join("/etc/systemd/system", service+".service") }
func nginxPath() string {
	return filepath.Join(config.DataDir(), "websites", "conf.d", NginxConfigName)
}

func ReadSettings() (Settings, error) {
	var s Settings
	b, err := os.ReadFile(settingsFile())
	if err == nil {
		err = json.Unmarshal(b, &s)
	}
	return s, err
}

func saveSettings(s Settings) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return util.AtomicWrite(settingsFile(), b, 0o600)
}

func Version() string {
	path, err := os.Readlink(current())
	if err != nil || filepath.Dir(path) != filepath.Join(root(), "versions") {
		return ""
	}
	version, _, _ := strings.Cut(filepath.Base(path), "-")
	if !validVersion(version) {
		return ""
	}
	return version
}

func ownsFile(path, marker string) bool {
	b, err := os.ReadFile(path)
	return err == nil && strings.Contains(string(b), marker)
}

func Status() string {
	if Version() == "" || !ownsFile(unitPath(), serviceMarker) {
		return "missing"
	}
	if ownsFile(nginxPath(), nginxMarker) && nativenginx.Status() == "running" && systemctl("is-active", "--quiet", service) == nil {
		return "running"
	}
	return "exited"
}

func run(timeout time.Duration, name string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	b, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s 执行失败: %s (%w)", filepath.Base(name), strings.TrimSpace(string(b)), err)
	}
	return nil
}

func systemctl(args ...string) error { return run(30*time.Second, "systemctl", args...) }

func validateEnvironment(s Settings) error {
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		return fmt.Errorf("原生 phpMyAdmin 需要 Linux systemd 和 root 权限")
	}
	if _, err := os.Stat("/run/systemd/system"); err != nil {
		return fmt.Errorf("当前系统未运行 systemd")
	}
	if !filepath.IsAbs(root()) || len(socket()) > 100 || strings.ContainsAny(root(), "\r\n\x00$%\"\\") {
		return fmt.Errorf("面板数据路径不适用于 phpMyAdmin 服务")
	}
	if err := s.Validate(); err != nil {
		return err
	}
	if nativenginx.Status() != "running" {
		return fmt.Errorf("请先安装并启动面板原生 Nginx")
	}
	minor, err := nativephp.Minor(s.PHP)
	if err != nil || minor != s.PHP || !nativephp.Installed(minor) {
		return fmt.Errorf("请先在应用商店安装原生 PHP-FPM")
	}
	return nil
}

func prepareWorker() (int, error) {
	u, err := user.Lookup(service)
	if _, missing := err.(user.UnknownUserError); missing {
		if err := run(30*time.Second, "useradd", "--system", "--user-group", "--no-create-home", "--shell", "/bin/false", service); err != nil {
			return 0, err
		}
		u, err = user.Lookup(service)
	}
	if err != nil {
		return 0, err
	}
	uid, uidErr := strconv.Atoi(u.Uid)
	gid, gidErr := strconv.Atoi(u.Gid)
	if uidErr != nil || gidErr != nil || uid <= 0 || gid <= 0 {
		return 0, fmt.Errorf("phpMyAdmin 需要独立的非 root 系统账号")
	}
	for _, path := range []string{tempPath(), sessionPath()} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return 0, err
		}
		if err := os.Chown(path, uid, gid); err != nil {
			return 0, err
		}
	}
	return gid, nil
}

func writeGroupFile(path, content string, gid int) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".pma-config-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, writeErr := io.WriteString(f, content)
	if err := errors.Join(writeErr, f.Chown(0, gid), f.Chmod(0o640), f.Close()); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

func configureDatabases(s Settings, dbs []Database, gid int) error {
	if len(dbs) == 0 {
		dbs = []Database{{Name: "本机 MySQL", Port: 3306}}
	}
	path := filepath.Join(root(), "cookie.key")
	secret, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		secret = make([]byte, 32)
		if _, err := rand.Read(secret); err != nil {
			return err
		}
		err = util.AtomicWrite(path, secret, 0o600)
	}
	if err != nil || len(secret) != 32 {
		return fmt.Errorf("无法读取有效的 phpMyAdmin Cookie 密钥")
	}
	contents := phpConfig(s, dbs, secret)
	if previous, err := os.ReadFile(phpConfigPath()); err == nil && string(previous) == contents {
		return nil
	}
	return writeGroupFile(phpConfigPath(), contents, gid)
}

func ConfigureDatabases(dbs []Database) error {
	mu.Lock()
	defer mu.Unlock()
	s, err := ReadSettings()
	if err != nil {
		return err
	}
	u, err := user.Lookup(service)
	if err != nil {
		return err
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		return err
	}
	return configureDatabases(s, dbs, gid)
}

func fpmConfigContents(worker, group, socketOwner string) string {
	// PHP-FPM opens error_log as a file. The journal's stderr stream cannot be reopened.
	return fmt.Sprintf(`[global]
daemonize = no
%s
[phpmyadmin]
user = %s
group = %s
listen = %q
listen.mode = 0660
listen.owner = %s
pm = ondemand
pm.max_children = 4
clear_env = yes
security.limit_extensions = .php
php_admin_value[open_basedir] = %q
php_admin_value[session.save_path] = %q
php_admin_value[upload_tmp_dir] = %q
php_admin_value[sys_temp_dir] = %q
php_admin_value[upload_max_filesize] = 120M
php_admin_value[post_max_size] = 128M
php_admin_value[memory_limit] = 256M
php_admin_value[max_execution_time] = 300
php_admin_flag[display_errors] = off
php_admin_flag[log_errors] = on
php_admin_flag[session.cookie_secure] = on
php_admin_flag[session.cookie_httponly] = on
php_admin_flag[mysqli.allow_local_infile] = off
`, fpmLogging, worker, group, socket(), socketOwner, root(), sessionPath(), tempPath(), tempPath())
}

func writeFPM(s Settings) error {
	worker, err := user.Lookup(service)
	if err != nil {
		return err
	}
	group, err := user.LookupGroupId(worker.Gid)
	if err != nil {
		return err
	}
	if err := util.AtomicWriteString(fpmConfig(), fpmConfigContents(worker.Username, group.Name, nginxAccount()), 0o600); err != nil {
		return err
	}
	binary := nativephp.FpmBinary(s.PHP)
	if err := run(25*time.Second, binary, "-n", "-t", "-y", fpmConfig()); err != nil {
		return err
	}
	unit := fmt.Sprintf(`[Unit]
%s
After=network.target
Before=kunpanel-nginx.service

[Service]
Type=simple
ExecStart=%q -n -F -y %q
Restart=on-failure
NoNewPrivileges=true
PrivateTmp=true

[Install]
WantedBy=multi-user.target
`, serviceMarker, binary, fpmConfig())
	return util.AtomicWriteString(unitPath(), unit, 0o644)
}

func ensureFPMLogging() error {
	conf, err := os.ReadFile(fpmConfig())
	if err != nil {
		return err
	}
	old := string(conf)
	// Repair the logging directive in an existing panel-generated configuration.
	if fixed := strings.Replace(old, "\nerror_log = /proc/self/fd/2\n", "\n"+fpmLogging+"\n", 1); fixed != old {
		return util.AtomicWriteString(fpmConfig(), fixed, 0o600)
	}
	return nil
}

func startFPM(args ...string) error {
	if err := ensureFPMLogging(); err != nil {
		return err
	}
	return systemctl(append(args, service)...)
}

func withFPMJournal(cause error, since time.Time, log io.Writer) error {
	if cause == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "journalctl", "-u", service, "--since", "@"+strconv.FormatInt(since.Unix(), 10),
		"-n", "20", "--no-pager", "-o", "cat").Output()
	if detail := strings.TrimSpace(string(out)); err == nil && detail != "" && detail != "-- No entries --" {
		if log != nil {
			fmt.Fprintln(log, "phpMyAdmin PHP-FPM 服务日志\n"+detail)
			return cause
		}
		return fmt.Errorf("%w\nphpMyAdmin PHP-FPM 服务日志:\n%s", cause, detail)
	}
	return cause
}

func nginxAccount() string {
	data, _ := os.ReadFile(nativenginx.ConfigPath())
	if m := nginxUser.FindSubmatch(data); len(m) == 3 {
		return string(m[1])
	}
	return "nobody"
}

func switchRelease(target string) error {
	link := current() + ".new"
	if err := os.Remove(link); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Symlink(target, link); err != nil {
		return err
	}
	defer os.Remove(link)
	return os.Rename(link, current())
}

func prepareRelease(s Settings, version string, log io.Writer) (string, error) {
	versions := filepath.Join(root(), "versions")
	if err := os.MkdirAll(versions, 0o755); err != nil {
		return "", err
	}
	work, err := os.MkdirTemp(root(), ".pma-install-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(work)
	source, err := download(version, work, log)
	if err != nil {
		return "", err
	}
	for _, name := range []string{"index.php", "vendor/autoload.php"} {
		file := filepath.Join(source, filepath.FromSlash(name))
		if info, err := os.Lstat(file); err != nil || !info.Mode().IsRegular() {
			return "", fmt.Errorf("官方包缺少 %s", name)
		}
	}
	if err := os.Symlink(phpConfigPath(), filepath.Join(source, "config.inc.php")); err != nil {
		return "", err
	}
	target, err := os.MkdirTemp(versions, version+"-")
	if err != nil {
		return "", err
	}
	if err := os.Remove(target); err != nil {
		return "", err
	}
	if err := os.Rename(source, target); err != nil {
		return "", err
	}
	autoload := filepath.Join(target, "vendor", "autoload.php")
	if err := run(25*time.Second, "runuser", "-u", service, "--", nativephp.Binary(s.PHP), "-n", "-d", "open_basedir="+root(), "-r", `require $argv[1];`, autoload); err != nil {
		_ = os.RemoveAll(target)
		return "", fmt.Errorf("phpMyAdmin 与所选 PHP 运行时的兼容性检查失败: %w", err)
	}
	return target, nil
}

func waitReady(s Settings, password string) error {
	_, local := listenAddress(s)
	crt, _ := certificatePaths(s.SelfSigned)
	pem, err := os.ReadFile(crt)
	if err != nil {
		return err
	}
	trust := x509.NewCertPool()
	if !trust.AppendCertsFromPEM(pem) {
		return fmt.Errorf("无法读取 phpMyAdmin HTTPS 证书")
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: trust, ServerName: s.Host, MinVersion: tls.VersionTLS12}}
	defer transport.CloseIdleConnections()
	// This probe only connects to the loopback address of our own listener.
	client := &http.Client{Transport: transport, Timeout: 2 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	url := "https://" + local + "/index.php"
	deadline := time.Now().Add(12 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("unix", socket(), time.Second)
		if err == nil {
			_ = conn.Close()
			req, err := http.NewRequest(http.MethodGet, url, nil)
			if err != nil {
				return err
			}
			req.Host = net.JoinHostPort(s.Host, strconv.Itoa(s.Port))
			if password != "" {
				req.SetBasicAuth(AccessUser, password)
			}
			resp, err := client.Do(req)
			if err == nil {
				_ = resp.Body.Close()
				if password != "" && resp.StatusCode == http.StatusOK || password == "" && resp.StatusCode == http.StatusUnauthorized && strings.Contains(resp.Header.Get("WWW-Authenticate"), "KunPanel phpMyAdmin") {
					return nil
				}
				err = fmt.Errorf("HTTP %d", resp.StatusCode)
			}
			lastErr = err
		} else {
			lastErr = err
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("phpMyAdmin 的 HTTPS 或 PHP-FPM 未就绪: %w", lastErr)
}

func start(s Settings, password string, log io.Writer) error {
	previous, err := os.ReadFile(nginxPath())
	if !os.IsNotExist(err) && (err != nil || !strings.HasPrefix(string(previous), nginxMarker)) {
		return fmt.Errorf("已存在其他 nginx 配置文件: %s", nginxPath())
	}
	wasRunning := systemctl("is-active", "--quiet", service) == nil
	since := time.Now()
	if err := startFPM("enable", "--now"); err != nil {
		return withFPMJournal(err, since, log)
	}
	rollback := func(cause error, restoreConfig bool) error {
		var restoreErr, stopErr error
		if restoreConfig {
			if previous == nil {
				restoreErr = os.Remove(nginxPath())
			} else {
				restoreErr = util.AtomicWrite(nginxPath(), previous, 0o644)
			}
			if restoreErr == nil {
				restoreErr = nativenginx.Action("reload")
			}
		}
		if !wasRunning {
			stopErr = systemctl("disable", "--now", service)
		}
		return errors.Join(cause, restoreErr, stopErr)
	}
	if err := util.AtomicWriteString(nginxPath(), nginxConfig(s), 0o644); err != nil {
		return rollback(err, false)
	}
	if err := nativenginx.Action("reload"); err != nil {
		return rollback(err, true)
	}
	if err := waitReady(s, password); err != nil {
		return rollback(withFPMJournal(err, since, log), true)
	}
	return nil
}

func stop() error {
	if ownsFile(nginxPath(), nginxMarker) {
		old, err := os.ReadFile(nginxPath())
		if err != nil {
			return err
		}
		if err := os.Remove(nginxPath()); err != nil {
			return err
		}
		if nativenginx.Status() == "running" {
			if err := nativenginx.Action("reload"); err != nil {
				return errors.Join(err, util.AtomicWrite(nginxPath(), old, 0o644))
			}
		}
	}
	if ownsFile(unitPath(), serviceMarker) {
		return systemctl("disable", "--now", service)
	}
	return nil
}

func cleanup() error {
	if err := stop(); err != nil {
		return err
	}
	if ownsFile(unitPath(), serviceMarker) {
		if err := os.Remove(unitPath()); err != nil {
			return err
		}
	}
	return systemctl("daemon-reload")
}

func Install(s Settings, version, password string, dbs []Database, log io.Writer) error {
	mu.Lock()
	defer mu.Unlock()
	if err := validateEnvironment(s); err != nil {
		return err
	}
	if err := ValidateAccessPassword(password); err != nil {
		return err
	}
	for _, file := range []string{unitPath(), nginxPath()} {
		if _, err := os.Lstat(file); !os.IsNotExist(err) {
			return fmt.Errorf("服务文件已存在或无法检查: %s", file)
		}
	}
	addr, _ := listenAddress(s)
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("访问端口 %d 不可用: %w", s.Port, err)
	}
	_ = l.Close()
	if err := os.MkdirAll(root(), 0o755); err != nil {
		return err
	}
	if err := run(15*time.Second, nativephp.Binary(s.PHP), "-n", "-r", `foreach (['mysqli','mbstring','openssl','session','xml','ctype','iconv'] as $ext) { if (!extension_loaded($ext)) { fwrite(STDERR, $ext . "\n"); exit(1); } }`); err != nil {
		return fmt.Errorf("所选原生 PHP 缺少 phpMyAdmin 所需扩展: %w", err)
	}
	gid, err := prepareWorker()
	if err != nil {
		return err
	}
	target, err := prepareRelease(s, version, log)
	if err != nil {
		return err
	}
	keep := false
	defer func() {
		if !keep {
			_ = os.RemoveAll(target)
		}
	}()
	if err := configureDatabases(s, dbs, gid); err != nil {
		return err
	}
	if err := writeAccessPassword(password); err != nil {
		return err
	}
	if err := prepareCertificate(&s); err != nil {
		return err
	}
	if err := writeFPM(s); err != nil {
		return err
	}
	previous, _ := os.Readlink(current())
	if err := switchRelease(target); err != nil {
		return errors.Join(err, cleanup())
	}
	rollback := func(cause error) error {
		if err := cleanup(); err != nil {
			keep = true
			return errors.Join(cause, err)
		}
		if previous != "" {
			return errors.Join(cause, switchRelease(previous))
		}
		return errors.Join(cause, os.Remove(current()))
	}
	if err := systemctl("daemon-reload"); err != nil {
		return rollback(err)
	}
	if err := start(s, password, log); err != nil {
		return rollback(err)
	}
	if err := saveSettings(s); err != nil {
		return rollback(err)
	}
	keep = true
	fmt.Fprintln(log, "原生 phpMyAdmin 已就绪", version, s.URL())
	return nil
}

func Upgrade(version string, log io.Writer) error {
	mu.Lock()
	defer mu.Unlock()
	s, err := ReadSettings()
	if err != nil {
		return err
	}
	if err := validateEnvironment(s); err != nil {
		return err
	}
	if !newerVersion(version, Version()) || !ownsFile(unitPath(), serviceMarker) {
		return fmt.Errorf("请选择比已安装版本更新的 phpMyAdmin 版本")
	}
	old, err := os.Readlink(current())
	if err != nil {
		return err
	}
	target, err := prepareRelease(s, version, log)
	if err != nil {
		return err
	}
	defer func() {
		if active, _ := os.Readlink(current()); active != target {
			_ = os.RemoveAll(target)
		}
	}()
	if err := switchRelease(target); err != nil {
		return err
	}
	if ownsFile(nginxPath(), nginxMarker) {
		restart := func() error {
			since := time.Now()
			if err := startFPM("restart"); err != nil {
				return withFPMJournal(err, since, log)
			}
			return withFPMJournal(waitReady(s, ""), since, log)
		}
		if err := restart(); err != nil {
			if rollbackErr := switchRelease(old); rollbackErr != nil {
				return errors.Join(err, rollbackErr)
			}
			return errors.Join(err, restart())
		}
	}
	fmt.Fprintln(log, "phpMyAdmin 已升级至", version)
	return nil
}

func Action(verb string) error {
	mu.Lock()
	defer mu.Unlock()
	if !ownsFile(unitPath(), serviceMarker) {
		return fmt.Errorf("原生 phpMyAdmin 服务不存在")
	}
	if verb == "stop" {
		return stop()
	}
	s, err := ReadSettings()
	if err != nil {
		return err
	}
	if err := validateEnvironment(s); err != nil {
		return err
	}
	switch verb {
	case "restart":
		since := time.Now()
		if err := startFPM("restart"); err != nil {
			return withFPMJournal(err, since, nil)
		}
	case "start":
	default:
		return fmt.Errorf("不支持的 phpMyAdmin 服务操作")
	}
	if err := prepareCertificate(&s); err != nil {
		return err
	}
	if err := start(s, "", nil); err != nil {
		return err
	}
	return saveSettings(s)
}

func Uninstall() error {
	mu.Lock()
	defer mu.Unlock()
	return cleanup()
}

func ChangeAccessPassword(password string) error {
	mu.Lock()
	defer mu.Unlock()
	if err := ValidateAccessPassword(password); err != nil {
		return err
	}
	if !ownsFile(unitPath(), serviceMarker) {
		return fmt.Errorf("原生 phpMyAdmin 服务不存在")
	}
	return writeAccessPassword(password)
}

// RestartPHP picks up the new PHP binary after its shared runtime is upgraded.
func RestartPHP(minor string) error {
	mu.Lock()
	defer mu.Unlock()
	s, err := ReadSettings()
	if err != nil {
		return err
	}
	if s.PHP != minor || !ownsFile(unitPath(), serviceMarker) || systemctl("is-active", "--quiet", service) != nil {
		return nil
	}
	since := time.Now()
	if err := startFPM("restart"); err != nil {
		return withFPMJournal(err, since, nil)
	}
	if ownsFile(nginxPath(), nginxMarker) && nativenginx.Status() == "running" {
		return withFPMJournal(waitReady(s, ""), since, nil)
	}
	return nil
}
