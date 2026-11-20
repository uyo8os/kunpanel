package websites

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"kunpanel/internal/appstore"
	"kunpanel/internal/nativephp"
)

// bindSitePHP records the chosen runtime on the site, so regenerating its
// configuration never silently converts a native site back to a Docker proxy.
func bindSitePHP(site *Site, key string) error {
	key = normalizePHPKey(key)
	instances, err := appstore.List()
	if err != nil {
		return fmt.Errorf("读取 PHP 运行时失败: %w", err)
	}
	if key == "" {
		for _, inst := range instances {
			if inst.Key == "php" {
				key = inst.Key
				break
			}
		}
		if key == "" {
			for _, inst := range instances {
				if appstore.IsPHPRuntimeKey(inst.Key) {
					key = inst.Key
					break
				}
			}
		}
	}
	for _, inst := range instances {
		if inst.Key != key || inst.InstallMode != "native" {
			continue
		}
		if !appstore.IsPHPRuntimeKey(inst.Key) {
			return fmt.Errorf("不是 PHP 运行时: %s", key)
		}
		if DetectNginx().Target != ManagedNginxBinary() {
			return fmt.Errorf("原生 PHP-FPM 需要先安装面板原生 Nginx")
		}
		minor, err := nativephp.Minor(inst.Params["Version"])
		if err != nil {
			return fmt.Errorf("PHP 运行时版本无效: %w", err)
		}
		if err := checkExistingNativePHPSiteDirectory(site.Root); err != nil {
			return err
		}
		if err := checkNativeNginxSocketAccess(nativephp.Socket(minor)); err != nil {
			return err
		}
		site.PhpKey = key
		site.PHPMode = "native"
		site.FpmPass = "unix:" + nativephp.Socket(minor)
		site.ProxyTo = ""
		return nil
	}
	// A previously native site must not silently be changed to a nonexistent
	// Docker upstream when its requested runtime has disappeared.
	if site.PHPMode == "native" {
		for _, inst := range instances {
			if inst.Key == key && appstore.IsPHPRuntimeKey(key) && inst.InstallMode != "native" {
				site.PhpKey = key
				site.PHPMode = ""
				site.FpmPass = ""
				site.ProxyTo = PhpUpstreamFor(key, filepath.Base(site.Root))
				return nil
			}
		}
		return fmt.Errorf("PHP 运行时 %s 不存在，无法切换站点", key)
	}
	// Legacy Docker PHP sites and installations keep their existing proxy path.
	site.PhpKey = key
	site.PHPMode = ""
	site.FpmPass = ""
	site.ProxyTo = PhpUpstreamFor(key, filepath.Base(site.Root))
	return nil
}

// Restore a native PHP binding when regenerating or reloading its site fails.
// Leave unrelated concurrent changes alone rather than overwriting them.
func restoreSiteAfterPHPChange(old, attempted Site, cause error) error {
	if old.PHPMode != "native" && attempted.PHPMode != "native" {
		return cause
	}
	mu.Lock()
	load()
	found := false
	var restored Site
	for i := range list {
		if list[i].ID != old.ID {
			continue
		}
		if list[i].Type != attempted.Type || list[i].PhpKey != attempted.PhpKey || list[i].PHPMode != attempted.PHPMode ||
			list[i].ProxyTo != attempted.ProxyTo || list[i].ProxyHost != attempted.ProxyHost || list[i].FpmPass != attempted.FpmPass {
			mu.Unlock()
			return fmt.Errorf("PHP 配置失败（站点已被其他操作修改，未自动回滚）: %w", cause)
		}
		// Revert only fields changed by the binding operation so concurrent
		// edits to other settings are not silently lost.
		list[i].Type, list[i].PhpKey, list[i].PHPMode = old.Type, old.PhpKey, old.PHPMode
		list[i].ProxyTo, list[i].ProxyHost, list[i].FpmPass = old.ProxyTo, old.ProxyHost, old.FpmPass
		restored = list[i]
		found = true
		break
	}
	if !found {
		mu.Unlock()
		return fmt.Errorf("PHP 配置失败（站点已不存在）: %w", cause)
	}
	err := save()
	mu.Unlock()
	if err != nil {
		return fmt.Errorf("PHP 配置失败: %v；恢复站点记录失败: %w", cause, err)
	}
	if err := writeConf(&restored); err != nil {
		return fmt.Errorf("PHP 配置失败: %v；恢复站点配置失败: %w", cause, err)
	}
	if err := Reload(); err != nil {
		return fmt.Errorf("PHP 配置失败: %v；已恢复站点配置，但重载失败: %w", cause, err)
	}
	return fmt.Errorf("PHP 配置失败，已恢复原站点: %w", cause)
}

func nativeNginxLiteral(s string) (string, error) {
	if s == "" || strings.ContainsAny(s, "\r\n\x00$") {
		return "", fmt.Errorf("原生 PHP 配置路径无效")
	}
	return strconv.Quote(s), nil
}

func writeNativePHPServer(b *strings.Builder, socket, root, index string, extraHasRoot bool) error {
	quotedRoot, err := nativeNginxLiteral(root)
	if err != nil {
		return err
	}
	if strings.TrimSpace(index) == "" {
		index = "index.php index.html index.htm"
	}
	fmt.Fprintf(b, "    root %s;\n    index %s;\n\n", quotedRoot, index)
	return writeNativePHPLocations(b, socket, extraHasRoot)
}

// Only existing .php files under the document root reach FPM; PHP-FPM sockets
// are local to the host and are never exposed as a network listener.
func writeNativePHPLocations(b *strings.Builder, socket string, extraHasRoot bool) error {
	if !strings.HasPrefix(socket, "unix:") || !filepath.IsAbs(strings.TrimPrefix(socket, "unix:")) {
		return fmt.Errorf("原生 PHP-FPM 地址无效")
	}
	quotedSocket, err := nativeNginxLiteral(socket)
	if err != nil {
		return err
	}
	fmt.Fprintf(b, `    location ~ \.php$ {
        try_files $uri =404;
        fastcgi_pass %s;
        fastcgi_index index.php;
        fastcgi_param SCRIPT_FILENAME $document_root$fastcgi_script_name;
        fastcgi_param QUERY_STRING $query_string;
        fastcgi_param REQUEST_METHOD $request_method;
        fastcgi_param CONTENT_TYPE $content_type;
        fastcgi_param CONTENT_LENGTH $content_length;
        fastcgi_param SCRIPT_NAME $fastcgi_script_name;
        fastcgi_param REQUEST_URI $request_uri;
        fastcgi_param DOCUMENT_URI $document_uri;
        fastcgi_param DOCUMENT_ROOT $document_root;
        fastcgi_param SERVER_PROTOCOL $server_protocol;
        fastcgi_param REQUEST_SCHEME $scheme;
        fastcgi_param HTTPS $https if_not_empty;
        fastcgi_param GATEWAY_INTERFACE CGI/1.1;
        fastcgi_param SERVER_SOFTWARE nginx/$nginx_version;
        fastcgi_param REMOTE_ADDR $remote_addr;
        fastcgi_param REMOTE_PORT $remote_port;
        fastcgi_param SERVER_ADDR $server_addr;
        fastcgi_param SERVER_PORT $server_port;
        fastcgi_param SERVER_NAME $server_name;
        fastcgi_param REDIRECT_STATUS 200;
        # Prevent client-supplied Proxy headers becoming the CGI HTTP_PROXY variable.
        fastcgi_param HTTP_PROXY "";
    }
`, quotedSocket)
	if !extraHasRoot {
		b.WriteString("    location / {\n        try_files $uri $uri/ =404;\n    }\n")
	}
	return nil
}
