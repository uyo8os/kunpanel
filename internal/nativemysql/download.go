package nativemysql

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/shirou/gopsutil/v3/disk"
)

type archiveCandidate struct{ url, filename string }

var glibcVersion = regexp.MustCompile(`[0-9]+\.[0-9]+`)

func archivesFor(release Release, arch, glibc string) []archiveCandidate {
	var out []archiveCandidate
	add := func(prefix, file string) {
		out = append(out, archiveCandidate{"https://cdn.mysql.com/" + prefix + "/" + file, file})
	}
	if release.Branch == "5.7" {
		if arch == "x86_64" && CompareVersions(glibc, "2.12") >= 0 {
			add("archives/mysql-5.7", "mysql-"+release.Version+"-linux-glibc2.12-x86_64.tar.gz")
		}
		return out
	}
	versions := []string{"2.28"}
	if release.Branch == "8.0" && arch == "x86_64" {
		versions = append(versions, "2.17", "2.12")
	}
	for _, suffix := range []string{"-minimal.tar.xz", ".tar.xz"} {
		for _, minimum := range versions {
			if CompareVersions(glibc, minimum) < 0 {
				continue
			}
			file := "mysql-" + release.Version + "-linux-glibc" + minimum + "-" + arch + suffix
			add("Downloads/MySQL-"+release.Branch, file)
			add("archives/mysql-"+release.Branch, file)
		}
	}
	return out
}

func downloadArchive(release Release, work string, w io.Writer) (string, error) {
	arch := map[string]string{"amd64": "x86_64", "arm64": "aarch64"}[runtime.GOARCH]
	if arch == "" {
		return "", fmt.Errorf("当前 CPU 架构没有支持的 MySQL 官方包")
	}
	out, err := output(5*time.Second, "ldd", "--version")
	line, _, _ := strings.Cut(out, "\n")
	versions := glibcVersion.FindAllString(line, -1)
	isGlibc := strings.Contains(strings.ToLower(line), "glibc") || strings.Contains(line, "GNU libc") || strings.Contains(line, "GNU C Library")
	if err != nil || !isGlibc || len(versions) == 0 {
		return "", fmt.Errorf("无法确认 Linux glibc 版本")
	}
	candidates := archivesFor(release, arch, versions[len(versions)-1])
	for _, entry := range candidates {
		sig, err := fetch(entry.url+".asc", 12*time.Second, 64<<10)
		var status *downloadStatus
		if errors.As(err, &status) && status.code == 404 {
			continue
		}
		if err != nil {
			return "", err
		}
		if !strings.HasPrefix(string(sig), "-----BEGIN PGP SIGNATURE-----") {
			return "", fmt.Errorf("官方安装包签名格式错误")
		}
		fmt.Fprintln(w, "下载官方安装包", entry.filename)
		filename := filepath.Join(work, entry.filename)
		if err := downloadToDisk(entry.url, filename); err != nil {
			return "", err
		}
		if err := os.WriteFile(filename+".asc", sig, 0o600); err != nil {
			return "", err
		}
		if err := verifyArchive(work, filename, release); err != nil {
			return "", err
		}
		fmt.Fprintln(w, "官方 MySQL 签名校验通过")
		return filename, nil
	}
	return "", fmt.Errorf("没有找到兼容当前架构与 glibc 的 MySQL %s 官方安装包", release.Version)
}

func downloadToDisk(url, filename string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()
	res, err := httpGet(ctx, url)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if usage, err := disk.Usage(filepath.Dir(filename)); err == nil && res.ContentLength > 0 && uint64(res.ContentLength) > usage.Free {
		return fmt.Errorf("面板数据盘空间不足，无法保存 MySQL 安装包")
	}
	const maxArchive = 4 << 30
	out, err := os.OpenFile(filename, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	written, copyErr := io.Copy(out, io.LimitReader(res.Body, maxArchive+1))
	closeErr := out.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		return err
	}
	if written > maxArchive || res.ContentLength >= 0 && written != res.ContentLength {
		return fmt.Errorf("MySQL 官方安装包长度异常")
	}
	return nil
}

const releaseKey = "BCA43417C3B485DD128EC6D4B7B3B788A8D3785C"
const archivedKey = "859BE8D7C586F538430B19C2467B942D3A79BD29"

func verifyArchive(work, filename string, release Release) error {
	keyName, fingerprint := "RPM-GPG-KEY-mysql-2025", releaseKey
	if release.Branch == "5.7" {
		keyName, fingerprint = "RPM-GPG-KEY-mysql-2022", archivedKey
	}
	key, err := fetch("https://repo.mysql.com/"+keyName, 15*time.Second, 64<<10)
	if err != nil {
		return err
	}
	keyPath := filepath.Join(work, "mysql-release-key.asc")
	if err := os.WriteFile(keyPath, key, 0o600); err != nil {
		return err
	}
	keyring := filepath.Join(work, "mysql-release-key.gpg")
	if out, err := output(20*time.Second, "gpg", "--no-options", "--batch", "--no-autostart", "--homedir", work, "--dearmor", "--output", keyring, keyPath); err != nil {
		return fmt.Errorf("读取 MySQL 官方签名公钥失败: %s (%w)", strings.TrimSpace(out), err)
	}
	out, err := output(3*time.Minute, "gpgv", "--homedir", work, "--keyring", keyring, "--status-fd", "1", filename+".asc", filename)
	if !signatureOK(out, fingerprint, release.Legacy, err) {
		return fmt.Errorf("MySQL 安装包签名校验失败: %s", strings.TrimSpace(out))
	}
	return nil
}

func signatureOK(output, fingerprint string, archived bool, verifyErr error) bool {
	valid, expired := false, false
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "[GNUPG:]" {
			continue
		}
		switch fields[1] {
		case "BADSIG", "ERRSIG", "EXPSIG", "REVKEYSIG", "KEYREVOKED", "NO_PUBKEY":
			return false
		case "EXPKEYSIG":
			expired = true
		case "VALIDSIG":
			valid = len(fields) >= 3 && (fields[2] == fingerprint || fields[len(fields)-1] == fingerprint)
		}
	}
	// Final releases remain verifiable after their historical signing keys expire.
	return valid && (!expired && verifyErr == nil || archived && expired)
}
