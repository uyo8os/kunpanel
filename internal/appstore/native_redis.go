package appstore

import (
	"fmt"
	"strconv"
	"time"

	"kunpanel/internal/nativeredis"
)

const NativeRedisKey = "redis-native"

func NativeRedisInstance() (Instance, bool) {
	for _, inst := range loadInstances() {
		if inst.Key == NativeRedisKey && inst.InstallMode == "native" {
			return inst, true
		}
	}
	return Instance{}, false
}

func RegisterNativeRedis() error {
	port, err := nativeredis.Port()
	if err != nil {
		return err
	}
	installMu.Lock()
	defer installMu.Unlock()
	list := loadInstances()
	maxID := 0
	for _, inst := range list {
		if inst.Key == NativeRedisKey {
			return fmt.Errorf("原生 Redis 已登记安装")
		}
		if id, err := strconv.Atoi(inst.ID); err == nil && id > maxID {
			maxID = id
		}
	}
	return saveInstances(append(list, Instance{
		ID: strconv.Itoa(maxID + 1), Key: NativeRedisKey, Name: "Redis", InstallMode: "native",
		Params: map[string]string{"Version": nativeredis.Version(), "Port": strconv.Itoa(port)},
		Dir:    nativeredis.Prefix(), CreatedAt: time.Now().Format("2006-01-02 15:04:05"),
	}))
}

func ForgetNativeRedis() error {
	installMu.Lock()
	defer installMu.Unlock()
	list := loadInstances()
	for i, inst := range list {
		if inst.Key == NativeRedisKey && inst.InstallMode == "native" {
			return saveInstances(append(list[:i], list[i+1:]...))
		}
	}
	return nil
}
