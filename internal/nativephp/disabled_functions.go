package nativephp

import (
	"bytes"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const disabledFunctionsINI = "99-kunpanel-disable-functions.ini"
const disabledFunctionsHeader = "; Managed by KunPanel\ndisable_functions = "

var phpFunctionName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(\\[A-Za-z_][A-Za-z0-9_]*)*$`)

func normalizeDisabledFunctions(value string) ([]string, error) {
	names := make([]string, 0)
	seen := make(map[string]bool)
	for _, name := range strings.FieldsFunc(value, func(r rune) bool {
		return r == ',' || r == '，' || unicode.IsSpace(r)
	}) {
		name = strings.TrimPrefix(name, `\`)
		if !phpFunctionName.MatchString(name) {
			return nil, fmt.Errorf("无效的 PHP 函数名: %q", name)
		}
		name = strings.ToLower(name)
		if !seen[name] {
			names = append(names, name)
			seen[name] = true
		}
	}
	return names, nil
}

func functionsFromFPMInfo(output string) ([]string, error) {
	for _, line := range strings.Split(output, "\n") {
		row, ok := strings.CutPrefix(line, "disable_functions => ")
		if !ok {
			continue
		}
		value, _, ok := strings.Cut(row, " => ")
		if !ok {
			continue
		}
		if strings.TrimSpace(value) == "no value" {
			value = ""
		}
		return normalizeDisabledFunctions(value)
	}
	return nil, fmt.Errorf("PHP-FPM 未返回禁用函数配置")
}

func DisabledFunctions(minor string) ([]string, error) {
	if m, err := Minor(minor); err != nil || m != minor || !Installed(minor) {
		return nil, fmt.Errorf("原生 PHP-FPM 未安装")
	}
	out, err := commandOutput(30*time.Second, FpmBinary(minor), "-i")
	if err != nil {
		// phpinfo output can include application environment variables.
		return nil, fmt.Errorf("读取 PHP-FPM 禁用函数配置失败: %w", err)
	}
	return functionsFromFPMInfo(out)
}

// SetDisabledFunctions replaces the branch-wide list. A blank list is written
// explicitly so lower-priority configuration does not restore old restrictions.
func SetDisabledFunctions(minor, value string) ([]string, bool, error) {
	if m, err := Minor(minor); err != nil || m != minor || !Installed(minor) {
		return nil, false, fmt.Errorf("原生 PHP-FPM 未安装")
	}
	names, err := normalizeDisabledFunctions(value)
	if err != nil {
		return nil, false, err
	}
	ini := filepath.Join(ScanDir(minor), disabledFunctionsINI)
	old, err := readRuntimeFile(ini)
	if err != nil {
		return nil, false, err
	}
	if old != nil {
		setting, managed := strings.CutPrefix(string(old), disabledFunctionsHeader)
		if _, err := strconv.Unquote(strings.TrimSuffix(setting, "\n")); !managed || err != nil {
			return nil, false, fmt.Errorf("禁用函数配置已被手动修改: %s", ini)
		}
	}
	newIni := []byte(disabledFunctionsHeader + strconv.Quote(strings.Join(names, ",")) + "\n")
	verify := func() error {
		actual, err := DisabledFunctions(minor)
		if err != nil {
			return err
		}
		if !slices.Equal(actual, names) {
			return fmt.Errorf("PHP-FPM 读取的禁用函数列表与保存值不同，请检查是否有其他配置覆盖")
		}
		return checkFPMConfig(minor)
	}
	if bytes.Equal(old, newIni) {
		return names, false, verify()
	}
	restarted, err := activateRuntimeINI(minor, ini, old, newIni, verify)
	return names, restarted, err
}
