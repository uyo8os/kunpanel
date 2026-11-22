<div align="center">

<img src="web/static/logo.png" width="88" alt="坤面板">

# 坤面板 KunPanel

**轻量级服务器运维管理面板 —— Go 单二进制,开箱即用**

[![](https://img.shields.io/badge/版本-v2.98.0-3b82f6?style=flat-square)](https://github.com/uyo8os/kunpanel/releases)
[![](https://img.shields.io/badge/Go-1.27+-00ADD8?style=flat-square)](https://golang.org)
[![](https://img.shields.io/badge/平台-Linux%20%7C%20Windows-lightgrey?style=flat-square)](./README.md)
[![](https://img.shields.io/badge/License-MIT-green?style=flat-square)](./LICENSE)
[![](https://img.shields.io/badge/📖_使用教程-TUTORIAL-8a2be2?style=flat-square)](docs/TUTORIAL.md)

</div>

---

> 📖 **[完整使用教程(TUTORIAL.md)](docs/TUTORIAL.md)** —— 首次登录、建站、SSL、迁移、WAF、计划任务、FAQ,手把手图文步骤。

---

## ⚡ 一键安装

### Linux(自适应发行版与架构)

支持 Ubuntu / Debian / CentOS / RHEL / Rocky / Alma / Fedora / openSUSE / Arch / Alpine 等主流发行版,
x86_64 / aarch64 / armv7 三架构,自动识别 systemd / OpenRC / SysV:

```bash
curl -fsSL 'https://raw.githubusercontent.com/uyo8os/kunpanel/main/scripts/install.sh' -o kp-install.sh && bash kp-install.sh
```

安装完成自动输出:**随机端口 + 随机安全入口 + 随机管理员密码**。
安装包和面板「在线更新」使用 GitHub Releases。

<details>
<summary>📦 安装参数与卸载</summary>

```bash
bash kp-install.sh --port 20000 --username admin --password yourpwd   # 自定义初始化
bash kp-install.sh --with-docker            # 同时安装 Docker 引擎(自动配国内镜像源)
bash kp-install.sh --mirror aliyun          # Docker 使用阿里云镜像源
bash kp-install.sh --proxy https://gh-proxy.com/   # GitHub 下载加速
bash kp-install.sh --version v2.84.0        # 安装指定版本
bash kp-install.sh --offline ./kunpanel_linux_amd64.tar.gz   # 离线安装(含 SHA-256 校验)
bash kp-install.sh --uninstall              # 卸载
```
</details>

### 原生 Nginx

在「应用商店」选择 Nginx，可从官方 Stable 或 Mainline 获取当前版本，下载源码并编译为面板管理的原生服务。该功能目前支持 Linux systemd，需 root 权限及可访问 Nginx 官方站点；编译依赖缺失时会通过系统软件源安装。安装、版本切换和卸载会保留网站数据及配置。

原生 Nginx 可在「安全中心 → WAF」安装内置防护。面板会为当前 Nginx 版本编译 Lua 模块，升级时重新编译并验证后再切换。已有其他 Nginx 服务、网关容器、端口占用或旧容器配置时，请先处理冲突，再安装原生 Nginx。

### 原生 PHP

应用商店提供从 PHP 官方源码编译的 PHP-FPM 8.3、8.4、8.5 和 8.6。各版本独立运行，网站可选择对应版本。需 Linux systemd、root 权限及面板原生 Nginx。已有 PHP 容器和网站绑定继续可用。

在「应用商店 → 已安装 → PHP → 管理 → 扩展」可以安装、更新或卸载 Redis、APCu、Imagick、igbinary、MessagePack 和 Xdebug。面板从各扩展的上游正式版本编译，兼容性由扩展上游决定。同分支 PHP 升级时会先为新版本重编已安装的扩展。

在原生 PHP 的「管理 → 禁用函数」中，可为该 PHP 版本设置 `disable_functions`。用逗号或换行分隔函数名，清空后保存可解除列表里的限制。保存成功后，运行中的 PHP-FPM 会重启。

### 原生 MySQL

在「应用商店 → MySQL」选择 5.7、8.0、8.4 LTS 或 9.7 LTS。面板直接下载 MySQL 官方 Linux 二进制包并校验签名，由 systemd 管理服务。安装需 Linux systemd 和 root 权限。5.7 和 8.0 提供各自的最终版本（5.7 仅支持 x86-64），8.4 LTS 与 9.7 LTS 的更新从官方发布说明发现。

数据库页支持建库、SQL、用户授权和 `.sql.gz` 备份恢复。已有 Docker MySQL 的服务器可另装原生版，原生版默认建议用 3308 端口。迁移时先为各业务库导出备份、在原生版建库并导入，再创建应用账号并修改连接端口。原生版仅监听本机 TCP，面板通过 Unix Socket 管理数据库。同分支升级会暂时停止服务并保留旧版数据副本，卸载会保留数据库文件和配置。

### 原生 phpMyAdmin

已安装原生 Nginx 和 PHP 后，可在应用商店安装 phpMyAdmin。面板从官方下载安装包，验证发布签名，并为 phpMyAdmin 启动独立的 PHP-FPM 服务。可从应用管理或数据库页打开，连接面板管理的 MySQL 和 MariaDB。

安装时设置独立的 HTTPS 端口和访问密码。浏览器先用 `phpmyadmin` 和访问密码通过入口认证，再用数据库账号登录。面板证书覆盖所填访问域名或 IP 时会复用该证书，否则生成自签名证书。已有容器版可与原生版并行。

### 原生 PostgreSQL

在「应用商店 → PostgreSQL」可选择官方仍维护的正式分支。面板从 PostgreSQL 官方下载源码并校验 SHA-256，在面板数据盘编译，再用 systemd 运行独立服务。默认监听 `127.0.0.1:5432`，已有容器版时安装表单建议用 `5433`。面板通过 Unix Socket 和本地账号认证执行数据库管理。

数据库页支持建库、SQL 查询、`postgres` 改密、SQL 备份恢复和定时备份。同一大版本内可检查并升级补丁。卸载时保留数据、角色和配置，按原大版本重新安装即可启用。容器迁移和跨大版本迁移使用逻辑备份，导入前需准备备份引用的角色。

### 原生 Redis

在「应用商店 → Redis」可选择官方清单中 7.2 及以上各分支的最新正式版。面板校验源码的官方 SHA-256，在面板数据盘编译 Redis 核心服务，再用 systemd 运行。默认监听 `127.0.0.1:6379`，启用 AOF 每秒刷盘。已有容器版时安装表单建议用 `6380`，两个实例各自使用独立的数据目录。

在「已安装 → Redis → 管理」可查看运行状态和日志、修改访问密码、生成 `dump.rdb` 快照以及检查同大版本更新。升级先编译再停机复制数据，旧版本的数据副本会保留。卸载保留数据、端口和密码，从应用商店重新启用即可继续使用。

### Windows

下载 [Release](https://github.com/uyo8os/kunpanel/releases) 中的 `kunpanel_windows_amd64.zip`,
解压后**右键以管理员身份运行 `install.bat`** —— 自动注册 Windows 服务、放行防火墙并输出访问地址。

### 🐳 Docker

```bash
docker build -t kunpanel:latest .
docker run -d --name kunpanel -p 20000:20000 \
  -v /opt/kunpanel/data:/opt/kunpanel/data \
  -v /var/run/docker.sock:/var/run/docker.sock \
  --restart unless-stopped kunpanel:latest
```

## 🖼 界面预览

| 概览 | 应用商店 |
|---|---|
| ![](docs/screenshots/dashboard.png) | ![](docs/screenshots/store.png) |
| **网站管理** | **1Panel 一键迁移** |
| ![](docs/screenshots/websites.png) | ![](docs/screenshots/1panel-migrate.png) |
| **安全中心(WAF)** | **监控中心(拨测)** |
| ![](docs/screenshots/security.png) | ![](docs/screenshots/monitor.png) |
| **文件管理** | **容器管理** |
| ![](docs/screenshots/files.png) | ![](docs/screenshots/docker.png) |

## ✨ 功能全景

### 🔄 面板迁移
- **宝塔 / 1Panel 双向互通**:本机迁移、远程 SSH 迁移、反向导出,全流程任务化带实时日志
- 站点文件 / SSL 证书 / 伪静态 / 自定义规则保真迁移,数据库可选导入并重建账号
- 流量切换自动停用源面板网关(禁自启)、改绑 80/443、**逐域名×逐路径状态码比对**,异常自动回滚

### 🛡 安全
- **WAF 防火墙**(原生 Nginx 或已有容器网关 + Lua):CC 攻击防御 **七种模式**(限流 / 5 秒跳转 / JS 无感 / Cloudflare 风格 Verifying / 滑块 / 点击 / 算术验证码)——**人机验证模式选完即全站启用**,首次访问先验证、通过后免验,阈值作为已验证访客的频率上限;IP 黑白名单(**支持 CIDR 网段**)、UA 黑名单与搜索引擎蜘蛛白名单、敏感文件与危险方法拦截、SQL 注入 / XSS / 文件包含 / 命令执行检测、**自定义正则规则**(热更新)
- **自动封禁**:违规超阈值的公网 IP 自动拉黑(私网强制排除防误伤),拦截日志一键封禁;**容器端口 DOCKER-USER 管控**(堵 Docker 绕过系统防火墙的口子)
- **一键安全自检**:9 项体检评分;SSH 加固;登录限流 + PNG 验证码 + TOTP 两步验证 + 随机安全入口;ICMP ping 开关

### 🌐 网站
- 静态 / PHP / 反向代理三类型,**支持随时互相转换**(转换即生效,数据保留)
- 反向代理**负载均衡**(多上游 + 权重)、**自定义发送域名 Host 头**(默认访客原始域名,可指定,上游按域名区分站点时免改上游配置)
- HTTP/2、HSTS、Gzip、浏览器缓存、限速、防盗链、防恶意解析、维护模式、安全响应头、伪静态预设、301 重定向
- Let's Encrypt 一键申请与自动续期(支持测试环境预签);证书上传 / 下载 / 自签
- **网站流量统计**(PV/UV/流量/Top 排行);站点日志独立落盘 + 一键轮转归档
- 站点备份:服务端留存 / 打包下载 / 上传恢复(原子化,失败不伤原站)

### 🗄 数据库 · 应用 · 容器
- MySQL / MariaDB / PostgreSQL 管理:SQL 控制台、备份导入导出和账号管理。Redis 在应用商店管理运行状态、访问密码和快照。
- 应用商店提供 Nginx、PHP、MySQL、PostgreSQL、phpMyAdmin 和 Redis，支持**自定义应用**(Compose 在线校验)
- 容器 / 镜像 / 网络 / 卷管理,容器终端,资源限制热更新,日志轮转配置

### 📂 文件 · 任务 · 监控
- **专业代码编辑器**:CodeMirror 高亮 13 种语言、快捷键、**历史版本回滚**、全屏;文件名 / 内容全文搜索
- 文件管理:拖拽上传 / 远程下载 / 压缩解压 / 权限 / **回收站(可恢复、定时清理)** / 图片预览
- 计划任务七类型(Shell / 目录备份 / 日志切割 / 数据库备份 / 回收站清理 / 面板备份 / 容器日志清理)+ 可视化执行周期,支持**备份完成后自动上传 SFTP 远程**
- **网站拨测**:每分钟探测 HTTP/TCP,连续失败防抖判宕,状态翻转推送告警,24h 可用率与延迟历史
- 磁盘 IO 与网络监控(24h 历史,异常退出不丢数据);**后台任务中心**(长任务实时进度条)+ 消息中心 + 每日威胁摘要
- **告警推送**:邮件 / 钉钉 / 飞书 / Webhook

### ⚡ 极致轻量
- Go 单二进制 + 原生前端(无 Node 运行时),空载内存 **约 12MB**、CPU **<0.5%**
- Docker 状态轮询自适应降频(操作时 8 秒、空闲 60 秒),后台资源占用可忽略
- 全 API 实测毫秒级响应;GC 调参 + 内存软上限,7×24 长驻不飘

## 🔒 安全设计

| 维度 | 措施 |
|---|---|
| 认证 | PNG 图形验证码、IP + 用户名双维度限流、TOTP 两步验证、随机安全入口 |
| 会话 | HMAC-SHA256 签名令牌,改密即轮换密钥强制下线 |
| 注入 | 域名白名单、compose 参数转义、解压 ZipSlip 防护、SQL 标识符白名单、反代 Host 头注入防护 |
| 传输 | CSP 脚本哈希白名单(无 unsafe-inline)、HSTS 可选、面板 HTTPS;远程迁移 SSH 指纹 TOFU 校验 |
| 供应链 | 升级包 SHA-256 校验和验证 |
| 沙箱 | systemd NoNewPrivileges / PrivateTmp / ProtectKernelTunables / ProtectControlGroups(v2.75.5 移除 ProtectSystem:其 /usr、/etc 只读会阻断包管理与 Docker 安装)/ 私网自动封禁排除 |
| 审计 | 登录与变更操作全留痕、真实状态码、安全回归测试锁定 |

> ⚠️ 注意:Docker 发布的端口**不经过** ufw / firewalld( iptables DOCKER 链直通),
> 请使用面板「容器端口管控」或 WAF 的 DOCKER-USER 管控功能限制外部访问。

## 🛠 从源码构建

```bash
git clone https://github.com/uyo8os/kunpanel.git && cd kunpanel
bash build.sh v2.98.1     # 产出 linux(amd64/arm64/arm) + windows 全平台安装包
```

Go 1.27+,前端为原生 JS(`go:embed` 内嵌),无 Node 构建步骤。

## 📦 发布新版本

发布前先在准备发布的分支上完成并推送以下修改：

1. 将 `main.go` 中的 `var version` 改为新版本，例如 `v2.99.0`。这是程序默认版本和发布校验的版本来源。
2. 将 `.github/workflows/publish-release.yml` 中手动输入的 `default` 和本文顶部版本徽章同步更新为同一版本；在 `CHANGELOG.md` 添加对应的更新记录。Docker 镜像默认使用 `main.go` 中的版本，也可用构建参数 `VERSION` 覆盖。
3. 确认提交中的版本号采用 `v数字.数字.数字` 格式，并检查发布分支和提交。工作流会校验版本、标签与构建出的程序版本，运行测试和构建，全部成功后发布 Release 并标记为 Latest。

有两种发布方式：

- **推送标签**：在要发布的提交上执行 `git tag v2.99.0`，然后执行 `git push origin v2.99.0`。推送以 `v` 开头的标签会触发 **Publish release** 工作流；不符合版本格式的标签会校验失败。
- **手动发布**：在 GitHub 仓库的 **Actions → Publish release → Run workflow** 中选择发布分支，填写必填项 **Stable version tag (vX.Y.Z)** 并运行。输入框默认值来自工作流文件，需要随版本更新。标签不存在时，工作流会为所选分支的提交创建标签；标签已存在且指向其他提交时会报错，不会移动标签。

如果该版本的 Release 已存在，工作流会停止并报错。工作流先创建草稿并上传文件，全部成功后才发布；如果中途失败，检查 Actions 日志、标签指向和 GitHub Releases 页面。若遗留草稿，请检查并删除该草稿后重试；保留指向正确提交的标签即可，不要移动已发布的标签来覆盖已有版本。

Release 包含 Linux 的 amd64、arm64、arm 安装包和 Windows amd64 安装包，每种都有带版本号和不带版本号的文件，以及 `SHA256SUMS` 校验文件。不带版本号的文件供安装脚本和在线更新使用。

工作流使用 GitHub 自动提供的 `GITHUB_TOKEN` 创建标签和 Release，需要 `contents: write` 权限，无需自行创建发布 Secrets。请在仓库设置中确认 Actions 允许工作流写入仓库，并检查标签保护或规则集允许该工作流创建版本标签。建议限制版本标签的修改和删除，以防其他操作在发布检查与发布之间更改标签。当前仓库没有自动签名或镜像发布工作流，也不需要为它们配置 Secrets。

## 📄 License

[MIT](LICENSE) 

