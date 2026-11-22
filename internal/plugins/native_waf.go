package plugins

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"kunpanel/internal/nativenginx"
	"kunpanel/internal/util"
	"kunpanel/internal/websites"
)

var nativeWorker = regexp.MustCompile(`(?m)^\s*user[ \t]+([^ \t;\r\n]+)[ \t]*(?:[^ \t;\r\n]+[ \t]*)?;`)

func nativeWafLogDir() string { return filepath.Join(wafDir(), "native-logs") }

func nativeWafSnippetPresent() bool {
	b, err := os.ReadFile(wafConfSnippet())
	return err == nil && bytes.HasPrefix(b, []byte(websites.NativeWAFSnippetMarker))
}

func syncNativeWafScript() (bool, error) {
	if err := os.MkdirAll(wafDir(), 0o755); err != nil {
		return false, err
	}
	body, err := assets.ReadFile("wafassets/waf.lua")
	if err != nil {
		return false, err
	}
	body = bytes.ReplaceAll(body, []byte(`"/usr/local/kunwaf/config.json"`), []byte(strconv.Quote(wafSettingsPath())))
	body = bytes.ReplaceAll(body, []byte(`"/usr/local/kunwaf/logs"`), []byte(strconv.Quote(nativeWafLogDir())))
	path := nativenginx.WAFScriptPath()
	if existing, err := os.ReadFile(path); err == nil && bytes.Equal(existing, body) {
		return false, nil
	}
	return true, util.AtomicWrite(path, body, 0o644)
}

func prepareNativeWafLogs() error {
	out, err := util.RunWithTimeout(30*time.Second, nativenginx.Binary(), "-p", nativenginx.Prefix()+"/", "-c", nativenginx.ConfigPath(), "-T")
	if err != nil {
		return fmt.Errorf("无法读取当前 Nginx 配置以确定 worker 用户: %w", err)
	}
	name := "nobody"
	if m := nativeWorker.FindStringSubmatch(out); len(m) == 2 {
		name = strings.Trim(m[1], `"'`)
	}
	worker, err := user.Lookup(name)
	if err != nil {
		return fmt.Errorf("无法读取 Nginx worker 用户 %s: %w", name, err)
	}
	uid, uidErr := strconv.Atoi(worker.Uid)
	gid, gidErr := strconv.Atoi(worker.Gid)
	if uidErr != nil || gidErr != nil || uid <= 0 || gid <= 0 {
		return fmt.Errorf("原生 WAF 需要以非 root 用户运行 Nginx worker")
	}
	dir := nativeWafLogDir()
	if err := os.Mkdir(dir, 0o750); err != nil && !os.IsExist(err) {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("原生 WAF 日志目录无效")
	}
	if err := os.Chown(dir, uid, gid); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o750); err != nil {
		return err
	}
	for _, name := range []string{"waf.log", "waf.log.old"} {
		file, err := openWafLog(filepath.Join(dir, name))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		err = file.Chown(uid, gid)
		file.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func installNativeWaf(w io.Writer) error {
	if w == nil {
		w = io.Discard
	}
	if nativenginx.Status() != "running" {
		return fmt.Errorf("请先启动原生 Nginx")
	}
	if _, err := syncNativeWafScript(); err != nil {
		return err
	}
	if err := prepareNativeWafLogs(); err != nil {
		return err
	}
	if _, err := os.Stat(wafSettingsPath()); os.IsNotExist(err) {
		if err := saveWafConfig(defaultWafConfig()); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	base, err := assets.ReadFile("wafassets/kunwaf.conf")
	if err != nil {
		return err
	}
	snippet := []byte(websites.NativeWAFSnippetMarker + nativenginx.WAFLuaPaths + string(base))
	if err := nativenginx.ConfigureWAF(true, w, func(main string) error {
		fmt.Fprintln(w, "为面板站点启用内置 WAF 并验证 Nginx 配置")
		return websites.ApplyNativeWAF(main, snippet)
	}); err != nil {
		return err
	}
	fmt.Fprintln(w, "原生 Nginx 的 WAF 已安装")
	return nil
}

func uninstallNativeWaf(w io.Writer) error {
	if !nativenginx.WAFConfigured() {
		return fmt.Errorf("原生 Nginx 未安装内置 WAF")
	}
	if err := nativenginx.ConfigureWAF(false, w, func(main string) error {
		return websites.ApplyNativeWAF(main, nil)
	}); err != nil {
		return err
	}
	if w != nil {
		fmt.Fprintln(w, "原生 Nginx 的 WAF 已卸载，保留规则和日志")
	}
	return nil
}
