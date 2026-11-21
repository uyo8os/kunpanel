package nativephp

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"golang.org/x/mod/semver"
)

type Extension struct {
	Name    string `json:"name"`
	Source  string `json:"source"`
	Version string `json:"version,omitempty"`
	Minimum string `json:"minimum,omitempty"`
}

const managedExtensionMarker = "; KunPanel extension "

func extensionIniPath(minor, name string) string {
	return filepath.Join(ScanDir(minor), "50-kunpanel-"+name+".ini")
}

func extensionModulePath(prefix, name string, rel extensionRelease) string {
	return filepath.Join(prefix, "kunpanel-extensions", name+"-"+rel.version+"-"+rel.commit+".so")
}

func (ext extensionSource) directive() string {
	if ext.zend {
		return "zend_extension"
	}
	return "extension"
}

func extensionIni(minor string, ext extensionSource, rel extensionRelease) string {
	return fmt.Sprintf("%s%s %s %s\n%s=%s\n", managedExtensionMarker, ext.name, rel.version, rel.commit,
		ext.directive(), strconv.Quote(extensionModulePath(Prefix(minor), ext.name, rel)))
}

func parseManagedExtension(minor, filename string, contents []byte) (extensionRelease, bool) {
	line, _, _ := strings.Cut(string(contents), "\n")
	meta, ok := strings.CutPrefix(line, managedExtensionMarker)
	fields := strings.Fields(meta)
	if !ok || len(fields) != 3 {
		return extensionRelease{}, false
	}
	ext, known := findExtension(fields[0])
	release := extensionRelease{fields[1], fields[2]}
	if !known || filename != extensionIniPath(minor, ext.name) || !stableExtensionVersion.MatchString(release.version) || !gitCommit.MatchString(release.commit) {
		return extensionRelease{}, false
	}
	return release, string(contents) == extensionIni(minor, ext, release)
}

func managedExtension(minor, name string) (extensionRelease, []byte, error) {
	filename := extensionIniPath(minor, name)
	data, err := readRuntimeFile(filename)
	if err != nil || data == nil {
		return extensionRelease{}, data, err
	}
	if rel, ok := parseManagedExtension(minor, filename, data); ok {
		return rel, data, nil
	}
	return extensionRelease{}, data, fmt.Errorf("面板扩展配置已被手动修改: %s", filename)
}

func Extensions(minor string) ([]Extension, error) {
	list := make([]Extension, 0, len(extensionSources))
	for _, ext := range extensionSources {
		rel, _, err := managedExtension(minor, ext.name)
		if err != nil {
			return nil, err
		}
		list = append(list, Extension{Name: ext.name, Source: "https://github.com/" + ext.repo, Version: rel.version, Minimum: ext.minimum[minor]})
	}
	return list, nil
}

func extensionDependencies(ext extensionSource, w io.Writer) error {
	var deb, rpm []string
	if !exists("autoconf") || !exists("gcc") || !exists("make") || !exists("pkg-config") || !exists("tar") {
		deb = []string{"autoconf", "build-essential", "pkg-config", "tar"}
		rpm = []string{"autoconf", "gcc", "make", "pkgconf-pkg-config", "tar"}
	}
	if ext.name == "imagick" && exec.Command("pkg-config", "--exists", "MagickWand").Run() != nil {
		deb = append(deb, "libmagickwand-dev")
		rpm = append(rpm, "ImageMagick-devel")
	}
	if len(deb) == 0 {
		return nil
	}
	fmt.Fprintln(w, "准备扩展编译工具和开发库...")
	return installDevPackages(w, deb, rpm)
}

func buildExtension(minor, prefix string, ext extensionSource, rel extensionRelease, w io.Writer) ([]byte, error) {
	if err := extensionDependencies(ext, w); err != nil {
		return nil, err
	}
	work, err := createBuildDir(minor)
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(work)
	fmt.Fprintf(w, "下载并编译 %s %s (%s)\n", ext.name, rel.version, ext.repo)
	url := "https://codeload.github.com/" + ext.repo + "/tar.gz/" + rel.commit
	archive, err := fetchWithClient(context.Background(), extensionClient, url, 32<<20)
	if err != nil {
		return nil, fmt.Errorf("下载 %s 官方源码失败: %w", ext.name, err)
	}
	archivePath := filepath.Join(work, "source.tar.gz")
	if err := os.WriteFile(archivePath, archive, 0o600); err != nil {
		return nil, err
	}
	if err := run(w, 2*time.Minute, work, "tar", "-xzf", archivePath, "--no-same-owner", "--no-same-permissions"); err != nil {
		return nil, err
	}
	source := filepath.Join(work, path.Base(ext.repo)+"-"+rel.commit)
	if ext.name == "imagick" {
		// Imagick ships a version placeholder in Git, filled by its packaging step.
		header := filepath.Join(source, "php_imagick.h")
		contents, err := os.ReadFile(header)
		if err != nil {
			return nil, err
		}
		const placeholder = `"@PACKAGE_VERSION@"`
		if strings.Contains(string(contents), placeholder) {
			contents := strings.Replace(string(contents), placeholder, strconv.Quote(rel.version), 1)
			if err := os.WriteFile(header, []byte(contents), 0o644); err != nil {
				return nil, err
			}
		}
	}
	phpize := filepath.Join(prefix, "bin", "phpize")
	phpConfig := filepath.Join(prefix, "bin", "php-config")
	if err := run(w, 2*time.Minute, source, phpize); err != nil {
		return nil, err
	}
	if err := run(w, 5*time.Minute, source, "./configure", "--with-php-config="+phpConfig); err != nil {
		return nil, err
	}
	if err := run(w, 20*time.Minute, source, "make", "-j", strconv.Itoa(min(runtime.NumCPU(), 2))); err != nil {
		return nil, err
	}
	module := filepath.Join(source, "modules", ext.name+".so")
	out, err := commandOutput(30*time.Second, filepath.Join(prefix, "bin", "php"), "-n", "-d", ext.directive()+"="+module,
		"-r", `if (!extension_loaded($argv[1])) exit(1); echo phpversion($argv[1]);`, "--", ext.name)
	if err != nil || strings.TrimSpace(out) != rel.version {
		return nil, fmt.Errorf("PHP %s 无法加载 %s %s: %s", minor, ext.name, rel.version, strings.TrimSpace(out))
	}
	data, err := os.ReadFile(module)
	if err != nil {
		return nil, err
	}
	return data, nil
}

func rebuildManagedExtensions(minor, target string, w io.Writer) error {
	for _, ext := range extensionSources {
		rel, _, err := managedExtension(minor, ext.name)
		if err != nil {
			return err
		}
		if rel.version == "" {
			continue
		}
		module, err := buildExtension(minor, target, ext, rel, w)
		if err != nil {
			return err
		}
		if err := writeRuntimeFile(extensionModulePath(target, ext.name, rel), module); err != nil {
			return err
		}
	}
	return nil
}

func hasModule(output, name string) bool {
	for _, line := range strings.Split(output, "\n") {
		if strings.EqualFold(strings.TrimSpace(line), name) {
			return true
		}
	}
	return false
}

func extensionModules(binary string) (string, error) {
	out, err := commandOutput(30*time.Second, binary, "-m")
	if err != nil || strings.Contains(out, "PHP Startup:") || strings.Contains(out, "already loaded") {
		return "", fmt.Errorf("PHP 扩展加载检查失败: %s %v", strings.TrimSpace(out), err)
	}
	return out, nil
}

func verifyManagedExtensions(minor string) error {
	for _, ext := range extensionSources {
		rel, _, err := managedExtension(minor, ext.name)
		if err != nil {
			return err
		}
		if rel.version != "" {
			return verifyExtensionModules(minor, "", false)
		}
	}
	return nil
}

func verifyExtensionModules(minor, removed string, checkConfig bool) error {
	list, err := Extensions(minor)
	if err != nil {
		return err
	}
	for _, binary := range []string{Binary(minor), FpmBinary(minor)} {
		modules, err := extensionModules(binary)
		if err != nil {
			return err
		}
		for _, ext := range list {
			if ext.Version != "" && !hasModule(modules, ext.Name) {
				return fmt.Errorf("%s 未加载扩展 %s", filepath.Base(binary), ext.Name)
			}
		}
		if removed != "" && hasModule(modules, removed) {
			return fmt.Errorf("其他 PHP 配置仍在加载 %s，请先检查手动配置", removed)
		}
	}
	if checkConfig {
		return checkFPMConfig(minor)
	}
	return nil
}

// ManageExtension installs the latest stable source from the upstream project,
// updates a panel-managed module, or removes it from a specific PHP branch.
func ManageExtension(minor, name string, remove bool, w io.Writer) error {
	mu.Lock()
	defer mu.Unlock()
	if w == nil {
		w = io.Discard
	}
	ext, ok := findExtension(name)
	if !ok {
		return fmt.Errorf("该扩展不在原生 PHP 支持列表中")
	}
	if m, err := Minor(minor); err != nil || m != minor || !systemd() || !Installed(minor) || os.Geteuid() != 0 {
		return fmt.Errorf("管理扩展需要已安装的原生 PHP 和 Linux systemd root 环境")
	}
	version := CurrentVersion(minor)
	if version == "" {
		return fmt.Errorf("无法识别当前 PHP 安装目录")
	}
	previous, oldIni, err := managedExtension(minor, name)
	if err != nil {
		return err
	}
	ini := extensionIniPath(minor, name)
	var oldPath, newPath, removed string
	var newIni []byte
	if oldIni != nil {
		oldPath = extensionModulePath(Prefix(minor), name, previous)
	}
	if remove {
		if oldIni == nil {
			return fmt.Errorf("未找到面板安装的 %s 扩展", name)
		}
		removed = name
	} else {
		if oldIni == nil {
			modules, err := extensionModules(Binary(minor))
			if err != nil {
				return err
			}
			if hasModule(modules, name) {
				return fmt.Errorf("%s 已由其他 PHP 配置加载，请先检查手动配置", name)
			}
		}
		release, err := latestExtension(minor, ext)
		if err != nil {
			return err
		}
		if previous.version != "" && semver.Compare("v"+release.version, "v"+previous.version) < 0 {
			return fmt.Errorf("当前 %s %s 比上游可用版本 %s 更新", name, previous.version, release.version)
		}
		if previous.version == release.version && previous.commit != release.commit {
			return fmt.Errorf("%s %s 的上游标签指向已变更，请检查扩展项目的发布记录", name, release.version)
		}
		if release == previous {
			if st, err := os.Stat(oldPath); err == nil && st.Mode().IsRegular() {
				fmt.Fprintf(w, "%s 已是当前官方正式版本 %s\n", name, release.version)
				return verifyExtensionModules(minor, "", true)
			}
		}
		target := filepath.Join(root(minor), "versions", version)
		module, err := buildExtension(minor, target, ext, release, w)
		if err != nil {
			return err
		}
		newPath = extensionModulePath(Prefix(minor), name, release)
		if err := writeRuntimeFile(newPath, module); err != nil {
			return err
		}
		newIni = []byte(extensionIni(minor, ext, release))
	}
	if err := activateExtension(minor, removed, ini, oldIni, newIni); err != nil {
		if newPath != "" && newPath != oldPath {
			// Keep the candidate if restoring the previous configuration failed.
			if data, readErr := readRuntimeFile(ini); readErr == nil && string(data) == string(oldIni) {
				_ = writeRuntimeFile(newPath, nil)
			}
		}
		return err
	}
	if oldPath != "" && oldPath != newPath {
		if err := writeRuntimeFile(oldPath, nil); err != nil {
			fmt.Fprintf(w, "旧扩展文件清理失败: %v\n", err)
		}
	}
	if remove {
		fmt.Fprintf(w, "已卸载 %s\n", name)
	} else {
		fmt.Fprintf(w, "%s 扩展已安装并通过 PHP-FPM 加载检查\n", name)
	}
	return nil
}

func activateExtension(minor, removed, ini string, oldIni, newIni []byte) error {
	// Keep the previously loaded module on disk until this atomic INI change
	// has been validated and the service has successfully accepted it.
	_, err := activateRuntimeINI(minor, ini, oldIni, newIni, func() error {
		return verifyExtensionModules(minor, removed, true)
	})
	return err
}
