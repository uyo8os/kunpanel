//go:build linux

package websites

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"kunpanel/internal/nativenginx"
	"kunpanel/internal/nativephp"
	"kunpanel/internal/util"
)

type phpWorkerIdentity struct {
	uid    int
	gid    int
	groups map[uint32]bool
}

var nginxUserDirective = regexp.MustCompile(`(?m)^\s*user\s+([^;\r\n]+)\s*;`)

// A panel-managed Nginx may be configured to run under a user other than
// nobody. Check actual configured worker permissions when it is running so an
// incompatible local socket does not silently result in a 502 response.
func checkNativeNginxSocketAccess(socket string) error {
	if nativenginx.Status() != "running" {
		return nil
	}
	fi, err := os.Stat(socket)
	if err != nil || fi.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("PHP-FPM socket 不可用，请确认该 PHP 版本正在运行: %s", socket)
	}
	out, err := util.RunWithTimeout(15*time.Second, nativenginx.Binary(), "-T", "-p", nativenginx.Prefix()+"/", "-c", nativenginx.ConfigPath())
	if err != nil {
		return fmt.Errorf("无法检查 Nginx 运行用户，请先检查 Nginx 配置: %w", err)
	}
	username, group := "nobody", ""
	if match := nginxUserDirective.FindStringSubmatch(out); len(match) > 1 {
		fields := strings.Fields(match[1])
		if len(fields) < 1 || len(fields) > 2 {
			return fmt.Errorf("无法识别 Nginx user 指令，请检查配置")
		}
		username = strings.Trim(fields[0], `"'`)
		if len(fields) == 2 {
			group = strings.Trim(fields[1], `"'`)
		}
	}
	return checkNginxUserSocketPermissions(socket, username, group, fi)
}

func checkNginxUserSocketPermissions(socket, username, group string, fi os.FileInfo) error {
	usr, err := user.Lookup(username)
	if err != nil {
		return fmt.Errorf("无法识别 Nginx 运行用户 %s: %w", username, err)
	}
	uid, err := strconv.Atoi(usr.Uid)
	if err != nil {
		return fmt.Errorf("Nginx 运行用户无效")
	}
	if uid == 0 {
		return nil
	}
	groups := map[uint32]bool{}
	if gids, err := usr.GroupIds(); err == nil {
		for _, id := range gids {
			if n, err := strconv.Atoi(id); err == nil {
				groups[uint32(n)] = true
			}
		}
	}
	if n, err := strconv.Atoi(usr.Gid); err == nil {
		groups[uint32(n)] = true
	}
	if group != "" {
		g, err := user.LookupGroup(group)
		if err != nil {
			return fmt.Errorf("无法识别 Nginx 运行用户组 %s: %w", group, err)
		}
		if n, err := strconv.Atoi(g.Gid); err == nil {
			groups[uint32(n)] = true
		}
	}
	stat, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("无法检查 PHP-FPM socket 权限")
	}
	bits := fi.Mode().Perm() & 0o7
	if int(stat.Uid) == uid {
		bits = (fi.Mode().Perm() >> 6) & 0o7
	} else if groups[stat.Gid] {
		bits = (fi.Mode().Perm() >> 3) & 0o7
	}
	if bits&0o2 == 0 {
		return fmt.Errorf("Nginx 运行用户 %s 无权连接 PHP-FPM socket；请将 Nginx 运行用户或 FPM socket 属组配置为可访问的相同用户/组，再重试", username)
	}
	for path := filepath.Dir(socket); ; path = filepath.Dir(path) {
		info, err := os.Stat(path)
		if err != nil || !info.IsDir() {
			return fmt.Errorf("Nginx 无法访问 PHP-FPM socket 的目录 %s", path)
		}
		owner, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return fmt.Errorf("无法检查 PHP-FPM socket 目录 %s 的权限", path)
		}
		mode := info.Mode().Perm() & 0o7
		if int(owner.Uid) == uid {
			mode = (info.Mode().Perm() >> 6) & 0o7
		} else if groups[owner.Gid] {
			mode = (info.Mode().Perm() >> 3) & 0o7
		}
		if mode&0o1 == 0 {
			return fmt.Errorf("Nginx 运行用户 %s 无法进入 PHP-FPM socket 目录 %s，请调整目录访问权限", username, path)
		}
		if path == "/" {
			break
		}
	}
	return nil
}

func nativePHPWorkerIdentity() (phpWorkerIdentity, error) {
	usr, err := user.Lookup(nativephp.WorkerUser())
	if err != nil {
		return phpWorkerIdentity{}, fmt.Errorf("无法获取 PHP-FPM 用户 %s: %w", nativephp.WorkerUser(), err)
	}
	uid, e1 := strconv.Atoi(usr.Uid)
	gid, e2 := strconv.Atoi(usr.Gid)
	if e1 != nil || e2 != nil {
		return phpWorkerIdentity{}, fmt.Errorf("PHP-FPM 用户身份无效")
	}
	groups := map[uint32]bool{uint32(gid): true}
	if gids, err := usr.GroupIds(); err == nil {
		for _, s := range gids {
			if id, err := strconv.Atoi(s); err == nil {
				groups[uint32(id)] = true
			}
		}
	}
	return phpWorkerIdentity{uid: uid, gid: gid, groups: groups}, nil
}

// checkExistingNativePHPSiteDirectory never changes an existing directory.
// It checks basic Unix permission bits for traversal and reading.
func checkExistingNativePHPSiteDirectory(path string) error {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil // The creation path will set ownership on its new leaf directory.
	} else if err != nil {
		return err
	}
	worker, err := nativePHPWorkerIdentity()
	if err != nil {
		return err
	}
	return checkNativePHPDirAccess(path, worker)
}

func checkNativePHPDirAccess(path string, worker phpWorkerIdentity) error {
	path = filepath.Clean(path)
	if !filepath.IsAbs(path) {
		return fmt.Errorf("原生 PHP 站点根目录必须是绝对路径")
	}
	for current := path; ; current = filepath.Dir(current) {
		fi, err := os.Stat(current)
		if err != nil {
			return fmt.Errorf("无法检查站点目录 %s: %w", current, err)
		}
		if !fi.IsDir() {
			return fmt.Errorf("站点路径不是目录: %s", current)
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok {
			return fmt.Errorf("无法检查站点目录权限: %s", current)
		}
		mode := fi.Mode().Perm()
		bits := mode & 0o7
		if int(st.Uid) == worker.uid {
			bits = (mode >> 6) & 0o7
		} else if worker.groups[st.Gid] {
			bits = (mode >> 3) & 0o7
		}
		need := os.FileMode(0o1) // Every parent directory must be traversable.
		if current == path {
			need = 0o5 // PHP only needs write access to directories the application modifies.
		}
		if bits&need != need {
			return fmt.Errorf("PHP-FPM 用户 %s 无法读取或进入站点目录 %s（受限路径 %s），请检查目录属主和权限", nativephp.WorkerUser(), path, current)
		}
		if current == "/" {
			return nil
		}
	}
}

// Only a leaf directory created by this operation is assigned to the worker.
// Existing paths and their contents are never recursively modified.
func prepareNativePHPSiteDirectory(path string) error {
	worker, err := nativePHPWorkerIdentity()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	created := false
	if err := os.Mkdir(path, 0o755); err == nil {
		created = true
	} else if !os.IsExist(err) {
		return err
	}
	if created {
		if err := os.Chown(path, worker.uid, worker.gid); err != nil {
			_ = os.Remove(path)
			return fmt.Errorf("设置新站点目录的 PHP-FPM 用户失败: %w", err)
		}
	}
	if err := checkNativePHPDirAccess(path, worker); err != nil {
		if created {
			_ = os.Remove(path) // only remove the empty leaf created by this call
		}
		return err
	}
	return nil
}
