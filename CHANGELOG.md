# Changelog

本项目遵循 Keep a Changelog 风格。版本号用于记录适配器行为变化，不代表 WPS 官方兼容性承诺。

## [0.9.7] - 2026-09-04

### Added

- 网页右上角新增云盘名称设置入口，名称可在网页内修改并持久化到服务端。
- 新增 `GET/PATCH /api/v1/settings`，修改后的名称对网页、WebDAV 和 REST 根目录元数据统一生效。
- 对自定义名称进行 HTML 和 JavaScript 安全编码，避免配置值破坏网页。

## [0.9.6] - 2026-09-04

### Added

- 网页文件管理器读取 `WPS_ROOT_NAME`，支持自定义浏览器标题、品牌名和根目录显示名称。
- 对自定义名称进行 HTML 和 JavaScript 安全编码，避免配置值破坏网页。

## [0.9.5] - 2026-09-04

### Fixed

- 修复卸载脚本只接受 `/usr/bin/python3`、导致旧版 `/usr/bin/python3.11` 服务无法卸载的问题；现在会校验完整服务标识并兼容 Python 3.x 路径。

## [0.9.4] - 2026-09-04

### Added

- 新增统一的 Native/Docker 卸载脚本，默认保留配置和凭据，支持显式清理与 Docker 镜像删除。
- 重写项目 README，集中说明部署、登录、访问、卸载、安全边界和当前限制。

## [0.9.3] - 2026-09-04

### Changed

- 登录助手默认使用 WPS 企业云盘根目录，不再把登录后自动恢复的旧文件夹误保存为适配器根目录。
- 新增 `--workspace-url`，只有明确指定具体 WPS 文件夹地址时才使用该文件夹作为适配器根目录。

## [0.9.2] - 2026-09-04

### Added

- 安装器下载优先使用国内 GitHub 加速节点，失败后自动回退到 GitHub 直连，并为每个地址设置连接和总超时。
- Native 和 Docker 安装器识别常见 Linux 包管理器；Native 在无 systemd 主机上使用便携后台模式，Docker 支持 systemd、OpenRC 和 SysV service 启动 daemon。
- Docker 安装器优先尝试国内 Python 基础镜像，并支持 `WPS_ADAPTER_DOCKER_BASE_IMAGE` 自定义镜像。
- 安装器归档路径校验和非 GNU `find`/`sha256sum` 兼容处理，保留阶段进度和下载进度显示。

## [0.9.1] - 2026-09-04

### Added

- Native 和 Docker 安装器显示阶段进度，并在下载项目归档时显示 curl 进度条。
- Docker 镜像构建阶段保留逐层构建输出，便于判断长时间任务是否仍在进行。

## [0.9.0] - 2026-09-04

### Added

- 登录助手从当前官方 WPS 企业云盘页面自动识别群组 ID 和映射根目录。
- Cookie、CSRF 和工作区状态通过 HTTP/HTTPS、SSH 或本地输出一起同步。
- 工作区状态以权限受限的 JSON 文件持久化，服务运行中可自动切换根目录。
- Native 和 Docker 安装器默认使用 `WPS_GROUP_ID=auto`、`WPS_ROOT_ID=auto`，不再询问群组 ID。

## [0.8.1] - 2026-09-03

### Security

- 安装器校验固定提交归档的文件清单和 SHA-256，拒绝归档中的符号链接和额外文件。
- 默认单次上传上限为 1 GiB，并在读取请求体前检查已声明的大小。
- Docker 以可写目录配合只读 Basic Auth 文件挂载，支持 Cookie/CSRF 的同目录原子轮换。
- REST 重命名/移动会同时检查源和目标锁；并发 WPS 会话刷新会串行执行。
- 带有跨站 `Origin`/`Referer` 的写请求必须指向当前适配器主机，并限制登录同步响应大小。
- HAR 脱敏会移除签名 URL、对象/租户/设备标识和文件名等敏感字段。
- 上游 JSON、对象存储控制响应和 multipart XML 响应增加内存上限。
- 拒绝异常 HTTP 请求分帧后继续复用连接，并限制登录助手的 CDP 连接只能访问本机回环地址。
- 递归 COPY 失败时尽力删除本次新建的目标目录。

## [0.8.0] - 2026-09-03

### Security

- 安装器默认从固定的完整 Git 提交归档安装，避免直接信任可变分支。
- 原生和 Docker 安装失败时尽量恢复旧应用、配置、凭据权限和服务状态。
- SSH 登录助手只能写入 `/etc/wps-adapter/secrets/` 下的直接文件。
- 签名对象存储地址限制在 WPS 域名，下载请求不跟随重定向。
- 控制请求体、目录分页、客户端连接和上传临时磁盘均增加边界保护。
- MOVE/COPY 不再为实现覆盖而先删除已有目标，避免失败时静默丢失数据。
- 手动配置的凭据文件拒绝符号链接、宽权限、非普通文件和过大文件。

## [0.7.0] - 2026-09-03

### Changed

- `wps_login.py` 现在是可独立下载运行的单文件登录助手，获取 Cookie 不再需要 clone 整个项目。
- HTTP/HTTPS 登录同步均可用；远程 HTTP 默认要求确认或 `--allow-http`。
- Native 和 Docker 安装器默认使用执行 `sudo` 的当前用户，并统一保护凭证文件权限。
- Docker 安装器从正确的 `deploy/Dockerfile` 构建，切换前完成构建，失败时尝试恢复原服务/容器。
- Docker 容器增加项目归属标记、当前用户 UID/GID 和能力限制，不再强制删除未知同名容器。

## [0.6.0] - 2026-09-03

### Added

- 新增 GitHub Raw 原生 systemd 一键安装脚本。
- 新增 GitHub Raw Docker 一键安装脚本、Dockerfile 和 Compose 示例。
- 原生、Docker 和 Python HTTPS 登录流程均支持自定义端口。
- Python SSH 登录流程支持自定义 SSH 端口。

### Security

- 安装器升级时保留 `/etc/wps-adapter/secrets/`，不会清空已有 Cookie、CSRF 或适配器密码。
- Docker 构建上下文排除 `.env`、secret、HAR 和抓包文件。

## [0.5.1] - 2026-09-03

### Added

- `wps_login.py` 无参数运行时会询问 VPS 地址和连接方式。
- SSH 登录支持密钥和密码认证；密码由系统 `ssh` 原生提示，不需要 `sshpass`。

## [0.5.0] - 2026-09-03

### Added

- Python 登录助手可自动检测 WPS 登录完成，不再要求回终端按回车。
- 新增受适配器 Basic Auth 保护的 HTTPS 凭据导入接口。
- 新增仓库根目录 `wps_login.py` 入口，可直接运行而不设置 `PYTHONPATH`。
- 新增交互式连接向导，支持 SSH 密钥、SSH 密码和 HTTPS；SSH 密码由系统 `ssh` 原生提示。
- SSH 同步和本地凭据输出继续保留为备用方式。

### Security

- 远程明文 HTTP 被登录助手拒绝；只有本机回环地址允许 HTTP 测试。
- HTTPS 导入不跟随重定向，Cookie 只放在请求体中，不进入 URL、日志或命令参数。
- 凭据文件更新尽量保持 Cookie 和 CSRF 成对，并使用权限受限的原子文件写入。

## [0.4.0] - 2026-09-03

### Added

- 本地隔离 Chrome 登录助手，可在官方 WPS 页面完成登录后通过 SSH 同步会话凭据。
- 基于 WPS SDK 观察结果的 `grant_token` 无交互续期和轮换 Cookie 持久化。
- WebDAV `COPY`、`LOCK`/`UNLOCK`、递归 `PROPFIND`、单范围下载和传输资源保护。
- 普通上传、覆盖更新和 10 MiB 分片上传流程。

### Security

- 登录助手不读取现有浏览器配置，不打印 Cookie 值，不把凭据放入命令参数。
- 原始抓包和本地 secret 文件保持在 Git 忽略范围内。

## [0.3.0] - 2026-09-03

- 增加 WPS 会话刷新原型和 Set-Cookie 持久化。

## [0.2.0] - 2026-09-03

- 增加 WebDAV 传输保护、Range、COPY 和锁兼容层。

## [0.1.0] - 2026-09-03

- 建立基于本人账号抓包验证的列表、上传、下载、目录和基础 WebDAV 原型。
