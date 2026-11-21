package nativepg

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"kunpanel/internal/util"
)

func checkPort(port int) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("请输入 1 到 65535 范围内的 PostgreSQL 端口")
	}
	listener, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return fmt.Errorf("本机端口 %d 不可用: %w", port, err)
	}
	return listener.Close()
}

func serviceUnit(port int) string {
	return fmt.Sprintf(`[Unit]
%s
After=network.target

[Service]
Type=simple
User=%s
ExecStart=%q -D %q -c listen_addresses=127.0.0.1 -c port=%d -c %q -c unix_socket_permissions=0700 -c password_encryption=scram-sha-256
KillSignal=SIGINT
TimeoutStopSec=300
Restart=on-failure
NoNewPrivileges=true

[Install]
WantedBy=multi-user.target
`, serviceMarker, workerUser, filepath.Join(Prefix(), "bin", "postgres"), DataDir(), port, "unix_socket_directories="+SocketDir())
}

func switchVersion(target string) error {
	link := Prefix() + ".new"
	if err := os.Remove(link); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Symlink(target, link); err != nil {
		return err
	}
	defer os.Remove(link)
	return os.Rename(link, Prefix())
}

func RetainedMajor() (string, error) {
	data, err := os.ReadFile(filepath.Join(DataDir(), "PG_VERSION"))
	if os.IsNotExist(err) {
		if _, err := os.Lstat(DataDir()); os.IsNotExist(err) {
			return "", nil
		}
	}
	if err != nil {
		return "", fmt.Errorf("无法识别已有 PostgreSQL 数据目录: %w", err)
	}
	major := strings.TrimSpace(string(data))
	if Major(major+".0") != major || major == "" {
		return "", fmt.Errorf("PostgreSQL 数据目录的 PG_VERSION 无效")
	}
	return major, nil
}

func clusterLocale() string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if available, err := exec.CommandContext(ctx, "locale", "-a").Output(); err == nil {
		for _, name := range strings.Fields(string(available)) {
			if strings.EqualFold(name, "C.utf8") || strings.EqualFold(name, "C.UTF-8") {
				return name
			}
		}
	}
	return "C"
}

func initData(prefix, password string, worker owner, log io.Writer) error {
	stage, err := os.MkdirTemp(root(), ".pg-init-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	if err := os.Chown(stage, worker.uid, worker.gid); err != nil {
		return err
	}
	passwordFile := filepath.Join(stage, "init-password")
	if err := os.WriteFile(passwordFile, []byte(password+"\n"), 0o600); err != nil {
		return err
	}
	if err := os.Chown(passwordFile, worker.uid, worker.gid); err != nil {
		return err
	}
	dir := filepath.Join(stage, "data")
	fmt.Fprintln(log, "初始化 PostgreSQL 数据目录")
	if err := run(log, 5*time.Minute, "", "runuser", "-u", workerUser, "--", filepath.Join(prefix, "bin", "initdb"), "--pgdata="+dir, "--username="+adminRole, "--pwfile="+passwordFile, "--auth-host=scram-sha-256", "--auth-local=peer", "--encoding=UTF8", "--locale="+clusterLocale()); err != nil {
		return err
	}
	if err := os.Remove(passwordFile); err != nil {
		return err
	}
	for file, content := range map[string]string{
		"pg_hba.conf":   "# KunPanel local administration\nlocal all postgres peer map=kunpanel_admin\n",
		"pg_ident.conf": "# KunPanel local administration\nkunpanel_admin " + workerUser + " postgres\n",
	} {
		path := filepath.Join(dir, file)
		original, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := os.WriteFile(path, append([]byte(content), original...), 0o600); err != nil {
			return err
		}
	}
	return os.Rename(dir, DataDir())
}

func cleanupService() error {
	if ownUnit() {
		if err := control("disable", "--now"); err != nil {
			return err
		}
		if err := os.Remove(unitPath()); err != nil {
			return err
		}
	}
	return systemctl("daemon-reload")
}

func Install(version string, port int, password string, log io.Writer) error {
	mu.Lock()
	defer mu.Unlock()
	if err := readyPlatform(); err != nil {
		return err
	}
	major := Major(version)
	if major == "" {
		return fmt.Errorf("请选择 PostgreSQL 官方版本")
	}
	if _, err := os.Lstat(unitPath()); !os.IsNotExist(err) {
		return fmt.Errorf("原生 PostgreSQL 服务文件已存在或无法检查")
	}
	if err := checkPort(port); err != nil {
		return err
	}
	retained, err := RetainedMajor()
	if err != nil {
		return err
	}
	if retained == "" {
		if err := ValidatePassword(password); err != nil {
			return err
		}
	} else if retained != major {
		return fmt.Errorf("已保留 PostgreSQL %s 的数据，请按原大版本重新启用或先做逻辑迁移", retained)
	}
	if err := os.MkdirAll(root(), 0o755); err != nil {
		return err
	}
	worker, err := ensureUser(workerUser, log)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(SocketDir(), 0o700); err != nil {
		return err
	}
	if err := os.Chown(SocketDir(), worker.uid, worker.gid); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(Prefix(), "bin", "postgres")); err != nil || Version() != version {
		target, err := build(version, log)
		if err != nil {
			return err
		}
		if err := switchVersion(target); err != nil {
			_ = os.RemoveAll(target)
			return err
		}
	}
	if retained == "" {
		if err := initData(Prefix(), password, worker, log); err != nil {
			return err
		}
	} else {
		fmt.Fprintln(log, "重新启用已保留的 PostgreSQL 数据和账号")
	}
	if err := util.AtomicWriteString(unitPath(), serviceUnit(port), 0o644); err != nil {
		return err
	}
	for _, step := range []func() error{
		func() error { return systemctl("daemon-reload") },
		func() error { return control("enable", "--now") },
		func() error { return waitReady(port) },
	} {
		if err := step(); err != nil {
			return errors.Join(err, cleanupService())
		}
	}
	fmt.Fprintln(log, "原生 PostgreSQL 已就绪", version)
	return nil
}

func Upgrade(version string, port int, log io.Writer) error {
	mu.Lock()
	defer mu.Unlock()
	if err := readyPlatform(); err != nil {
		return err
	}
	if port < 1 || port > 65535 {
		return fmt.Errorf("PostgreSQL 端口记录无效")
	}
	active := Version()
	if !Installed() || Major(version) != Major(active) || minor(version) <= minor(active) {
		return fmt.Errorf("请选择同一大版本的较新 PostgreSQL 补丁")
	}
	old, err := os.Readlink(Prefix())
	if err != nil {
		return err
	}
	target, err := build(version, log)
	if err != nil {
		return err
	}
	defer func() {
		if current, err := os.Readlink(Prefix()); err == nil && current != target {
			_ = os.RemoveAll(target)
		}
	}()
	wasRunning := Status() == "running"
	if err := control("stop"); err != nil {
		return err
	}
	start := func() error {
		if !wasRunning {
			return nil
		}
		if err := control("start"); err != nil {
			return err
		}
		return waitReady(port)
	}
	if err := switchVersion(target); err != nil {
		return errors.Join(err, start())
	}
	if err := start(); err != nil {
		if stopErr := control("stop"); stopErr != nil {
			return errors.Join(err, stopErr)
		}
		if restoreErr := switchVersion(old); restoreErr != nil {
			return errors.Join(err, restoreErr)
		}
		return errors.Join(err, start())
	}
	fmt.Fprintln(log, "PostgreSQL 已升级至", version)
	return nil
}

func Uninstall(log io.Writer) error {
	mu.Lock()
	defer mu.Unlock()
	if !systemd() {
		return fmt.Errorf("原生 PostgreSQL 需要 Linux systemd")
	}
	if _, err := os.Lstat(unitPath()); !os.IsNotExist(err) && !ownUnit() {
		return fmt.Errorf("找不到面板管理的 PostgreSQL 服务文件")
	}
	if err := cleanupService(); err != nil {
		return err
	}
	fmt.Fprintln(log, "已卸载 PostgreSQL 服务，数据保留在", DataDir())
	return nil
}
