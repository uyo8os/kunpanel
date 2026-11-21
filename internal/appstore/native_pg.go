package appstore

import (
	"fmt"
	"strconv"
	"time"

	"kunpanel/internal/nativepg"
)

const NativePGKey = "postgresql-native"

func IsPostgreSQLKey(key string) bool { return key == "postgresql" || key == NativePGKey }

func NativePGInstance() (Instance, bool) {
	for _, inst := range loadInstances() {
		if inst.Key == NativePGKey && inst.InstallMode == "native" {
			return inst, true
		}
	}
	return Instance{}, false
}

func RegisterNativePG(port int) error {
	installMu.Lock()
	defer installMu.Unlock()
	list := loadInstances()
	maxID := 0
	for _, inst := range list {
		if inst.Key == NativePGKey {
			return fmt.Errorf("原生 PostgreSQL 已登记安装")
		}
		if id, err := strconv.Atoi(inst.ID); err == nil && id > maxID {
			maxID = id
		}
	}
	list = append(list, Instance{
		ID: strconv.Itoa(maxID + 1), Key: NativePGKey, Name: "PostgreSQL", InstallMode: "native",
		Params: map[string]string{"Version": nativepg.Version(), "Port": strconv.Itoa(port)},
		Dir:    nativepg.Prefix(), CreatedAt: time.Now().Format("2006-01-02 15:04:05"),
	})
	return saveInstances(list)
}

func ForgetNativePG() error {
	installMu.Lock()
	defer installMu.Unlock()
	list := loadInstances()
	for i, inst := range list {
		if inst.Key == NativePGKey && inst.InstallMode == "native" {
			return saveInstances(append(list[:i], list[i+1:]...))
		}
	}
	return nil
}
