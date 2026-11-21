package server

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"kunpanel/internal/appstore"
	"kunpanel/internal/nativepg"
	"kunpanel/internal/taskx"
)

const nativePGTask = "app-postgresql-native"

func retainedPGVersion(major string) string {
	version := nativepg.Version()
	if major != "" && nativepg.Major(version) == major {
		return version
	}
	return ""
}

func (s *Server) handleNativePGVersions(w http.ResponseWriter, r *http.Request) {
	retained, err := nativepg.RetainedMajor()
	if err != nil {
		Fail(w, http.StatusConflict, err.Error())
		return
	}
	version := retainedPGVersion(retained)
	_, installed := appstore.NativePGInstance()
	releases, err := nativepg.Versions()
	if err != nil && (installed || version == "") {
		Fail(w, http.StatusBadGateway, "获取 PostgreSQL 官方版本失败: "+err.Error())
		return
	}
	if !installed && version != "" && !slices.ContainsFunc(releases, func(r nativepg.Release) bool { return r.Major == retained }) {
		releases = append(releases, nativepg.Release{Major: retained, Version: version})
	}
	OK(w, map[string]any{"releases": releases, "retained_major": retained, "retained_version": version})
}

func (s *Server) installNativePG(w http.ResponseWriter, params map[string]string) {
	version, upgradeKey := strings.TrimSpace(params["Version"]), params["UpgradeKey"]
	port, _ := strconv.Atoi(params["Port"])
	password := params["Password"]
	_, installed := appstore.NativePGInstance()
	upgrading := upgradeKey == appstore.NativePGKey
	if nativepg.Major(version) == "" || (upgradeKey != "" && !upgrading) || installed != upgrading {
		Fail(w, http.StatusBadRequest, "请选择 PostgreSQL 官方版本及正确的安装操作")
		return
	}
	if !upgrading && (port < 1 || port > 65535) {
		Fail(w, http.StatusBadRequest, "请输入 1 到 65535 范围内的端口")
		return
	}
	t, err := taskx.M.Start(nativePGTask, func(log io.Writer) error {
		unlock := lockAppOp(appstore.NativePGKey)
		defer unlock()
		instance, installed := appstore.NativePGInstance()
		if installed != upgrading {
			return fmt.Errorf("PostgreSQL 安装状态已改变，请刷新列表")
		}
		retained, err := nativepg.RetainedMajor()
		if err != nil {
			return err
		}
		if upgrading && retained != nativepg.Major(version) {
			return fmt.Errorf("只可在同一 PostgreSQL 大版本内升级")
		}
		if upgrading || version != retainedPGVersion(retained) {
			releases, err := nativepg.Versions()
			if err != nil {
				return err
			}
			if !slices.ContainsFunc(releases, func(r nativepg.Release) bool { return r.Version == version }) {
				return fmt.Errorf("所选 PostgreSQL 版本已变化，请刷新版本列表")
			}
		}
		if upgrading {
			port, _ := strconv.Atoi(instance.Params["Port"])
			return nativepg.Upgrade(version, port, log)
		}
		if err := nativepg.Install(version, port, password, log); err != nil {
			return err
		}
		if err := appstore.RegisterNativePG(port); err != nil {
			return errors.Join(fmt.Errorf("保存 PostgreSQL 安装记录失败: %w", err), nativepg.Uninstall(log))
		}
		return nil
	})
	if err != nil {
		Fail(w, http.StatusConflict, err.Error())
		return
	}
	OK(w, t)
}

func (s *Server) nativePGAction(w http.ResponseWriter, action string) {
	if action == "uninstall" {
		t, err := taskx.M.Start(nativePGTask, func(log io.Writer) error {
			unlock := lockAppOp(appstore.NativePGKey)
			defer unlock()
			if _, ok := appstore.NativePGInstance(); !ok {
				return fmt.Errorf("原生 PostgreSQL 未安装")
			}
			if err := nativepg.Uninstall(log); err != nil {
				return err
			}
			return appstore.ForgetNativePG()
		})
		if err != nil {
			Fail(w, http.StatusConflict, err.Error())
			return
		}
		OK(w, map[string]string{"task_id": t.ID})
		return
	}
	unlock := lockAppOp(appstore.NativePGKey)
	defer unlock()
	instance, ok := appstore.NativePGInstance()
	if !ok {
		Fail(w, http.StatusNotFound, "原生 PostgreSQL 未安装")
		return
	}
	port, _ := strconv.Atoi(instance.Params["Port"])
	if err := nativepg.Action(action, port); err != nil {
		Fail(w, http.StatusBadRequest, err.Error())
		return
	}
	OK(w, nil)
}
