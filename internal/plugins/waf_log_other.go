//go:build !linux

package plugins

import "os"

func openWafLog(path string) (*os.File, error) { return os.Open(path) }
