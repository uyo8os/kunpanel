package server

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"kunpanel/internal/appstore"
	"kunpanel/internal/nativemysql"
	"kunpanel/internal/taskx"
)

const nativeMySQLTask = "app-mysql-native"

func (s *Server) handleNativeMySQLVersions(w http.ResponseWriter, r *http.Request) {
	OK(w, nativemysql.Versions())
}

func (s *Server) installNativeMySQL(w http.ResponseWriter, params map[string]string) {
	version := strings.TrimSpace(params["Version"])
	branch := nativemysql.Branch(version)
	if branch == "" {
		Fail(w, http.StatusBadRequest, "请选择 MySQL 官方版本")
		return
	}
	upgradeKey := params["UpgradeKey"]
	password := params["RootPassword"]
	port, _ := strconv.Atoi(params["Port"])
	if upgradeKey != "" {
		if inst, ok := appstore.NativeMySQLInstance(upgradeKey); !ok || nativemysql.Branch(inst.Params["Version"]) != branch {
			Fail(w, http.StatusBadRequest, "原生 MySQL 仅支持同一分支更新")
			return
		}
	} else {
		if _, err := appstore.NativeMySQLInstallKey(); err != nil {
			Fail(w, http.StatusConflict, err.Error())
			return
		}
		if err := nativemysql.ValidatePassword(password); err != nil {
			Fail(w, http.StatusBadRequest, err.Error())
			return
		}
		if port < 1 || port > 65535 {
			Fail(w, http.StatusBadRequest, "请输入 1 到 65535 范围内的 MySQL 端口")
			return
		}
	}
	t, err := taskx.M.Start(nativeMySQLTask, func(log io.Writer) error {
		unlock := lockAppOp(appstore.ParallelMySQLKey)
		defer unlock()
		release, err := nativemysql.Latest(branch)
		if err != nil {
			return fmt.Errorf("查询 MySQL 官方版本失败: %w", err)
		}
		if release.Version != version {
			return fmt.Errorf("MySQL 官方版本已更新，请刷新版本列表")
		}
		if upgradeKey != "" {
			instance, ok := appstore.NativeMySQLInstance(upgradeKey)
			if !ok {
				return fmt.Errorf("找不到原生 MySQL 安装记录")
			}
			installedPort, err := strconv.Atoi(instance.Params["Port"])
			if err != nil {
				return fmt.Errorf("原生 MySQL 端口记录无效: %w", err)
			}
			return nativemysql.Upgrade(release, installedPort, log)
		}
		if _, err := appstore.NativeMySQLInstallKey(); err != nil {
			return err
		}
		if err := nativemysql.Install(release, port, password, log); err != nil {
			return err
		}
		if err := appstore.RegisterNativeMySQL(port); err != nil {
			return errors.Join(fmt.Errorf("保存安装记录失败，安装的数据库文件已保留: %w", err), nativemysql.Uninstall(log))
		}
		return nil
	})
	if err != nil {
		Fail(w, http.StatusConflict, err.Error())
		return
	}
	OK(w, t)
}

func (s *Server) nativeMySQLAction(w http.ResponseWriter, key, action string) {
	if action == "uninstall" {
		t, err := taskx.M.Start(nativeMySQLTask, func(log io.Writer) error {
			unlock := lockAppOp(appstore.ParallelMySQLKey)
			defer unlock()
			if _, ok := appstore.NativeMySQLInstance(key); !ok {
				return fmt.Errorf("找不到原生 MySQL 安装记录")
			}
			if err := nativemysql.Uninstall(log); err != nil {
				return err
			}
			return appstore.ForgetNativeMySQL(key)
		})
		if err != nil {
			Fail(w, http.StatusConflict, err.Error())
			return
		}
		OK(w, map[string]string{"task_id": t.ID})
		return
	}
	unlock := lockAppOp(appstore.ParallelMySQLKey)
	defer unlock()
	if _, ok := appstore.NativeMySQLInstance(key); !ok {
		Fail(w, http.StatusNotFound, "找不到原生 MySQL 安装记录")
		return
	}
	if err := nativemysql.Action(action); err != nil {
		Fail(w, http.StatusBadRequest, err.Error())
		return
	}
	OK(w, nil)
}
