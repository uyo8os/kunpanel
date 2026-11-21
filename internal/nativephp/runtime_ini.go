package nativephp

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"kunpanel/internal/util"
)

func readRuntimeFile(filename string) ([]byte, error) {
	st, err := os.Lstat(filename)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("PHP 文件不是普通文件: %s", filename)
	}
	return os.ReadFile(filename)
}

func writeRuntimeFile(filename string, data []byte) error {
	if data == nil {
		if err := os.Remove(filename); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
		return err
	}
	return util.AtomicWrite(filename, data, 0o644)
}

func checkFPMConfig(minor string) error {
	out, err := commandOutput(30*time.Second, FpmBinary(minor), "-t", "-y", ConfigPath(minor))
	if err != nil {
		return fmt.Errorf("PHP-FPM 配置检查失败: %s (%w)", strings.TrimSpace(out), err)
	}
	return nil
}

// activateRuntimeINI switches a branch's configuration atomically and restores
// it if validation or restarting an active service fails.
func activateRuntimeINI(minor, ini string, oldIni, newIni []byte, verify func() error) (restarted bool, err error) {
	if err := writeRuntimeFile(ini, newIni); err != nil {
		return false, err
	}
	restartAttempted := false
	defer func() {
		if err == nil {
			return
		}
		if restoreErr := writeRuntimeFile(ini, oldIni); restoreErr != nil {
			err = fmt.Errorf("%v；恢复原 PHP 配置失败: %w", err, restoreErr)
			return
		}
		if restartAttempted {
			if restartErr := Action(minor, "restart"); restartErr != nil {
				err = fmt.Errorf("%v；原配置已恢复，但 PHP-FPM 恢复失败: %w", err, restartErr)
				return
			}
		}
		err = fmt.Errorf("已恢复原 PHP 配置: %w", err)
	}()
	if err := verify(); err != nil {
		return false, err
	}
	if Status(minor) == "running" {
		restartAttempted = true
		if err := Action(minor, "restart"); err != nil {
			return false, err
		}
		return true, nil
	}
	return false, nil
}
