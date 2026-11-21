package nativeredis

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unicode"

	"kunpanel/internal/config"
	"kunpanel/internal/util"
)

const ServiceName = "kunpanel-redis.service"
const serviceMarker = "Description=KunPanel native Redis"
const workerUser = "kunpanel-redis"
const panelUser = "kunpanel"

func root() string      { return filepath.Join(config.DataDir(), "native", "redis") }
func Prefix() string    { return filepath.Join(root(), "current") }
func DataDir() string   { return filepath.Join(Prefix(), "data") }
func socketDir() string { return filepath.Join(root(), "run") }
func Socket() string    { return filepath.Join(socketDir(), "redis.sock") }
func aclPath() string   { return filepath.Join(root(), "users.acl") }
func tokenPath() string { return filepath.Join(root(), "panel-token") }
func unitPath() string  { return filepath.Join("/etc/systemd/system", ServiceName) }

func readyPlatform() error {
	st, err := os.Stat("/run/systemd/system")
	if runtime.GOOS != "linux" || err != nil || !st.IsDir() || os.Geteuid() != 0 {
		return fmt.Errorf("原生 Redis 需要 Linux systemd 和 root 权限")
	}
	if !filepath.IsAbs(root()) || len(Socket()) >= 108 ||
		strings.IndexFunc(root(), func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 ||
		strings.ContainsAny(root(), "%$\\\"") {
		return fmt.Errorf("面板数据路径不适合 Redis 编译或 Unix Socket: %s", root())
	}
	return nil
}

func current() (string, error) {
	prefix, err := os.Readlink(Prefix())
	if err != nil {
		return "", err
	}
	version, id, _ := strings.Cut(filepath.Base(prefix), "-")
	if filepath.Dir(prefix) != filepath.Join(root(), "versions") || !SupportedVersion(version) || id == "" {
		return "", fmt.Errorf("Redis 当前版本目录无效")
	}
	return prefix, nil
}

func Version() string {
	target, err := current()
	if err != nil {
		return ""
	}
	v, _, _ := strings.Cut(filepath.Base(target), "-")
	return v
}

func Port() (int, error) {
	content, err := os.ReadFile(filepath.Join(Prefix(), "redis.conf"))
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(content), "\n") {
		if field, ok := strings.CutPrefix(line, "port "); ok {
			port, err := strconv.Atoi(field)
			if err == nil && port > 0 && port <= 65535 {
				return port, nil
			}
			break
		}
	}
	return 0, fmt.Errorf("Redis 配置中的端口无效")
}

func ownUnit() bool {
	data, err := os.ReadFile(unitPath())
	return err == nil && strings.Contains(string(data), "\n"+serviceMarker+"\n")
}

func Status() string {
	if !ownUnit() || Version() == "" {
		return "missing"
	}
	if run(io.Discard, 5*time.Second, "", "systemctl", "is-active", "--quiet", ServiceName) == nil {
		return "running"
	}
	return "exited"
}

func systemctl(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "systemctl", args...)
	cmd.Env = util.ChildEnv()
	data, err := cmd.CombinedOutput()
	out := strings.TrimSpace(string(data))
	if err != nil {
		return out, fmt.Errorf("Redis 服务操作失败: %s (%w)", out, err)
	}
	return out, nil
}

func control(action string, options ...string) error {
	args := append([]string{action}, options...)
	_, err := systemctl(append(args, ServiceName)...)
	return err
}

func Action(action string) error {
	if !ownUnit() || Version() == "" {
		return fmt.Errorf("原生 Redis 未安装")
	}
	switch action {
	case "start", "stop", "restart":
		return control(action)
	default:
		return fmt.Errorf("不支持的 Redis 服务操作")
	}
}

func Logs() (string, error) {
	if !ownUnit() {
		return "", fmt.Errorf("原生 Redis 未安装")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "journalctl", "-u", ServiceName, "-n", "120", "--no-pager", "-o", "short-iso").Output()
	return string(out), err
}

func redisCLI(args ...string) (string, error) {
	token, err := os.ReadFile(tokenPath())
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	options := []string{"-e", "--raw", "-s", Socket(), "--user", panelUser}
	cmd := exec.CommandContext(ctx, filepath.Join(Prefix(), "bin", "redis-cli"), append(options, args...)...)
	for _, item := range util.ChildEnv() {
		if !strings.HasPrefix(item, "REDISCLI_") {
			cmd.Env = append(cmd.Env, item)
		}
	}
	cmd.Env = append(cmd.Env, "REDISCLI_AUTH="+string(token))
	out, err := cmd.CombinedOutput()
	result := strings.TrimSpace(string(out))
	if err != nil {
		return "", fmt.Errorf("Redis 命令执行失败: %s (%w)", result, err)
	}
	return result, nil
}

func Metrics() (map[string]string, error) {
	text, err := redisCLI("INFO")
	if err != nil {
		return nil, err
	}
	metrics := make(map[string]string)
	for _, line := range strings.Split(text, "\n") {
		key, value, _ := strings.Cut(strings.TrimSuffix(line, "\r"), ":")
		switch key {
		case "used_memory_human", "connected_clients", "aof_enabled", "aof_last_write_status", "rdb_last_bgsave_status", "rdb_last_save_time", "rdb_bgsave_in_progress":
			metrics[key] = value
		}
	}
	return metrics, nil
}

func Snapshot() error {
	_, err := redisCLI("BGSAVE")
	return err
}

func ValidatePassword(password string) error {
	if len(password) < 8 || len(password) > 128 || strings.IndexFunc(password, unicode.IsControl) >= 0 {
		return fmt.Errorf("Redis 密码须为 8 到 128 个字节，且不能包含控制字符")
	}
	return nil
}

func aclContent(password string, token []byte) string {
	return fmt.Sprintf("user default reset on #%x ~* &* +@all\nuser %s reset on #%x +info +bgsave\n",
		sha256.Sum256([]byte(password)), panelUser, sha256.Sum256(token))
}
