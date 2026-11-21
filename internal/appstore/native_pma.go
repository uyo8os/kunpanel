package appstore

import (
	"fmt"
	"strconv"
	"time"

	"kunpanel/internal/nativepma"
)

const NativePMAKey = "phpmyadmin-native"

func NativePMAInstance() (Instance, bool) {
	for _, instance := range loadInstances() {
		if instance.Key == NativePMAKey && instance.InstallMode == "native" {
			return instance, true
		}
	}
	return Instance{}, false
}

func RegisterNativePMA(s nativepma.Settings) error {
	installMu.Lock()
	defer installMu.Unlock()
	list := loadInstances()
	maxID := 0
	for _, instance := range list {
		if instance.Key == NativePMAKey {
			return fmt.Errorf("原生 phpMyAdmin 已登记安装")
		}
		if id, err := strconv.Atoi(instance.ID); err == nil && id > maxID {
			maxID = id
		}
	}
	list = append(list, Instance{
		ID: strconv.Itoa(maxID + 1), Key: NativePMAKey, Name: "phpMyAdmin", InstallMode: "native",
		Params:    map[string]string{"Version": nativepma.Version(), "PHP": s.PHP, "Port": strconv.Itoa(s.Port)},
		CreatedAt: time.Now().Format("2006-01-02 15:04:05"),
	})
	return saveInstances(list)
}

func ForgetNativePMA() error {
	installMu.Lock()
	defer installMu.Unlock()
	list := loadInstances()
	for i, instance := range list {
		if instance.Key == NativePMAKey && instance.InstallMode == "native" {
			return saveInstances(append(list[:i], list[i+1:]...))
		}
	}
	return nil
}
