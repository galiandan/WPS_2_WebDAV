# 本地接口

适配器只在本机或你自己的 VPS 上提供接口。默认端口是 `54321`，可通过 `ADAPTER_PORT` 或安装脚本的 `--port` 修改；网页入口是 `/`，WebDAV 挂载点是 `/dav/`，REST 前缀是 `/api/v1`。

## 浏览器页面

打开 `http://<服务器地址>:<端口>/` 并通过适配器 Basic Auth 后，可以在网页中浏览目录、打开文件夹、上传文件、下载文件、新建文件夹、重命名、移动和删除。页面只调用同源 REST 接口；上传使用浏览器请求体直接送入适配器，下载由适配器流式转发到浏览器。

## WebDAV

当前已实现并会访问 WPS 的方法：

| 方法 | 地址 | 行为 |
| --- | --- | --- |
| `OPTIONS` | `/dav/` | 返回能力列表 |
| `PROPFIND` | `/dav/<path>` | 目录属性和子项，支持 `Depth: 0/1/infinity` |
| `GET` | `/dav/<path>` | 直接流式下载文件 |
| `HEAD` | `/dav/<path>` | 查询文件元数据 |
| `PUT` | `/dav/<new-file>` | 上传新文件，要求 `Content-Length` |
| `MKCOL` | `/dav/<new-folder>/` | 创建文件夹 |
| `DELETE` | `/dav/<file-or-folder>/` | 删除文件或文件夹，等待 WPS 异步任务完成 |
| `MOVE` | `/dav/<old-path>` | 同目录内重命名，或跨目录移动并保留原名 |
| `COPY` | `/dav/<source-path>` | 文件/文件夹流式复制，支持 `Depth: 0/1/infinity`；为保证失败时不丢目标数据，暂不支持覆盖已有目标 |
| `LOCK` | `/dav/<path>` | 适配器进程内的短期独占写锁，返回 `Lock-Token` |
| `UNLOCK` | `/dav/<path>` | 释放适配器进程内的锁 |

WebDAV 使用 `MOVE` 和 `COPY` 请求的 `Destination` 目标地址。`MOVE` 同目录时表示重命名，跨目录时目标路径最后一个组件必须与原名称相同；`COPY` 的目标路径是复制后的完整路径。`Overwrite: F` 在目标存在时返回 `412`；目标已存在且要求覆盖时返回 `501`，避免 WPS 私有接口的非原子操作造成数据丢失。跨目录同时改名暂不支持。删除和移动都使用 WPS 的异步任务接口；适配器只在任务报告成功后才返回成功。

锁是适配器本地的兼容层：它不会调用未确认的 WPS 锁接口，只在当前进程内阻止没有对应 `If`/`Lock-Token` 的写操作。服务重启或锁超时后锁会消失。锁默认最长 24 小时，同时最多保留 4096 把活动锁；超过数量时返回 `503`。

WebDAV `PUT` 对同名文件执行覆盖更新；REST `PUT` 默认不覆盖同名文件，需显式加 `overwrite=true`。WPS 的覆盖更新会保留原文件 ID，并由上游生成新版本。上传内容达到 `WPS_MULTIPART_THRESHOLD` 时，适配器会改用已观察的分片流程；默认阈值为 50 MiB，默认分片大小为 10 MiB。该流程已在本人账号和 VPS 上用 100 MiB 测试文件回放成功；当前对普通上传和单个分片提供有限重试，失败后会重新获取签名地址并从该分片开始重传。进程退出后的任意续传、分片取消/清理和分片覆盖仍未宣称支持。

下载支持单个字节范围，例如 `Range: bytes=1048576-2097151` 或 `Range: bytes=-1048576`。适配器返回 `206`、`Content-Range` 和实际长度；范围请求如果上游对象存储没有返回 `206` 会失败，不会把完整文件误当成断点片段。多范围请求返回 `416`。

为适应低内存 VPS，默认最多同时进行 2 个上传和 4 个下载；单次上传默认限制为 1 GiB，设置 `WPS_MAX_UPLOAD_BYTES=0` 才会取消该上限。上传 spool 默认只在内存保留 8 MiB，超出后使用请求级临时文件，默认要求 spool 文件系统保留至少 512 MiB 空闲空间。适配器生成的单个 JSON/XML 响应默认不超过 16 MiB。达到条目数、递归深度、响应大小或磁盘/文件大小限制时返回 `507`；等待传输槽超时返回 `503`。可通过 `.env` 中的 `WPS_MAX_*`、`WPS_UPLOAD_*` 参数调整。

## REST

所有 `path` 都是 URL 查询参数，值是以 `/` 开头的远端路径：

```text
GET  /api/v1/entries?path=/
GET  /api/v1/metadata?path=/folder/file.txt
GET  /api/v1/download?path=/folder/file.txt
PUT  /api/v1/upload?path=/folder/new.txt
PUT  /api/v1/upload?path=/folder/file.txt&overwrite=true
POST /api/v1/folders?path=/folder/new-folder
DELETE /api/v1/entries?path=/folder/file.txt
PATCH /api/v1/entries?path=/folder/file.txt
POST  /api/v1/session/import
```

重命名时，`PATCH` 请求体使用 JSON，例如 `{"name":"new-name.txt"}`。也接受字段名 `fname` 以便与 WPS 字段对应。移动到目标目录并保留原名时使用 `{"parent_path":"/folder"}`；也可以使用完整目标路径 `{"destination":"/folder/file.txt"}`。适配器会使用自己的 secret 中的 CSRF，不使用调用方提交的认证值。

其中 `GET entries`、`metadata`、`download`、`PUT upload`、`POST folders`、`DELETE entries`、`PATCH entries` 和 WebDAV `MOVE` 已连接到当前 WPS 原型；`PUT upload` 对大文件会透明选择分片上传。COPY 在适配器层通过已有的下载/上传能力完成，不需要新的 WPS API。跨目录同时改名仍返回 `501`。上传请求需要 `Content-Length`，文件内容不会被适配器作为长期缓存保存。

### Importing a WPS session

`POST /api/v1/session/import` 使用适配器自己的 Basic Auth，建议通过 HTTPS 访问；没有域名或证书时也可以在可信网络使用 HTTP。它供本地 Python 登录助手使用，不供浏览器页面直接调用。请求体只接受从临时官方 WPS 登录窗口筛选出的 Cookie：

```json
{
  "cookies": [
    {"name": "rtk", "value": "<redacted>", "domain": ".kdocs.cn", "path": "/passport/secure"},
    {"name": "csrf", "value": "<redacted>", "domain": "365.kdocs.cn", "path": "/"}
  ],
  "workspace": {
    "group_id": "<group-id-from-current-space-url>",
    "root_id": "0"
  }
}
```

登录助手默认发送 `root_id=0`，表示企业云盘根目录；只有使用 `--workspace-url` 时才发送具体文件夹 ID。服务端会再次限制 WPS 域名、检查 `rtk`/`csrf` 和工作区 ID，然后更新配置的 `WPS_COOKIE_FILE`、`WPS_CSRF_TOKEN_FILE` 和 `WPS_WORKSPACE_FILE`。发送 `workspace` 时必须已配置 `WPS_GROUP_ID=auto` 或 `WPS_ROOT_ID=auto`；成功响应为 `200` JSON，服务会立即切换自动根目录并清理目录缓存。凭据更新后不需要重启服务。通过 HTTP 访问时，Cookie 和 Basic Auth 会明文传输。

所有写操作如果带有 `Origin` 或 `Referer`，适配器会要求其主机与当前请求的 `Host` 一致，用于阻止浏览器缓存 Basic Auth 后的跨站写入；没有这两个头的 WebDAV、curl 和 NAS 请求仍可正常使用。

## 状态码

- `401`: 适配器自身的 Basic Auth 未通过。
- `404`: 远端路径不存在。
- `409`: 路径冲突、把文件当目录使用，或出现重复名称。
- `412`: WebDAV 的 `Overwrite: F` 发现目标已存在。
- `501`: MOVE/COPY 需要覆盖已有目标，或 WPS 操作尚未确认/实现。
- `416`: Range 请求无法满足。
- `423`: 写操作被适配器本地锁阻止。
- `507`: 达到适配器的磁盘、文件大小、复制条目或递归深度保护。
- `502`: WPS 或对象存储请求失败；响应不会包含上游响应正文或签名 URL。
- `503`: WPS 会话过期且自动续期失败、反向代理/外部健康检查产生，或适配器的传输槽等待超时。

REST 遇到 WPS 请求失败时还会返回 `code: "wps_unavailable"`；WPS 返回 `401` 且自动续期失败时返回 `code: "wps_session_expired"`。网页会据此显示“WPS 未连接”或“登录已过期”，不会把适配器进程在线误认为 WPS 已连接。

`GET /healthz` 不访问 WPS，只返回进程健康状态。
