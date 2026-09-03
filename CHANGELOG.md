# Changelog

本项目遵循 Keep a Changelog 风格。版本号用于记录适配器行为变化，不代表 WPS 官方兼容性承诺。

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
