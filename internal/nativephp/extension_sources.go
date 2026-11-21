package nativephp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"golang.org/x/mod/semver"
)

type extensionSource struct {
	name, repo, tagPrefix string
	zend                  bool
	minimum               map[string]string
}

// Only exclude releases with known compatibility gaps. New stable tags are
// picked up automatically, then still have to pass compilation and load checks.
var extensionSources = []extensionSource{
	{name: "redis", repo: "phpredis/phpredis", minimum: map[string]string{"8.6": "6.3.1"}},
	{name: "apcu", repo: "krakjoe/apcu", minimum: map[string]string{"8.6": "5.1.29"}},
	{name: "imagick", repo: "Imagick/imagick", minimum: map[string]string{"8.6": "3.8.2"}},
	{name: "igbinary", repo: "igbinary/igbinary", minimum: map[string]string{"8.5": "3.2.17", "8.6": "3.2.17"}},
	{name: "msgpack", repo: "msgpack/msgpack-php", tagPrefix: "msgpack-", minimum: map[string]string{"8.6": "3.0.2"}},
	{name: "xdebug", repo: "xdebug/xdebug", zend: true, minimum: map[string]string{"8.3": "3.3.0", "8.4": "3.4.0", "8.5": "3.5.0", "8.6": "3.6.0"}},
}

func findExtension(name string) (extensionSource, bool) {
	for _, ext := range extensionSources {
		if ext.name == name {
			return ext, true
		}
	}
	return extensionSource{}, false
}

func SupportsExtension(name string) bool {
	_, ok := findExtension(name)
	return ok
}

type extensionRelease struct {
	version string
	commit  string
}

var stableExtensionVersion = regexp.MustCompile(`^v?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
var gitCommit = regexp.MustCompile(`^[a-f0-9]{40}$`)

var extensionClient = &http.Client{
	Timeout: 2 * time.Minute,
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return fmt.Errorf("扩展官方地址发生重定向")
	},
}

func latestExtension(minor string, ext extensionSource) (extensionRelease, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	url := "https://api.github.com/repos/" + ext.repo + "/tags?per_page=100"
	body, err := fetchWithClient(ctx, extensionClient, url, 1<<20)
	if err != nil {
		return extensionRelease{}, fmt.Errorf("获取 %s 官方发布标签失败: %w", ext.name, err)
	}
	return selectExtensionRelease(minor, ext, body)
}

func selectExtensionRelease(minor string, ext extensionSource, body []byte) (extensionRelease, error) {
	var tags []struct {
		Name   string
		Commit struct{ SHA string }
	}
	if err := json.Unmarshal(body, &tags); err != nil {
		return extensionRelease{}, err
	}
	var selected extensionRelease
	for _, tag := range tags {
		name := strings.TrimPrefix(tag.Name, ext.tagPrefix)
		if len(name) > 32 || !stableExtensionVersion.MatchString(name) || !gitCommit.MatchString(tag.Commit.SHA) {
			continue
		}
		version := strings.TrimPrefix(name, "v")
		if minimum := ext.minimum[minor]; minimum != "" && semver.Compare("v"+version, "v"+minimum) < 0 {
			continue
		}
		if semver.Compare("v"+version, "v"+selected.version) > 0 {
			selected = extensionRelease{version, tag.Commit.SHA}
		}
	}
	if selected.version == "" {
		return extensionRelease{}, fmt.Errorf("上游暂未提供可用于 PHP %s 的 %s 正式版本", minor, ext.name)
	}
	return selected, nil
}
