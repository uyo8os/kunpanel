package nativeredis

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"golang.org/x/mod/semver"
)

const catalogURL = "https://raw.githubusercontent.com/redis/redis-hashes/master/README"
const sourceURL = "https://download.redis.io/releases/"

type Release struct {
	Version string `json:"version"`
	Branch  string `json:"branch"`
	digest  string
}

var stableVersion = regexp.MustCompile(`^[1-9][0-9]*\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

func SupportedVersion(v string) bool {
	return stableVersion.MatchString(v) && semver.Compare("v"+v, "v7.2.0") >= 0
}

func CanUpgrade(current, target string) bool {
	return SupportedVersion(current) && SupportedVersion(target) &&
		semver.Major("v"+current) == semver.Major("v"+target) &&
		semver.Compare("v"+current, "v"+target) < 0
}

func archiveName(v string) string { return "redis-" + v + ".tar.gz" }

func parseCatalog(data string) ([]Release, error) {
	latest := make(map[string]Release)
	for _, line := range strings.Split(data, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 5 || fields[0] != "hash" || fields[2] != "sha256" {
			continue
		}
		name := fields[1]
		v := strings.TrimSuffix(strings.TrimPrefix(name, "redis-"), ".tar.gz")
		expectedURL := sourceURL + name
		if !SupportedVersion(v) || name != archiveName(v) ||
			(fields[4] != expectedURL && fields[4] != "http"+strings.TrimPrefix(expectedURL, "https")) {
			continue
		}
		digest, err := hex.DecodeString(fields[3])
		if err != nil || len(digest) != sha256.Size {
			continue
		}
		branch := strings.TrimPrefix(semver.MajorMinor("v"+v), "v")
		if semver.Compare("v"+v, "v"+latest[branch].Version) > 0 {
			latest[branch] = Release{Version: v, Branch: branch, digest: strings.ToLower(fields[3])}
		}
	}
	releases := make([]Release, 0, len(latest))
	for _, release := range latest {
		releases = append(releases, release)
	}
	if len(releases) == 0 {
		return nil, fmt.Errorf("官方清单中没有可校验的 Redis 正式版")
	}
	slices.SortFunc(releases, func(a, b Release) int {
		return semver.Compare("v"+b.Version, "v"+a.Version)
	})
	return releases, nil
}

var releaseClient = &http.Client{CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
	return fmt.Errorf("Redis 官方下载地址返回了重定向")
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
		resp.Body.Close()
		return nil, fmt.Errorf("Redis 官方地址返回 HTTP %d", resp.StatusCode)
	}
	return resp, nil
}

func Versions(ctx context.Context) ([]Release, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	resp, err := get(ctx, catalogURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	const limit = 1 << 20
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if len(b) > limit {
		return nil, fmt.Errorf("Redis 官方清单超过大小限制")
	}
	return parseCatalog(string(b))
}

func download(release Release, work string, log io.Writer) (string, error) {
	if !SupportedVersion(release.Version) || len(release.digest) != sha256.Size*2 {
		return "", fmt.Errorf("Redis 版本缺少官方 SHA-256")
	}
	name := archiveName(release.Version)
	fmt.Fprintln(log, "下载 Redis 官方源码", release.Version)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	resp, err := get(ctx, sourceURL+name)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	path := filepath.Join(work, name)
	out, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	const limit = 160 << 20
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, hash), io.LimitReader(resp.Body, limit+1))
	if err := errors.Join(err, out.Close()); err != nil {
		return "", err
	}
	if n > limit || hex.EncodeToString(hash.Sum(nil)) != release.digest {
		return "", fmt.Errorf("Redis 源码 SHA-256 校验失败")
	}
	fmt.Fprintln(log, "Redis 官方 SHA-256 校验通过")
	return path, nil
}
