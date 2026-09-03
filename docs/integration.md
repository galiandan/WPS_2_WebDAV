# Integration Guide

本文面向需要把适配器接入桌面客户端、NAS、脚本或反向代理的使用者。部署步骤见 [`deployment.md`](deployment.md)，接口完整列表见 [`api.md`](api.md)。

## Endpoints

假设服务地址为 `https://<adapter-host>`：

| Interface | URL |
| --- | --- |
| Browser UI | `https://<adapter-host>/` |
| WebDAV | `https://<adapter-host>/dav/` |
| REST | `https://<adapter-host>/api/v1/` |
| Health | `https://<adapter-host>/healthz` |

所有 WebDAV 和 REST 请求都使用适配器 Basic Auth。健康检查不需要认证，但只用于检查进程状态，不代表 WPS 会话有效。

## Recommended rollout

先使用一个专用测试目录，按以下顺序验收：

1. 列目录。
2. 上传一个不含隐私的小文件。
3. 下载并校验内容。
4. 创建文件夹、重命名、移动和删除测试对象。
5. 测试单范围 `Range` 下载。
6. 测试 WebDAV `COPY`、`LOCK`/`UNLOCK` 和递归 `PROPFIND`。
7. 最后再测试大文件分片上传和客户端同步。

示例：

```bash
curl -u <adapter-user> \
  'https://<adapter-host>/api/v1/entries?path=/'

curl -u <adapter-user> \
  -H 'Content-Type: application/octet-stream' \
  --upload-file ./test.txt \
  'https://<adapter-host>/api/v1/upload?path=/test.txt'

curl -u <adapter-user> \
  -o ./test-downloaded.txt \
  'https://<adapter-host>/api/v1/download?path=/test.txt'

cmp ./test.txt ./test-downloaded.txt
```

不要把 `<adapter-user>` 的密码写进命令；让 curl 提示输入，或由客户端的安全凭据存储管理。

## WebDAV client settings

客户端类型通常选择“WebDAV”，地址填写：

```text
https://<adapter-host>/dav/
```

用户名和密码填写适配器 Basic Auth。客户端若询问“锁定支持”“保持连接”或“自动重试”，可以先使用默认设置；发现兼容性问题时，记录客户端发送的 method、`Depth`、`Destination`、`Range` 和状态码，不要提交认证头或完整请求。

## Streaming and limits

- 下载按块从 WPS 对象存储转发，不把整个文件读入内存。
- 上传在内存 spool 超限后使用请求级临时文件，完成或失败后清理。
- 默认最多 2 个并发上传、4 个并发下载；可在环境文件中调整。
- 递归 `PROPFIND` 和 `COPY` 有最大深度与条目数限制。
- 无效范围返回 `416`，资源保护触发时返回 `507`，传输槽等待超时返回 `503`。

这些限制是为了适配低内存 VPS。调整前先确认临时磁盘和上游带宽，不要通过无限制并发压测 WPS。

## Authentication lifecycle

首次登录使用本地隔离 Chrome 助手，见 [`login.md`](login.md)。适配器服务本身不会代填 WPS 密码、SSO、验证码或风控。

已有会话包含 `rtk` 时，服务在 WPS 返回 `401` 后会调用已确认的账号 SDK 刷新请求，并持久化轮换后的 `Set-Cookie`。如果刷新票据失效，重新运行登录助手即可；同步 secret 后无需重启服务。

## Operations checklist

- 使用 HTTPS 反向代理保护公网 WebDAV/REST。
- 保持 `ADAPTER_USERNAME_FILE`、`ADAPTER_PASSWORD_FILE`、`WPS_COOKIE_FILE` 和 `WPS_CSRF_TOKEN_FILE` 的权限为 `0600`。
- 不在反向代理、systemd 或应用日志中记录 `Authorization`、Cookie、签名 URL 或请求体。
- 升级前在自己的测试目录执行回归测试，并确认 WPS 账号仍可正常完成同样的 UI 操作。
- 删除测试对象时只删除本次创建的对象。

## Compatibility limits

适配器的 `COPY` 是下载/上传中继；`LOCK` 是进程内短期兼容锁；它们不代表 WPS 提供了对应的服务端 API。跨进程分片续传、取消/清理、部分快速上传路径和某些跨目录改名场景仍未确认。
