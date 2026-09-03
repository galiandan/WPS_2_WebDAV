# WPS Enterprise Cloud Adapter

一个面向**已授权 WPS 企业云盘账号**的实验性 WebDAV / REST 适配器：把 WPS 云盘接入 Windows、Linux、手机、NAS 和其他支持 WebDAV 的客户端。

```text
WPS Enterprise Drive -> Adapter -> WebDAV / REST -> client applications
```

> 当前版本为 `0.4.0` 原型。WPS 相关接口不是公开稳定契约，升级前请先在自己的测试目录验证。项目只适用于你本人正常拥有权限的数据，不绕过权限、验证码、SSO、风控或租户隔离。

## Features

- WebDAV：`PROPFIND`、`GET`、`HEAD`、`PUT`、`MKCOL`、`DELETE`、`MOVE`、`COPY`、`LOCK`、`UNLOCK`。
- REST：列目录、读取元数据、上传、下载、创建文件夹、删除、重命名和移动。
- 流式下载、单范围 `Range` 下载、上传并发限制和临时空间保护。
- 普通上传、覆盖更新和基于已观察 WPS 流程的大文件分片上传。
- 适配器层的递归 `PROPFIND`、`COPY` 和短期写锁兼容能力。
- Cookie/CSRF 文件动态读取；上游 `401` 时按已确认的 WPS SDK `grant_token` 流程尝试续期。
- 本地隔离 Chrome 登录助手：在官方 WPS 页面登录后，通过 SSH 将会话安全同步到 VPS。
- 仅依赖 Python 标准库；不需要 Docker、Playwright 或浏览器插件。
- 同源浏览器文件管理页面，入口为服务根路径 `/`。

## Important security notes

- 这是第三方实验性适配器，不是 WPS 官方客户端或官方 SDK。
- 只在自己的账号、企业空间和测试文件上使用；不要扫描 ID、重放他人请求或扩大权限。
- Cookie、`rtk`、CSRF、refresh token、签名 URL、Basic Auth 密码和原始 HAR 都属于敏感信息，不能提交到 GitHub、Issue、聊天或日志。
- 生产环境不要直接用 HTTP 暴露公网。请在 HTTPS 反向代理后使用，并关闭代理访问日志中的认证信息。
- 当前 systemd 示例以 root 运行以简化部署；正式环境建议改为专用低权限用户，并仅允许其访问 secret 目录。

## Requirements

- Python `3.11+`。
- 运行服务只需要 Python 标准库。
- 使用自动登录助手时，需要本机已有 Chrome/Chromium 和可用的 SSH 客户端。
- WPS 企业云盘账号需要已经能在官方网页端正常登录和操作目标文件。

## Quick start

### 1. Get the source

```bash
git clone https://github.com/galiandan/WPS_2_WebDAV.git
cd WPS_2_WebDAV
```

项目不要求安装第三方 Python 包。所有命令都可以通过 `PYTHONPATH=src` 直接运行；也可以按标准 Python 包方式执行 `python3 -m pip install -e .`。

### 2. Create configuration

```bash
cp .env.example .env
```

编辑 `.env`，至少设置：

```dotenv
WPS_GROUP_ID=your-enterprise-group-id
WPS_ROOT_ID=0
WPS_COOKIE_FILE=/etc/wps-adapter/secrets/wps-cookie
WPS_CSRF_TOKEN_FILE=/etc/wps-adapter/secrets/wps-csrf
ADAPTER_USERNAME_FILE=/etc/wps-adapter/secrets/adapter-username
ADAPTER_PASSWORD_FILE=/etc/wps-adapter/secrets/adapter-password
```

`WPS_GROUP_ID` 和 `WPS_ROOT_ID` 是你自己账号上下文中的标识，不要从别人请求中复制。secret 文件必须由管理员创建并设置为 `0600`；不要把凭据写入 `.env`、命令行或仓库。

### 3. Check and run locally

```bash
set -a
. ./.env
set +a
PYTHONPATH=src python3 -m wps_adapter check-config
PYTHONPATH=src python3 -m wps_adapter serve --bind 127.0.0.1 --port 54321
```

服务入口：

```text
Web UI:     http://127.0.0.1:54321/
WebDAV:     http://127.0.0.1:54321/dav/
REST:       http://127.0.0.1:54321/api/v1/
Health:     http://127.0.0.1:54321/healthz
```

### 4. Bootstrap WPS login

适配器网页不能读取另一个域名的 HttpOnly Cookie，所以登录助手必须在账号所有者自己的电脑上运行。它会打开一个临时隔离的 Chrome 窗口；你只在官方 WPS 页面登录，看到云盘页面后回到终端按回车。

```bash
PYTHONPATH=src python3 -m wps_adapter login \
  --ssh-target root@your-vps-host \
  --ssh-identity ~/.ssh/id_ed25519
```

助手只选取匹配 WPS 云盘域名的 Cookie，要求存在 `rtk` 和 `csrf`，不显示 Cookie 值，并通过 SSH 标准输入写入远端 secret 文件。登录结束后临时浏览器配置会删除。完整步骤见 [`docs/login.md`](docs/login.md)。

### 5. Try the API

```bash
curl -u your-adapter-user \
  'http://127.0.0.1:54321/api/v1/entries?path=/'
```

curl 会提示输入适配器密码。不要把密码写进命令或提交到配置文件。

## WebDAV clients

WebDAV 根地址是：

```text
http(s)://your-host:54321/dav/
```

使用适配器 Basic Auth 登录。桌面同步软件、NAS、文件管理器和 Office 客户端的具体配置方式不同；先用 `PROPFIND`、小文件上传和小文件下载完成验收，再接入自动同步。

REST 入口适合脚本和自定义客户端：

```text
GET    /api/v1/entries?path=/
GET    /api/v1/metadata?path=/folder/file.txt
GET    /api/v1/download?path=/folder/file.txt
PUT    /api/v1/upload?path=/folder/file.txt
POST   /api/v1/folders?path=/folder
PATCH  /api/v1/entries?path=/folder/file.txt
DELETE /api/v1/entries?path=/folder/file.txt
```

接口细节、状态码和示例见 [`docs/api.md`](docs/api.md)。

## Documentation

- [`docs/architecture.md`](docs/architecture.md)：组件、请求流和资源模型。
- [`docs/integration.md`](docs/integration.md)：WebDAV、REST、客户端和运维验收。
- [`docs/deployment.md`](docs/deployment.md)：systemd、secret 文件和升级步骤。
- [`docs/login.md`](docs/login.md)：使用官方 WPS 页面建立并同步会话。
- [`docs/research/`](docs/research/)：抓包方案、脱敏实验事实和研究边界。

## Configuration

`.env.example` 是完整模板。常用参数如下：

| Variable | Purpose |
| --- | --- |
| `WPS_GROUP_ID` | WPS 企业空间/群组标识 |
| `WPS_ROOT_ID` | 适配器映射的根文件夹，`0` 表示尝试空间根目录 |
| `WPS_COOKIE_FILE` | 完整 WPS Cookie 文件 |
| `WPS_CSRF_TOKEN_FILE` | CSRF Cookie 值文件 |
| `WPS_AUTO_REFRESH` | 是否在上游 `401` 后尝试自动续期 |
| `WPS_MULTIPART_THRESHOLD` | 进入分片上传的文件大小阈值 |
| `WPS_MAX_UPLOADS` / `WPS_MAX_DOWNLOADS` | 同时传输数量上限 |
| `WPS_UPLOAD_MIN_FREE_BYTES` | 临时上传文件系统的最小保留空间 |
| `ADAPTER_BIND` / `ADAPTER_PORT` | 服务监听地址和端口 |
| `ADAPTER_USERNAME_FILE` / `ADAPTER_PASSWORD_FILE` | 适配器 Basic Auth 文件 |

不要把 `WPS_COOKIE`、`WPS_CSRF_TOKEN`、`ADAPTER_PASSWORD` 等秘密值放在环境变量或 shell 历史中；优先使用权限为 `0600` 的文件。

## Deployment

项目包含不依赖 Docker 的 systemd 模板：

```text
deploy/wps-adapter.service
deploy/wps-adapter-hardening.conf
deploy/wps-adapter-hardening.env
```

通用 VPS 安装和升级步骤见 [`docs/deployment.md`](docs/deployment.md)。部署前先完成本地测试，并使用 HTTPS 反向代理保护公网流量。

## Current limitations

- WPS 接口随时可能变化，项目不会把未观察到的接口当成事实。
- 适配器的 `COPY` 是下载/上传中继，不代表 WPS 存在服务端 COPY API。
- `LOCK` 是进程内短期兼容锁，服务重启后消失，不是 WPS 远端锁。
- 失败后的跨进程分片续传、取消/清理、快速上传成功路径和部分跨目录改名场景仍未确认。
- 服务器本身不会自动填写 WPS 密码，也不会处理 SSO、验证码或风控；需要重新登录时使用本地登录助手。

## Development

运行测试和静态检查：

```bash
PYTHONPATH=src python3 -m unittest discover -s tests -v
python3 -m compileall -q src tests
git diff --check
```

测试默认不访问 WPS。涉及真实请求的实验必须只使用自己的测试目录，并把脱敏后的结论记录到 [`docs/research/findings.md`](docs/research/findings.md)。原始 HAR 放在本地 `captures/`，该目录已被 Git 忽略。

项目结构：

```text
src/wps_adapter/       核心客户端、存储、WebDAV/REST 服务和登录助手
tests/                 标准库单元测试
tools/                 HAR 摘要和只读探针
deploy/                systemd 与资源保护模板
docs/                  API、架构、部署、登录、集成和研究记录
docs/research/         抓包方案、实验事实、范围约束和请求模板
.github/workflows/     GitHub Actions 测试
```

贡献方式见 [`CONTRIBUTING.md`](CONTRIBUTING.md)，安全问题见 [`SECURITY.md`](SECURITY.md)，版本变化见 [`CHANGELOG.md`](CHANGELOG.md)。

## License

本项目以 [MIT License](LICENSE) 发布。WPS 商标、服务和接口归其各自权利人所有；本项目不代表 WPS 官方立场。
