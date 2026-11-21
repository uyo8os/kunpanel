package nativepg

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

const catalogURL = "https://www.postgresql.org/versions.json"
const sourceURL = "https://ftp.postgresql.org/pub/source/v"

type Release struct {
	Major   string `json:"major"`
	Version string `json:"version"`
}

var versionPattern = regexp.MustCompile(`^[1-9][0-9]{1,2}\.(0|[1-9][0-9]{0,3})$`)

func Major(version string) string {
	if !versionPattern.MatchString(version) {
		return ""
	}
	major, _, _ := strings.Cut(version, ".")
	return major
}

func minor(version string) int {
	_, patch, _ := strings.Cut(version, ".")
	n, _ := strconv.Atoi(patch)
	return n
}

var releaseClient = &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error {
	if len(via) < 5 && req.URL.Scheme == "https" && req.URL.Port() == "" {
		switch req.URL.Hostname() {
		case "www.postgresql.org", "postgresql.org", "ftp.postgresql.org":
			return nil
		}
	}
	return fmt.Errorf("拒绝非 PostgreSQL 官方地址的重定向")
}}

func get(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := releaseClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("%s 返回 HTTP %d", url, resp.StatusCode)
	}
	return resp, nil
}

func fetch(url string, limit int64) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	resp, err := get(ctx, url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("PostgreSQL 官方响应超过大小限制")
	}
	return data, err
}

func Versions() ([]Release, error) {
	b, err := fetch(catalogURL, 128<<10)
	if err != nil {
		return nil, err
	}
	return parseVersions(b, time.Now())
}

func parseVersions(b []byte, now time.Time) ([]Release, error) {
	var catalog []struct {
		Major       string `json:"major"`
		LatestMinor string `json:"latestMinor"`
		ReleaseDate string `json:"relDate"`
		Supported   bool   `json:"supported"`
	}
	if err := json.Unmarshal(b, &catalog); err != nil {
		return nil, fmt.Errorf("无法解析 PostgreSQL 官方版本清单: %w", err)
	}
	var releases []Release
	for _, entry := range catalog {
		version := entry.Major + "." + entry.LatestMinor
		date, err := time.Parse(time.DateOnly, entry.ReleaseDate)
		if entry.Supported && err == nil && !date.After(now) && Major(version) == entry.Major && entry.Major != "" {
			releases = append(releases, Release{Major: entry.Major, Version: version})
		}
	}
	if len(releases) == 0 {
		return nil, fmt.Errorf("官方清单中没有可用的 PostgreSQL 正式版")
	}
	slices.SortFunc(releases, func(a, b Release) int {
		left, _ := strconv.Atoi(a.Major)
		right, _ := strconv.Atoi(b.Major)
		return right - left
	})
	return releases, nil
}

func download(version, work string, log io.Writer) (string, error) {
	if Major(version) == "" {
		return "", fmt.Errorf("无效的 PostgreSQL 版本")
	}
	name := "postgresql-" + version + ".tar.gz"
	url := sourceURL + version + "/" + name
	checksum, err := fetch(url+".sha256", 4096)
	if err != nil {
		return "", err
	}
	parts := strings.Fields(string(checksum))
	if len(parts) != 2 || strings.TrimPrefix(parts[1], "*") != name {
		return "", fmt.Errorf("PostgreSQL 官方校验文件与源码包不匹配")
	}
	if digest, err := hex.DecodeString(parts[0]); err != nil || len(digest) != sha256.Size {
		return "", fmt.Errorf("PostgreSQL 官方 SHA-256 格式无效")
	}
	fmt.Fprintln(log, "下载 PostgreSQL 官方源码", version)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	resp, err := get(ctx, url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	const maxSource = 160 << 20
	file := filepath.Join(work, name)
	out, err := os.OpenFile(file, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, hash), io.LimitReader(resp.Body, maxSource+1))
	if err := errors.Join(err, out.Close()); err != nil {
		return "", err
	}
	if n > maxSource || hex.EncodeToString(hash.Sum(nil)) != strings.ToLower(parts[0]) {
		return "", fmt.Errorf("PostgreSQL 源码 SHA-256 校验失败")
	}
	fmt.Fprintln(log, "PostgreSQL 官方 SHA-256 校验通过")
	return file, nil
}
