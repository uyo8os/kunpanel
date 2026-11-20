//go:build !linux

package websites

import "fmt"

func checkExistingNativePHPSiteDirectory(path string) error {
	return fmt.Errorf("原生 PHP-FPM 网站目前仅支持 Linux")
}

func checkNativeNginxSocketAccess(socket string) error {
	return fmt.Errorf("原生 PHP-FPM 网站目前仅支持 Linux")
}

func prepareNativePHPSiteDirectory(path string) error {
	return fmt.Errorf("原生 PHP-FPM 网站目前仅支持 Linux")
}
