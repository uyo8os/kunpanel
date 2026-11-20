// 面板在线更新:从 GitHub Releases 检查版本并下载安装包。
package panelupdate

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"kunpanel/internal/notifyx"
)

const githubRepo = "uyo8os/kunpanel"

const uaHeader = "kunpanel-updater"

var (
	mu        sync.Mutex
	lastInfo  *Info
	current   string
	client    = &http.Client{Timeout: 30 * time.Second}
	bigClient = &http.Client{Timeout: 20 * time.Minute}
)

// Info 更新检查结果
type Info struct {
	Current     string `json:"current"`
	Latest      string `json:"latest"`
	HasUpdate   bool   `json:"has_update"`
	Name        string `json:"name"`
	Notes       string `json:"notes"`
	PublishedAt string `json:"published_at"`
	CheckedAt   string `json:"checked_at"`
	Source      string `json:"source"` // 本次检查命中的更新源
}

func SetCurrent(v string) { current = v }

// httpGet 带 UA 的 GET;statusCode 非 200 返回错误
func httpGet(url string, timeout time.Duration) (*http.Response, error) {
	c := client
	if timeout > time.Minute {
		c = bigClient
	}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", uaHeader)
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		resp.Body.Close()
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return resp, nil
}

func httpGetString(url string, limit int64) (string, error) {
	resp, err := httpGet(url, 30*time.Second)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if limit <= 0 {
		limit = 1 << 20
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

type release struct {
	TagName     string `json:"tag_name"`
	Name        string `json:"name"`
	Body        string `json:"body"`
	PublishedAt string `json:"published_at"`
}

// githubLatest 获取最新的 GitHub Release。
func githubLatest() (*release, error) {
	s, err := httpGetString("https://api.github.com/repos/"+githubRepo+"/releases/latest", 1<<20)
	if err != nil {
		return nil, err
	}
	var r release
	if err := json.Unmarshal([]byte(s), &r); err != nil {
		return nil, err
	}
	if r.TagName == "" {
		return nil, fmt.Errorf("无 tag_name")
	}
	return &r, nil
}

func githubAssetURL(tag, asset string) string {
	return "https://github.com/" + githubRepo + "/releases/download/" + tag + "/" + asset
}

// updateSource 更新源描述
type updateSource struct {
	name     string
	latest   func() (*release, error)
	sums     func(tag string) (string, error)                // SHA256SUMS 文本
	download func(tag, asset, dst string, w io.Writer) error // 安装包下载
}

var sources = []updateSource{
	{
		name:   "GitHub",
		latest: githubLatest,
		sums: func(tag string) (string, error) {
			return httpGetString(githubAssetURL(tag, "SHA256SUMS"), 1<<20)
		},
		download: func(tag, asset, dst string, w io.Writer) error {
			return downloadFile(githubAssetURL(tag, asset), dst, w)
		},
	},
}

// ---------- 检查 ----------

// Check 检查 GitHub Releases 的最新版本。
func Check() (*Info, error) {
	var lastErr error
	for _, src := range sources {
		r, err := src.latest()
		if err != nil {
			lastErr = err
			continue
		}
		mu.Lock()
		lastInfo = &Info{
			Current:     current,
			Latest:      r.TagName,
			HasUpdate:   newer(r.TagName, current),
			Name:        r.Name,
			Notes:       r.Body,
			PublishedAt: r.PublishedAt,
			CheckedAt:   time.Now().Format("2006-01-02 15:04:05"),
			Source:      src.name,
		}
		info := lastInfo
		mu.Unlock()
		return info, nil
	}
	return nil, fmt.Errorf("GitHub 更新源不可达: %v", lastErr)
}

// Cached 上次检查结果(可能为 nil)
func Cached() *Info {
	mu.Lock()
	defer mu.Unlock()
	return lastInfo
}

// Start 定时检查:启动 5 分钟后一次,之后每 6 小时一次;发现新版本写消息中心
func Start(stop <-chan struct{}) {
	go func() {
		time.Sleep(5 * time.Minute)
		if info, err := Check(); err == nil && info != nil && info.HasUpdate {
			notifyx.Add("update", "发现面板新版本 "+info.Latest,
				"当前 "+info.Current+",可前往 面板设置 → 关于 → 面板更新 一键在线更新")
		}
		t := time.NewTicker(6 * time.Hour)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				if info, err := Check(); err == nil && info != nil && info.HasUpdate {
					notifyx.Add("update", "发现面板新版本 "+info.Latest,
						"当前 "+info.Current+",可前往 面板设置 → 关于 → 面板更新 一键在线更新")
				}
			}
		}
	}()
}

// ---------- 版本比较 ----------

// newer 判断 latest 是否严格大于 current(v 前缀语义化版本;解析失败退化为不等即新)
func newer(latest, current string) bool {
	l, ok1 := Semver(latest)
	c, ok2 := Semver(current)
	if !ok1 || !ok2 {
		return latest != "" && latest != current
	}
	for i := 0; i < 3; i++ {
		if l[i] != c[i] {
			return l[i] > c[i]
		}
	}
	return false
}

// Semver 解析 v 前缀语义化版本,非法返回 false
func Semver(v string) ([3]int, bool) {
	var out [3]int
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) == 0 || parts[0] == "" {
		return out, false
	}
	for i := 0; i < 3 && i < len(parts); i++ {
		n, err := strconv.Atoi(parts[i])
		if err != nil {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

// ---------- 下载与校验 ----------

// expectedHash 从 SHA256SUMS 里取目标资产的哈希(行格式:<hash>  ./<name> 或 <hash>  <name>)
func expectedHash(sums, asset string) string {
	for _, ln := range strings.Split(sums, "\n") {
		f := strings.Fields(ln)
		if len(f) == 2 && (f[1] == "./"+asset || f[1] == asset || strings.HasSuffix(f[1], "/"+asset)) {
			if len(f[0]) == 64 {
				return strings.ToLower(f[0])
			}
		}
	}
	return ""
}

func fileHash(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func assetName(tag string) string {
	name := fmt.Sprintf("kunpanel_%s_%s_%s", tag, runtime.GOOS, runtime.GOARCH)
	if runtime.GOOS == "windows" {
		name += ".zip"
	} else {
		name += ".tar.gz"
	}
	return name
}

func downloadFile(url, dst string, w io.Writer) error {
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("User-Agent", uaHeader)
	resp, err := bigClient.Do(req)
	if err != nil {
		return fmt.Errorf("下载失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("下载失败: HTTP %d", resp.StatusCode)
	}
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer f.Close()
	n, err := io.Copy(f, io.LimitReader(resp.Body, 512<<20))
	if err != nil {
		return fmt.Errorf("下载中断: %v", err)
	}
	fmt.Fprintf(w, "已下载 %.1f MB\n", float64(n)/1024/1024)
	if n < 1<<20 {
		return fmt.Errorf("下载内容异常(小于 1MB)")
	}
	return nil
}
