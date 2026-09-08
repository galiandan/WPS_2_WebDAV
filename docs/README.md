# 项目文档

这里放运行和维护 WPS 2 WebDAV 所需的长期文档。Go 服务已经是生产入口，
迁移计划和阶段性执行记录不再保留。

## 使用文档

- [`../README.md`](../README.md)：三步安装、登录、访问、升级和卸载。
- [`deployment.md`](deployment.md)：Native、Docker、手工部署、反向代理和回滚。
- [`login.md`](login.md)：独立 WPS 登录助手、空间选择和凭据同步。
- [`integration.md`](integration.md)：浏览器、WebDAV 客户端和常见连接方式。
- [`api.md`](api.md)：本地 REST、WebDAV 和状态接口。
- [`fd.md`](fd.md)：前端视觉、布局、交互和动效设计依据。

## 维护文档

- [`architecture.md`](architecture.md)：Go 服务的请求链路和资源边界。
- [`research/`](research/)：已确认的 WPS 请求、兼容性和安全研究记录。

真实 Cookie、CSRF、refresh token、签名 URL、HAR、PCAP 和用户文件不得提交到
仓库。抓包目录默认被 `.gitignore` 忽略。
