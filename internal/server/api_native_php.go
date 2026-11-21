package server

import (
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"

	"kunpanel/internal/appstore"
	"kunpanel/internal/nativephp"
	"kunpanel/internal/nativepma"
	"kunpanel/internal/taskx"
	"kunpanel/internal/websites"
)

var nativePHPVersion = regexp.MustCompile(`^(8\.[3456])\.([0-9]+)$`)

func (s *Server) handleNativePHPVersions(w http.ResponseWriter, r *http.Request) {
	releases, err := nativephp.Versions()
	if err != nil {
		Fail(w, http.StatusBadGateway, "获取 PHP 官方版本失败: "+err.Error())
		return
	}
	versions := make(map[string]string)
	for _, release := range releases {
		versions[release.Minor] = release.Version
	}
	OK(w, versions)
}

func (s *Server) installNativePHP(w http.ResponseWriter, params map[string]string) {
	version := strings.TrimSpace(params["Version"])
	m := nativePHPVersion.FindStringSubmatch(version)
	if len(m) != 3 {
		Fail(w, http.StatusBadRequest, "请选择官方 PHP 8.3、8.4、8.5 或 8.6 的完整版本")
		return
	}
	minor, key := m[1], appstore.NativePHPKey(m[1])
	upgradeKey := strings.TrimSpace(params["UpgradeKey"])
	if upgradeKey != "" && upgradeKey != key {
		Fail(w, http.StatusBadRequest, "只能更新同一 PHP 分支")
		return
	}
	_, isNative := appstore.NativePHPInstance(key)
	if upgradeKey != "" && !isNative {
		Fail(w, http.StatusNotFound, "原生 PHP 实例不存在")
		return
	}
	if upgradeKey == "" && (appstore.PHPKeyOccupied(key) || nativephp.Installed(minor)) {
		Fail(w, http.StatusConflict, "该 PHP 版本已有安装记录或占用，请先处理现有实例")
		return
	}
	t, err := taskx.M.Start("app-native-"+key, func(w io.Writer) error {
		unlock := lockAppOp(key)
		defer unlock()
		_, exists := appstore.NativePHPInstance(key)
		if upgradeKey != "" && !exists {
			return fmt.Errorf("原生 PHP 实例不存在")
		}
		if upgradeKey == "" && (appstore.PHPKeyOccupied(key) || nativephp.Installed(minor)) {
			return fmt.Errorf("该 PHP 版本已有安装记录或占用")
		}
		if err := nativephp.Install(minor, version, w); err != nil {
			return err
		}
		if exists {
			if err := appstore.UpdateNativePHPVersion(key, version); err != nil {
				return fmt.Errorf("PHP 已升级，但更新应用记录失败: %w", err)
			}
			if pma, ok := appstore.NativePMAInstance(); ok && pma.Params["PHP"] == minor {
				if err := nativepma.RestartPHP(minor); err != nil {
					return fmt.Errorf("PHP 已升级，phpMyAdmin 的 PHP-FPM 重启失败: %w", err)
				}
			}
			return nil
		}
		if err := appstore.RegisterNativePHP(minor, version); err != nil {
			if cleanupErr := nativephp.Uninstall(minor, w); cleanupErr != nil {
				return fmt.Errorf("保存 PHP 应用记录失败: %v；清理新安装失败: %w", err, cleanupErr)
			}
			return fmt.Errorf("保存 PHP 应用记录失败，已清理新安装: %w", err)
		}
		return nil
	})
	if err != nil {
		Fail(w, http.StatusConflict, err.Error())
		return
	}
	OK(w, t)
}

func (s *Server) nativePHPAction(w http.ResponseWriter, inst appstore.Instance, action string) {
	key, minor := inst.Key, inst.Params["Minor"]
	if action == "uninstall" {
		if pma, ok := appstore.NativePMAInstance(); ok && pma.Params["PHP"] == minor {
			Fail(w, http.StatusConflict, "原生 phpMyAdmin 正在使用该 PHP 版本")
			return
		}
		if using := websites.PHPSitesUsing(key); len(using) > 0 {
			Fail(w, http.StatusConflict, fmt.Sprintf("有 %d 个站点正在使用该 PHP 版本，请先切换版本", len(using)))
			return
		}
		t, err := taskx.M.Start("app-native-"+key, func(w io.Writer) error {
			unlock := lockAppOp(key)
			defer unlock()
			if _, ok := appstore.NativePHPInstance(key); !ok {
				return fmt.Errorf("原生 PHP 实例不存在")
			}
			if pma, ok := appstore.NativePMAInstance(); ok && pma.Params["PHP"] == minor {
				return fmt.Errorf("原生 phpMyAdmin 正在使用该 PHP 版本")
			}
			if using := websites.PHPSitesUsing(key); len(using) > 0 {
				return fmt.Errorf("该 PHP 版本正在被站点使用")
			}
			if err := nativephp.Uninstall(minor, w); err != nil {
				return err
			}
			return appstore.ForgetNativePHP(key)
		})
		if err != nil {
			Fail(w, http.StatusConflict, err.Error())
			return
		}
		OK(w, map[string]string{"task_id": t.ID})
		return
	}
	if action != "start" && action != "stop" && action != "restart" {
		Fail(w, http.StatusBadRequest, "不支持的原生 PHP 操作")
		return
	}
	unlock := lockAppOp(key)
	defer unlock()
	if _, ok := appstore.NativePHPInstance(key); !ok {
		Fail(w, http.StatusNotFound, "原生 PHP 实例不存在")
		return
	}
	if err := nativephp.Action(minor, action); err != nil {
		Fail(w, http.StatusBadRequest, err.Error())
		return
	}
	OK(w, nil)
}
