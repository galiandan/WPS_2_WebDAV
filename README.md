# WPS 2 WebDAV

把你自己的 WPS 企业云盘映射成 WebDAV 和 REST API，让 Windows、Linux、手机、NAS 以及脚本工具都可以访问同一份文件。

```text
WPS 企业云盘 -> WPS 2 WebDAV -> WebDAV / REST / 网页
```

这是一个实验性开源项目，不是 WPS 官方客户端。WPS 相关接口不是公开稳定 API，项目只复现已经在本人账号上观察并验证过的操作。

## 先看这里

- 只用于你自己的 WPS 账号和你有权限访问的企业空间。
- 不绕过权限、验证码、SSO、风控或租户隔离。
- Cookie、`rtk`、CSRF、Basic Auth 密码、签名 URL 和原始 HAR 都是敏感信息，不能提交到 GitHub、Issue、聊天或日志。
- 服务器不长期保存文件。上传超过内存阈值时会使用临时文件，上传完成或失败后删除；因此仍然需要预留临时磁盘空间。
- 公网部署建议使用 HTTPS 反向代理。没有域名或证书时也可以直接使用 HTTP，但 Cookie、Basic Auth 和文件内容都会明文传输，只适合可信网络。

当前版本：`0.7.0`（原型阶段）

## 能做什么

### 文件操作

- 列目录和浏览文件夹
- 创建文件夹
- 上传、覆盖上传和下载
- 删除、重命名和移动
- 大文件分片上传
- 流式下载和单范围 `Range` 下载

### 接入方式

- WebDAV：供系统文件管理器、NAS、同步工具和 Office 客户端使用
- REST：供脚本和自定义客户端使用
- 网页文件管理器：打开服务根路径即可使用
- 适配器 Basic Auth：保护 WebDAV、REST 和网页
- WPS 会话自动续期：上游返回 `401` 时，使用保存的 `rtk` 按已验证流程尝试刷新

### WebDAV 兼容能力

已实现 `PROPFIND`、`GET`、`HEAD`、`PUT`、`MKCOL`、`DELETE`、`MOVE`、`COPY`、`LOCK` 和 `UNLOCK`。

其中：

- `COPY` 是适配器的下载/上传中继，不是 WPS 的服务端复制接口。
- `LOCK` 是当前适配器进程内的短期兼容锁，服务重启后消失。
- `Depth: infinity` 有条目数和深度上限，避免低配 VPS 被递归请求耗尽资源。

## 需要什么

### VPS

- Debian 或 Ubuntu 系统更容易使用一键安装脚本
- root 或可以执行 `sudo` 的账号
- 一个已经能正常访问 WPS 企业云盘的账号
- 一个未被占用的对外端口

### 你自己的电脑

只有在使用登录助手时才需要：

- Python `3.11+`
- Chrome 或 Chromium
- SSH 登录方式需要系统自带的 `ssh` 命令；HTTP/HTTPS 方式不需要 SSH

服务端只使用 Python 标准库，不需要安装第三方 Python 包、Playwright 或浏览器扩展。

## 最快部署

下面两种方式选一种即可，不要同时让 Native 和 Docker 占用同一个端口。

安装脚本会询问：

1. WPS 企业群组 ID
2. 适配器 Basic Auth 用户名
3. 适配器 Basic Auth 密码
4. 监听端口

`[]` 中的内容是默认值，直接按回车即可使用。默认服务运行用户是执行 `sudo` 的当前用户；如果直接以 root 执行，运行用户就是 root。

### 方式一：Native systemd

适合不想使用 Docker 的 VPS：

```bash
curl -fsSL https://raw.githubusercontent.com/galiandan/WPS_2_WebDAV/main/scripts/install-native.sh \
  | sudo bash -s -- --port 54321
```

自定义端口，例如 `18080`：

```bash
curl -fsSL https://raw.githubusercontent.com/galiandan/WPS_2_WebDAV/main/scripts/install-native.sh \
  | sudo bash -s -- --port 18080
```

### 方式二：Docker

适合已经使用 Docker 的 VPS。Debian/Ubuntu 上如果没有 Docker，脚本会尝试安装 `docker.io`：

```bash
curl -fsSL https://raw.githubusercontent.com/galiandan/WPS_2_WebDAV/main/scripts/install-docker.sh \
  | sudo bash -s -- --port 54321
```

如果当前正在运行 Native 服务，切换到 Docker 时明确加上：

```bash
curl -fsSL https://raw.githubusercontent.com/galiandan/WPS_2_WebDAV/main/scripts/install-docker.sh \
  | sudo bash -s -- --port 54321 --replace-native
```

两种安装器都会保留 `/etc/wps-adapter/secrets/` 中的凭据。Docker 安装器在构建或健康检查失败时，会尝试恢复原来的服务或容器。

> 一键脚本会从 GitHub 下载当前 `main` 分支代码并以 root 权限安装。生产环境执行前建议先打开脚本检查内容。若仓库是 Private，未获授权的用户无法访问 GitHub Raw 地址。

安装完成后，记下安装器显示的地址。以端口 `54321` 为例：

```text
网页：   http://<VPS-IP>:54321/
WebDAV： http://<VPS-IP>:54321/dav/
REST：   http://<VPS-IP>:54321/api/v1/
健康检查：http://<VPS-IP>:54321/healthz
```

还要在 VPS 防火墙和云平台安全组中放行你实际使用的端口。

## 第一次登录 WPS

安装器只负责部署服务，不会替你登录 WPS。登录助手必须在你自己的电脑上运行，因为 WPS 登录 Cookie 不能由适配器网页跨域读取。

登录助手是一个独立的单文件脚本，不需要 clone 整个项目。直接下载这一个文件：

```bash
curl -fsSLo wps_login.py \
  https://raw.githubusercontent.com/galiandan/WPS_2_WebDAV/main/wps_login.py
```

然后运行向导：

```bash
python3 wps_login.py
```

如果你已经 clone 了项目，也可以直接运行仓库根目录中的同名文件。登录助手只依赖本机 Python 和 Chrome/Chromium，不需要安装项目本身。

向导会依次询问 VPS 地址和连接方式。共有三种方式：

1. `SSH 私钥`：适合使用 SSH 密钥登录 VPS。
2. `SSH 密码`：适合没有 SSH 密钥的用户，密码由系统 `ssh` 命令隐藏询问。
3. `HTTP/HTTPS 适配器接口`：直接把登录凭据同步到适配器，不需要 SSH。

脚本随后会打开一个临时隔离的 Chrome/Chromium 窗口：

1. 只在官方 WPS 页面登录自己的账号。
2. 正常完成学校 SSO、扫码、验证码或二次验证。
3. 登录完成后脚本自动检测，不需要回终端按回车。
4. 脚本只保留匹配 WPS 云盘域名的 Cookie，并要求存在 `rtk` 和 `csrf`。
5. 凭据写入 VPS 后，适配器无需重启。

脚本不会显示 Cookie 值，也不会把 Cookie 放入命令参数、URL 或日志。临时浏览器配置会在流程结束后删除。

### HTTPS 同步

有域名和 HTTPS 反向代理时：

```bash
python3 wps_login.py \
  --adapter-url https://<你的域名> \
  --adapter-port 54321 \
  --adapter-user <适配器用户名>
```

### HTTP 同步

没有域名或证书时，可以使用 VPS IP 和 HTTP：

```bash
python3 wps_login.py \
  --adapter-url http://<VPS-IP>:54321 \
  --adapter-user <适配器用户名> \
  --allow-http
```

HTTP 会明文传输 WPS Cookie 和适配器密码。只在可信网络使用，或者改用 SSH 同步。

### SSH 同步

SSH 私钥：

```bash
python3 wps_login.py \
  --ssh-target <VPS用户>@<VPS-IP> \
  --ssh-identity ~/.ssh/id_ed25519
```

SSH 密码：

```bash
python3 wps_login.py \
  --ssh-target <VPS用户>@<VPS-IP> \
  --ssh-password-auth
```

## 验证是否成功

先列出根目录。`curl` 会提示输入适配器 Basic Auth 密码：

```bash
curl -u <适配器用户名> \
  'http://<VPS-IP>:54321/api/v1/entries?path=/'
```

上传、下载并校验一个测试文件：

```bash
printf 'WPS adapter smoke test\n' > /tmp/wps-adapter-smoke.txt

curl -u <适配器用户名> \
  -H 'Content-Type: text/plain' \
  --upload-file /tmp/wps-adapter-smoke.txt \
  'http://<VPS-IP>:54321/api/v1/upload?path=/adapter-smoke.txt'

curl -u <适配器用户名> \
  -o /tmp/wps-adapter-downloaded.txt \
  'http://<VPS-IP>:54321/api/v1/download?path=/adapter-smoke.txt'

cmp /tmp/wps-adapter-smoke.txt /tmp/wps-adapter-downloaded.txt \
  && echo '上传下载内容一致'
```

如果返回 WPS `401`，服务会自动尝试刷新会话；如果 `rtk` 已失效，重新运行 `wps_login.py` 即可。更新凭据后不需要重启服务。

## 使用网页

浏览器打开：

```text
http://<VPS-IP>:<端口>/
```

输入适配器 Basic Auth 后，可以：

- 浏览目录和进入文件夹
- 拖动文件到页面任意位置上传
- 查看上传进度和速度
- 创建文件夹
- 下载、重命名、移动和删除文件

网页和 WebDAV/REST 使用同一套适配器账号密码。

## 使用 WebDAV

WebDAV 地址为：

```text
http(s)://<服务器地址>:<端口>/dav/
```

在 Windows、Linux、手机或 NAS 客户端中：

1. 类型选择 `WebDAV`。
2. 地址填写上面的 `/dav/` 地址。
3. 用户名和密码填写适配器 Basic Auth。
4. 先连接测试目录，再接入正式文件。

常用 WebDAV 方法：

| 方法 | 作用 |
| --- | --- |
| `PROPFIND` | 列目录和读取属性 |
| `GET` / `HEAD` | 下载文件和读取元数据 |
| `PUT` | 上传或覆盖文件 |
| `MKCOL` | 创建文件夹 |
| `DELETE` | 删除文件或文件夹 |
| `MOVE` | 重命名或移动 |
| `COPY` | 复制文件或文件夹 |
| `LOCK` / `UNLOCK` | 适配器进程内的写锁 |

## 使用 REST API

所有 `path` 都是以 `/` 开头的远端路径，调用时请进行 URL 编码：

```text
GET    /api/v1/entries?path=/
GET    /api/v1/metadata?path=/folder/file.txt
GET    /api/v1/download?path=/folder/file.txt
PUT    /api/v1/upload?path=/folder/file.txt
POST   /api/v1/folders?path=/folder
PATCH  /api/v1/entries?path=/folder/file.txt
DELETE /api/v1/entries?path=/folder/file.txt
```

重命名：

```bash
curl -u <适配器用户名> \
  -X PATCH \
  -H 'Content-Type: application/json' \
  --data '{"name":"new-name.txt"}' \
  'http://<VPS-IP>:54321/api/v1/entries?path=/old-name.txt'
```

移动到另一个目录并保留原文件名：

```bash
curl -u <适配器用户名> \
  -X PATCH \
  -H 'Content-Type: application/json' \
  --data '{"parent_path":"/target-folder"}' \
  'http://<VPS-IP>:54321/api/v1/entries?path=/old-folder/file.txt'
```

完整接口、请求体和状态码见 [`docs/api.md`](docs/api.md)。

## 配置

一键安装器会自动生成 `/etc/wps-adapter/wps-adapter.env` 和以下 secret 文件：

```text
/etc/wps-adapter/secrets/wps-cookie
/etc/wps-adapter/secrets/wps-csrf
/etc/wps-adapter/secrets/adapter-username
/etc/wps-adapter/secrets/adapter-password
```

常用配置：

| 变量 | 作用 | 默认值 |
| --- | --- | --- |
| `WPS_GROUP_ID` | WPS 企业群组 ID | 无，必须填写 |
| `WPS_ROOT_ID` | 映射到适配器的根文件夹 ID | `0` |
| `WPS_COOKIE_FILE` | WPS Cookie 文件 | `/etc/wps-adapter/secrets/wps-cookie` |
| `WPS_CSRF_TOKEN_FILE` | CSRF 文件 | `/etc/wps-adapter/secrets/wps-csrf` |
| `WPS_AUTO_REFRESH` | 是否在 `401` 后尝试续期 | `true` |
| `ADAPTER_BIND` | 监听地址 | 安装器默认 `0.0.0.0` |
| `ADAPTER_PORT` | 监听端口 | `54321` |
| `ADAPTER_USERNAME_FILE` | 适配器用户名文件 | `/etc/wps-adapter/secrets/adapter-username` |
| `ADAPTER_PASSWORD_FILE` | 适配器密码文件 | `/etc/wps-adapter/secrets/adapter-password` |
| `WPS_MULTIPART_THRESHOLD` | 进入分片上传的大小阈值 | `50 MiB` |
| `WPS_MAX_UPLOADS` / `WPS_MAX_DOWNLOADS` | 并发传输上限 | `2` / `4` |

完整模板见 [`.env.example`](.env.example)。不要把 Cookie、CSRF 或密码直接写入公开配置、命令行或 shell 历史。

## 安全部署建议

- 公网优先使用 HTTPS 反向代理，把适配器绑定到 `127.0.0.1`。
- 如果必须直接使用 HTTP，至少使用强 Basic Auth，并限制云平台安全组和防火墙来源 IP。
- 不要把 `/healthz` 当作 WPS 登录成功证明；它只检查适配器进程是否运行。
- 保持四个 secret 文件为 `0600`，secret 目录为 `0700`。
- 不要在 Nginx、Caddy、systemd 或应用日志中记录 `Authorization`、Cookie、签名 URL 或请求体。
- 升级前先在自己的测试目录验证列表、上传、下载和删除。

## 当前限制

- WPS 私有接口可能随时变化，不能保证长期兼容。
- 上传请求需要 `Content-Length`，暂不接受 HTTP chunked request body。
- 大文件分片上传有有限重试，但进程退出后的跨进程续传、分片取消和清理尚未实现。
- `COPY` 会产生一次下载和一次上传，速度和临时空间取决于 VPS 与 WPS 的网络条件。
- `LOCK` 只在当前适配器进程内有效，不是 WPS 远端锁。
- 跨目录同时移动并重命名暂不支持。
- WPS 要求重新登录、撤销 `rtk` 或改变登录策略时，需要重新运行登录助手。

## 详细文档

- [`docs/deployment.md`](docs/deployment.md)：Native、Docker、systemd、升级和回滚
- [`docs/login.md`](docs/login.md)：登录助手、HTTP/HTTPS 同步和 SSH 备用方式
- [`docs/api.md`](docs/api.md)：WebDAV、REST、状态码和限制
- [`docs/integration.md`](docs/integration.md)：Windows、NAS、脚本和验收流程
- [`docs/architecture.md`](docs/architecture.md)：组件和数据流
- [`docs/research/`](docs/research/)：抓包计划、脱敏发现和研究边界
- [`SECURITY.md`](SECURITY.md)：安全问题报告方式

## 开发

项目不依赖第三方 Python 包。运行测试：

```bash
PYTHONPATH=src python3 -m unittest discover -s tests -v
python3 -m compileall -q src tests
git diff --check
```

真实 WPS 实验必须只使用自己的测试目录。原始 HAR 不要提交到仓库，脱敏后的事实记录在 [`docs/research/findings.md`](docs/research/findings.md)。

## License

本项目使用 [MIT License](LICENSE)。WPS 商标、服务和接口归其各自权利人所有；本项目不代表 WPS 官方立场。
