package nativepg

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"kunpanel/internal/config"
	"kunpanel/internal/util"
)

const ServiceName = "kunpanel-postgresql"
const serviceMarker = "Description=KunPanel native PostgreSQL"
const workerUser = "kunpanel-pg"
const adminRole = "postgres"

var mu sync.Mutex

func root() string      { return filepath.Join(config.DataDir(), "native", "postgresql") }
func Prefix() string    { return filepath.Join(root(), "current") }
func DataDir() string   { return filepath.Join(root(), "data") }
func SocketDir() string { return filepath.Join(root(), "run") }
func unitPath() string  { return filepath.Join("/etc/systemd/system", ServiceName+".service") }

func Version() string {
	target, err := os.Readlink(Prefix())
	if err != nil || filepath.Dir(target) != filepath.Join(root(), "versions") {
		return ""
	}
	version, _, _ := strings.Cut(filepath.Base(target), "-")
	if Major(version) == "" {
		return ""
	}
	return version
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
	file, err := os.Stat(filepath.Join(Prefix(), "bin", "postgres"))
	return err == nil && file.Mode().IsRegular()
}

func Status() string {
	if !Installed() {
		return "missing"
	}
	if run(io.Discard, 5*time.Second, "", "systemctl", "is-active", "--quiet", ServiceName) == nil {
		return "running"
	}
	return "exited"
}

func control(verb string, args ...string) error {
	argv := append([]string{verb}, args...)
	return systemctl(append(argv, ServiceName)...)
}

func systemctl(args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	data, err := exec.CommandContext(ctx, "systemctl", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("PostgreSQL 服务操作失败: %s (%w)", strings.TrimSpace(string(data)), err)
	}
	return nil
}

type owner struct{ uid, gid int }

func ensureUser(name string, log io.Writer) (owner, error) {
	u, err := user.Lookup(name)
	if _, missing := err.(user.UnknownUserError); missing {
		if err := run(log, 30*time.Second, "", "useradd", "--system", "--user-group", "--no-create-home", "--shell", "/bin/false", name); err != nil {
			return owner{}, err
		}
		u, err = user.Lookup(name)
	}
	if err != nil {
		return owner{}, err
	}
	uid, uidErr := strconv.Atoi(u.Uid)
	gid, gidErr := strconv.Atoi(u.Gid)
	if uidErr != nil || gidErr != nil || uid <= 0 || gid <= 0 {
		return owner{}, fmt.Errorf("PostgreSQL 需要独立的非 root 系统用户")
	}
	return owner{uid, gid}, nil
}

func readyPlatform() error {
	if !systemd() || os.Geteuid() != 0 {
		return fmt.Errorf("原生 PostgreSQL 需要 Linux systemd 和 root 权限")
	}
	if !filepath.IsAbs(root()) || len(SocketDir())+len("/.s.PGSQL.65535") >= 108 || strings.IndexFunc(root(), unicode.IsSpace) >= 0 || strings.ContainsAny(root(), "%$\\\"\x00") {
		return fmt.Errorf("面板数据路径不适合 PostgreSQL 编译或 Unix Socket: %s", root())
	}
	return nil
}

func ValidatePassword(password string) error {
	if len(password) < 6 || len(password) > 128 || strings.IndexFunc(password, unicode.IsControl) >= 0 {
		return fmt.Errorf("postgres 密码须为 6 到 128 个字节，且不能包含控制字符")
	}
	return nil
}

// libpq permits a connection string in --dbname. Quote the value as a single
// dbname keyword so a database name cannot replace the local Socket address.
func databaseOption(database string) string {
	if database == "" {
		database = "postgres"
	}
	return "--dbname=dbname='" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(database) + "'"
}

// Command invokes clients as the unprivileged PostgreSQL account. Peer
// authentication authorizes the panel over its private Unix Socket.
func Command(ctx context.Context, utility, database string, port int, options ...string) (*exec.Cmd, error) {
	if utility != "psql" && utility != "pg_dump" {
		return nil, fmt.Errorf("不支持的 PostgreSQL 客户端")
	}
	if !Installed() {
		return nil, fmt.Errorf("原生 PostgreSQL 服务未安装")
	}
	if port < 1 || port > 65535 {
		return nil, fmt.Errorf("PostgreSQL 端口记录无效")
	}
	return client(ctx, utility, database, port, options...), nil
}

func client(ctx context.Context, utility, database string, port int, options ...string) *exec.Cmd {
	args := []string{"-u", workerUser, "--", filepath.Join(Prefix(), "bin", utility), "--host=" + SocketDir(), "--port=" + strconv.Itoa(port), "--username=" + adminRole, "--no-password", databaseOption(database)}
	if utility == "psql" {
		args = append(args, "--no-psqlrc", "--set=ON_ERROR_STOP=1")
	}
	cmd := exec.CommandContext(ctx, "runuser", append(args, options...)...)
	for _, item := range util.ChildEnv() {
		name, _, _ := strings.Cut(item, "=")
		if !strings.HasPrefix(name, "PG") && name != "HOME" {
			cmd.Env = append(cmd.Env, item)
		}
	}
	cmd.WaitDelay = 5 * time.Second
	return cmd
}

func waitReady(port int) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	ticker := time.NewTicker(350 * time.Millisecond)
	defer ticker.Stop()
	var diagnostic string
	for {
		attempt, stop := context.WithTimeout(ctx, 4*time.Second)
		out, err := client(attempt, "psql", "", port, "--tuples-only", "--no-align", "--command=SELECT 1").CombinedOutput()
		stop()
		if err == nil && strings.TrimSpace(string(out)) == "1" {
			return nil
		}
		diagnostic = strings.TrimSpace(string(out))
		select {
		case <-ctx.Done():
			return fmt.Errorf("PostgreSQL 未通过 Socket 就绪检查: %s", diagnostic)
		case <-ticker.C:
		}
	}
}

func Action(verb string, port int) error {
	mu.Lock()
	defer mu.Unlock()
	if !systemd() || !Installed() {
		return fmt.Errorf("原生 PostgreSQL 服务未安装")
	}
	switch verb {
	case "stop":
		return control(verb)
	case "start", "restart":
		if err := control(verb); err != nil {
			return err
		}
		return waitReady(port)
	default:
		return fmt.Errorf("不支持的 PostgreSQL 服务操作")
	}
}

func ChangePassword(password string, port int) error {
	mu.Lock()
	defer mu.Unlock()
	if err := ValidatePassword(password); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	// psql hashes the new password before sending it to the server.
	cmd, err := Command(ctx, "psql", "", port, `--command=\password postgres`)
	if err != nil {
		return err
	}
	cmd.Stdin = strings.NewReader(password + "\n" + password + "\n")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("修改 postgres 密码失败: %w", err)
	}
	return nil
}
