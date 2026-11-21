package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"kunpanel/internal/appstore"
	"kunpanel/internal/nativeredis"
	"kunpanel/internal/taskx"
)

const nativeRedisTask = "app-redis-native"

func (s *Server) handleNativeRedisVersions(w http.ResponseWriter, r *http.Request) {
	if _, installed := appstore.NativeRedisInstance(); !installed {
		if version := nativeredis.Version(); version != "" {
			port, err := nativeredis.Port()
			if err != nil {
				Fail(w, http.StatusConflict, err.Error())
				return
			}
			OK(w, map[string]any{"retained_version": version, "retained_port": port})
			return
		}
	}
	releases, err := nativeredis.Versions(r.Context())
	if err != nil {
		Fail(w, http.StatusBadGateway, "获取 Redis 官方版本失败: "+err.Error())
		return
	}
	OK(w, map[string]any{"releases": releases})
}

func (s *Server) installNativeRedis(w http.ResponseWriter, params map[string]string) {
	version, upgradeKey := strings.TrimSpace(params["Version"]), params["UpgradeKey"]
	port, _ := strconv.Atoi(params["Port"])
	password := params["Password"]
	_, installed := appstore.NativeRedisInstance()
	upgrading := upgradeKey == appstore.NativeRedisKey
	if (upgradeKey != "" && !upgrading) || installed != upgrading {
		Fail(w, http.StatusConflict, "Redis 安装状态已改变，请刷新列表")
		return
	}
	retained := !installed && nativeredis.Version() != ""
	if retained && version != nativeredis.Version() {
		Fail(w, http.StatusConflict, "Redis 有保留数据，请重新接管对应版本")
		return
	}
	if !retained && !nativeredis.SupportedVersion(version) {
		Fail(w, http.StatusBadRequest, "请选择 Redis 官方正式版")
		return
	}
	if !retained && !upgrading {
		if port < 1 || port > 65535 {
			Fail(w, http.StatusBadRequest, "请输入 1 到 65535 范围内的端口")
			return
		}
		if err := nativeredis.ValidatePassword(password); err != nil {
			Fail(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	t, err := taskx.M.Start(nativeRedisTask, func(log io.Writer) error {
		unlock := lockAppOp(appstore.NativeRedisKey)
		defer unlock()
		_, installed := appstore.NativeRedisInstance()
		current := nativeredis.Version()
		if installed != upgrading || (!upgrading && current != "" && current != version) {
			return fmt.Errorf("Redis 安装状态已改变，请刷新列表")
		}
		if upgrading && !nativeredis.CanUpgrade(current, version) {
			return fmt.Errorf("仅可升级到同一 Redis 大版本中较新的正式版")
		}
		var selected nativeredis.Release
		if upgrading || current == "" {
			releases, err := nativeredis.Versions(context.Background())
			if err != nil {
				return err
			}
			for _, release := range releases {
				if release.Version == version {
					selected = release
					break
				}
			}
			if selected.Version == "" {
				return fmt.Errorf("所选 Redis 版本已变化，请刷新版本列表")
			}
		}
		if upgrading {
			return nativeredis.Upgrade(selected, log)
		}
		if err := nativeredis.Install(selected, port, password, log); err != nil {
			return err
		}
		if err := appstore.RegisterNativeRedis(); err != nil {
			return errors.Join(fmt.Errorf("保存 Redis 安装记录失败: %w", err), nativeredis.Uninstall(log))
		}
		return nil
	})
	if err != nil {
		Fail(w, http.StatusConflict, err.Error())
		return
	}
	OK(w, t)
}

func (s *Server) nativeRedisAction(w http.ResponseWriter, action string) {
	run := func(log io.Writer) error {
		unlock := lockAppOp(appstore.NativeRedisKey)
		defer unlock()
		if _, ok := appstore.NativeRedisInstance(); !ok {
			return fmt.Errorf("原生 Redis 未安装")
		}
		fmt.Fprintln(log, "正在操作 Redis 服务")
		if action != "uninstall" {
			return nativeredis.Action(action)
		}
		if err := nativeredis.Uninstall(log); err != nil {
			return err
		}
		return appstore.ForgetNativeRedis()
	}
	switch action {
	case "start", "stop", "restart", "uninstall":
		s.startNativeRedisTask(w, run)
	default:
		Fail(w, http.StatusBadRequest, "不支持的 Redis 服务操作")
	}
}

func (s *Server) startNativeRedisTask(w http.ResponseWriter, run func(io.Writer) error) {
	t, err := taskx.M.Start(nativeRedisTask, run)
	if err != nil {
		Fail(w, http.StatusConflict, err.Error())
		return
	}
	OK(w, map[string]string{"task_id": t.ID})
}

func (s *Server) nativeRedisRuntime(w http.ResponseWriter, r *http.Request) {
	if _, ok := appstore.NativeRedisInstance(); !ok {
		Fail(w, http.StatusNotFound, "原生 Redis 未安装")
		return
	}
	port, _ := nativeredis.Port()
	status := nativeredis.Status()
	info := map[string]any{
		"key": appstore.NativeRedisKey, "install_mode": "native", "status": status, "version": nativeredis.Version(),
		"host": "127.0.0.1", "port": port, "socket": nativeredis.Socket(), "data_dir": nativeredis.DataDir(),
	}
	if status == "running" {
		if metrics, err := nativeredis.Metrics(); err != nil {
			info["metrics_error"] = err.Error()
		} else {
			info["metrics"] = metrics
		}
	}
	OK(w, info)
}

func (s *Server) nativeRedisRuntimeAction(w http.ResponseWriter, r *http.Request) {
	if _, ok := appstore.NativeRedisInstance(); !ok {
		Fail(w, http.StatusNotFound, "原生 Redis 未安装")
		return
	}
	switch r.PathValue("action") {
	case "logs":
		logs, err := nativeredis.Logs()
		if err != nil {
			Fail(w, http.StatusInternalServerError, "读取 Redis 日志失败: "+err.Error())
			return
		}
		OK(w, map[string]string{"logs": logs})
	case "snapshot":
		unlock := lockAppOp(appstore.NativeRedisKey)
		defer unlock()
		if nativeredis.Status() != "running" {
			Fail(w, http.StatusConflict, "请先启动 Redis")
			return
		}
		if err := nativeredis.Snapshot(); err != nil {
			Fail(w, http.StatusConflict, err.Error())
			return
		}
		OK(w, nil)
	case "password":
		var req struct {
			Password string `json:"password"`
		}
		if err := readJSON(r, &req); err != nil {
			Fail(w, http.StatusBadRequest, "请输入新的 Redis 密码")
			return
		}
		if err := nativeredis.ValidatePassword(req.Password); err != nil {
			Fail(w, http.StatusBadRequest, err.Error())
			return
		}
		s.startNativeRedisTask(w, func(log io.Writer) error {
			unlock := lockAppOp(appstore.NativeRedisKey)
			defer unlock()
			if _, ok := appstore.NativeRedisInstance(); !ok {
				return fmt.Errorf("原生 Redis 未安装")
			}
			return nativeredis.ChangePassword(req.Password, log)
		})
	default:
		Fail(w, http.StatusBadRequest, "不支持的 Redis 管理操作")
	}
}
