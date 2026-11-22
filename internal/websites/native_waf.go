package websites

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"kunpanel/internal/nativenginx"
	"kunpanel/internal/util"
)

const NativeWAFSnippetMarker = "# KunPanel native WAF HTTP\n"

// ApplyNativeWAF updates the managed main config, HTTP snippet, and sites under
// the website lock. A failed config test or reload restores their exact bytes.
// A nil snippet removes the managed HTTP configuration.
func ApplyNativeWAF(main string, snippet []byte) error {
	mu.Lock()
	defer mu.Unlock()
	load()
	mainPath := nativenginx.ConfigPath()
	snippetPath := filepath.Join(confDir(), "kunwaf.conf")
	paths := []string{mainPath, snippetPath}
	for _, site := range list {
		paths = append(paths, filepath.Join(confDir(), site.ID+".conf"))
	}
	type snapshot struct {
		path   string
		data   []byte
		mode   os.FileMode
		exists bool
	}
	snapshots := make([]snapshot, 0, len(paths))
	for _, path := range paths {
		st, err := os.Lstat(path)
		if os.IsNotExist(err) {
			snapshots = append(snapshots, snapshot{path: path})
			continue
		}
		if err != nil {
			return err
		}
		if !st.Mode().IsRegular() {
			return fmt.Errorf("不是普通配置文件: %s", path)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if path == snippetPath && !strings.HasPrefix(string(data), NativeWAFSnippetMarker) {
			return fmt.Errorf("发现其他 WAF 配置 %s，请先处理旧配置", path)
		}
		snapshots = append(snapshots, snapshot{path: path, data: data, mode: st.Mode().Perm(), exists: true})
	}
	if err := os.MkdirAll(confDir(), 0o755); err != nil {
		return err
	}
	remove := func(path string) error {
		if err := os.Remove(path); !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	restore := func(cause error) error {
		for _, old := range snapshots {
			if old.exists {
				cause = errors.Join(cause, util.AtomicWrite(old.path, old.data, old.mode))
			} else {
				cause = errors.Join(cause, remove(old.path))
			}
		}
		return cause
	}
	if err := util.AtomicWriteString(mainPath, main, snapshots[0].mode); err != nil {
		return err
	}
	var err error
	if snippet != nil {
		err = util.AtomicWrite(snippetPath, snippet, 0o644)
	} else {
		err = remove(snippetPath)
	}
	if err != nil {
		return restore(err)
	}
	InvalidateNginxCache()
	for _, site := range list {
		if err := writeConf(&site); err != nil {
			return restore(err)
		}
	}
	if snippet != nil {
		out, err := util.RunWithTimeout(30*time.Second, nativenginx.Binary(), "-p", nativenginx.Prefix()+"/", "-c", mainPath, "-T")
		if err != nil {
			return restore(fmt.Errorf("Nginx 配置校验失败: %s", strings.TrimSpace(out)))
		}
		if !strings.Contains(out, "# configuration file "+snippetPath+":") {
			return restore(fmt.Errorf("Nginx 的 http 配置未包含面板 WAF 片段 %s", snippetPath))
		}
	}
	if nativenginx.Status() != "running" {
		out, err := util.RunWithTimeout(30*time.Second, nativenginx.Binary(), "-p", nativenginx.Prefix()+"/", "-c", mainPath, "-t")
		if err != nil {
			return restore(fmt.Errorf("Nginx 配置校验失败: %s", strings.TrimSpace(out)))
		}
		return nil
	}
	if err := Reload(); err != nil {
		return restore(err)
	}
	return nil
}
