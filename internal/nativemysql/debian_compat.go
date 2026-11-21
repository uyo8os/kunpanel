package nativemysql

import (
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"
)

// SHA-256 values from the signed Debian Bookworm package indexes for ncurses 6.4-4.
var debianNCurses5 = map[string]struct{ pkg, amd64, arm64 string }{
	"libtinfo.so.5": {
		"libtinfo5",
		"dd347f794e651039e7b4c391f86c674fed7f415b3dca6b0937beb0d470f09c1a",
		"81f30fd9b111ad70c237066580a312fdef9ebb7cb26162ba26b54d1edca766d1",
	},
	"libncurses.so.5": {
		"libncurses5",
		"02f4f7f52c4ce2fc4021793a931bfd85f7870554b8e4d56576d73a4ed0bdb390",
		"ae865f34559a578ad01d39ac96843c7ec6d4265804b7f2dfccea7d7233f813a6",
	},
	"libncursesw.so.5": {
		"libncursesw5",
		"631cbb8ed396736021e94b817a7041a763146d825f50e5dd856885bc7271ce39",
		"ba983b5eaf94883b288f8e1f0c151e341218c5c438a72b0c03df3bd03244a675",
	},
}

func isDebian() bool {
	data, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if value, ok := strings.CutPrefix(line, "ID="); ok {
			return strings.Trim(value, `"'`) == "debian"
		}
	}
	return false
}

func installDebianNCurses5(prefix string, names []string, w io.Writer) error {
	triplet := map[string]string{"amd64": "x86_64-linux-gnu", "arm64": "aarch64-linux-gnu"}[runtime.GOARCH]
	if triplet == "" {
		return fmt.Errorf("当前 CPU 架构没有 MySQL 所需的 Debian ncurses 5 兼容包")
	}
	compat := filepath.Join(prefix, "compat")
	if err := os.MkdirAll(compat, 0o755); err != nil {
		return err
	}
	work, err := os.MkdirTemp(filepath.Dir(prefix), ".debian-ncurses-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	// ldd cannot report libtinfo when the ncurses library that needs it is missing.
	if !slices.Contains(names, "libtinfo.so.5") {
		names = append(names, "libtinfo.so.5")
	}
	for _, name := range names {
		pkg := debianNCurses5[name]
		digest := pkg.amd64
		if runtime.GOARCH == "arm64" {
			digest = pkg.arm64
		}
		file := pkg.pkg + "_6.4-4_" + runtime.GOARCH + ".deb"
		fmt.Fprintln(w, "提取 MySQL 私有兼容库", name, "（Debian 官方包）")
		data, err := downloadDebianNCurses(file, digest)
		if err != nil {
			return err
		}
		archive := filepath.Join(work, file)
		if err := os.WriteFile(archive, data, 0o600); err != nil {
			return err
		}
		unpacked := filepath.Join(work, pkg.pkg)
		if err := run(w, 30*time.Second, "dpkg-deb", "--extract", archive, unpacked); err != nil {
			return err
		}
		library, err := os.ReadFile(filepath.Join(unpacked, "lib", triplet, name))
		if err != nil {
			return fmt.Errorf("读取 Debian 兼容库 %s 失败: %w", name, err)
		}
		if err := os.WriteFile(filepath.Join(compat, name), library, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func downloadDebianNCurses(file, digest string) ([]byte, error) {
	const pool = "https://deb.debian.org/debian/pool/main/n/ncurses/"
	client := &http.Client{Timeout: 45 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) < 5 && req.URL.Scheme == "https" && req.URL.Host == "deb.debian.org" {
			return nil
		}
		return fmt.Errorf("拒绝非 Debian 官方镜像地址的重定向")
	}}
	resp, err := client.Get(pool + file)
	if err != nil {
		return nil, fmt.Errorf("下载 Debian 兼容包 %s 失败: %w", file, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("下载 Debian 兼容包 %s 失败: HTTP %d", file, resp.StatusCode)
	}
	const limit = 1 << 20
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if len(data) > limit || fmt.Sprintf("%x", sha256.Sum256(data)) != digest {
		return nil, fmt.Errorf("Debian 兼容包 %s 的 SHA-256 校验失败", file)
	}
	return data, nil
}
