package nativeredis

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"kunpanel/internal/util"
)

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

func checkPort(port int) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("请输入 1 到 65535 范围内的 Redis 端口")
	}
	l, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return fmt.Errorf("本机端口 %d 不可用: %w", port, err)
	}
	return l.Close()
}

func privateDir(path string, worker owner) error {
	if err := os.Mkdir(path, 0o700); err != nil && !os.IsExist(err) {
		return err
	}
	st, err := os.Lstat(path)
	if err != nil || !st.IsDir() {
		return fmt.Errorf("Redis 目录不可用: %s", path)
	}
	if err := os.Chown(path, worker.uid, worker.gid); err != nil {
		return err
	}
	return os.Chmod(path, 0o700)
}

func writeACL(data []byte, gid int) error {
	path := aclPath()
	if err := util.AtomicWrite(path, data, 0o600); err != nil {
		return err
	}
	if err := os.Chown(path, 0, gid); err != nil {
		return err
	}
	return os.Chmod(path, 0o640)
}

func writeConfig(target string, port int) error {
	text := fmt.Sprintf(`bind 127.0.0.1
protected-mode yes
port %d
unixsocket %q
unixsocketperm 700
daemonize no
supervised systemd
logfile ""
dir %q
aclfile %q
dbfilename dump.rdb
appendonly yes
appendfilename appendonly.aof
appenddirname appendonlydir
appendfsync everysec
`, port, Socket(), filepath.Join(target, "data"), aclPath())
	return util.AtomicWriteString(filepath.Join(target, "redis.conf"), text, 0o644)
}

func installUnit() error {
	if _, err := os.Lstat(unitPath()); err != nil && !os.IsNotExist(err) {
		return err
	} else if err == nil {
		if ownUnit() {
			return nil
		}
		return fmt.Errorf("Redis 服务文件已存在")
	}
	text := fmt.Sprintf(`[Unit]
%s
After=network.target

[Service]
Type=notify
User=%s
ExecStart=%q %q
UMask=0077
KillMode=mixed
TimeoutStartSec=180
TimeoutStopSec=180
SendSIGKILL=no
Restart=on-failure
NoNewPrivileges=true
ProtectSystem=strict
ReadWritePaths=%q %q

[Install]
WantedBy=multi-user.target
`, serviceMarker, workerUser, filepath.Join(Prefix(), "bin", "redis-server"),
		filepath.Join(Prefix(), "redis.conf"), DataDir(), socketDir())
	return util.AtomicWriteString(unitPath(), text, 0o644)
}

// Install reattaches existing managed data without replacing its credentials.
func Install(release Release, port int, password string, log io.Writer) error {
	if err := readyPlatform(); err != nil {
		return err
	}
	if _, err := os.Lstat(unitPath()); err == nil && !ownUnit() {
		return fmt.Errorf("Redis 服务文件已存在")
	}
	target, err := current()
	retained := err == nil
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if retained {
		port, err = Port()
		if err != nil {
			return err
		}
		fmt.Fprintln(log, "重新接管保留的 Redis 数据、端口与凭据")
	} else if err := ValidatePassword(password); err != nil {
		return err
	}
	if Status() != "running" {
		if err := checkPort(port); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(root(), 0o755); err != nil {
		return err
	}
	worker, err := ensureUser(workerUser, log)
	if err != nil {
		return err
	}
	if err := privateDir(socketDir(), worker); err != nil {
		return err
	}
	if !retained {
		target, err = build(release, log)
		if err != nil {
			return err
		}
		defer func() {
			active, err := current()
			if active != target && os.IsNotExist(err) {
				_ = os.RemoveAll(target)
			}
		}()
		if err := privateDir(filepath.Join(target, "data"), worker); err != nil {
			return err
		}
		var secret [32]byte
		rand.Read(secret[:])
		token := []byte(hex.EncodeToString(secret[:]))
		if err := util.AtomicWrite(tokenPath(), token, 0o600); err != nil {
			return err
		}
		if err := writeACL([]byte(aclContent(password, token)), worker.gid); err != nil {
			return err
		}
		if err := writeConfig(target, port); err != nil {
			return err
		}
		if err := switchVersion(target); err != nil {
			return err
		}
	}
	if err := installUnit(); err != nil {
		return err
	}
	if _, err := systemctl("daemon-reload"); err != nil {
		return err
	}
	if err := control("enable", "--now"); err != nil {
		logStartupFailure(log)
		return errors.Join(err, control("disable", "--now"))
	}
	fmt.Fprintln(log, "Redis 已启动，监听", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	return nil
}

func logStartupFailure(log io.Writer) {
	if logs, err := Logs(); err == nil {
		fmt.Fprintln(log, logs)
	}
}

func Uninstall(log io.Writer) error {
	if err := readyPlatform(); err != nil {
		return err
	}
	if ownUnit() {
		if err := stopForMaintenance(); err != nil {
			return err
		}
		if err := control("disable"); err != nil {
			return err
		}
		if err := os.Remove(unitPath()); err != nil {
			return err
		}
	}
	if _, err := systemctl("daemon-reload"); err != nil {
		return err
	}
	fmt.Fprintln(log, "已卸载 Redis 服务，保留数据和配置:", root())
	return nil
}

func stopForMaintenance() error {
	if err := control("stop"); err != nil {
		return err
	}
	result, err := systemctl("show", "--property=Result", "--value", ServiceName)
	if err != nil {
		return err
	}
	if result == "timeout" {
		return fmt.Errorf("Redis 停止超时，已取消后续操作")
	}
	return nil
}

func copyData(source, dest string, log io.Writer) error {
	if err := filepath.WalkDir(source, func(path string, entry fs.DirEntry, err error) error {
		if err == nil && !entry.IsDir() && !entry.Type().IsRegular() {
			return fmt.Errorf("Redis 数据目录含有无法安全复制的文件: %s", path)
		}
		return err
	}); err != nil {
		return err
	}
	// Keep the trailing /. so cp copies the contents, including the complete AOF set.
	if err := run(log, 20*time.Minute, "", "runuser", "-u", workerUser, "--",
		"cp", "-a", "--reflink=auto", "--", source+"/.", dest); err != nil {
		return err
	}
	return run(log, 5*time.Minute, "", "sync", "-f", "--", dest)
}

func Upgrade(release Release, log io.Writer) error {
	if err := readyPlatform(); err != nil {
		return err
	}
	if !ownUnit() || !CanUpgrade(Version(), release.Version) {
		return fmt.Errorf("仅可升级到同一 Redis 大版本中较新的正式版")
	}
	old, err := current()
	if err != nil {
		return err
	}
	port, err := Port()
	if err != nil {
		return err
	}
	target, err := build(release, log)
	if err != nil {
		return err
	}
	switched := false
	defer func() {
		if !switched {
			_ = os.RemoveAll(target)
		}
	}()
	worker, err := ensureUser(workerUser, log)
	if err != nil {
		return err
	}
	newData := filepath.Join(target, "data")
	if err := privateDir(newData, worker); err != nil {
		return err
	}
	if err := writeConfig(target, port); err != nil {
		return err
	}
	state, err := systemctl("show", "--property=ActiveState", "--value", ServiceName)
	if err != nil {
		return err
	}
	if state != "active" && state != "inactive" && state != "failed" {
		return fmt.Errorf("Redis 正处于 %s 状态，请稍后重试", state)
	}
	running := state == "active"
	if err := stopForMaintenance(); err != nil {
		return err
	}
	restoreService := func(err error) error {
		if running {
			return errors.Join(err, control("start"))
		}
		return err
	}
	fmt.Fprintln(log, "复制停机后的 Redis 数据，保留升级前的数据目录")
	if err := copyData(filepath.Join(old, "data"), newData, log); err != nil {
		return restoreService(err)
	}
	if err := switchVersion(target); err != nil {
		return restoreService(err)
	}
	switched = true
	if running {
		// Type=notify waits for Redis to load the dataset before it reports readiness.
		if err := control("start"); err != nil {
			logStartupFailure(log)
			if stopErr := stopForMaintenance(); stopErr != nil {
				return errors.Join(err, stopErr)
			}
			if switchErr := switchVersion(old); switchErr != nil {
				return errors.Join(err, switchErr)
			}
			fmt.Fprintln(log, "新版本未能启动，已恢复旧版本路径，新版本数据副本保存在", newData)
			return restoreService(err)
		}
	}
	fmt.Fprintln(log, "已切换 Redis 版本，升级前数据保存在", filepath.Join(old, "data"))
	return nil
}

func ChangePassword(password string, log io.Writer) error {
	if err := ValidatePassword(password); err != nil {
		return err
	}
	if err := readyPlatform(); err != nil {
		return err
	}
	if !ownUnit() {
		return fmt.Errorf("原生 Redis 未安装")
	}
	worker, err := ensureUser(workerUser, log)
	if err != nil {
		return err
	}
	token, err := os.ReadFile(tokenPath())
	if err != nil {
		return err
	}
	old, err := os.ReadFile(aclPath())
	if err != nil {
		return err
	}
	state, err := systemctl("show", "--property=ActiveState", "--value", ServiceName)
	if err != nil {
		return err
	}
	if state != "active" && state != "inactive" && state != "failed" {
		return fmt.Errorf("Redis 正处于 %s 状态，请稍后重试", state)
	}
	running := state == "active"
	if running {
		if err := stopForMaintenance(); err != nil {
			return err
		}
	}
	err = writeACL([]byte(aclContent(password, token)), worker.gid)
	if err == nil && running {
		err = control("start")
	}
	if err != nil {
		if running {
			logStartupFailure(log)
		}
		if restoreErr := writeACL(old, worker.gid); restoreErr != nil {
			return errors.Join(err, restoreErr)
		}
		if running {
			return errors.Join(err, control("start"))
		}
		return err
	}
	fmt.Fprintln(log, "Redis 访问密码已修改")
	return nil
}
