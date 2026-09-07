# WPS 2 WebDAV

把你有权访问的 WPS 企业云盘接入 WebDAV，同时提供一个无需额外前端依赖的网页文件管理器和 REST 接口。

~~~text
WPS 企业云盘 -> Go 适配器 -> 网页 / WebDAV / REST
~~~

当前长期运行服务是 Go 单二进制。项目仍是实验性适配器，不是 WPS 官方软件。它只适用于你自己的账号和你有权限访问的数据。

## 你能得到什么

- 浏览器网页：浏览、搜索、排序、列表/网格视图、拖放上传、上传速度、下载、新建文件夹、重命名、移动、复制和删除。
- WebDAV：Windows、Linux、macOS、手机、NAS、同步软件和其他 WebDAV 客户端。
- REST：脚本化列目录、上传、下载、创建文件夹、重命名、移动、复制、删除和状态检查。
- 多 WPS 空间：登录后按实时显示的空间名称选择一个、多个或全部，空间会显示为根目录下的文件夹。
- 资源保护：上传/下载并发、临时磁盘、目录递归、响应大小和大目录读取都有上限。
- 不需要浏览器扩展；登录只需要一个独立的 wps_login.py 文件。

## 三步开始

### 1. 在 Linux VPS 安装

先准备一台能访问 WPS 的 Linux VPS，并放行你要使用的端口。下面以 54321 为例，端口可以替换成任意未占用端口。

Native（推荐，运行时不需要 Docker）：

~~~bash
set -o pipefail; curl -fL --progress-bar --connect-timeout 10 --max-time 120 'https://ghfast.top/https://raw.githubusercontent.com/galiandan/WPS_2_WebDAV/main/scripts/install-native.sh' | sudo bash -s -- --port 54321
~~~

Docker：

~~~bash
set -o pipefail; curl -fL --progress-bar --connect-timeout 10 --max-time 120 'https://ghfast.top/https://raw.githubusercontent.com/galiandan/WPS_2_WebDAV/main/scripts/install-docker.sh' | sudo bash -s -- --port 54321
~~~

安装器会显示阶段进度和下载进度，并在首次安装时询问网页/WebDAV 共用的 Basic Auth 用户名和密码。密码不会显示，请记住它。

Native 安装器不要求 VPS 预装 Go：如果系统没有 Go 1.25+，它会从国内 Go 镜像临时下载固定版本、编译静态二进制，安装完成后删除临时工具链。服务运行时只使用编译好的 Go 二进制，不需要 Python、Node.js 或 Go 运行时。

Docker 安装器会从国内 Docker 镜像获取 Go 构建镜像，最终容器只包含服务二进制和 CA 证书；你的个人电脑不需要安装 Docker。

安装器默认只访问命令中显示的国内加速地址；网络不可用时不会静默切换到其他地址。也不会执行项目归档或 Go 工具链的哈希校验。不要把未知网页中的安装命令直接交给 root。

安装完成后会打印实际端口、网页地址和 WebDAV 地址。服务默认使用执行 sudo 的当前用户运行，不会强制创建名为 wps-adapter 的 Linux 用户。

### 2. 在自己的电脑登录 WPS

只下载一个登录脚本，不需要 clone 整个项目：

~~~bash
curl -fL --progress-bar --connect-timeout 10 --max-time 120 'https://ghfast.top/https://raw.githubusercontent.com/galiandan/WPS_2_WebDAV/main/wps_login.py' -o wps_login.py && python3 wps_login.py
~~~

电脑需要 Python 3.11+、Chrome 或 Chromium。脚本会询问：

1. VPS 地址或域名。
2. 连接方式：SSH 私钥、SSH 密码，或 HTTP/HTTPS 适配器接口。
3. SSH 用户、端口、私钥路径，或适配器端口和 Basic Auth 信息。

随后脚本打开临时隔离的官方 WPS 登录窗口。只在这个官方窗口中完成登录、SSO、扫码、验证码或二次验证。登录完成后：

1. 脚本自动读取当前账号能看到的全部 WPS 空间。
2. 浏览器自动关闭，回到终端选择空间；输入序号选择一个，输入 1,3 选择多个，输入 all 选择全部。
3. 脚本自动验证所选空间并把 Cookie、CSRF 和工作区配置安全同步到 VPS。

WPS 登录后自动恢复的旧文件夹不会被误当成目标目录。默认使用企业云盘空间根目录；只有明确使用 --workspace-url 时才会指定某个具体文件夹。脚本不会显示 Cookie、CSRF、密码或签名 URL，也不需要手动填写企业 ID、群组 ID 或文件夹 ID。

如果使用 HTTP 同步，脚本会要求明确确认风险，因为 HTTP 会明文传输凭据和文件内容。公网使用建议给适配器套 HTTPS 反向代理；没有域名和证书时，个人可信网络可以暂时使用 HTTP。

### 3. 打开网页或连接 WebDAV

浏览器：

~~~text
http://<VPS地址>:54321/
~~~

WebDAV：

~~~text
http://<VPS地址>:54321/dav/
~~~

用户名和密码就是安装时设置的 Basic Auth。自定义端口时，把地址中的 54321 换成实际端口。服务显示 WPS 未连接 时，表示适配器进程正常但 WPS 凭据尚未同步、已过期或当前空间无权访问；重新运行 wps_login.py 即可。

## 登录助手的三种同步方式

通常直接运行 python3 wps_login.py，按提示选择即可。也可以明确指定 HTTP/HTTPS：

~~~bash
python3 wps_login.py --adapter-url https://<VPS地址或域名> --adapter-port 54321 --adapter-user <Basic Auth用户名>
~~~

没有 HTTPS 时：

~~~bash
python3 wps_login.py --adapter-url http://<VPS地址> --adapter-port 54321 --adapter-user <Basic Auth用户名> --allow-http
~~~

SSH 私钥和 SSH 密码方式会把凭据写入 /etc/wps-adapter/secrets/，HTTP/HTTPS 方式调用受 Basic Auth 保护的 session import 接口。同步成功后不需要重启服务。

## 服务状态和常用检查

健康检查只表示 Go 进程正在运行，不代表 WPS 已登录：

~~~bash
curl 'http://<VPS地址>:54321/healthz'
~~~

查看 WPS 会话状态：

~~~bash
curl -u <Basic Auth用户名> 'http://<VPS地址>:54321/api/v1/status'
~~~

常见状态：

- connected：WPS 凭据和目标空间可访问。
- not_configured：还没有同步 Cookie、CSRF 或工作区配置。
- session_expired：会话过期，重新运行登录助手。
- permission_denied：当前账号无法访问所选空间或根目录。
- upstream_unavailable：WPS 或对象存储暂时不可达。

服务遇到 WPS 401 时会尝试已确认的 rtk/grant_token 自动续期流程并重试一次。WPS 撤销刷新凭据或改变登录策略后，仍需重新登录。

## HTTP、HTTPS 和端口

适配器支持 HTTP，适合没有域名和证书的个人环境；但 HTTP 会明文传输 Basic Auth、WPS 会话和文件内容，不适合直接暴露到公网。

有域名时，建议使用 Caddy、Nginx 或其他反向代理提供 HTTPS，并把请求转发到 http://127.0.0.1:<端口>。WebDAV 客户端使用：

~~~text
https://<你的域名>/dav/
~~~

安装器支持自定义端口。例如：

~~~bash
set -o pipefail; curl -fL --progress-bar --connect-timeout 10 --max-time 120 'https://ghfast.top/https://raw.githubusercontent.com/galiandan/WPS_2_WebDAV/main/scripts/install-native.sh' | sudo bash -s -- --port 18080
~~~

登录助手中的端口也必须填写 18080，不能继续使用默认的 54321。

## 手工运行和开发

生产服务代码在 go/；前端是 Go embed 的原生 HTML、CSS 和 JavaScript，没有 Node.js 构建步骤或第三方前端依赖。

本机开发需要 Go 1.25+。国内 Go 模块镜像：

~~~bash
export GOPROXY=https://goproxy.cn,direct
~~~

构建和测试：

~~~bash
cd go
gofmt -w .
go test ./...
go vet ./...
CGO_ENABLED=0 go build -trimpath -o /tmp/wps-adapter ./cmd/wps-adapter
~~~

如果需要从源码包开始，使用国内 GitHub 加速地址下载 main 源码归档，再按 go/README.md 构建。普通用户不需要执行这些步骤。

## 卸载

卸载脚本会自动识别 Native 和 Docker，删除服务、程序、本机配置、Basic Auth、Cookie、CSRF 和工作区文件：

~~~bash
set -o pipefail; curl -fL --progress-bar --connect-timeout 10 --max-time 120 'https://ghfast.top/https://raw.githubusercontent.com/galiandan/WPS_2_WebDAV/main/scripts/uninstall.sh' | sudo bash -s --
~~~

如果还安装过 Docker，并希望同时删除本项目镜像：

~~~bash
set -o pipefail; curl -fL --progress-bar --connect-timeout 10 --max-time 120 'https://ghfast.top/https://raw.githubusercontent.com/galiandan/WPS_2_WebDAV/main/scripts/uninstall.sh' | sudo bash -s -- --remove-image
~~~

卸载不会删除 Docker 软件，也不会删除 WPS 云盘中的远端文件。没有 Docker 时会自动跳过容器和镜像清理。脚本会要求输入 YES；自动化场景可额外添加 --yes。

## 项目目录

~~~text
go/              Go 生产服务、嵌入式网页和 Go 测试
scripts/         Native、Docker 和卸载脚本
deploy/          systemd 加固文件和 Docker 构建配置
wps_login.py     独立 WPS 登录/凭据同步助手
docs/            使用、接口、架构、部署和研究文档
contract_tests/  脱敏 JSON 契约金标准，供 Go 回归测试读取
~~~

## 当前限制

- WPS 私有接口可能变化，项目不承诺长期兼容。
- 上传请求需要 Content-Length，暂不接受 HTTP chunked request body。
- 大文件失败后会在当前请求内有限重试；跨进程断点恢复仍属于实验性能力。
- 文件夹 COPY 使用流式中继；LOCK 是当前进程内的兼容锁，服务重启后失效。
- 多空间之间暂不支持移动和复制。
- 递归 PROPFIND、递归 COPY、上传和下载都受深度、条目、并发和磁盘预算限制。

## 安全

不要把以下内容提交到 GitHub、Issue、聊天或日志：

- WPS Cookie、rtk、CSRF、refresh token；
- WebDAV/网页 Basic Auth 密码；
- 签名对象存储 URL；
- 原始 HAR、PCAP 和真实文件内容。

详见 SECURITY.md、docs/login.md 和 docs/deployment.md。项目采用 GNU GPL v3，见 LICENSE。
