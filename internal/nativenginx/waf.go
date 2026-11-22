package nativenginx

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unicode"

	"kunpanel/internal/config"
)

const wafBundle = "1.31.8.1"

// Digest of the upstream bundle verified with OpenResty's source-release signing key.
const wafBundleSHA256 = "b569c63b420f80a4ac8466f2deffe73ed93ab760026532e039c3658409a997f4"

const wafModuleBlock = `# KunPanel native WAF
load_module waf/ndk_http_module.so;
load_module waf/ngx_http_lua_module.so;
load_module waf/ngx_http_headers_more_filter_module.so;
# End KunPanel native WAF
`

// ngx_lua expands $prefix using the effective Nginx prefix (-p). That lets a
// candidate load its own libraries during config checks before switching current.
const WAFLuaPaths = `lua_package_path "$prefix/waf/lua/?.lua;;";
lua_package_cpath "$prefix/waf/lib/?.so;;";
`

var wafModules = []string{"ndk_http_module.so", "ngx_http_lua_module.so", "ngx_http_headers_more_filter_module.so"}

var wafSourceClient = &http.Client{Timeout: 45 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
	if len(via) >= 5 || req.URL.Scheme != "https" || req.URL.Host != "openresty.org" {
		return fmt.Errorf("拒绝非 openresty.org 的下载重定向")
	}
	return nil
}}

func buildOptions(target string) []string {
	return []string{"--prefix=" + target, "--with-http_ssl_module", "--with-http_v2_module",
		"--with-http_realip_module", "--with-http_stub_status_module", "--with-http_gzip_static_module", "--with-threads"}
}

func WAFScriptPath() string {
	return filepath.Join(config.DataDir(), "plugins", "waf", "native.lua")
}

func WAFConfigured() bool {
	b, err := os.ReadFile(ConfigPath())
	return err == nil && strings.Contains(string(b), wafModuleBlock)
}

// ConfigureWAF serializes plugin changes with version switches. apply updates
// the module and site configurations as a transaction and reloads the gateway.
func ConfigureWAF(enable bool, log io.Writer, apply func(string) error) error {
	installMu.Lock()
	defer installMu.Unlock()
	if !Installed() || !systemd() {
		return fmt.Errorf("原生 Nginx 未安装")
	}
	main, err := os.ReadFile(ConfigPath())
	if err != nil {
		return err
	}
	text := strings.Replace(string(main), wafModuleBlock, "", 1)
	if enable {
		target, err := os.Readlink(Prefix())
		if err != nil {
			return err
		}
		version := filepath.Base(target)
		if filepath.Dir(target) != filepath.Join(root(), "versions") || !validVersion.MatchString(version) {
			return fmt.Errorf("无法识别正在使用的原生 Nginx 版本")
		}
		if err := verifyBuildManifest(target, version); err != nil {
			return err
		}
		if err := prepareWAFModules(target, version, log); err != nil {
			return err
		}
		text = wafModuleBlock + text
	}
	return apply(text)
}

func extractSource(work, name string, data []byte, log io.Writer) error {
	archive := filepath.Join(work, name)
	if err := os.WriteFile(archive, data, 0o600); err != nil {
		return err
	}
	return run(log, 2*time.Minute, work, "tar", "-xzf", archive, "--no-same-owner", "--no-same-permissions")
}

func archiveComponent(bundle, name string) (string, error) {
	parts, err := filepath.Glob(filepath.Join(bundle, name+"-*"))
	if err != nil || len(parts) != 1 {
		return "", fmt.Errorf("OpenResty 组件包中无法识别 %s", name)
	}
	return parts[0], nil
}

func copyWAFFile(source, target string) error {
	b, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	return os.WriteFile(target, b, 0o644)
}

func prepareWAFModules(target, version string, log io.Writer) error {
	if strings.IndexFunc(target, unicode.IsSpace) >= 0 || strings.ContainsAny(target, "`\"'\\$;|&<>%*?[]()") {
		return fmt.Errorf("Nginx 数据路径含 WAF 编译无法处理的字符")
	}
	if log == nil {
		log = io.Discard
	}
	dest := filepath.Join(target, "waf")
	if _, err := os.Stat(dest); err == nil {
		return probeWAF(target, log)
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := dependencies(log); err != nil {
		return err
	}
	work, err := os.MkdirTemp(root(), ".waf-build-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	fmt.Fprintf(log, "为官方 Nginx %s 编译 Lua 防护模块\n", version)
	source := "https://nginx.org/download/nginx-" + version + ".tar.gz"
	nginx, err := fetch(source, 16<<20)
	if err != nil {
		return err
	}
	signature, err := fetch(source+".asc", 64<<10)
	if err != nil {
		return err
	}
	archive := filepath.Join(work, "nginx.tar.gz")
	if err := os.WriteFile(archive, nginx, 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(archive+".asc", signature, 0o600); err != nil {
		return err
	}
	if err := verifySource(work, archive, archive+".asc", log); err != nil {
		return err
	}
	if err := run(log, 2*time.Minute, work, "tar", "-xzf", archive, "--no-same-owner", "--no-same-permissions"); err != nil {
		return err
	}
	pkg := "openresty-" + wafBundle
	bundle, err := fetchWithClient(wafSourceClient, "https://openresty.org/download/"+pkg+".tar.gz", 24<<20)
	if err != nil {
		return err
	}
	if fmt.Sprintf("%x", sha256.Sum256(bundle)) != wafBundleSHA256 {
		return fmt.Errorf("OpenResty Lua 组件包的 SHA-256 不匹配")
	}
	if err := extractSource(work, pkg+".tar.gz", bundle, log); err != nil {
		return err
	}
	components := filepath.Join(work, pkg, "bundle")
	luajit, err := archiveComponent(components, "LuaJIT")
	if err != nil {
		return err
	}
	jobs := strconv.Itoa(min(2, max(1, runtime.NumCPU())))
	if err := run(log, 25*time.Minute, luajit, "make", "-j"+jobs); err != nil {
		return err
	}
	jitPrefix := filepath.Join(work, "luajit")
	if err := run(log, 3*time.Minute, luajit, "make", "install", "PREFIX="+jitPrefix); err != nil {
		return err
	}
	jitLib, jitInclude := filepath.Join(jitPrefix, "lib"), filepath.Join(jitPrefix, "include", "luajit-2.1")
	cjson, err := archiveComponent(components, "lua-cjson")
	if err != nil {
		return err
	}
	if err := run(log, 5*time.Minute, cjson, "make", "LUA_INCLUDE_DIR="+jitInclude); err != nil {
		return err
	}
	stage := filepath.Join(work, "waf")
	if err := os.MkdirAll(filepath.Join(stage, "lib"), 0o755); err != nil {
		return err
	}
	for _, file := range []struct{ source, name string }{
		{filepath.Join(jitLib, "libluajit-5.1.so.2"), "libluajit-5.1.so.2"},
		{filepath.Join(cjson, "cjson.so"), "cjson.so"},
	} {
		if err := copyWAFFile(file.source, filepath.Join(stage, "lib", file.name)); err != nil {
			return err
		}
	}
	for _, lib := range []string{"lua-resty-core", "lua-resty-lrucache", "lua-resty-dns"} {
		path, err := archiveComponent(components, lib)
		if err != nil {
			return err
		}
		if err := os.CopyFS(filepath.Join(stage, "lua"), os.DirFS(filepath.Join(path, "lib"))); err != nil {
			return err
		}
	}
	src := filepath.Join(work, "nginx-"+version)
	args := []string{"LUAJIT_LIB=" + jitLib, "LUAJIT_INC=" + jitInclude, "./configure"}
	args = append(args, buildOptions(target)...)
	args = append(args, "--with-ld-opt=-Wl,-rpath,"+filepath.Join(dest, "lib"))
	for _, name := range []string{"ngx_devel_kit", "ngx_lua", "headers-more-nginx-module"} {
		path, err := archiveComponent(components, name)
		if err != nil {
			return err
		}
		args = append(args, "--add-dynamic-module="+path)
	}
	if err := run(log, 6*time.Minute, src, "env", args...); err != nil {
		return err
	}
	if err := run(log, 25*time.Minute, src, "make", "-j"+jobs, "modules"); err != nil {
		return err
	}
	for _, name := range wafModules {
		if err := copyWAFFile(filepath.Join(src, "objs", name), filepath.Join(stage, name)); err != nil {
			return err
		}
	}
	if err := os.Rename(stage, dest); err != nil {
		return err
	}
	if err := probeWAF(target, log); err != nil {
		_ = os.RemoveAll(dest)
		return err
	}
	return nil
}

func probeWAF(target string, log io.Writer) error {
	tmp, err := os.MkdirTemp("/run", "kunpanel-waf-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	socket := filepath.Join(tmp, "check.sock")
	conf := filepath.Join(tmp, "nginx.conf")
	text := wafModuleBlock + fmt.Sprintf(`
daemon off;
master_process off;
pid %s;
error_log stderr notice;
events {}
http {
%s
    lua_shared_dict kunpanel_waf_probe 64k;
    server {
        listen unix:%s;
        more_set_headers 'X-KunPanel-WAF: ready';
        location / {
            content_by_lua_block {
                local dict = ngx.shared.kunpanel_waf_probe
                if not require("resty.core") or not require("resty.dns.resolver")
                    or not require("cjson.safe").encode({ok=true})
                    or not ngx.hmac_sha1("check", "ready") or not dict:set("ready", true) then
                    return ngx.exit(500)
                end
                return ngx.exit(204)
            }
        }
    }
}
`, filepath.Join(tmp, "nginx.pid"), WAFLuaPaths, socket)
	if err := os.WriteFile(conf, []byte(text), 0o600); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, filepath.Join(target, "sbin", "nginx"), "-p", target+"/", "-c", conf)
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait(); close(done) }()
	defer func() { _ = cmd.Process.Signal(os.Interrupt); <-done }()
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: time.Second}
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://localhost/", nil)
		if err != nil {
			return err
		}
		if resp, err := client.Do(req); err == nil {
			resp.Body.Close()
			if resp.StatusCode != http.StatusNoContent || resp.Header.Get("X-KunPanel-WAF") != "ready" {
				return fmt.Errorf("Nginx WAF 模块在运行时检查中返回 HTTP %d", resp.StatusCode)
			}
			fmt.Fprintln(log, "Nginx WAF 模块运行时检查通过")
			return nil
		}
		select {
		case err := <-done:
			return fmt.Errorf("Nginx WAF 模块检查启动失败: %v", err)
		case <-ctx.Done():
			return fmt.Errorf("等待 Nginx WAF 模块就绪超时")
		case <-time.After(100 * time.Millisecond):
		}
	}
}
