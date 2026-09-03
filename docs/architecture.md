# Architecture

## Overview

```text
WebDAV clients / REST clients / browser UI
                    |
              adapter server
       authentication, paths, locks,
       streaming, range, resource limits
                    |
                WpsStorage
       path resolution and short metadata cache
                    |
              WpsDriveClient
       confirmed WPS control requests
                    |
       WPS object-storage upload/download
```

适配器是单进程 Python 服务，默认只使用标准库。WPS 控制请求负责目录、元数据、上传会话和下载地址；实际文件内容通过短时内存/临时 spool 或流式对象请求传输，不作为长期缓存保存在服务器上。

## Request flow

1. 客户端请求进入 WebDAV、REST 或同源文件管理页。
2. 服务完成 Basic Auth、路径校验、并发槽和请求大小检查。
3. `WpsStorage` 将远端路径解析为 WPS 文件夹/文件 ID，并使用短 TTL 元数据缓存减少重复列表请求。
4. `WpsDriveClient` 从 secret 文件读取当前 Cookie/CSRF，调用已经从本人账号观察并记录的 WPS 请求形状。
5. 文件上传和下载尽量通过流式中继完成；签名对象存储请求不会收到 WPS Cookie。
6. 上游 `401` 时，文件凭据源优先检测管理员替换，然后按已观察的 SDK `grant_token` 流程尝试续期并重试一次。

## Authentication boundaries

- 适配器 Basic Auth 保护对外 REST/WebDAV/UI。
- WPS Cookie 和 CSRF 只存放在本机或 VPS 的权限受限 secret 文件中。
- 交互式登录由本地 `login` 助手启动官方 WPS 页面完成；服务器不代填密码、SSO、验证码或风控。
- `rtk` 是当前自动续期原型所需的 WPS 持久刷新 Cookie。没有它时，重新运行本地登录助手。

## Resource model

上传超过内存 spool 阈值后使用请求级临时文件，完成或失败后清理；下载按块读取。上传、下载、递归 `PROPFIND` 和 `COPY` 都有并发、数量、深度或磁盘空间上限，避免低配 VPS 被单个客户端请求耗尽资源。

## Deliberate limitations

WPS 相关接口不是公开稳定契约。项目只实现已经在本人账号上观察或重放验证的流程；适配器层的 `COPY` 和 `LOCK` 是兼容层，不代表 WPS 提供对应服务端 API。跨目录同时改名、进程退出后的分片续传以及某些快速上传路径仍未宣称支持。
