package server

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"kunpanel/internal/config"
	"kunpanel/internal/nativenginx"
	"kunpanel/internal/util"
)

type nativeNginxConfOps struct {
	test    func(string) (string, error)
	running func() bool
	reload  func() error
}

// saveNativeNginxConfig validates an isolated candidate before replacing the
// active file. A failed reload restores the original file and attempts to reload it.
func saveNativeNginxConfig(path, content string, ops nativeNginxConfOps) (string, error) {
	if strings.TrimSpace(content) == "" || len(content) > 512*1024 || strings.ContainsRune(content, 0) {
		return "", fmt.Errorf("配置内容为空、过大或含非法字符")
	}
	fi, err := os.Lstat(path)
	if err != nil {
		return "", fmt.Errorf("读取现有配置失败: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("主配置不是普通文件")
	}
	old, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("读取现有配置失败: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".nginx-conf-test-*")
	if err != nil {
		return "", fmt.Errorf("创建临时配置失败: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	output, err := ops.test(tmpName)
	if err != nil {
		return "", fmt.Errorf("配置校验失败: %s (%v)", strings.TrimSpace(output), err)
	}
	running := ops.running()
	if err := util.AtomicWrite(path, []byte(content), fi.Mode().Perm()); err != nil {
		return "", fmt.Errorf("保存配置失败: %w", err)
	}
	if !running {
		return "nginx.conf 已保存；服务当前未运行", nil
	}
	if err := ops.reload(); err != nil {
		if restoreErr := util.AtomicWrite(path, old, fi.Mode().Perm()); restoreErr != nil {
			return "", fmt.Errorf("重载失败: %v；恢复原配置失败: %v", err, restoreErr)
		}
		if restoreErr := ops.reload(); restoreErr != nil {
			return "", fmt.Errorf("重载失败: %v；原配置已恢复，但再次重载失败: %v", err, restoreErr)
		}
		return "", fmt.Errorf("重载失败，已恢复原配置: %w", err)
	}
	return "nginx.conf 已保存并平滑重载", nil
}

func nativeNginxTest(configPath string) (string, error) {
	return util.RunWithTimeout(30*time.Second, nativenginx.Binary(), "-p", nativenginx.Prefix()+"/", "-c", configPath, "-t")
}

func nativeNginxErrorLog() string {
	return filepath.Join(config.DataDir(), "websites", "logs", "error.log")
}

func nativeNginxTail(path string, n int) (string, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer f.Close()
	// Limit I/O and memory even for very large logs.
	st, err := f.Stat()
	if err != nil {
		return "", err
	}
	const limit int64 = 2 << 20
	if st.Size() > limit {
		if _, err := f.Seek(-limit, io.SeekEnd); err != nil {
			return "", err
		}
	}
	b, err := io.ReadAll(io.LimitReader(f, limit))
	if err != nil {
		return "", err
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n"), nil
}

func (s *Server) nativeNginxRuntimeAction(w http.ResponseWriter, r *http.Request, action string) {
	unlock := lockAppOp("nginx")
	defer unlock()
	if !nativenginx.Installed() {
		Fail(w, http.StatusBadRequest, "未安装原生 Nginx")
		return
	}
	switch action {
	case "modules":
		out, err := util.RunWithTimeout(30*time.Second, nativenginx.Binary(), "-V")
		if err != nil {
			Fail(w, http.StatusInternalServerError, "读取模块信息失败: "+err.Error())
			return
		}
		OK(w, map[string]string{"raw": out})
	case "test":
		out, err := nativeNginxTest(nativenginx.ConfigPath())
		if err != nil {
			Fail(w, http.StatusBadRequest, "配置校验失败: "+strings.TrimSpace(out)+" "+err.Error())
			return
		}
		OK(w, strings.TrimSpace(out))
	case "reload":
		if nativenginx.Status() != "running" {
			Fail(w, http.StatusBadRequest, "Nginx 未运行，无法重载")
			return
		}
		if err := nativenginx.Action("reload"); err != nil {
			Fail(w, http.StatusBadRequest, "配置检查或重载失败: "+err.Error())
			return
		}
		OK(w, "已重载")
	case "conf-get":
		b, err := os.ReadFile(nativenginx.ConfigPath())
		if err != nil {
			Fail(w, http.StatusInternalServerError, "读取 nginx.conf 失败: "+err.Error())
			return
		}
		OK(w, map[string]string{"content": string(b)})
	case "conf-save":
		var req struct {
			Content string `json:"content"`
		}
		if err := readJSON(r, &req); err != nil {
			Fail(w, http.StatusBadRequest, "参数错误")
			return
		}
		msg, err := saveNativeNginxConfig(nativenginx.ConfigPath(), req.Content, nativeNginxConfOps{
			test:    nativeNginxTest,
			running: func() bool { return nativenginx.Status() == "running" },
			reload:  func() error { return nativenginx.Action("reload") },
		})
		if err != nil {
			Fail(w, http.StatusBadRequest, err.Error())
			return
		}
		OK(w, msg)
	case "errorlog":
		content, err := nativeNginxTail(nativeNginxErrorLog(), 120)
		if err != nil {
			Fail(w, http.StatusInternalServerError, "读取错误日志失败: "+err.Error())
			return
		}
		OK(w, map[string]string{"content": content})
	case "errorlog-clear":
		path := nativeNginxErrorLog()
		fi, err := os.Lstat(path)
		if os.IsNotExist(err) {
			OK(w, "已清空")
			return
		}
		if err != nil || !fi.Mode().IsRegular() {
			Fail(w, http.StatusBadRequest, "无法安全访问错误日志")
			return
		}
		if err := os.Truncate(path, 0); err != nil {
			Fail(w, http.StatusInternalServerError, "清空错误日志失败: "+err.Error())
			return
		}
		OK(w, "已清空")
	default:
		Fail(w, http.StatusBadRequest, "原生 Nginx 不支持该运行时操作")
	}
}
