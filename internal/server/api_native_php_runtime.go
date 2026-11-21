package server

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"kunpanel/internal/appstore"
	"kunpanel/internal/nativephp"
	"kunpanel/internal/taskx"
	"kunpanel/internal/util"
)

func (s *Server) nativePHPInfo(w http.ResponseWriter, inst appstore.Instance) {
	minor := inst.Params["Minor"]
	extensions, err := nativePHPModules(minor)
	if err != nil {
		Fail(w, http.StatusInternalServerError, "读取 PHP 扩展失败: "+err.Error())
		return
	}
	OK(w, map[string]any{"key": inst.Key, "version": inst.Params["Version"], "status": nativephp.Status(minor), "install_mode": "native", "extensions": extensions})
}

func nativePHPModules(minor string) ([]string, error) {
	out, err := util.RunWithTimeout(30*time.Second, nativephp.Binary(minor), "-m")
	if err != nil {
		return nil, err
	}
	var extensions []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "[") {
			extensions = append(extensions, line)
		}
	}
	return extensions, nil
}

func nativePHPExtensionCatalog(w http.ResponseWriter, minor string) {
	list, err := nativephp.Extensions(minor)
	if err != nil {
		Fail(w, http.StatusBadRequest, err.Error())
		return
	}
	modules, err := nativePHPModules(minor)
	if err != nil {
		Fail(w, http.StatusInternalServerError, "读取 PHP 扩展失败: "+err.Error())
		return
	}
	type entry struct {
		nativephp.Extension
		Desc   string `json:"desc"`
		Loaded bool   `json:"loaded"`
	}
	descriptions := phpExtCatalog()
	entries := make([]entry, 0, len(list))
	for _, ext := range list {
		item := entry{Extension: ext, Desc: descriptions[ext.Name].Desc}
		for _, module := range modules {
			if strings.EqualFold(module, ext.Name) {
				item.Loaded = true
				break
			}
		}
		entries = append(entries, item)
	}
	OK(w, entries)
}

func startNativePHPExtensionTask(w http.ResponseWriter, r *http.Request, key string, remove bool) {
	var req struct {
		Ext string `json:"ext"`
	}
	if err := readJSON(r, &req); err != nil || !nativephp.SupportsExtension(req.Ext) {
		Fail(w, http.StatusBadRequest, "请选择支持的第三方 PHP 扩展")
		return
	}
	t, err := taskx.M.Start("app-native-"+key, func(w io.Writer) error {
		unlock := lockAppOp(key)
		defer unlock()
		inst, ok := appstore.NativePHPInstance(key)
		if !ok {
			return fmt.Errorf("原生 PHP 实例不存在")
		}
		return nativephp.ManageExtension(inst.Params["Minor"], req.Ext, remove, w)
	})
	if err != nil {
		Fail(w, http.StatusConflict, err.Error())
		return
	}
	OK(w, map[string]string{"task_id": t.ID})
}

func (s *Server) nativePHPRuntimeAction(w http.ResponseWriter, r *http.Request, inst appstore.Instance, action string) {
	switch action {
	case "php-ext-catalog":
		nativePHPExtensionCatalog(w, inst.Params["Minor"])
		return
	case "php-ext", "php-ext-remove":
		startNativePHPExtensionTask(w, r, inst.Key, action == "php-ext-remove")
		return
	}
	unlock := lockAppOp(inst.Key)
	defer unlock()
	inst, ok := appstore.NativePHPInstance(inst.Key)
	if !ok {
		Fail(w, http.StatusNotFound, "原生 PHP 实例不存在")
		return
	}
	minor := inst.Params["Minor"]
	switch action {
	case "php-disabled-functions-get":
		names, err := nativephp.DisabledFunctions(minor)
		if err != nil {
			Fail(w, http.StatusBadRequest, err.Error())
			return
		}
		OK(w, map[string]any{"functions": names})
	case "php-disabled-functions-save":
		var req struct {
			Content *string `json:"content"`
		}
		if err := readJSON(r, &req); err != nil || req.Content == nil || len(*req.Content) > 64*1024 {
			Fail(w, http.StatusBadRequest, "禁用函数列表格式错误或过长")
			return
		}
		names, restarted, err := nativephp.SetDisabledFunctions(minor, *req.Content)
		if err != nil {
			Fail(w, http.StatusBadRequest, err.Error())
			return
		}
		msg := "已保存禁用函数"
		if restarted {
			msg += "并重启 PHP-FPM"
		}
		OK(w, map[string]any{"functions": names, "message": msg})
	case "php-ini-get":
		b, err := os.ReadFile(nativephp.IniPath(minor))
		if err != nil {
			Fail(w, http.StatusInternalServerError, "读取 php.ini 失败: "+err.Error())
			return
		}
		OK(w, map[string]string{"content": string(b)})
	case "php-ini-file-save":
		var req struct {
			Content string `json:"content"`
		}
		if err := readJSON(r, &req); err != nil || strings.TrimSpace(req.Content) == "" || len(req.Content) > 512*1024 || strings.ContainsRune(req.Content, 0) {
			Fail(w, http.StatusBadRequest, "配置为空、过大或含非法字符")
			return
		}
		msg, err := saveNativePHPIni(minor, req.Content)
		if err != nil {
			Fail(w, http.StatusBadRequest, err.Error())
			return
		}
		OK(w, msg)
	case "restart":
		if err := nativephp.Action(minor, "restart"); err != nil {
			Fail(w, http.StatusBadRequest, err.Error())
			return
		}
		OK(w, "已重启")
	default:
		Fail(w, http.StatusBadRequest, "原生 PHP 暂不支持该操作")
	}
}

type nativePHPIniOps struct {
	test    func(string) (string, error)
	running func() bool
	restart func() error
}

func saveNativePHPIni(minor, content string) (string, error) {
	return saveNativePHPIniWithOps(nativephp.IniPath(minor), content, nativePHPIniOps{
		test: func(path string) (string, error) {
			return util.RunWithTimeout(30*time.Second, nativephp.Binary(minor), "-c", path, "-m")
		},
		running: func() bool { return nativephp.Status(minor) == "running" },
		restart: func() error { return nativephp.Action(minor, "restart") },
	})
}

func saveNativePHPIniWithOps(path, content string, ops nativePHPIniOps) (string, error) {
	fi, err := os.Lstat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return "", fmt.Errorf("无法读取原有 php.ini")
	}
	old, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".php-ini-test-*")
	if err != nil {
		return "", err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	out, err := ops.test(name)
	if err != nil || strings.Contains(out, "PHP Warning:") || strings.Contains(out, "PHP Fatal error:") || strings.Contains(out, "PHP:  syntax error") {
		return "", fmt.Errorf("配置检查失败: %s", strings.TrimSpace(out))
	}
	if err := util.AtomicWrite(path, []byte(content), fi.Mode().Perm()); err != nil {
		return "", err
	}
	if !ops.running() {
		return "php.ini 已保存；服务当前未运行", nil
	}
	if err := ops.restart(); err != nil {
		if e := util.AtomicWrite(path, old, fi.Mode().Perm()); e != nil {
			return "", fmt.Errorf("重启失败: %v；恢复配置失败: %v", err, e)
		}
		if e := ops.restart(); e != nil {
			return "", fmt.Errorf("重启失败: %v；原配置已恢复，但服务恢复失败: %v", err, e)
		}
		return "", fmt.Errorf("重启失败，已恢复原配置: %w", err)
	}
	return "php.ini 已保存并重启 PHP", nil
}
