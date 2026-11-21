package nativemysql

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"kunpanel/internal/config"
	"kunpanel/internal/util"
)

const ServiceName = "kunpanel-mysql"
const serviceMarker = "Description=KunPanel native MySQL"

var mu sync.Mutex

func root() string           { return filepath.Join(config.DataDir(), "native", "mysql") }
func Prefix() string         { return filepath.Join(root(), "current") }
func ConfigPath() string     { return filepath.Join(root(), "my.cnf") }
func Socket() string         { return filepath.Join(root(), "run", "mysql.sock") }
func DataDir() string        { return filepath.Join(Prefix(), "data") }
func credentialPath() string { return filepath.Join(root(), "client.cnf") }
func unitPath() string       { return filepath.Join("/etc/systemd/system", ServiceName+".service") }

func Version() string {
	target, err := os.Readlink(Prefix())
	if err != nil || filepath.Dir(target) != filepath.Join(root(), "versions") || Branch(filepath.Base(target)) == "" {
		return ""
	}
	return filepath.Base(target)
}

func systemd() bool {
	st, err := os.Stat("/run/systemd/system")
	return runtime.GOOS == "linux" && err == nil && st.IsDir()
}

func ownUnit() bool {
	b, err := os.ReadFile(unitPath())
	return err == nil && strings.Contains(string(b), "\n"+serviceMarker+"\n")
}

func Installed() bool {
	if Version() == "" || !ownUnit() {
		return false
	}
	st, err := os.Stat(filepath.Join(Prefix(), "bin", "mysqld"))
	return err == nil && st.Mode().IsRegular()
}

func Status() string {
	if !Installed() {
		return "missing"
	}
	if _, err := output(5*time.Second, "systemctl", "is-active", "--quiet", ServiceName); err == nil {
		return "running"
	}
	return "exited"
}

func output(timeout time.Duration, command string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, command, args...).CombinedOutput()
	if ctx.Err() != nil {
		return string(out), ctx.Err()
	}
	return string(out), err
}

func run(w io.Writer, timeout time.Duration, command string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Stdout, cmd.Stderr = w, w
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("%s 执行超时: %w", command, ctx.Err())
		}
		return fmt.Errorf("%s 执行失败: %w", command, err)
	}
	return nil
}

func service(verb string, args ...string) error {
	out, err := output(4*time.Minute, "systemctl", append(append([]string{verb}, args...), ServiceName)...)
	if err != nil {
		return fmt.Errorf("MySQL 服务 %s 失败: %s (%w)", verb, strings.TrimSpace(out), err)
	}
	return nil
}

func reloadUnits() error {
	out, err := output(30*time.Second, "systemctl", "daemon-reload")
	if err != nil {
		return fmt.Errorf("加载 MySQL 服务配置失败: %s (%w)", strings.TrimSpace(out), err)
	}
	return nil
}

func Action(verb string) error {
	mu.Lock()
	defer mu.Unlock()
	if !systemd() || !Installed() {
		return fmt.Errorf("原生 MySQL 服务未安装")
	}
	switch verb {
	case "stop":
		return service(verb)
	case "start", "restart":
		if err := service(verb); err != nil {
			return err
		}
		return waitForClient(Prefix(), Socket(), credentialPath(), nil, 90*time.Second)
	default:
		return fmt.Errorf("不支持的 MySQL 服务操作")
	}
}

// Command runs the matching client with credentials and socket kept out of SQL arguments.
func Command(ctx context.Context, utility string, args ...string) (*exec.Cmd, error) {
	if utility != "mysql" && utility != "mysqldump" {
		return nil, fmt.Errorf("不支持的 MySQL 客户端")
	}
	if !Installed() {
		return nil, fmt.Errorf("原生 MySQL 服务未安装")
	}
	return client(ctx, Prefix(), Socket(), credentialPath(), utility, args...), nil
}

func libraryEnv(prefix string) []string {
	var env []string
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if key != "HOME" && key != "LD_LIBRARY_PATH" && !strings.HasPrefix(key, "MYSQL_") {
			env = append(env, entry)
		}
	}
	return append(env, "HOME="+filepath.Join(root(), ".client-home"), "LD_LIBRARY_PATH="+filepath.Join(prefix, "compat"))
}

func client(ctx context.Context, prefix, socket, credentials, utility string, args ...string) *exec.Cmd {
	flags := []string{"--defaults-file=" + credentials, "--protocol=SOCKET", "--socket=" + socket}
	cmd := exec.CommandContext(ctx, filepath.Join(prefix, "bin", utility), append(flags, args...)...)
	cmd.Env = libraryEnv(prefix)
	return cmd
}

func waitForClient(prefix, socket, credentials string, done <-chan struct{}, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		tryCtx, stop := context.WithTimeout(ctx, 3*time.Second)
		cmd := client(tryCtx, prefix, socket, credentials, "mysql", "--batch", "--skip-column-names", "--execute=SELECT 1")
		out, err := cmd.Output()
		stop()
		if err == nil && strings.TrimSpace(string(out)) == "1" {
			return nil
		}
		select {
		case <-done:
			return fmt.Errorf("MySQL 未就绪时已退出，请检查错误日志")
		case <-ctx.Done():
			return fmt.Errorf("MySQL 未能在规定时间内通过 Socket 登录检查")
		case <-ticker.C:
		}
	}
}

func ValidatePassword(password string) error {
	if len(password) < 6 || len(password) > 128 || strings.ContainsAny(password, "\x00\r\n") {
		return fmt.Errorf("MySQL root 密码须为 6 到 128 个字节，且不能包含换行或空字符")
	}
	return nil
}

func option(value string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\t", `\t`).Replace(value) + `"`
}

func credentials(path, password string) error {
	return util.AtomicWriteString(path, "[client]\nuser=root\npassword="+option(password)+"\n", 0o600)
}

func alterPassword(ctx context.Context, defaults, newPassword string) error {
	cmd := client(ctx, Prefix(), Socket(), defaults, "mysql", "--batch", "--binary-mode", "--local-infile=0")
	cmd.Stdin = strings.NewReader(passwordSQL(newPassword))
	return cmd.Run()
}

func passwordSQL(password string) string {
	return "SET SESSION sql_mode='NO_BACKSLASH_ESCAPES';\nALTER USER 'root'@'localhost' IDENTIFIED BY '" + strings.ReplaceAll(password, "'", "''") + "';\n"
}

func ChangePassword(password string) error {
	mu.Lock()
	defer mu.Unlock()
	if err := ValidatePassword(password); err != nil {
		return err
	}
	if Status() != "running" {
		return fmt.Errorf("原生 MySQL 未运行")
	}
	previous, err := os.ReadFile(credentialPath())
	if err != nil {
		return err
	}
	current, err := strconv.Unquote(strings.TrimSpace(strings.TrimPrefix(string(previous), "[client]\nuser=root\npassword=")))
	if err != nil {
		return fmt.Errorf("无法读取原有 MySQL 客户端凭据")
	}
	file, err := os.CreateTemp(root(), ".mysql-credentials-")
	if err != nil {
		return err
	}
	path := file.Name()
	defer os.Remove(path)
	if err := file.Close(); err != nil {
		return err
	}
	if err := credentials(path, password); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := alterPassword(ctx, credentialPath(), password); err != nil {
		return fmt.Errorf("修改 MySQL root 密码失败: %w", err)
	}
	if err := os.Rename(path, credentialPath()); err != nil {
		undo, stop := context.WithTimeout(context.Background(), 20*time.Second)
		defer stop()
		if undoErr := alterPassword(undo, path, current); undoErr != nil {
			return fmt.Errorf("保存 MySQL 新凭据失败，恢复旧密码也失败: %w", err)
		}
		return fmt.Errorf("保存 MySQL 新凭据失败，已恢复旧密码: %w", err)
	}
	return nil
}
