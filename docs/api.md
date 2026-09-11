# 本地接口

适配器只在本机或你自己的 VPS 上提供接口。默认端口是 `54321`，可通过 `ADAPTER_PORT` 或安装脚本的 `--port` 修改；网页入口是 `/`，WebDAV 挂载点是 `/dav/`，REST 前缀是 `/api/v1`。

## 浏览器页面

打开 `http://<服务器地址>:<端口>/` 会进入内置登录页面；网页直接使用安装时设置的适配器 Basic Auth 账号，不再触发浏览器原生 Basic Auth 弹窗。登录后可以浏览目录、打开文件夹、上传文件、在线浏览 TXT、下载文件、新建文件夹、重命名、移动和删除。点击右上角齿轮可以直接修改云盘显示名称；页面只调用同源 REST 接口，上传使用浏览器请求体直接送入适配器，下载由适配器流式转发到浏览器。当前目录读取完成后，网页会在后台以受限并发预取最多 24 个直接子文件夹，缓存 30 秒；进入已预取或已经访问过的文件夹时，网页先立即显示缓存内容，再后台刷新，不会先清空列表等待 WPS。刷新目录或执行写操作会清理这批缓存。

网页不提供注册功能，也不创建额外的用户数据库。唯一网页登录账号就是安装时写入 `/opt/wps-adapter/config/secrets/adapter-username` 和 `/opt/wps-adapter/config/secrets/adapter-password` 的适配器账号；浏览器会话使用 HttpOnly Cookie，服务重启后会话失效。替换这两个文件后，网页登录和 WebDAV 会同时使用新凭据。

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
| `COPY` | `/dav/<source-path>` | 单文件同空间复制优先使用 WPS 服务端能力；文件夹使用受限流式中继，支持 `Depth: 0/1/infinity`；暂不支持覆盖已有目标 |
| `LOCK` | `/dav/<path>` | 适配器进程内的短期独占写锁，返回 `Lock-Token` |
| `UNLOCK` | `/dav/<path>` | 释放适配器进程内的锁 |

WebDAV 使用 `MOVE` 和 `COPY` 请求的 `Destination` 目标地址。`MOVE` 同目录时表示重命名，跨目录时目标路径最后一个组件必须与原名称相同；`COPY` 的目标路径是复制后的完整路径。`Overwrite: F` 在目标存在时返回 `412`；目标已存在且要求覆盖时返回 `501`，避免 WPS 私有接口的非原子操作造成数据丢失。跨目录同时改名暂不支持。删除和移动都使用 WPS 的异步任务接口；适配器只在任务报告成功后才返回成功。

登录助手可以绑定多个 WPS 空间。网页把这些空间挂载为 `/空间名称/`，例如 `/A/` 和 `/B/`；WebDAV 不使用这个多空间根，而是只映射到一个已选空间中的一个目录。例如顶层配置为 `/A/web` 时，`/dav/test.txt` 实际写入 A 的 `web/test.txt`。上传、`MOVE` 和 `COPY` 都被限制在这个唯一 WebDAV 子树内。

锁是适配器本地的兼容层：它不会调用未确认的 WPS 锁接口，只在当前进程内阻止没有对应 `If`/`Lock-Token` 的写操作。服务重启或锁超时后锁会消失。锁默认最长 24 小时，同时最多保留 4096 把活动锁；超过数量时返回 `503`。

WebDAV `PUT` 对同名文件执行覆盖更新；REST `PUT` 默认不覆盖同名文件，需显式加 `overwrite=true`。WPS 的覆盖更新会保留原文件 ID，并由上游生成新版本。上传内容达到 `WPS_MULTIPART_THRESHOLD` 时，适配器会改用已观察的分片流程；默认阈值为 50 MiB，默认分片大小为 10 MiB。该流程已在本人账号和 VPS 上用 100 MiB 测试文件回放成功；当前对普通上传和单个分片提供有限重试，失败后会重新获取签名地址并从该分片开始重传。进程退出后的任意续传、分片取消/清理和分片覆盖仍未宣称支持。

下载支持单个字节范围，例如 `Range: bytes=1048576-2097151` 或 `Range: bytes=-1048576`。适配器返回 `206`、`Content-Range` 和实际长度；范围请求如果上游对象存储没有返回 `206` 会失败，不会把完整文件误当成断点片段。普通下载优先使用对象存储响应的实际 `Content-Length`，不使用可能过期的目录元数据长度；上游未提供长度时使用连接关闭表示 EOF。响应会发送 `no-transform`，并在最后一个字节刷新后显式关闭 TCP 写方向，避免浏览器或中间代理在进度达到 100% 后继续等待。多范围请求返回 `416`。

为适应低内存 VPS，默认最多同时进行 2 个上传和 4 个下载；单次上传默认限制为 1 GiB，设置 `WPS_MAX_UPLOAD_BYTES=0` 才会取消该上限。上传 spool 默认只在内存保留 8 MiB，超出后使用请求级临时文件，默认要求 spool 文件系统保留至少 512 MiB 空闲空间。适配器生成的单个 JSON/XML 响应默认不超过 16 MiB。达到条目数、递归深度、响应大小或磁盘/文件大小限制时返回 `507`；等待传输槽超时返回 `503`。可通过 `.env` 中的 `WPS_MAX_*`、`WPS_UPLOAD_*` 参数调整。

## REST

所有 `path` 都是 URL 查询参数，值是以 `/` 开头的远端路径。网页认证接口如下：

```text
GET  /api/v1/auth/me
POST /api/v1/auth/login
POST /api/v1/auth/logout
```

`login` 接收 `{"username":"...","password":"..."}`，凭据必须与安装时的适配器账号一致；成功后通过 HttpOnly `wps_session` Cookie 建立会话。认证接口和网页资源可以匿名访问；文件、设置和 WPS 状态接口必须有网页会话或适配器 Basic Auth。WebDAV 始终使用 Basic Auth，不接受网页会话 Cookie。

文件 API：

```text
GET  /api/v1/entries?path=/
GET  /api/v1/metadata?path=/folder/file.txt
GET  /api/v1/download?path=/folder/file.txt
GET  /api/v1/preview?path=/folder/file.txt
GET  /api/v1/status
PUT  /api/v1/upload?path=/folder/new.txt
PUT  /api/v1/upload?path=/folder/file.txt&overwrite=true
POST /api/v1/folders?path=/folder/new-folder
DELETE /api/v1/entries?path=/folder/file.txt
PATCH /api/v1/entries?path=/folder/file.txt
GET  /api/v1/settings
PATCH /api/v1/settings
GET   /api/v1/update
POST  /api/v1/update
POST  /api/v1/session/import
GET   /api/v1/storage
GET   /api/v1/storage/entries?path=/空间名称/子文件夹
PATCH /api/v1/storage
```

重命名时，`PATCH` 请求体使用 JSON，例如 `{"name":"new-name.txt"}`。也接受字段名 `fname` 以便与 WPS 字段对应。移动到目标目录并保留原名时使用 `{"parent_path":"/folder"}`；也可以使用完整目标路径 `{"destination":"/folder/file.txt"}`。适配器会使用自己的 secret 中的 CSRF，不使用调用方提交的认证值。

其中 `GET entries`、`metadata`、`download`、`preview`、`PUT upload`、`POST folders`、`DELETE entries`、`PATCH entries` 和 WebDAV `MOVE` 已连接到企业和个人 WPS 原型；个人端自动使用 `drive.wps.cn/api/...`，企业端使用 `365.kdocs.cn/3rd/drive/api/...`。`preview` 只接受 `.txt` 文件，默认最多返回前 2 MiB，超出时通过 `X-Preview-Truncated: true` 标记，不返回下载附件头。`PUT upload` 对大文件会透明选择分片上传。COPY 在适配器层通过已有的下载/上传能力完成，不需要新的 WPS API。跨目录同时改名仍返回 `501`。上传请求需要 `Content-Length`，文件内容不会被适配器作为长期缓存保存。

### WPS status

`GET /api/v1/status` 使用网页会话或适配器 Basic Auth，执行低频、只读的 WPS 会话预检。它先请求账号服务的 `api/v3/islogin`，再对当前映射的群组根目录做一次最小列表验证。成功结果会缓存 30 秒，失败结果会短暂退避；并发请求会共享同一次预检。状态检查本身不会主动调用刷新令牌，文件接口遇到上游 `401` 时仍按原有规则执行自动续期。个人账号的 `account_type` 会显示为 `personal`。

```json
{
  "status": "connected",
  "wps": "connected",
  "workspace": "ready",
  "account_type": "business",
  "last_checked_at": 1788350000,
  "retry_after": 0
}
```

`status` 可能是 `connected`、`not_configured`、`session_expired`、`permission_denied`、`upstream_unavailable` 或 `invalid_response`。响应不会包含 Cookie、CSRF、`rtk`、企业/群组/用户 ID、签名 URL 或 WPS 原始正文。`/healthz` 仍然只检查适配器进程，不访问 WPS。

### Web settings

`GET /api/v1/settings` 返回当前网页显示名称。使用网页会话或适配器 Basic Auth 发送下面的请求即可修改名称；网页按钮会自动完成同样的请求：

```http
PATCH /api/v1/settings
Content-Type: application/json

{"name":"我的云盘"}
```

成功响应为 `200` JSON。名称只影响适配器网页、虚拟根目录元数据和 WebDAV `displayname`，不会重命名 WPS 远端文件夹。服务会将名称以权限受限的 JSON 文件保存到 `/opt/wps-adapter/config/secrets/web-settings.json`，不访问 WPS，因此即使 WPS 当前未连接也可以修改。

### 更新

`GET /api/v1/update` 使用网页会话或适配器 Basic Auth 查询最新 Release。发现新版本时，网页首页会显示更新按钮：

```json
{
  "state": "available",
  "current_version": "1.0.4",
  "latest_version": "1.0.5",
  "update_available": true,
  "release_url": "https://github.com/galiandan/WPS_2_WebDAV/releases/tag/v1.0.5",
  "message": "发现新版本"
}
```

网页按钮发送 `POST /api/v1/update`。服务返回 `202` 后在后台下载当前 Linux 架构的 Release 二进制，确认其 `--version` 与 Release 标签一致，再原子替换部署目录中的运行文件并重启自身。`state` 可能是 `checking`、`downloading`、`restarting`、`available`、`idle` 或 `error`。更新不会执行 shell、不会接触 Docker Socket，也不会修改 `config/` 中的凭据和工作区文件。Native 需要 systemd 的 `ReadWritePaths` 包含 `runtime/`；Docker 安装器会将 `runtime/` 作为可写挂载并从该目录启动服务。旧 Docker 部署需要先通过最新安装器重新部署一次，才能获得 Docker 的持久化自更新入口。

更新服务默认请求国内加速的 GitHub API 和 Release 地址。如果 VPS 无法访问默认镜像，可以设置 `WPS_ADAPTER_UPDATE_API_URL` 和 `WPS_ADAPTER_UPDATE_BASE_URL`，两者必须使用 HTTPS。项目遵循当前安装约定，不执行哈希校验，但会限制响应大小、拒绝符号链接目标，并运行新文件的版本检查。

### WebDAV storage location

`GET /api/v1/storage` 返回当前 WebDAV 映射的显示信息，不返回 WPS 的群组 ID、文件夹 ID、Cookie 或签名地址：

```json
{
  "status": "ok",
  "mode": "spaces",
  "locations": [
    {"name": "A", "path": "/A", "root_path": "/"},
    {"name": "B", "path": "/B", "root_path": "/"}
  ],
  "current": {"name": "A", "path": "/A", "root_path": "/web"}
}
```

`locations` 是网页可浏览的空间列表，不是多个 WebDAV 根目录。`current` 是唯一的 WebDAV 根目录。网页设置中的“选择文件夹”使用 `GET /api/v1/storage/entries?path=...` 浏览任意一个已选空间的原始根目录，例如：

```json
{"path":"/A/web"}
```

保存后，WebDAV 地址仍然是 `/dav/`，但它映射到新的 WPS 文件夹；网页的 A、B 空间根目录不会改变。这个操作不会在 WPS 中移动、复制或删除任何文件；它只更新 `/opt/wps-adapter/config/secrets/wps-workspace.json` 的顶层 `group_id`、`root_id`、`root_path`，并保留 `spaces` 数组不变。服务会立即清理目录缓存并使用新位置，无需重启。

### Importing a WPS session

`POST /api/v1/session/import` 是保留的受保护管理接口，使用适配器自己的 Basic Auth；它不再被 `wps_login.py` 调用，登录助手固定通过 SSH 写入配置目录。若自行调用该接口，必须自行承担传输协议的安全责任。请求体只接受从临时官方 WPS 登录窗口筛选出的 Cookie：

```json
{
  "cookies": [
    {"name": "rtk", "value": "<redacted>", "domain": ".kdocs.cn", "path": "/passport/secure"},
    {"name": "csrf", "value": "<redacted>", "domain": "365.kdocs.cn", "path": "/"}
  ],
  "workspace": {
    "group_id": "<group-id-from-current-space-url>",
    "root_id": "<single-webdav-folder-id>",
    "root_path": "/web",
    "spaces": [
      {"group_id": "<group-a>", "root_id": "0", "name": "A"},
      {"group_id": "<group-b>", "root_id": "0", "name": "B"}
    ]
  }
}
```

登录助手默认使用所选 WebDAV 空间的 `root_id=0`；如果选了文件夹，则顶层 `root_id/root_path` 指向该文件夹。`spaces` 中的每个空间始终使用 `root_id=0`，供网页从空间根目录开始浏览。输入 `s` 或直接回车可以省略目录选择，之后在网页设置中选择唯一的 WebDAV 根目录。服务端会再次限制 WPS 域名、检查 `rtk`/`csrf` 和工作区 ID，然后更新配置的 `WPS_COOKIE_FILE`、`WPS_CSRF_TOKEN_FILE` 和 `WPS_WORKSPACE_FILE`。发送 `workspace` 时必须已配置 `WPS_GROUP_ID=auto` 或 `WPS_ROOT_ID=auto`；成功响应为 `200` JSON，服务会立即切换自动根目录并清理目录缓存。凭据更新后不需要重启服务。

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

REST 遇到 WPS 请求失败时还会返回 `code: "wps_unavailable"`；WPS 返回 `401` 且自动续期失败时返回 `code: "wps_session_expired"`。网页会据此显示“WPS 尚未连接”“登录已过期”或“WPS 暂时不可用”，不会把适配器进程在线误认为 WPS 已连接。

`GET /healthz` 不访问 WPS，只返回进程健康状态。
