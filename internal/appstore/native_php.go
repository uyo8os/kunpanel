package appstore

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"kunpanel/internal/nativephp"
)

// NativePHPKey uses a stable key per minor release, independent of patch version.
func NativePHPKey(minor string) string { return "php" + strings.ReplaceAll(minor, ".", "") }

func NativePHPInstance(key string) (Instance, bool) {
	for _, inst := range loadInstances() {
		if inst.Key == key && IsPHPRuntimeKey(key) && inst.InstallMode == "native" {
			refreshNativePHPVersion(&inst)
			return inst, true
		}
	}
	return Instance{}, false
}

func refreshNativePHPVersion(inst *Instance) {
	if version := nativephp.CurrentVersion(inst.Params["Minor"]); version != "" {
		inst.Params["Version"] = version
	}
}

func PHPKeyOccupied(key string) bool {
	for _, inst := range loadInstances() {
		if inst.Key == key {
			return true
		}
	}
	return false
}

func RegisterNativePHP(minor, version string) error {
	installMu.Lock()
	defer installMu.Unlock()
	key := NativePHPKey(minor)
	list := loadInstances()
	maxID := 0
	for _, inst := range list {
		if inst.Key == key {
			return fmt.Errorf("%s 已有安装记录", key)
		}
		if id, err := strconv.Atoi(inst.ID); err == nil && id > maxID {
			maxID = id
		}
	}
	list = append(list, Instance{ID: strconv.Itoa(maxID + 1), Key: key, Name: "PHP " + minor,
		Params: map[string]string{"Version": version, "Minor": minor}, Dir: nativephp.Prefix(minor),
		InstallMode: "native", CreatedAt: time.Now().Format("2006-01-02 15:04:05")})
	return saveInstances(list)
}

func UpdateNativePHPVersion(key, version string) error {
	installMu.Lock()
	defer installMu.Unlock()
	list := loadInstances()
	for i := range list {
		if list[i].Key == key && list[i].InstallMode == "native" && IsPHPRuntimeKey(key) {
			if list[i].Params == nil {
				list[i].Params = map[string]string{}
			}
			list[i].Params["Version"] = version
			return saveInstances(list)
		}
	}
	return fmt.Errorf("原生 PHP 安装记录不存在")
}

func ForgetNativePHP(key string) error {
	installMu.Lock()
	defer installMu.Unlock()
	list := loadInstances()
	kept := make([]Instance, 0, len(list))
	for _, inst := range list {
		if inst.Key == key && inst.InstallMode == "native" && IsPHPRuntimeKey(key) {
			continue
		}
		kept = append(kept, inst)
	}
	return saveInstances(kept)
}
