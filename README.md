# WPS 2 WebDAV

把你自己的 WPS 企业云盘接入 WebDAV。安装完成后，可以用浏览器、Windows、Linux、手机、NAS 或其他 WebDAV 客户端访问文件。

```text
WPS 企业云盘 -> WPS 2 WebDAV -> 网页 / WebDAV / REST
```

当前版本：`0.9.8`。项目仍属于实验性适配器，不是 WPS 官方软件。

## 最简单的使用方法

整个流程只需要三步：在 VPS 安装服务，在自己的电脑运行一次登录助手，然后打开网页或连接 WebDAV。

### 第一步：安装到 VPS

下面两条命令选一条。端口可以改成任意未占用端口；下面以 `54321` 为例。

Native（推荐，VPS 不需要 Docker）：

```bash
set -o pipefail; curl -fL --progress-bar --connect-timeout 10 --max-time 60 --retry 1 'https://raw.githubusercontent.com/galiandan/WPS_2_WebDAV/main/scripts/install-native.sh' | sudo bash -s -- --port 54321
```

Docker：

```bash
set -o pipefail; curl -fL --progress-bar --connect-timeout 10 --max-time 60 --retry 1 'https://raw.githubusercontent.com/galiandan/WPS_2_WebDAV/main/scripts/install-docker.sh' | sudo bash -s -- --port 54321
```

安装器会显示下载和安装进度，并在首次安装时询问 WebDAV/网页共用的 Basic Auth 用户名和密码。密码不会显示，请记住它，后面连接服务时要使用。

安装器默认使用执行 `sudo` 的当前用户运行 Native 服务，不要求你额外创建 Linux 用户。Docker 方式要求 VPS 已能运行 Docker，但你的个人电脑不需要安装 Docker。

如果 VPS 无法访问 GitHub Raw，可将命令中的地址替换为以下地址之一：

```text
https://gh-proxy.com/https://raw.githubusercontent.com/galiandan/WPS_2_WebDAV/main/...
https://ghfast.top/https://raw.githubusercontent.com/galiandan/WPS_2_WebDAV/main/...
```

安装器会校验固定版本的文件清单。看到“下载归档的内容清单校验失败”时，重新复制当前 README 的命令执行，不要混用旧命令或旧校验值。

### 第二步：登录 WPS

在你自己的电脑上下载并运行独立登录脚本：

```bash
curl -fL --progress-bar --connect-timeout 10 --max-time 60 --retry 1 'https://raw.githubusercontent.com/galiandan/WPS_2_WebDAV/main/wps_login.py' -o wps_login.py && python3 wps_login.py
```

电脑需要 Python `3.11+`、Chrome 或 Chromium。如果选择 SSH 同步，还需要系统自带的 `ssh` 命令。

脚本会询问 VPS 地址、连接方式、端口和 Basic Auth 信息，然后打开一个临时隔离的官方 WPS 登录窗口：

1. 只在官方 WPS 页面完成登录、学校 SSO、扫码或验证码。
2. 登录完成后脚本自动读取当前账号可见的 WPS 空间。
3. 浏览器自动关闭后，回到终端选择空间：输入 `1` 选择一个，输入 `1,3` 选择多个，输入 `all` 选择全部。
4. 选择的空间会在 WebDAV 根目录下显示为文件夹，例如 `/学校云盘/`、`/自动备份/`。
5. 脚本验证空间可访问后，自动把凭据和工作区配置同步到 VPS。

不会需要你手动填写企业 ID、群组 ID 或文件夹 ID，也不会把 Cookie 显示出来。WPS 自动跳转到的旧文件夹不会被当作目标，默认使用空间根目录。

### 第三步：访问

浏览器打开：

```text
http://<VPS-IP>:54321/
```

WebDAV 地址：

```text
http://<VPS-IP>:54321/dav/
```

用户名和密码就是安装时设置的 Basic Auth 凭据。端口不是 `54321` 时，把地址中的端口替换成安装时填写的端口。

网页支持浏览、上传、拖动上传、上传速度显示、下载、新建文件夹、重命名、移动和删除。点击网页右上角齿轮可以修改云盘显示名称。

## HTTP 和 HTTPS

没有域名和证书时可以使用 HTTP，适合个人可信网络或临时测试。但 HTTP 会明文传输 Basic Auth、WPS 会话和文件内容，不适合直接暴露在公网。

有域名时，建议使用 Nginx、Caddy 或其他反向代理提供 HTTPS，再让代理转发到适配器。WebDAV 客户端应使用：

```text
https://<你的域名>/dav/
```

## 登录同步方式

登录助手默认会询问三种方式：

1. SSH 私钥：适合已经用 SSH 密钥管理 VPS 的用户。
2. SSH 密码：适合没有 SSH 私钥的用户，登录完成后由 SSH 自己询问密码。
3. HTTP/HTTPS 适配器接口：输入服务地址、Basic Auth 用户名和密码即可同步。

没有 HTTPS 时，HTTP 方式必须明确确认风险：

```bash
python3 wps_login.py --adapter-url http://<VPS-IP>:54321 --adapter-user <用户名> --allow-http
```

如果登录脚本提示 Chrome 会话未启动、Cookie 不完整或无权访问，请确认官方 WPS 窗口已经完成登录并进入企业云盘，然后重新运行脚本。服务同步新凭据后不需要重启。

## WPS 空间文件夹

选择多个空间或 `all` 后，根目录结构类似：

```text
/
├── 学校云盘/
├── 个人团队/
└── 自动备份/
```

这些空间文件夹是适配器提供的虚拟入口，不会在 WPS 中创建同名文件夹。空间内部的文件仍直接来自对应 WPS 空间。

跨空间 `MOVE` 和 `COPY` 会被拒绝，避免误操作。旧版只保存一个 `group_id/root_id` 的工作区配置仍然可以读取。

## 检查服务状态

健康检查只检查适配器进程是否运行，不代表 WPS 已登录：

```bash
curl 'http://<VPS-IP>:54321/healthz'
```

检查 WPS 会话：

```bash
curl -u <用户名> 'http://<VPS-IP>:54321/api/v1/status'
```

正常会返回 `connected`。如果返回 `not_configured`、`session_expired` 或 `permission_denied`，重新运行 `wps_login.py` 即可。

服务遇到 WPS `401` 时，会尝试使用保存的 `rtk` 自动续期会话。WPS 撤销刷新凭据或改变登录策略时，仍需要重新登录。

## 卸载

默认卸载服务和程序，但保留本机配置、Basic Auth、Cookie 和工作区文件：

```bash
curl -fL --progress-bar --connect-timeout 10 --max-time 60 --retry 1 'https://raw.githubusercontent.com/galiandan/WPS_2_WebDAV/main/scripts/uninstall.sh' | sudo bash -s --
```

连同本机配置和凭据一起删除：

```bash
curl -fL --progress-bar --connect-timeout 10 --max-time 60 --retry 1 'https://raw.githubusercontent.com/galiandan/WPS_2_WebDAV/main/scripts/uninstall.sh' | sudo bash -s -- --purge
```

Docker 镜像需要额外添加 `--remove-image`。卸载不会删除 WPS 云盘中的远端文件，也不会删除 Docker 软件本身。

## 安全说明

本项目只适用于你自己的 WPS 账号和你有权限访问的数据，不绕过权限、SSO、验证码、风控或租户隔离。

以下内容不能提交到 GitHub、Issue、聊天或日志：

- WPS Cookie、`rtk`、CSRF 和 refresh token；
- WebDAV/网页 Basic Auth 密码；
- 签名对象存储 URL；
- 原始 HAR、PCAP 和真实文件内容。

服务默认限制上传并发、下载并发、目录递归深度、目录条目数、响应大小和上传临时磁盘占用，以适配低配 VPS。上传和下载尽量流经 WPS，不长期保存文件正文。

## 当前限制

- WPS 私有接口可能变化，项目不承诺长期兼容。
- 上传请求需要 `Content-Length`，暂不接受 HTTP chunked request body。
- 大文件失败后会在当前请求内有限重试；跨进程断点恢复仍属于实验性能力。
- 文件夹 `COPY` 使用 VPS 流式中继，目标已存在时不会覆盖；单文件同名复制可使用 WPS 原生 COPY。
- `LOCK` 是当前进程内的兼容锁，服务重启后失效。
- 多空间之间暂不支持移动和复制。

## 高级文档

- [`docs/login.md`](docs/login.md)：登录助手和凭据同步
- [`docs/deployment.md`](docs/deployment.md)：升级、回滚和服务管理
- [`docs/api.md`](docs/api.md)：REST、WebDAV 和状态码
- [`docs/integration.md`](docs/integration.md)：Windows、NAS 和验收
- [`docs/architecture.md`](docs/architecture.md)：组件和数据流
- [`docs/research/`](docs/research/)：脱敏抓包记录和实验边界
- [`SECURITY.md`](SECURITY.md)：安全问题报告

## 开发测试

项目运行时不依赖第三方 Python 包。开发环境需要 Python `3.11+`：

```bash
PYTHONPATH=src python3 -m unittest discover -s tests -v
python3 -m compileall -q src tests
python3 tools/build_login_script.py --check
python3 tools/build_release_manifest.py --check
git diff --check
```

真实 WPS 实验必须使用专用测试目录，原始抓包和真实文件不要提交。

## License

本项目采用 [GNU General Public License v3.0 or later](LICENSE) 发布。WPS 商标、服务和接口归其各自权利人所有；本项目不代表 WPS 官方立场。
