package nativepg

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"time"

	"kunpanel/internal/util"
)

const buildUser = "kunpanel-pgbuild"

func buildDependencies(log io.Writer) error {
	// These pkg-config modules cover the features enabled by configure below.
	allTools := true
	for _, tool := range []string{"gcc", "make", "pkg-config", "bison", "flex", "tar", "gzip", "perl"} {
		if _, err := exec.LookPath(tool); err != nil {
			allTools = false
			break
		}
	}
	if allTools && run(io.Discard, 10*time.Second, "", "pkg-config", "--exists", "readline", "openssl", "icu-i18n", "zlib", "libxml-2.0", "liblz4", "libzstd") == nil {
		return nil
	}
	fmt.Fprintln(log, "安装 PostgreSQL 编译依赖")
	if _, err := exec.LookPath("apt-get"); err == nil {
		if err := run(log, 6*time.Minute, "", "apt-get", "update"); err != nil {
			return err
		}
		return run(log, 12*time.Minute, "", "env", "DEBIAN_FRONTEND=noninteractive", "apt-get", "install", "-y", "--no-install-recommends",
			"build-essential", "bison", "flex", "pkg-config", "perl", "tar", "gzip", "libreadline-dev", "libssl-dev", "libicu-dev", "zlib1g-dev", "libxml2-dev", "liblz4-dev", "libzstd-dev")
	}
	for _, manager := range []string{"dnf", "yum"} {
		if _, err := exec.LookPath(manager); err == nil {
			return run(log, 12*time.Minute, "", manager, "install", "-y", "gcc", "make", "bison", "flex", "pkgconfig", "perl", "tar", "gzip", "readline-devel", "openssl-devel", "libicu-devel", "zlib-devel", "libxml2-devel", "lz4-devel", "libzstd-devel")
		}
	}
	return fmt.Errorf("缺少 PostgreSQL 编译依赖，当前系统未找到 apt-get、dnf 或 yum")
}

func run(log io.Writer, timeout time.Duration, dir, name string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir, cmd.Stdout, cmd.Stderr = dir, log, log
	cmd.Env = util.ChildEnv()
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("%s 执行超时: %w", name, ctx.Err())
		}
		return fmt.Errorf("%s 执行失败: %w", name, err)
	}
	return nil
}

func build(version string, log io.Writer) (string, error) {
	if err := buildDependencies(log); err != nil {
		return "", err
	}
	if _, err := ensureUser(buildUser, log); err != nil {
		return "", err
	}
	versions := filepath.Join(root(), "versions")
	if err := os.MkdirAll(versions, 0o755); err != nil {
		return "", err
	}
	target, err := os.MkdirTemp(versions, version+"-")
	if err != nil {
		return "", err
	}
	defer os.Remove(target)
	work, err := os.MkdirTemp(root(), ".pg-build-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(work)
	archive, err := download(version, work, log)
	if err != nil {
		return "", err
	}
	if err := run(log, 3*time.Minute, work, "tar", "-xzf", archive, "--no-same-owner", "--no-same-permissions"); err != nil {
		return "", err
	}
	if err := os.Remove(archive); err != nil {
		return "", err
	}
	tmp := filepath.Join(work, "tmp")
	if err := os.Mkdir(tmp, 0o700); err != nil {
		return "", err
	}
	if err := run(log, time.Minute, "", "chown", "-hR", buildUser, "--", work); err != nil {
		return "", err
	}
	src := filepath.Join(work, "postgresql-"+version)
	stage := filepath.Join(work, "installed")
	buildStep := func(timeout time.Duration, name string, args ...string) error {
		argv := []string{"-u", buildUser, "--", "env", "TMPDIR=" + tmp, "LC_ALL=C", name}
		return run(log, timeout, src, "runuser", append(argv, args...)...)
	}
	fmt.Fprintln(log, "编译 PostgreSQL 和官方附带扩展，耗时取决于服务器性能")
	if err := buildStep(8*time.Minute, "./configure", "--prefix="+target, "--with-openssl", "--with-icu", "--with-libxml", "--with-lz4", "--with-zstd"); err != nil {
		return "", err
	}
	jobs := strconv.Itoa(max(1, min(runtime.NumCPU(), 2)))
	if err := buildStep(80*time.Minute, "make", "-s", "-j"+jobs, "world-bin"); err != nil {
		return "", err
	}
	if err := buildStep(12*time.Minute, "make", "-s", "install-world-bin", "DESTDIR="+stage); err != nil {
		return "", err
	}
	installed := filepath.Join(stage, target)
	for _, binary := range []string{"postgres", "psql", "initdb", "pg_dump", "pg_restore"} {
		st, err := os.Lstat(filepath.Join(installed, "bin", binary))
		if err != nil || !st.Mode().IsRegular() || st.Mode()&0o111 == 0 {
			return "", fmt.Errorf("编译结果缺少 PostgreSQL 程序 %s", binary)
		}
	}
	if err := run(log, time.Minute, "", "chown", "-hR", "root:root", "--", installed); err != nil {
		return "", err
	}
	if err := os.Remove(target); err != nil {
		return "", err
	}
	if err := os.Rename(installed, target); err != nil {
		return "", err
	}
	return target, nil
}
