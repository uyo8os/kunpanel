package nativepma

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"golang.org/x/crypto/openpgp"
	"golang.org/x/mod/semver"
)

const versionURL = "https://www.phpmyadmin.net/home_page/version.json"
const filesURL = "https://files.phpmyadmin.net/"

var versionPattern = regexp.MustCompile(`^[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,4}$`)

var releaseHTTP = &http.Client{Timeout: 2 * time.Minute, CheckRedirect: func(req *http.Request, via []*http.Request) error {
	if len(via) < 5 && req.URL.Scheme == "https" && req.URL.Port() == "" {
		switch req.URL.Hostname() {
		case "phpmyadmin.net", "www.phpmyadmin.net", "files.phpmyadmin.net":
			return nil
		}
	}
	return fmt.Errorf("拒绝非 phpMyAdmin 官方地址的重定向")
}}

func fetch(url string, limit int64) ([]byte, error) {
	resp, err := releaseHTTP.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s 返回 HTTP %d", url, resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("phpMyAdmin 官方响应超过大小限制")
	}
	return data, err
}

func Latest() (string, error) {
	b, err := fetch(versionURL, 64<<10)
	if err != nil {
		return "", err
	}
	var release struct{ Version string }
	if err := json.Unmarshal(b, &release); err != nil || !validVersion(release.Version) {
		return "", fmt.Errorf("无法识别 phpMyAdmin 官方正式版本")
	}
	return release.Version, nil
}

func validVersion(version string) bool {
	return versionPattern.MatchString(version) && semver.IsValid("v"+version)
}

func newerVersion(next, current string) bool {
	return validVersion(next) && semver.Compare("v"+next, "v"+current) > 0
}

// Fingerprints published in the phpMyAdmin release verification instructions.
func verifySignature(archive, signature, keyring []byte) error {
	keys, err := openpgp.ReadKeyRing(bytes.NewReader(keyring))
	if bytes.HasPrefix(keyring, []byte("-----BEGIN PGP PUBLIC KEY BLOCK-----")) {
		keys, err = openpgp.ReadArmoredKeyRing(bytes.NewReader(keyring))
	}
	if err != nil {
		return fmt.Errorf("读取 phpMyAdmin 发布公钥失败: %w", err)
	}
	signer, err := openpgp.CheckArmoredDetachedSignature(keys, bytes.NewReader(archive), bytes.NewReader(signature))
	if err != nil {
		return fmt.Errorf("phpMyAdmin 官方签名校验失败: %w", err)
	}
	switch strings.ToUpper(hex.EncodeToString(signer.PrimaryKey.Fingerprint[:])) {
	case "3D06A59ECE730EB71B511C17CE752F178259BD92", "63CB1DF1EF12CF2AC0EE5A329C27B31342B7511D":
		return nil
	}
	return fmt.Errorf("phpMyAdmin 官方包的签名人不在受信列表中")
}

func download(version, work string, log io.Writer) (string, error) {
	if !validVersion(version) {
		return "", fmt.Errorf("无效的 phpMyAdmin 版本")
	}
	dir := "phpMyAdmin-" + version + "-all-languages"
	url := filesURL + "phpMyAdmin/" + version + "/" + dir + ".tar.gz"
	fmt.Fprintln(log, "下载 phpMyAdmin 官方发行包", version)
	archive, err := fetch(url, 48<<20)
	if err != nil {
		return "", err
	}
	signature, err := fetch(url+".asc", 64<<10)
	if err != nil {
		return "", err
	}
	keys, err := fetch(filesURL+"phpmyadmin.keyring", 1<<20)
	if err != nil {
		return "", err
	}
	if err := verifySignature(archive, signature, keys); err != nil {
		return "", err
	}
	fmt.Fprintln(log, "phpMyAdmin 官方签名校验通过")
	file := filepath.Join(work, "release.tar.gz")
	if err := os.WriteFile(file, archive, 0o600); err != nil {
		return "", err
	}
	if err := run(time.Minute, "tar", "-xzf", file, "-C", work, "--no-same-owner", "--no-same-permissions"); err != nil {
		return "", err
	}
	return filepath.Join(work, dir), nil
}
