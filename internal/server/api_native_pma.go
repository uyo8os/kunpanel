package server

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"kunpanel/internal/appstore"
	"kunpanel/internal/nativephp"
	"kunpanel/internal/nativepma"
	"kunpanel/internal/taskx"
)

const nativePMATask = "app-native-phpmyadmin"

func (s *Server) handleNativePMAVersion(w http.ResponseWriter, r *http.Request) {
	version, err := nativepma.Latest()
	if err != nil {
		Fail(w, http.StatusBadGateway, "获取 phpMyAdmin 官方版本失败: "+err.Error())
		return
	}
	OK(w, map[string]string{"version": version})
}

func managedPMADatabases() []nativepma.Database {
	list, _ := appstore.List()
	var dbs []nativepma.Database
	for _, instance := range list {
		if !appstore.IsMySQLKey(instance.Key) && instance.Key != "mariadb" {
			continue
		}
		port, _ := strconv.Atoi(instance.Params["Port"])
		name := "MySQL（容器）"
		if instance.InstallMode == "native" {
			name = "MySQL（原生）"
		}
		if instance.Key == "mariadb" {
			name = "MariaDB"
		}
		if port > 0 && port <= 65535 {
			dbs = append(dbs, nativepma.Database{Key: instance.Key, Name: name, Port: port})
		}
	}
	slices.SortFunc(dbs, func(a, b nativepma.Database) int { return a.ServerID() - b.ServerID() })
	return dbs
}

func (s *Server) installNativePMA(w http.ResponseWriter, params map[string]string) {
	version, password := strings.TrimSpace(params["Version"]), params["AccessPassword"]
	_, exists := appstore.NativePMAInstance()
	upgrading := params["UpgradeKey"] == appstore.NativePMAKey
	if version == "" || exists != upgrading {
		Fail(w, http.StatusBadRequest, "请选择 phpMyAdmin 官方版本及正确的安装操作")
		return
	}
	var opt nativepma.Settings
	if upgrading {
		var err error
		opt, err = nativepma.ReadSettings()
		if err != nil {
			Fail(w, http.StatusBadRequest, "无法读取原生 phpMyAdmin 配置")
			return
		}
	} else {
		opt.PHP, opt.Host = params["PHP"], strings.Trim(strings.TrimSpace(params["Host"]), "[]")
		opt.Port, _ = strconv.Atoi(params["Port"])
		if err := opt.Validate(); err != nil {
			Fail(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := nativepma.ValidateAccessPassword(password); err != nil {
			Fail(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	minor, err := nativephp.Minor(opt.PHP)
	if err != nil || minor != opt.PHP {
		Fail(w, http.StatusBadRequest, "请选择已安装的原生 PHP 分支")
		return
	}
	phpKey := appstore.NativePHPKey(minor)
	t, err := taskx.M.Start(nativePMATask, func(log io.Writer) error {
		unlockNG := lockAppOp("nginx")
		defer unlockNG()
		unlockPHP := lockAppOp(phpKey)
		defer unlockPHP()
		unlock := lockAppOp(appstore.NativePMAKey)
		defer unlock()
		if _, ok := appstore.NativePHPInstance(phpKey); !ok {
			return fmt.Errorf("所选原生 PHP 未安装")
		}
		if _, ok := appstore.NativeNginxInstance(); !ok {
			return fmt.Errorf("请先安装原生 Nginx")
		}
		if _, exists := appstore.NativePMAInstance(); exists != upgrading {
			return fmt.Errorf("原生 phpMyAdmin 安装状态已变化，请刷新应用列表")
		}
		latest, err := nativepma.Latest()
		if err != nil {
			return err
		}
		if latest != version {
			return fmt.Errorf("所选 phpMyAdmin 版本已变化，请刷新版本")
		}
		if upgrading {
			return nativepma.Upgrade(version, log)
		}
		if err := nativepma.Install(opt, version, password, managedPMADatabases(), log); err != nil {
			return err
		}
		if err := appstore.RegisterNativePMA(opt); err != nil {
			return errors.Join(err, nativepma.Uninstall())
		}
		return nil
	})
	if err != nil {
		Fail(w, http.StatusConflict, err.Error())
		return
	}
	OK(w, t)
}

func (s *Server) nativePMAAction(w http.ResponseWriter, action string) {
	unlock := lockAppOp(appstore.NativePMAKey)
	defer unlock()
	if _, ok := appstore.NativePMAInstance(); !ok {
		Fail(w, http.StatusNotFound, "原生 phpMyAdmin 未安装")
		return
	}
	if action == "uninstall" {
		if err := nativepma.Uninstall(); err != nil {
			Fail(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := appstore.ForgetNativePMA(); err != nil {
			Fail(w, http.StatusInternalServerError, err.Error())
			return
		}
	} else if err := nativepma.Action(action); err != nil {
		Fail(w, http.StatusBadRequest, err.Error())
		return
	}
	OK(w, nil)
}

func (s *Server) nativePMAInfo(w http.ResponseWriter) {
	opt, err := nativepma.ReadSettings()
	if err != nil {
		Fail(w, http.StatusBadRequest, "无法读取原生 phpMyAdmin 配置")
		return
	}
	OK(w, map[string]any{"version": nativepma.Version(), "status": nativepma.Status(), "settings": opt, "url": opt.URL(), "username": nativepma.AccessUser})
}

func (s *Server) nativePMAAccess(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Database string `json:"database"`
		Name     string `json:"name"`
	}
	if err := readJSON(r, &req); err != nil {
		Fail(w, http.StatusBadRequest, "请求格式有误")
		return
	}
	unlock := lockAppOp(appstore.NativePMAKey)
	defer unlock()
	opt, err := nativepma.ReadSettings()
	if err != nil || nativepma.Status() != "running" {
		Fail(w, http.StatusBadRequest, "原生 phpMyAdmin 未运行")
		return
	}
	dbs := managedPMADatabases()
	if err := nativepma.ConfigureDatabases(dbs); err != nil {
		Fail(w, http.StatusBadRequest, "同步数据库实例失败: "+err.Error())
		return
	}
	address := opt.URL()
	for _, db := range dbs {
		if db.Key == req.Database {
			q := url.Values{"server": {strconv.Itoa(db.ServerID())}}
			if req.Name != "" {
				q.Set("db", req.Name)
			}
			address += "?" + q.Encode()
			break
		}
	}
	OK(w, map[string]string{"url": address})
}

func (s *Server) nativePMAAccessPassword(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Password string `json:"password"`
	}
	if err := readJSON(r, &req); err != nil {
		Fail(w, http.StatusBadRequest, "请求格式有误")
		return
	}
	if err := nativepma.ChangeAccessPassword(req.Password); err != nil {
		Fail(w, http.StatusBadRequest, err.Error())
		return
	}
	OK(w, nil)
}
