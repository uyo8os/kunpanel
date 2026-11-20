package server

import (
	"fmt"
	"io"
	"net/http"
	"strings"

	"kunpanel/internal/appstore"
	"kunpanel/internal/nativenginx"
	"kunpanel/internal/taskx"
	"kunpanel/internal/websites"
)

func (s *Server) handleNativeNginxVersions(w http.ResponseWriter, r *http.Request) {
	versions, err := nativenginx.Versions()
	if err != nil {
		Fail(w, http.StatusBadGateway, "获取 Nginx 官方版本失败: "+err.Error())
		return
	}
	grouped := map[string][]string{"stable": {}, "mainline": {}}
	for _, v := range versions {
		if _, ok := grouped[v.Channel]; ok {
			grouped[v.Channel] = append(grouped[v.Channel], v.Version)
		}
	}
	OK(w, grouped)
}

func (s *Server) installNativeNginx(w http.ResponseWriter, params map[string]string) {
	version, channel := strings.TrimSpace(params["Version"]), strings.ToLower(strings.TrimSpace(params["Channel"]))
	if version == "" || (channel != "stable" && channel != "mainline") {
		Fail(w, http.StatusBadRequest, "请选择官方 Nginx 版本及 Stable/Mainline 频道")
		return
	}
	_, nativeInstalled := appstore.NativeNginxInstance()
	if !nativeInstalled && (appstore.HasNginxInstance() || nativenginx.Installed()) {
		Fail(w, http.StatusConflict, "Nginx 已安装或已有安装记录，请先处理现有实例")
		return
	}
	t, err := taskx.M.Start("app-nginx-native", func(w io.Writer) error {
		unlock := lockAppOp("nginx")
		defer unlock()
		_, upgrading := appstore.NativeNginxInstance()
		if !upgrading && (appstore.HasNginxInstance() || nativenginx.Installed()) {
			return fmt.Errorf("Nginx 已安装或已有安装记录")
		}
		versions, err := nativenginx.Versions()
		if err != nil {
			return fmt.Errorf("获取官方版本失败: %w", err)
		}
		found := false
		for _, v := range versions {
			if v.Version == version && v.Channel == channel {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("所选版本不再是官方 %s 频道的当前版本，请刷新版本列表", channel)
		}
		if err := nativenginx.Install(version, w); err != nil {
			return err
		}
		if upgrading {
			err = appstore.UpdateNativeNginxVersion(version, channel)
		} else {
			err = appstore.RegisterNativeNginx(version, channel)
		}
		if err != nil {
			return fmt.Errorf("Nginx 已安装，但保存应用记录失败: %w", err)
		}
		websites.InvalidateNginxCache()
		return nil
	})
	if err != nil {
		Fail(w, http.StatusConflict, err.Error())
		return
	}
	OK(w, t)
}

func (s *Server) nativeNginxAction(w http.ResponseWriter, action string) {
	if action == "uninstall" {
		t, err := taskx.M.Start("app-nginx-native", func(w io.Writer) error {
			unlock := lockAppOp("nginx")
			defer unlock()
			if _, ok := appstore.NativeNginxInstance(); !ok {
				return fmt.Errorf("原生 Nginx 安装记录不存在")
			}
			if err := nativenginx.Uninstall(w); err != nil {
				return err
			}
			if err := appstore.ForgetNativeNginx(); err != nil {
				return err
			}
			websites.InvalidateNginxCache()
			return nil
		})
		if err != nil {
			Fail(w, http.StatusConflict, err.Error())
			return
		}
		OK(w, map[string]string{"task_id": t.ID})
		return
	}
	if action != "start" && action != "stop" && action != "restart" {
		Fail(w, http.StatusBadRequest, "不支持的原生 Nginx 操作")
		return
	}
	unlock := lockAppOp("nginx")
	defer unlock()
	if _, ok := appstore.NativeNginxInstance(); !ok {
		Fail(w, http.StatusNotFound, "原生 Nginx 安装记录不存在")
		return
	}
	if err := nativenginx.Action(action); err != nil {
		Fail(w, http.StatusBadRequest, err.Error())
		return
	}
	websites.InvalidateNginxCache()
	OK(w, nil)
}
