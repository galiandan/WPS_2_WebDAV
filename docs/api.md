# 本地接口

适配器只在本机或你自己的 VPS 上提供接口。默认端口是 `54321`，可通过 `ADAPTER_PORT` 或安装脚本的 `--port` 修改；网页入口是 `/`，WebDAV 挂载点是 `/dav/`，REST 前缀是 `/api/v1`。

## 浏览器页面

打开 `http://<服务器地址>:<端口>/` 会进入内置登录页面；网页直接使用安装时设置的适配器 Basic Auth 账号，不再触发浏览器原生 Basic Auth 弹窗。登录后可以浏览目录、打开文件夹、上传文件、在线浏览纯文本文件、下载文件、新建文件夹、重命名、移动和删除。左侧提供多级目录树：箭头展开或收起，点击名称进入目录；当前目录精确高亮，从面包屑、列表或直接链接进入深层目录时自动展开路径。目录树只列文件夹，按需读取一层并复用目录缓存；空目录、失败重试和目录变更同步均在侧栏显示。支持键盘左右键展开/收起和父子目录间移动，手机端从导航按钮打开抽屉。列表和网格视图中，双击支持预览的文件即可打开在线浏览，也可通过文件菜单进入。 文件工具栏的“新建文本文件”支持填写名称和内容，以 UTF-8 在当前目录创建可预览格式的文件（支持空文件），复用 PUT /api/v1/upload 且不覆盖同名项目。保存失败或关闭窗口后草稿保留在当前页面，并保持原目标目录；刷新或离开页面会丢失草稿。点击右上角齿轮可以直接修改云盘显示名称；页面只调用同源 REST 接口，上传使用浏览器请求体直接送入适配器，下载由适配器流式转发到浏览器。当前目录读取完成后，网页会在后台以单并发预取最多 8 个直接子文件夹，缓存 30 秒；进入已预取或已经访问过的文件夹时，网页先立即显示缓存内容，再后台刷新，不会先清空列表等待 WPS。状态检查仅用于后台状态徽标，不会阻塞目录导航。刷新目录或执行写操作会清理这批缓存。

网页不提供注册功能，也不创建额外的用户数据库。唯一网页登录账号就是安装时写入 `/opt/wps-adapter/config/secrets/adapter-username` 和 `/opt/wps-adapter/config/secrets/adapter-password` 的适配器账号；浏览器会话使用 HttpOnly Cookie，服务重启后会话失效。替换这两个文件后，网页登录和 WebDAV 会同时使用新凭据。


文本阅读交互参考 [OpenList 文本预览](https://github.com/OpenListTeam/OpenList-Frontend/blob/4520f96204408982a563e6a075adc020bac5dccb/src/pages/home/previews/text-editor.tsx)和[编码选择](https://github.com/OpenListTeam/OpenList-Frontend/blob/4520f96204408982a563e6a075adc020bac5dccb/src/components/EncodingSelect.tsx)；本项目使用原生 `TextDecoder` 和 `<pre>` 实现只读预览，无需加载外部编辑器。

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

其中 `GET entries`、`metadata`、`download`、`preview`、`PUT upload`、`POST folders`、`DELETE entries`、`PATCH entries` 和 WebDAV `MOVE` 已连接到企业和个人 WPS 原型；个人端自动使用 `drive.wps.cn/api/...`，企业端使用 `365.kdocs.cn/3rd/drive/api/...`。`preview` 接受 `.txt`、`.log`、`.md`、`.csv`、`.json`、`.xml`、`.yaml`、`.yml`、`.ini`、`.conf`、`.toml`（不区分大小写），默认最多返回前 2 MiB 原始字节。响应类型为 `application/octet-stream`，不声明文本编码，不返回下载附件头；包含 `Cache-Control: no-store`、`X-Content-Type-Options: nosniff`、`X-Preview-Limit`（字节上限）和 `X-Preview-Truncated`（是否截断）。网页使用 BOM / UTF-8 检查并回退到 GB18030，支持手动选择 UTF-8、GB18030/GBK、Big5 和 UTF-16 LE/BE；切换编码复用已读字节。截断时隐藏末尾不完整字符，显示实际读取上限。文本仅作为纯文本展示；含二进制控制字符时提示切换编码或下载。关闭预览会取消读取。 图片和 PDF 使用同一 `GET /api/v1/preview` 路由，支持 JPG/JPEG、PNG、GIF、WebP、AVIF、BMP、ICO、PDF（不区分大小写），按允许列表设置类型并返回 `Content-Disposition: inline`，复用下载并发、流式读取和单 Range 能力，不套用文本的 2 MiB 截断规则。HTML 源码可按纯文本/语法高亮查看，但不会按网页执行；SVG 不提供在线预览。PDF 由浏览器内置阅读器显示；不支持的浏览器可下载查看。`PUT upload` 对大文件会透明选择分片上传。COPY 在适配器层通过已有的下载/上传能力完成，不需要新的 WPS API。跨目录同时改名仍返回 `501`。上传请求需要 `Content-Length`，文件内容不会被适配器作为长期缓存保存。

### 音视频、源码与缩略图

`GET /api/v1/preview` 额外允许 MP4/M4V、WebM、OGV、MP3、M4A、AAC、OGG/OGA、WAV、FLAC，以允许列表 MIME 内联返回，复用单 Range 流式下载。实际能否播放取决于浏览器编解码器，无转码服务。网页提供同目录播放列表、倍速、播放进度记忆与本地 VTT/SRT 字幕；字幕不上传。

源码预览扩展至 `.markdown/.js/.mjs/.cjs/.ts/.jsx/.tsx/.go/.py/.sh/.bash/.css/.html/.htm/.sql/.rs/.java/.c/.h/.cpp/.hpp/.diff/.patch`。这些文件与纯文本一样作为有上限的 `application/octet-stream` 字节返回，HTML 不作为活动文档执行。编辑和新建文本的扩展名限制不因此扩大。

`GET /api/v1/thumbnail?path=...` 为 JPEG、PNG、GIF 首帧生成保持比例、最长边 256 像素的 JPEG，透明背景合成为白色。输入最多 8 MiB、800 万像素、单边 8192 像素；输出最多 128 KiB。服务端仅 1 个活动生成任务，最多 8 个等待者、等待 15 秒；关闭连接会取消读取与缩放。超限返回 507，不支持格式返回 501，读取/解码失败提供脱敏错误，前端保留文件图标。

有账号/工作区指纹和文件版本时，服务端缓存最多 64 张、4 MiB、5 分钟；键包含文件路径、元数据及私有身份。没有身份或版本则不缓存。浏览器响应使用 `no-store`，不长期保存私有缩略图。

富文本使用内嵌 marked、DOMPurify 与 highlight.js，在 Web Worker 内解析/高亮，2 秒后可中止；输入最多 256 KiB，生成 HTML 最多 1 MiB，最多 10000 个元素，长代码块回退原文。原始 HTML 转义后再清理，链接仅允许 HTTP(S) 或已规范化的适配器内部路径，相对图片只允许本适配器安全图片预览；远程图片、脚本、SVG/data URL 不自动加载。目录 `README.md` 使用同样规则显示，目录切换取消旧请求。

### 持久化后台任务

`POST /api/v1/tasks` 接收与 `/batch` 相同的 `operation/paths/destination` JSON，返回 202 和 `{task: ...}`。网页批量复制、移动、删除使用此接口；原同步 `/batch` 继续保留。

- `GET /api/v1/tasks`：返回 `{tasks: [...], persistence_error: false}`。
- `GET /api/v1/tasks/<id>`：返回 `{task: ...}`。
- `POST /api/v1/tasks/<id>/cancel`：停止尚未开始的项目，当前项目可能继续完成。
- `POST /api/v1/tasks/<id>/retry`：为可安全重试的项目建立新任务；成功项目、不确定结果的已开始项目不会自动重放。

任务包含 `id/operation/destination/state/created_at/updated_at/items/total/completed/succeeded/failed/retryable_count`。逐项包含 `path/state/status/error/started/retryable/retried_as`。任务状态为 `queued/running/completed/failed/cancelled/interrupted`，项目成功为 `succeeded`。界面按项目显示进度；一个文件夹仍视作一个所选项目。

每次最多 100 项，工作线程为 1，最多 16 个排队或执行任务，最多保存最近 100 条记录。状态以 0600 JSON 原子落盘，每个项目执行前和得到结果后均记录；启动时将旧排队/执行记录标记中断，不自动恢复。源文件、目标目录 ID 与私有账号/工作区指纹绑定，变化时拒绝旧任务。锁在实际执行时检查。不确定网络失败、超时或进程中断可能已经影响上游，需先核对远端结果。状态写入失败后不再启动新任务；修复文件路径/权限后重启。

### 在线文本编辑

`GET /api/v1/text?path=/空间/文件.txt` 返回完整原始字节（最多 2 MiB）与强 `ETag` 编辑版本。支持的扩展名与纯文本预览一致，旧中文编码可在网页中切换。`PUT` 同一路径上传 UTF-8 原始正文，必须带读取时的 `If-Match`；成功返回 `{path,entry,revision}` 和新的 `ETag`。

缺少版本返回 428；内容、文件身份、账号或工作区变化返回 412；超过编辑大小返回 413；WebDAV 锁冲突返回 423；仍受部署上传预算限制。接口只更新原文件，不主动回退为新建文件。保存前跳过目录缓存重新下载比较完整内容，保存后复读确认，多个编辑请求串行执行以限制内存。版本绑定服务进程，重启后需重新读取。

WPS 没有已验证的原子 compare-and-swap 覆盖接口，检查与上传之间的外部写入仍可能竞争。上传已开始后取消/失败可能已改变远端，服务返回 `text_save_uncertain`（502）或 `text_save_unverified`（409）时应保留草稿并重新读取确认，不自动重试。网页不在保存中关闭编辑窗口，关闭其他阶段会取消读取；刷新页面会丢失未保存草稿。

### 批量操作与打包下载

`POST /api/v1/batch` 接收 `{"operation":"copy","paths":["/空间/目录/文件.txt"],"destination":"/空间/目标目录"}`。`operation` 支持 `copy`、`move`、`delete`；删除省略 `destination`。最多 100 个不重叠路径，拒绝根目录和目标落在源目录内部，复制/移动不覆盖同名目标。沿用现有空间边界，不增加 WPS 原生目录复制或跨空间操作。响应包含 `results: [{path,ok,status,error?}]`、`succeeded` 和 `failed`；HTTP 200 不代表所有项目成功。取消请求后不启动剩余项目，已经完成的项目不回滚，也不会隐式重试。

`POST /api/v1/archive` 接收 `{"paths":["/空间/文件.txt","/空间/目录"]}`；也接受表单 `paths` 字段（值为 JSON 数组），使浏览器直接保存下载而不在 JavaScript 中缓冲整个 ZIP。`GET /api/v1/archive?path=...&path=...` 提供相同读取行为。返回 ZIP 附件，保留相对目录与空目录，采用不压缩的流式打包。每次最多选择 100 项、展开 10000 项、64 层、10 GiB 数据和 8 MiB 累计名称；同名或大小写冲突拒绝打包。开始传输后若上游失败、大小变化或超过预算，立即中断 HTTP 传输，不生成貌似成功的残缺 ZIP。ZIP 不支持 Range 续传。

### 全局文件名与路径搜索

- `POST /api/v1/search/refresh?path=/` 建立所有已选空间的索引；`path=/空间` 仅扫描该空间。返回 202；已有扫描时返回 409。一次只运行一个扫描，新扫描替换旧索引。
- `GET /api/v1/search?q=报告&path=/空间&type=document&match=name&offset=0&limit=100` 查询已有索引。`type` 为 `all/file/folder/image/video/audio/document`，`match` 为 `name/path`；每页 1–200 项。
- `DELETE /api/v1/search` 停止扫描。底层目录请求有自身超时，取消在当前目录请求返回后生效；关闭搜索窗口不会停止后台扫描。

查询返回 `results: [{path,entry}]`、`total`、`has_more`、`scope_covered` 和 `index`。`index` 包含 `state`、`path`、`generation`、`entries`、`scanned_folders`、`skipped_folders`、`complete`、`reason` 和时间信息。状态为 `idle/indexing/ready/partial/cancelling/cancelled/failed`。只有 `complete=true` 且 `scope_covered=true` 时，空结果才代表已扫描范围内没有匹配项。索引只包含元数据，不查询文件正文；类型按扩展名识别。

索引在内存中最多保存 50000 项及 32 MiB 元数据；最多遍历 5000 个目录、64 层。扫描在目录请求之间检查 10 分钟时限；当前目录请求需等待自身超时或完成，因此不是严格的 10 分钟硬截止。目录请求间隔至少 100 毫秒。错误和超限仅报告固定原因，不返回上游原文。手动刷新会清理目录缓存；服务重启、REST/WebDAV 文件写入、登录凭据或工作区热替换会失效索引。来自 WPS 官网或其他客户端的变更需手动更新。

### WPS status

`GET /api/v1/status` 使用网页会话或适配器 Basic Auth，执行低频、只读的 WPS 会话预检。它先请求账号服务的 `api/v3/islogin`，再对当前映射的群组根目录做一次最小列表验证。成功结果会缓存 30 秒，失败结果会短暂退避；并发请求会共享同一次预检。状态检查本身不会主动调用刷新令牌，文件接口遇到上游 `401` 时仍按原有规则执行自动续期。个人账号的 `account_type` 会显示为 `personal`。这个接口只用于状态徽标和诊断，不是目录导航的前置条件；网页以实际目录请求是否成功作为当前目录可用性的依据，避免一次状态探测抖动遮住已经返回的文件。

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

`GET /api/v1/update` 使用网页会话或适配器 Basic Auth 查询最新 Release。网页会在后台检查；正常时云盘名称下方显示灰色版本号胶囊，发现新版本时版本号会变黄。点击版本号可查看当前版本、最新版本和发布页，并重新检查或立即更新。检查失败会保留文件页面，不会静默改变文件操作状态：

```json
{
  "state": "available",
  "current_version": "1.0.15",
  "latest_version": "1.0.16",
  "update_available": true,
  "release_url": "https://github.com/galiandan/WPS_2_WebDAV/releases/tag/v1.0.16",
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
