package appstore

import (
	"fmt"
	"strconv"
	"time"

	"kunpanel/internal/nativemysql"
)

const ParallelMySQLKey = "mysql-native"

func IsMySQLKey(key string) bool { return key == "mysql" || key == ParallelMySQLKey }

func NativeMySQLInstance(key string) (Instance, bool) {
	if IsMySQLKey(key) {
		for _, inst := range loadInstances() {
			if inst.Key == key && inst.InstallMode == "native" {
				refreshNativeMySQL(&inst)
				return inst, true
			}
		}
	}
	return Instance{}, false
}

func refreshNativeMySQL(inst *Instance) {
	if version := nativemysql.Version(); version != "" {
		if inst.Params == nil {
			inst.Params = make(map[string]string)
		}
		inst.Params["Version"] = version
	}
}

func NativeMySQLInstallKey() (string, error) {
	return nativeMySQLInstallKey(loadInstances())
}

func nativeMySQLInstallKey(list []Instance) (string, error) {
	key := "mysql"
	for _, inst := range list {
		if !IsMySQLKey(inst.Key) {
			continue
		}
		if inst.InstallMode == "native" || inst.Key == ParallelMySQLKey {
			return "", fmt.Errorf("原生 MySQL 已登记安装，请在已安装列表管理")
		}
		key = ParallelMySQLKey
	}
	return key, nil
}

func RegisterNativeMySQL(port int) error {
	installMu.Lock()
	defer installMu.Unlock()
	list := loadInstances()
	key, err := nativeMySQLInstallKey(list)
	if err != nil {
		return err
	}
	maxID := 0
	for _, inst := range list {
		if id, err := strconv.Atoi(inst.ID); err == nil && id > maxID {
			maxID = id
		}
	}
	list = append(list, Instance{
		ID: strconv.Itoa(maxID + 1), Key: key, Name: "MySQL", InstallMode: "native",
		Params: map[string]string{"Version": nativemysql.Version(), "Port": strconv.Itoa(port)},
		Dir:    nativemysql.Prefix(), CreatedAt: time.Now().Format("2006-01-02 15:04:05"),
	})
	return saveInstances(list)
}

func ForgetNativeMySQL(key string) error {
	installMu.Lock()
	defer installMu.Unlock()
	list := loadInstances()
	for i, inst := range list {
		if IsMySQLKey(key) && inst.Key == key && inst.InstallMode == "native" {
			return saveInstances(append(list[:i], list[i+1:]...))
		}
	}
	return nil
}
