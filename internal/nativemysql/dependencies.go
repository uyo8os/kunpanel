package nativemysql

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type systemPackage struct{ apt, rpm string }

type packageManager struct{ name string }

func findPackageManager(w io.Writer) (packageManager, error) {
	for _, name := range []string{"apt-get", "dnf", "yum"} {
		if _, err := exec.LookPath(name); err != nil {
			continue
		}
		if name == "apt-get" {
			if err := run(w, 5*time.Minute, name, "update"); err != nil {
				return packageManager{}, err
			}
		}
		return packageManager{name}, nil
	}
	return packageManager{}, fmt.Errorf("缺少 MySQL 依赖，当前系统未找到 apt-get、dnf 或 yum")
}

func (m packageManager) install(w io.Writer, packages ...systemPackage) error {
	var names []string
	seen := map[string]bool{}
	for _, p := range packages {
		name := p.rpm
		if m.name == "apt-get" {
			name = p.apt
		}
		if name != "" && !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return nil
	}
	args := []string{"install", "-y"}
	if m.name == "apt-get" {
		args = append(args, "--no-install-recommends")
	}
	return run(w, 8*time.Minute, m.name, append(args, names...)...)
}

func prepareTools(w io.Writer) error {
	var missing []systemPackage
	for _, tool := range []struct {
		name string
		pkg  systemPackage
	}{
		{"gpg", systemPackage{"gpg", "gnupg2"}},
		{"gpgv", systemPackage{"gpgv", "gnupg2"}},
		{"tar", systemPackage{"tar", "tar"}},
		{"xz", systemPackage{"xz-utils", "xz"}},
		{"gzip", systemPackage{"gzip", "gzip"}},
		{"ldd", systemPackage{"libc-bin", "glibc"}},
	} {
		if _, err := exec.LookPath(tool.name); err != nil {
			missing = append(missing, tool.pkg)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	m, err := findPackageManager(w)
	if err != nil {
		return err
	}
	return m.install(w, missing...)
}

var mysqlLibraries = map[string]systemPackage{
	"libaio.so.1":      {"libaio1", "libaio"},
	"libnuma.so.1":     {"libnuma1", "numactl-libs"},
	"libtinfo.so.5":    {"libtinfo5", "ncurses-compat-libs"},
	"libtinfo.so.6":    {"libtinfo6", "ncurses-libs"},
	"libncurses.so.5":  {"libncurses5", "ncurses-compat-libs"},
	"libncursesw.so.5": {"libncursesw5", "ncurses-compat-libs"},
	"libncurses.so.6":  {"libncurses6", "ncurses-libs"},
	"libncursesw.so.6": {"libncursesw6", "ncurses-libs"},
	"libcrypt.so.1":    {"libcrypt1", "libxcrypt-compat"},
	"libz.so.1":        {"zlib1g", "zlib"},
}

func missingLibraries(prefix string) ([]string, error) {
	missing := map[string]bool{}
	for _, name := range []string{"mysqld", "mysql", "mysqldump"} {
		file := filepath.Join(prefix, "bin", name)
		st, err := os.Stat(file)
		if err != nil || !st.Mode().IsRegular() {
			return nil, fmt.Errorf("官方 MySQL 安装包缺少 %s", name)
		}
		cmd := exec.Command("ldd", file)
		cmd.Env = append(libraryEnv(prefix), "LC_ALL=C")
		out, err := cmd.CombinedOutput()
		if err != nil {
			return nil, fmt.Errorf("无法检查 MySQL %s 的系统依赖: %s (%w)", name, strings.TrimSpace(string(out)), err)
		}
		for _, line := range strings.Split(string(out), "\n") {
			fields := strings.Fields(line)
			if len(fields) >= 4 && strings.Join(fields[1:4], " ") == "=> not found" {
				missing[fields[0]] = true
			}
		}
	}
	libraries := make([]string, 0, len(missing))
	for name := range missing {
		libraries = append(libraries, name)
	}
	sort.Strings(libraries)
	return libraries, nil
}

func prepareLibraries(prefix string, w io.Writer) error {
	missing, err := missingLibraries(prefix)
	if err != nil || len(missing) == 0 {
		return err
	}
	m, err := findPackageManager(w)
	if err != nil {
		return err
	}
	var packages []systemPackage
	var legacyNCurses []string
	var aioCompat bool
	debian := m.name == "apt-get" && isDebian()
	for _, library := range missing {
		p, ok := mysqlLibraries[library]
		if !ok {
			return fmt.Errorf("当前系统缺少 MySQL 依赖 %s", library)
		}
		if library == "libaio.so.1" && m.name == "apt-get" && !aptHasPackage(p.apt) {
			// Both supported architectures are 64-bit. The t64 library keeps the libaio ABI there.
			p.apt, aioCompat = "libaio1t64", true
		}
		if _, legacy := debianNCurses5[library]; debian && legacy && !aptHasPackage(p.apt) {
			legacyNCurses = append(legacyNCurses, library)
			continue
		}
		packages = append(packages, p)
	}
	if err := m.install(w, packages...); err != nil {
		return fmt.Errorf("安装 MySQL 系统依赖 %s 失败: %w", strings.Join(missing, ", "), err)
	}
	if aioCompat {
		if err := linkLibAIO(prefix); err != nil {
			return err
		}
	}
	if len(legacyNCurses) > 0 {
		if err := installDebianNCurses5(prefix, legacyNCurses, w); err != nil {
			return err
		}
	}
	missing, err = missingLibraries(prefix)
	if err == nil && len(missing) != 0 {
		return fmt.Errorf("MySQL 仍缺少系统依赖: %s", strings.Join(missing, ", "))
	}
	return err
}

func aptHasPackage(name string) bool {
	out, err := output(10*time.Second, "apt-cache", "show", "--no-all-versions", name)
	return err == nil && strings.Contains("\n"+out, "\nPackage: "+name+"\n")
}

func linkLibAIO(prefix string) error {
	out, err := output(10*time.Second, "ldconfig", "-p")
	if err != nil {
		return err
	}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 || fields[0] != "libaio.so.1t64" || !filepath.IsAbs(fields[len(fields)-1]) {
			continue
		}
		dir := filepath.Join(prefix, "compat")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		return os.Symlink(fields[len(fields)-1], filepath.Join(dir, "libaio.so.1"))
	}
	return fmt.Errorf("未找到 64 位 MySQL 所需的 libaio 兼容库")
}
