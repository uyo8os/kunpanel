package nativemysql

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"kunpanel/internal/util"
)

const workerName = "kunpanel-mysql"

type owner struct{ uid, gid int }

func mysqlOwner(w io.Writer) (owner, error) {
	u, err := user.Lookup(workerName)
	if _, missing := err.(user.UnknownUserError); missing {
		if err := run(w, 30*time.Second, "useradd", "--system", "--user-group", "--no-create-home", "--shell", "/bin/false", workerName); err != nil {
			return owner{}, err
		}
		u, err = user.Lookup(workerName)
	}
	if err != nil {
		return owner{}, err
	}
	uid, e1 := strconv.Atoi(u.Uid)
	gid, e2 := strconv.Atoi(u.Gid)
	if e1 != nil || e2 != nil || uid <= 0 || gid <= 0 {
		return owner{}, fmt.Errorf("原生 MySQL 需要独立的非 root 系统账号")
	}
	return owner{uid, gid}, nil
}

func (o owner) mkdir(path string, mode os.FileMode) error {
	if err := os.MkdirAll(path, mode); err != nil {
		return err
	}
	st, err := os.Lstat(path)
	if err != nil || !st.IsDir() {
		return fmt.Errorf("MySQL 路径不是普通目录: %s", path)
	}
	return os.Chown(path, o.uid, o.gid)
}

func platformReady() error {
	if !systemd() || os.Geteuid() != 0 {
		return fmt.Errorf("原生 MySQL 需要 Linux systemd 和 root 权限")
	}
	if !filepath.IsAbs(root()) || strings.ContainsAny(root(), "%$\\\"\r\n\x00") || len(root()) > 70 {
		return fmt.Errorf("面板数据路径不适合 MySQL systemd 服务或 Unix Socket: %s", root())
	}
	return nil
}

func configContents(branch string, port int) string {
	conf := fmt.Sprintf("# Managed by KunPanel\n[mysqld]\nbasedir=%s\ndatadir=%s\nsocket=%s\npid-file=%s\nlog-error=%s\nport=%d\nbind-address=127.0.0.1\ncharacter-set-server=utf8mb4\nlocal-infile=0\nsecure-file-priv=NULL\n", option(Prefix()), option(DataDir()), option(Socket()), option(filepath.Join(root(), "run", "mysql.pid")), option(filepath.Join(root(), "logs", "mysql.log")), port)
	if branch != "5.7" {
		conf += "mysqlx=OFF\n"
	}
	return conf
}

func writeUnit() error {
	if _, err := os.Lstat(unitPath()); err == nil {
		return fmt.Errorf("MySQL 服务文件已存在: %s", unitPath())
	} else if !os.IsNotExist(err) {
		return err
	}
	unit := fmt.Sprintf("[Unit]\n%s\nAfter=network.target\n\n[Service]\nType=simple\nUser=%s\nGroup=%s\nEnvironment=%s\nExecStart=%s %s\nRestart=on-failure\nLimitNOFILE=65535\nTimeoutStopSec=300\n\n[Install]\nWantedBy=multi-user.target\n",
		serviceMarker, workerName, workerName, strconv.Quote("LD_LIBRARY_PATH="+filepath.Join(Prefix(), "compat")), strconv.Quote(filepath.Join(Prefix(), "bin", "mysqld")), strconv.Quote("--defaults-file="+ConfigPath()))
	return util.AtomicWriteString(unitPath(), unit, 0o644)
}

func setCurrent(target string) error {
	temp := Prefix() + ".new"
	if err := os.Remove(temp); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Symlink(target, temp); err != nil {
		return err
	}
	defer os.Remove(temp)
	return os.Rename(temp, Prefix())
}

func versionDir(version string) string { return filepath.Join(root(), "versions", version) }

func prepareBinary(release Release, w io.Writer) (string, error) {
	target := versionDir(release.Version)
	if _, err := os.Lstat(target); err == nil {
		if Version() == release.Version {
			return "", fmt.Errorf("MySQL %s 已经安装", release.Version)
		}
		retained, err := os.MkdirTemp(root(), "retained-"+release.Version+"-")
		if err != nil {
			return "", err
		}
		if err := os.Rename(target, filepath.Join(retained, release.Version)); err != nil {
			_ = os.Remove(retained)
			return "", err
		}
		fmt.Fprintln(w, "已保留上一次安装的文件", retained)
	} else if !os.IsNotExist(err) {
		return "", err
	}
	if err := prepareTools(w); err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return "", err
	}
	work, err := os.MkdirTemp(root(), ".mysql-download-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(work)
	archive, err := downloadArchive(release, work, w)
	if err != nil {
		return "", err
	}
	unpacked := filepath.Join(work, "extract")
	if err := os.Mkdir(unpacked, 0o700); err != nil {
		return "", err
	}
	if err := run(w, 10*time.Minute, "tar", "-xf", archive, "-C", unpacked, "--no-same-owner", "--no-same-permissions"); err != nil {
		return "", err
	}
	entries, err := os.ReadDir(unpacked)
	if err != nil || len(entries) != 1 || !entries[0].IsDir() {
		return "", fmt.Errorf("官方 MySQL 安装包目录异常")
	}
	from := filepath.Join(unpacked, entries[0].Name())
	if err := prepareLibraries(from, w); err != nil {
		return "", err
	}
	if err := os.Rename(from, target); err != nil {
		return "", err
	}
	return target, nil
}

func checkPort(port int) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("MySQL 端口无效")
	}
	l, err := net.Listen("tcp4", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		return fmt.Errorf("本机端口 %d 不可用: %w", port, err)
	}
	return l.Close()
}

func writablePaths(o owner) error {
	if err := os.MkdirAll(root(), 0o755); err != nil {
		return err
	}
	for _, dir := range []string{"run", "logs"} {
		if err := o.mkdir(filepath.Join(root(), dir), 0o750); err != nil {
			return err
		}
	}
	return nil
}

func newTrial(o owner) (string, error) {
	dir, err := os.MkdirTemp(root(), ".mysql-trial-")
	if err != nil {
		return "", err
	}
	if err := os.Chown(dir, o.uid, o.gid); err != nil {
		_ = os.Remove(dir)
		return "", err
	}
	return dir, nil
}

// testData starts the candidate with a private socket, never on a TCP port.
// For upgrades the candidate can modify only the copy of the old data.
func testData(prefix, trial, data string, w io.Writer, initFile string, inspect func(string) error) (err error) {
	socket := filepath.Join(trial, "mysql.sock")
	log := filepath.Join(trial, "mysql.log")
	args := []string{"--defaults-file=" + ConfigPath(), "--user=" + workerName, "--basedir=" + prefix, "--datadir=" + data, "--socket=" + socket, "--pid-file=" + filepath.Join(trial, "mysql.pid"), "--log-error=" + log, "--skip-networking"}
	if initFile != "" {
		args = append(args, "--init-file="+initFile)
	}
	cmd := exec.Command(filepath.Join(prefix, "bin", "mysqld"), args...)
	cmd.Env = libraryEnv(prefix)
	if err := cmd.Start(); err != nil {
		return err
	}
	finished := make(chan struct{})
	var exitErr error
	go func() { exitErr = cmd.Wait(); close(finished) }()
	defer func() {
		select {
		case <-finished:
		default:
			_ = cmd.Process.Signal(syscall.SIGTERM)
			select {
			case <-finished:
			case <-time.After(2 * time.Minute):
				_ = cmd.Process.Kill()
				<-finished
			}
		}
		if exitErr != nil && err == nil {
			err = fmt.Errorf("候选 MySQL 退出异常: %w", exitErr)
		}
		// Initialization SQL includes the root password. Keep its log private.
		if err != nil && initFile == "" {
			if lines, readErr := os.ReadFile(log); readErr == nil {
				fmt.Fprintln(w, string(lines[max(0, len(lines)-4000):]))
			}
		}
	}()
	if err := waitForClient(prefix, socket, credentialPath(), finished, 3*time.Minute); err != nil {
		return err
	}
	if inspect != nil {
		return inspect(socket)
	}
	return nil
}

func initializeData(prefix, trial, password string, o owner, w io.Writer) error {
	data := filepath.Join(trial, "data")
	if err := o.mkdir(data, 0o700); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, filepath.Join(prefix, "bin", "mysqld"), "--no-defaults", "--initialize-insecure", "--user="+workerName, "--basedir="+prefix, "--datadir="+data)
	cmd.Stdout, cmd.Stderr, cmd.Env = w, w, libraryEnv(prefix)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("初始化 MySQL 数据目录失败: %w", err)
	}
	bootstrap := filepath.Join(trial, "initialize.sql")
	if err := os.WriteFile(bootstrap, []byte(passwordSQL(password)), 0o600); err != nil {
		return err
	}
	if err := os.Chown(bootstrap, o.uid, o.gid); err != nil {
		return err
	}
	return testData(prefix, trial, data, w, bootstrap, nil)
}

func failedService(err error) error {
	if stopErr := service("disable", "--now"); stopErr != nil {
		return errors.Join(err, stopErr)
	}
	if removeErr := os.Remove(unitPath()); removeErr != nil {
		return errors.Join(err, removeErr)
	}
	return errors.Join(err, reloadUnits())
}

// Install creates a new instance or reattaches data retained by Uninstall.
func Install(release Release, port int, password string, w io.Writer) error {
	mu.Lock()
	defer mu.Unlock()
	if err := platformReady(); err != nil {
		return err
	}
	if _, err := os.Lstat(unitPath()); !os.IsNotExist(err) {
		return fmt.Errorf("原生 MySQL 服务已存在或无法检查服务文件")
	}
	if err := ValidatePassword(password); err != nil {
		return err
	}
	if err := checkPort(port); err != nil {
		return err
	}
	if Branch(release.Version) == "" || Branch(release.Version) != release.Branch {
		return fmt.Errorf("版本与 MySQL 分支不一致")
	}
	o, err := mysqlOwner(w)
	if err != nil {
		return err
	}
	if err := writablePaths(o); err != nil {
		return err
	}
	if _, err := os.Lstat(DataDir()); err == nil {
		if Branch(Version()) != release.Branch {
			return fmt.Errorf("面板保留着其他分支的 MySQL 数据，请选择原分支恢复后再迁移")
		}
		stored, err := os.ReadFile(credentialPath())
		if err != nil || !bytes.Equal(stored, []byte("[client]\nuser=root\npassword="+option(password)+"\n")) {
			return fmt.Errorf("检测到保留的 MySQL 数据，请使用该实例原有的 root 密码")
		}
		fmt.Fprintln(w, "重新启用已保留的原生 MySQL 数据目录，当前版本", Version())
	} else if !os.IsNotExist(err) {
		return err
	} else if err := installNew(release, port, password, o, w); err != nil {
		return err
	}
	if err := util.AtomicWriteString(ConfigPath(), configContents(release.Branch, port), 0o644); err != nil {
		return err
	}
	if err := writeUnit(); err != nil {
		return err
	}
	if err := reloadUnits(); err != nil {
		return failedService(err)
	}
	if err := service("enable"); err != nil {
		return failedService(err)
	}
	if err := service("start"); err != nil {
		return failedService(err)
	}
	if err := waitForClient(Prefix(), Socket(), credentialPath(), nil, 90*time.Second); err != nil {
		return failedService(err)
	}
	fmt.Fprintln(w, "原生 MySQL 已就绪", Version())
	return nil
}

func installNew(release Release, port int, password string, o owner, w io.Writer) error {
	target, err := prepareBinary(release, w)
	if err != nil {
		return err
	}
	defer func() {
		if Version() != release.Version {
			_ = os.RemoveAll(target)
		}
	}()
	trial, err := newTrial(o)
	if err != nil {
		return err
	}
	defer os.RemoveAll(trial)
	if err := util.AtomicWriteString(ConfigPath(), configContents(release.Branch, port), 0o644); err != nil {
		return err
	}
	if err := credentials(credentialPath(), password); err != nil {
		return err
	}
	fmt.Fprintln(w, "初始化新的 MySQL 数据目录")
	if err := initializeData(target, trial, password, o, w); err != nil {
		return err
	}
	if err := os.Rename(filepath.Join(trial, "data"), filepath.Join(target, "data")); err != nil {
		return err
	}
	return setCurrent(target)
}

func Uninstall(w io.Writer) error {
	mu.Lock()
	defer mu.Unlock()
	if !systemd() {
		return fmt.Errorf("原生 MySQL 需要 Linux systemd")
	}
	if _, err := os.Lstat(unitPath()); os.IsNotExist(err) {
		return reloadUnits()
	}
	if !ownUnit() {
		return fmt.Errorf("找不到面板管理的原生 MySQL 服务")
	}
	if err := service("disable", "--now"); err != nil {
		return err
	}
	if err := os.Remove(unitPath()); err != nil {
		return err
	}
	if err := reloadUnits(); err != nil {
		return err
	}
	fmt.Fprintln(w, "已移除原生 MySQL 服务，数据库文件和配置已保留在", root())
	return nil
}

func checkTablespacePaths(prefix, socket string) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := client(ctx, prefix, socket, credentialPath(), "mysql", "--batch", "--skip-column-names", "--raw", "--execute=SELECT FILE_NAME FROM INFORMATION_SCHEMA.FILES WHERE ENGINE='InnoDB' AND FILE_NAME IS NOT NULL")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("无法检查 InnoDB 表空间位置: %s (%w)", strings.TrimSpace(string(out)), err)
	}
	for _, name := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		clean := filepath.Clean(name)
		if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") {
			return fmt.Errorf("检测到外置 InnoDB 表空间，无法安全复制: %s", name)
		}
	}
	return nil
}

// Upgrade tests a newer patch against a copy of the stopped server's data.
func Upgrade(release Release, port int, w io.Writer) error {
	mu.Lock()
	defer mu.Unlock()
	if err := platformReady(); err != nil {
		return err
	}
	current := Version()
	if !Installed() || Branch(current) != release.Branch || Branch(release.Version) != release.Branch || CompareVersions(release.Version, current) <= 0 {
		return fmt.Errorf("请选择同一分支中比当前更新的 MySQL 版本")
	}
	conf, err := os.ReadFile(ConfigPath())
	if err != nil || string(conf) != configContents(release.Branch, port) {
		return fmt.Errorf("自动升级需要确认数据和日志路径，当前 my.cnf 已被修改，请先备份并处理自定义配置")
	}
	o, err := mysqlOwner(w)
	if err != nil {
		return err
	}
	target, err := prepareBinary(release, w)
	if err != nil {
		return err
	}
	keepCandidate := false
	defer func() {
		if !keepCandidate && Version() != release.Version {
			_ = os.RemoveAll(target)
		}
	}()
	trial, err := newTrial(o)
	if err != nil {
		return err
	}
	defer os.RemoveAll(trial)
	wasRunning := Status() == "running"
	if err := service("stop"); err != nil {
		return err
	}
	failed := func(cause error) error {
		if wasRunning {
			if err := service("start"); err != nil {
				return errors.Join(cause, fmt.Errorf("恢复原 MySQL 服务失败: %w", err))
			}
			if err := waitForClient(Prefix(), Socket(), credentialPath(), nil, 90*time.Second); err != nil {
				return errors.Join(cause, fmt.Errorf("原 MySQL 启动校验失败: %w", err))
			}
		}
		return cause
	}
	oldPrefix := versionDir(current)
	oldData := filepath.Join(oldPrefix, "data")
	if err := filepath.WalkDir(oldData, func(path string, entry fs.DirEntry, err error) error {
		if err == nil && (entry.Type()&os.ModeSymlink != 0 || strings.HasSuffix(entry.Name(), ".isl")) {
			return fmt.Errorf("数据目录中包含外部路径链接，无法安全复制: %s", path)
		}
		return err
	}); err != nil {
		return failed(err)
	}
	if err := testData(oldPrefix, trial, oldData, w, "", func(socket string) error {
		return checkTablespacePaths(oldPrefix, socket)
	}); err != nil {
		return failed(err)
	}
	fmt.Fprintln(w, "复制已停止的 MySQL 数据，测试候选版本")
	newData := filepath.Join(target, "data")
	if err := run(w, 40*time.Minute, "cp", "-a", "--reflink=auto", "--", oldData, newData); err != nil {
		return failed(err)
	}
	if err := testData(target, trial, newData, w, "", nil); err != nil {
		return failed(err)
	}
	if err := setCurrent(target); err != nil {
		return failed(err)
	}
	keepCandidate = true
	if wasRunning {
		err := service("start")
		if err == nil {
			err = waitForClient(Prefix(), Socket(), credentialPath(), nil, 90*time.Second)
		}
		if err != nil {
			if stopErr := service("stop"); stopErr != nil {
				return errors.Join(err, fmt.Errorf("无法停止新版，原数据保存在 %s: %w", oldData, stopErr))
			}
			if restoreErr := setCurrent(oldPrefix); restoreErr != nil {
				return errors.Join(err, fmt.Errorf("恢复原 MySQL 版本失败，原数据保存在 %s: %w", oldData, restoreErr))
			}
			return failed(fmt.Errorf("新版启动失败，新版数据保存在 %s: %w", newData, err))
		}
	}
	fmt.Fprintln(w, "MySQL 升级完成，原版本数据保存在", oldData)
	return nil
}
