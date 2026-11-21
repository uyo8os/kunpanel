package nativeredis

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
	"time"

	"kunpanel/internal/util"
)

const buildUser = "kunpanel-rbuild"

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
		return owner{}, fmt.Errorf("Redis 需要独立的非 root 系统用户")
	}
	return owner{uid, gid}, nil
}

func run(log io.Writer, timeout time.Duration, dir, name string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir, cmd.Stdout, cmd.Stderr = dir, log, log
	cmd.Env, cmd.WaitDelay = util.ChildEnv(), 5*time.Second
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("%s 执行超时: %w", name, ctx.Err())
		}
		return fmt.Errorf("%s 执行失败: %w", name, err)
	}
	return nil
}

func buildDependencies(log io.Writer) error {
	ready := true
	for _, tool := range []string{"gcc", "make", "pkg-config", "python3", "tar", "gzip"} {
		if _, err := exec.LookPath(tool); err != nil {
			ready = false
			break
		}
	}
	if ready && run(io.Discard, 5*time.Second, "", "pkg-config", "--exists", "libsystemd", "openssl") == nil {
		return nil
	}
	fmt.Fprintln(log, "安装 Redis 编译依赖")
	if _, err := exec.LookPath("apt-get"); err == nil {
		if err := run(log, 6*time.Minute, "", "apt-get", "update"); err != nil {
			return err
		}
		return run(log, 12*time.Minute, "", "env", "DEBIAN_FRONTEND=noninteractive", "apt-get", "install", "-y", "--no-install-recommends",
			"build-essential", "pkg-config", "python3", "tar", "gzip", "libsystemd-dev", "libssl-dev")
	}
	for _, manager := range []string{"dnf", "yum"} {
		if _, err := exec.LookPath(manager); err == nil {
			return run(log, 12*time.Minute, "", manager, "install", "-y",
				"gcc", "make", "pkgconfig", "python3", "tar", "gzip", "systemd-devel", "openssl-devel")
		}
	}
	return fmt.Errorf("缺少 Redis 编译依赖，当前系统未找到 apt-get、dnf 或 yum")
}

func build(release Release, log io.Writer) (string, error) {
	if err := buildDependencies(log); err != nil {
		return "", err
	}
	builder, err := ensureUser(buildUser, log)
	if err != nil {
		return "", err
	}
	work, err := os.MkdirTemp(root(), ".redis-build-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(work)
	archive, err := download(release, work, log)
	if err != nil {
		return "", err
	}
	if err := run(log, 4*time.Minute, work, "tar", "-xzf", archive, "--no-same-owner", "--no-same-permissions"); err != nil {
		return "", err
	}
	if err := os.Remove(archive); err != nil {
		return "", err
	}
	tmp := filepath.Join(work, "tmp")
	if err := os.Mkdir(tmp, 0o700); err != nil {
		return "", err
	}
	if err := run(log, time.Minute, "", "chown", "-hR", fmt.Sprintf("%d:%d", builder.uid, builder.gid), "--", work); err != nil {
		return "", err
	}
	src := filepath.Join(work, "redis-"+release.Version, "src")
	jobs := strconv.Itoa(max(1, min(runtime.NumCPU(), 2)))
	fmt.Fprintln(log, "正在编译 Redis 核心服务，临时文件保存在面板数据盘")
	if err := run(log, 45*time.Minute, src, "runuser", "-u", buildUser, "--", "env", "TMPDIR="+tmp, "LC_ALL=C",
		"make", "-s", "-j"+jobs, "OPTIMIZATION=-O2", "DEBUG=", "USE_SYSTEMD=yes", "BUILD_TLS=yes", "redis-server", "redis-cli"); err != nil {
		return "", err
	}
	versions := filepath.Join(root(), "versions")
	if err := os.MkdirAll(versions, 0o755); err != nil {
		return "", err
	}
	target, err := os.MkdirTemp(versions, release.Version+"-")
	if err != nil {
		return "", err
	}
	success := false
	defer func() {
		if !success {
			_ = os.RemoveAll(target)
		}
	}()
	bin := filepath.Join(target, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		return "", err
	}
	for _, name := range []string{"redis-server", "redis-cli"} {
		file := filepath.Join(src, name)
		st, err := os.Lstat(file)
		if err != nil || !st.Mode().IsRegular() || st.Mode()&0o111 == 0 {
			return "", fmt.Errorf("编译结果缺少 Redis 程序 %s", name)
		}
		if err := run(log, time.Minute, "", "install", "-m", "0755", "--", file, filepath.Join(bin, name)); err != nil {
			return "", err
		}
	}
	if err := os.Chmod(target, 0o755); err != nil {
		return "", err
	}
	success = true
	return target, nil
}
