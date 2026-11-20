package appstore

import (
	"fmt"
	"strconv"
	"time"

	"kunpanel/internal/nativenginx"
)

// NativeNginxInstance only recognises records explicitly created by the native installer.
// Existing records without an install mode remain Docker instances.
func NativeNginxInstance() (Instance, bool) {
	for _, inst := range loadInstances() {
		if inst.Key == "nginx" && inst.InstallMode == "native" {
			return inst, true
		}
	}
	return Instance{}, false
}

// HasNginxInstance prevents the native installer from replacing a Docker installation.
func HasNginxInstance() bool {
	for _, inst := range loadInstances() {
		if inst.Key == "nginx" {
			return true
		}
	}
	return false
}

// RegisterNativeNginx is called only after installation has succeeded.
func RegisterNativeNginx(version, channel string) error {
	installMu.Lock()
	defer installMu.Unlock()
	list := loadInstances()
	maxID := 0
	for _, inst := range list {
		if inst.Key == "nginx" {
			return fmt.Errorf("Nginx 已登记安装，请先处理现有实例")
		}
		if id, err := strconv.Atoi(inst.ID); err == nil && id > maxID {
			maxID = id
		}
	}
	list = append(list, Instance{
		ID: strconv.Itoa(maxID + 1), Key: "nginx", Name: "Nginx",
		Params: map[string]string{"Version": version, "Channel": channel},
		Dir:    nativenginx.Prefix(), InstallMode: "native",
		CreatedAt: time.Now().Format("2006-01-02 15:04:05"),
	})
	return saveInstances(list)
}

// UpdateNativeNginxVersion records a successful upgrade without changing its identity.
func UpdateNativeNginxVersion(version, channel string) error {
	installMu.Lock()
	defer installMu.Unlock()
	list := loadInstances()
	for i := range list {
		if list[i].Key == "nginx" && list[i].InstallMode == "native" {
			if list[i].Params == nil {
				list[i].Params = map[string]string{}
			}
			list[i].Params["Version"] = version
			list[i].Params["Channel"] = channel
			return saveInstances(list)
		}
	}
	return fmt.Errorf("原生 Nginx 安装记录不存在")
}

// ForgetNativeNginx removes only the native record after the service is removed.
func ForgetNativeNginx() error {
	installMu.Lock()
	defer installMu.Unlock()
	list := loadInstances()
	kept := make([]Instance, 0, len(list))
	for _, inst := range list {
		if inst.Key == "nginx" && inst.InstallMode == "native" {
			continue
		}
		kept = append(kept, inst)
	}
	return saveInstances(kept)
}
