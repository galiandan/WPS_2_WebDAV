# Changelog

本项目遵循 Keep a Changelog 风格。版本号用于记录适配器行为变化，不代表 WPS 官方兼容性承诺。

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
