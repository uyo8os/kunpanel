package nativemysql

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

type Release struct {
	Branch  string `json:"branch"`
	Version string `json:"version"`
	Legacy  bool   `json:"legacy"`
	Error   string `json:"error,omitempty"`
}

var branches = []Release{{Branch: "8.4"}, {Branch: "9.7"}, {Branch: "8.0", Version: "8.0.46", Legacy: true}, {Branch: "5.7", Version: "5.7.44", Legacy: true}}
var versionPattern = regexp.MustCompile(`^[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,4}(\.[0-9]{1,4})?$`)
var publishedVersion = regexp.MustCompile(`Changes in MySQL ([0-9.]+) \(([0-9]{4}-[0-9]{2}-[0-9]{2})\)`)

var mysqlHTTP = &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error {
	if len(via) < 5 && req.URL.Scheme == "https" && req.URL.Port() == "" {
		switch req.URL.Hostname() {
		case "dev.mysql.com", "cdn.mysql.com", "downloads.mysql.com", "repo.mysql.com":
			return nil
		}
	}
	return fmt.Errorf("拒绝非 MySQL 官方下载地址的重定向")
}}

func Branch(version string) string {
	if versionPattern.MatchString(version) {
		for _, branch := range branches {
			if strings.HasPrefix(version, branch.Branch+".") {
				return branch.Branch
			}
		}
	}
	return ""
}

func CompareVersions(a, b string) int {
	parse := func(version string) [4]int {
		var parts [4]int
		for i, value := range strings.SplitN(version, ".", 4) {
			parts[i], _ = strconv.Atoi(value)
		}
		return parts
	}
	left, right := parse(a), parse(b)
	for i := range left {
		if left[i] != right[i] {
			return left[i] - right[i]
		}
	}
	return 0
}

func httpGet(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	res, err := mysqlHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		res.Body.Close()
		return nil, &downloadStatus{url: url, code: res.StatusCode}
	}
	return res, nil
}

type downloadStatus struct {
	url  string
	code int
}

func (s *downloadStatus) Error() string { return fmt.Sprintf("%s: HTTP %d", s.url, s.code) }

func fetch(url string, timeout time.Duration, limit int64) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	res, err := httpGet(ctx, url)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, limit+1))
	if err == nil && int64(len(data)) > limit {
		return nil, fmt.Errorf("MySQL 官方响应超过大小限制")
	}
	return data, err
}

func versionFromNotes(branch string, body []byte, now time.Time) (string, error) {
	var selected string
	for _, match := range publishedVersion.FindAllSubmatch(body, -1) {
		version := string(match[1])
		if Branch(version) != branch || string(match[2]) > now.UTC().Format(time.DateOnly) {
			continue
		}
		if selected == "" || CompareVersions(version, selected) > 0 {
			selected = version
		}
	}
	if selected == "" {
		return "", fmt.Errorf("官方发布说明没有已发布的 MySQL %s 版本", branch)
	}
	return selected, nil
}

func Latest(branch string) (Release, error) {
	for _, release := range branches {
		if release.Branch != branch {
			continue
		}
		if release.Legacy {
			if branch == "5.7" && runtime.GOARCH != "amd64" {
				return Release{}, fmt.Errorf("MySQL 5.7 官方归档包仅支持 Linux x86-64")
			}
			return release, nil
		}
		body, err := fetch("https://dev.mysql.com/doc/relnotes/mysql/"+branch+"/en/", 15*time.Second, 2<<20)
		if err != nil {
			return Release{}, err
		}
		release.Version, err = versionFromNotes(branch, body, time.Now())
		return release, err
	}
	return Release{}, fmt.Errorf("仅支持 MySQL 5.7、8.0、8.4 LTS 和 9.7 LTS")
}

func Versions() []Release {
	type result struct {
		index   int
		release Release
	}
	results := make(chan result, len(branches))
	for i, branch := range branches {
		go func() {
			release, err := Latest(branch.Branch)
			if err != nil {
				release = Release{Branch: branch.Branch, Legacy: branch.Legacy, Error: err.Error()}
			}
			results <- result{i, release}
		}()
	}
	list := make([]Release, len(branches))
	for range branches {
		r := <-results
		list[r.index] = r.release
	}
	return list
}
