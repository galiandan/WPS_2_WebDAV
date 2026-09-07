# 外部兼容性与安全契约

> 本文冻结 Go 重构必须保持的外部行为，不包含实现代码。
> `必须` 表示未通过对应测试不得发布；`暂不支持` 表示不得自行补全或猜测。

## 1. 总原则

1. URL、方法、状态码、关键响应头、JSON/XML 字段、文件内容和资源上限都属于契约。
2. Python 与 Go 的 JSON 空白和对象键顺序可以不同，字段、类型和含义不能不同。
3. XML namespace 前缀可不同，namespace URI、树结构、元素值和 href 不能不同。
4. WPS 私有请求只移植已观察或重放的形状。
5. 不把 WPS 原始错误正文转发给客户端。
6. 不记录 Cookie、CSRF、rtk、Basic Auth、签名 URL、真实对象 ID 或文件正文。
7. 任何有意差异先加入 Python 特征测试、决策记录、文档和回滚说明。

## 2. 公共入口

| 入口 | 默认地址 | 认证 | 是否访问 WPS |
| --- | --- | --- | --- |
| 健康检查 | `GET /healthz` | 否 | 永不 |
| 网页 | `GET /`、`GET /web`、`GET /web/` | Basic Auth 启用时需要 | 页面壳不访问 |
| REST | `/api/v1/...` | Basic Auth 启用时需要 | 按路由 |
| WebDAV | `/dav`、`/dav/...` | Basic Auth 启用时需要 | 按方法 |

DAV 与 REST 前缀可通过原环境变量修改。Go 不得只支持默认前缀。

## 3. HTTP framing

必须保持：

- HTTP/1.1。
- 拒绝任何 `Transfer-Encoding`，不只拒绝 chunked。
- 拒绝多个 `Content-Length`。
- GET、HEAD、OPTIONS 带非零 Content-Length 时返回 400。
- 上传必须有一个非负 Content-Length；缺失返回 411。
- 非法、负数或短于声明的正文返回 400，并关闭连接。
- 普通控制体默认上限 1 MiB；session import 512 KiB；LOCK XML 64 KiB。
- 控制体过大返回 413；上传/响应/递归资源上限返回 507。
- 超连接数当前直接关闭 socket；是否改为 503 必须先批准 D-09。

Go `net/http` 默认会解 chunked、管理 keep-alive 和选择响应 framing，因此必须用 raw HTTP 测试固定结果，不能只测 handler 函数。

## 4. Basic Auth

1. `/healthz` 永远跳过认证。
2. 其他网页、静态资源、REST、DAV 和 OPTIONS 都按配置认证。
3. scheme 大小写不敏感，但 Base64 必须严格合法。
4. 解码结果必须是 UTF-8 且至少包含一个冒号；第一个冒号分隔用户名与密码。
5. 用户名和密码分别使用恒定时间比较。
6. 文件型用户名/密码每请求读取，支持运行时轮换。
7. 凭据文件读取失败按认证失败处理，不回显文件内容。
8. 认证失败：401、空 body、`WWW-Authenticate: Basic realm="wps-adapter"`、`Connection: close`、`Content-Length: 0`。
9. 非 loopback bind 且完全未启用认证时拒绝启动。
10. 半配置状态的处理由 D-05 决定；不得无说明改变。

## 5. 浏览器同源写保护

以下方法都必须检查：PUT、POST、DELETE、PATCH、MKCOL、MOVE、COPY、LOCK、UNLOCK。

规则：

1. 有 Origin 时只检查 Origin。
2. 没有 Origin 时检查 Referer。
3. 两者都没有时允许，以兼容 curl、NAS 和 WebDAV 客户端。
4. 只接受 http/https。
5. host 必须与请求 Host 一致，显式端口不能冲突。
6. 拒绝 userinfo、query、fragment、控制字符和非法 URL。
7. Origin 只能是根 origin；Referer 可以带页面 path。
8. 失败返回 403、固定错误且关闭连接。

## 6. 通用响应

- 普通 JSON/XML/HTML/文本响应带精确 Content-Length 和 `Cache-Control: no-store`。
- REST JSON 为 UTF-8 紧凑对象。
- DAV 普通错误为 `text/plain; charset=utf-8`，正文是消息加换行。
- REST 错误至少为 `{"error":"..."}`。
- HEAD 不发送 body。
- 流式下载在未知长度时关闭连接，不能错误声明长度。

## 7. 健康与状态

### 7.1 health

返回 200，字段固定：

- status=`ok`。
- service=`wps-enterprise-adapter`。
- version=当前版本。
- network_calls=`on-demand`。

不得读取 storage、workspace、凭据或 WPS。

### 7.2 WPS status

`GET /api/v1/status` 的 HTTP 状态通常为 200，业务状态在 JSON：

- `connected`
- `not_configured`
- `session_expired`
- `permission_denied`
- `upstream_unavailable`
- `invalid_response`

字段固定为：status、wps、workspace、account_type、last_checked_at、retry_after。

成功缓存默认 30 秒，失败退避默认 5 秒，并发探测共享一次请求。缓存身份包含凭据、group 和 root。不得返回 Cookie、CSRF、rtk、ID、签名 URL或上游正文。多空间当前只检查首个 mount，扩展必须单独版本化。D-02 决定根列表 401 是否允许 refresh。

### 7.3 网页目录预取与缓存

以下是浏览器文件管理页的行为契约，不是新的对外 API：

1. 当前目录的 `GET /api/v1/entries` 成功后，网页只从该响应中筛选直接子文件夹，不递归预取孙目录。
2. 每次当前目录最多安排 24 个子文件夹预取，顺序与当前目录响应中的文件夹顺序一致。
3. 预取最多同时运行 2 个 `GET /api/v1/entries` 请求；队列不得因为一个请求失败或完成而停止推进。
4. 每个目录的网页内存缓存有效期为 30 秒。有效缓存命中时，点击该文件夹不得再次等待同一目录的 WPS 列表请求；同一目录正在请求时，页面请求应复用 pending 请求。
5. 缓存只保存 `entries` 元数据和过期时间，不保存文件正文、Cookie、CSRF、Basic Auth 或签名 URL。
6. 手动刷新必须强制重新读取当前目录并开始新的预取 generation；成功上传、新建、重命名、移动、删除以及从断开恢复连接时，必须清理网页目录缓存。
7. 清理不要求强行中止已经发出的请求，但必须用 cache epoch、预取 generation 和导航 generation 阻止旧请求写入新缓存或更新当前页面。
8. 预取错误不得写入空数组缓存，也不得把预取错误显示成当前目录为空；用户随后真正打开该目录时，才显示该目录请求的实际错误。
9. 该缓存只存在浏览器页面生命周期内，不改变 REST、WebDAV、WPS 服务端的响应、顺序、分页或 TTL 契约。

## 8. REST 契约

### 8.1 GET

| 路由 | 输入 | 成功响应 |
| --- | --- | --- |
| `status` | 无 | 200 WpsStatus |
| `settings` | 无 | 200 `{status:"ok",name}` |
| `entries`、`list` | query path，默认 `/` | 200 `{path,entries:[...]}` |
| `metadata` | query path | 200 `{path,entry}` |
| `download` | query path，可带 Range | 文件流 200/206 |

对 file 调 entries 返回 409，不伪装为空目录。

### 8.2 PUT

`upload` 与兼容别名 `files`：

- path 为完整文件路径。
- Content-Length 必须存在。
- overwrite 默认 false；真值接受 1/true/yes/on，假值接受 0/false/no/off。
- 成功返回 201 `{path,entry}`。
- REST 默认不覆盖同名文件。

### 8.3 POST

- `folders`、`folder`：path 为新目录完整路径；成功 201 `{path,entry}`。
- `session/import`：见第 16 节。

### 8.4 DELETE

`entries`、`files`、`delete`：成功 204 空 body；根目录不可删。

### 8.5 PATCH

- `settings`：JSON 必须恰好只有 name；成功 200 `{status:"ok",name}`。
- `entries`、`files`：JSON 只能选择一类目标。
- name 或 fname 表示同目录重命名。
- destination 表示完整目标路径。
- parent_path 表示移动到目标父目录并保留原名。
- 同时出现重命名和移动字段、同类出现多个字段、错误类型或空对象返回 400。
- 成功返回 200 `{path:规范化新路径,entry}`。

### 8.6 Entry JSON

公开字段始终为：id、name、kind、parent_id、size、modified_at、etag。内部 link_id 和 raw 不得暴露。

## 9. 路径契约

1. 业务路径必须以 `/` 开头。
2. 根路径规范为 `/`。
3. 尾斜线可规范掉；DAV folder href 再加回。
4. 每个组件不能为空，不能是 `.` 或 `..`。
5. 拒绝反斜线、NUL、控制字符和非法 UTF-8。
6. 每个名称 UTF-8 最多 4096 bytes。
7. 名称按大小写敏感的完整字符串精确匹配。
8. 一个父目录无匹配为 404；多个同名匹配为 409；经过 file 继续下钻为 409。
9. href 对每个组件分别 percent encode。
10. REST 当前可能被 query parser 与业务 parser 二次解码；必须先完成 D-04 特征测试再决定兼容或修正。

## 10. WebDAV 契约

### 10.1 OPTIONS

认证后返回 200：

- `DAV: 1,2`
- Allow 精确包含 OPTIONS、PROPFIND、GET、HEAD、PUT、MKCOL、DELETE、MOVE、COPY、LOCK、UNLOCK。

当前 OPTIONS 不先检查 URL 是否位于 DAV 前缀，Go 是否保持由 golden 决定。

### 10.2 PROPFIND

- Depth 默认 1，只接受 0、1、infinity。
- 当前忽略请求 XML 的 allprop/propname/prop，固定返回属性集合。
- 成功 207、`application/xml; charset=utf-8`、`DAV: 1,2`。
- 每项包含 href、resourcetype、displayname、getcontentlength、getcontenttype、可选 getetag/getlastmodified、propstat 200。
- folder 的 resourcetype 含 collection，href 以 `/` 结尾。
- 遇重复 entry ID 失败，不形成循环。
- 默认最大 10000 项、深度 64、生成响应 16 MiB；超限 507。
- 客户端断开必须停止递归和上游列表。

### 10.3 GET/HEAD

- file GET 为流式下载；folder GET 返回 409。
- file HEAD 返回元数据，不读取对象正文。
- folder HEAD 返回 200、`Content-Type: httpd/unix-directory`、长度 0。
- MIME 由文件名推断；Python 与 Go 表差异需用固定 golden 解决。

### 10.4 PUT/MKCOL/DELETE

- DAV PUT 默认 overwrite=true，与 REST 不同；成功 201 raw entry JSON + Location。
- MKCOL 丢弃有界 body；成功 201 raw entry JSON + Location。
- DELETE 成功 204；根或空间虚拟根禁止删除。

### 10.5 Destination

MOVE/COPY 必须有 Destination。允许 DAV 内相对路径或同适配器绝对 URL。绝对 URL 的 host/port 必须与 Host 一致。拒绝 userinfo、query、fragment、非法端口和 DAV 前缀外路径。

### 10.6 MOVE

- Overwrite 默认 T，只接受 T/F。
- 目标存在且 F：412。
- 目标存在且 T：501，因为当前不做非原子覆盖。
- 同目录改 basename 表示 rename。
- 跨目录必须保持 basename；同时移动并改名返回 501。
- 跨空间返回 501。
- 成功新目标 201、Location、raw entry JSON。

### 10.7 COPY

- Depth 默认 infinity，只接受 0/1/infinity。
- 目标存在：F 为 412，T 为 501；绝不先删除。
- 根、自身、folder 到自身后代禁止。
- 同空间普通文件且 basename 不变时优先已确认原生 COPY。
- 文件改名 COPY 和 folder COPY 使用下载/上传中继。
- folder Depth 0 只建空根；Depth 1 处理直接子项；infinity 递归。
- 受最大条目与深度保护。
- 新建目标根后中途失败，best-effort 删除该新根；不能误删旧目标或源。
- COPY 不是事务，清理失败需脱敏记录。

## 11. Range 与 If-Range

1. 只接受单个 `bytes=` 范围。
2. 支持 start-end、start-、-suffix。
3. end 超出文件尾时 clamp。
4. suffix 大于文件大小时返回全文件但仍为 206。
5. 多范围、无效、越界、未知 size 返回 416，并带 `Content-Range: bytes */N` 或 `*`。
6. If-Range 仅比较 ETag，接受当前带/不带引号形式；不匹配或无 ETag 时忽略 Range 返回完整 200。
7. 不自行增加 If-Range 日期语义。
8. 范围成功返回 206、Accept-Ranges、Content-Range、实际 Content-Length。
9. 对象存储也必须返回 206，且 Content-Range/length 严格一致；否则关闭并返回上游失败。

## 12. DAV Lock

- 仅当前进程内 exclusive write lock，不调用 WPS、不持久化。
- token 为 `opaquelocktoken:<uuid>`。
- 从 If 与 Lock-Token 提取 token。
- Depth 默认 infinity，只接受 0/infinity；infinity 覆盖后代。
- Timeout 默认 Second-3600；Infinite 或过大 clamp 到 86400 秒。
- owner 从 XML 文本提取、压缩空白、最多 512 字符。
- 拒绝 DOCTYPE、ENTITY、畸形 XML和超过 64 KiB。
- 最大 4096 活动锁；超限 503 + Retry-After 5。
- 对象存在的新锁 200，不存在 201，但不创建 WPS 对象。
- refresh 必须唯一有效 token 且同 path；成功 200。
- 被锁写操作 423；无效 refresh/unlock token 409；UNLOCK 成功 204。
- 服务重启或超时后锁消失；不得宣传为分布式或持久锁。

## 13. 多空间

1. 根 ID 为 `multi-space-root`。
2. 每个 mount 以虚拟 folder 暴露，虚拟 ID 为 `space:<group_id>`。
3. 列根只返回 mounts，不访问 WPS。
4. 第一路径组件严格选择空间。
5. 虚拟 folder 不在 WPS 创建同名对象。
6. 根和空间 mount 本身不可写。
7. 跨空间 MOVE/COPY 为 501。
8. workspace 更新后无需重启，必须重建路由并清缓存。
9. Go 目标的上传、下载、spool 与 refresh 预算必须进程级共享，不能按空间倍增。

## 14. HTTP 错误映射

| 错误 | 状态 |
| --- | --- |
| InvalidPath、ValueError、TypeError | 400 |
| EntryNotFound | 404 |
| NotFolder、AlreadyExists、AmbiguousPath | 409 |
| 控制体过大 | 413 |
| 需要 Content-Length | 411 |
| Range 无法满足 | 416 |
| DAV 锁阻止写 | 423 |
| InsufficientStorage | 507 |
| ServiceBusy | 503 + Retry-After 5 |
| Unsupported | 501 |
| WPS 401 | 503 + Retry-After 60，REST code=`wps_session_expired` |
| 其他 WPS HTTP/解析 | 502，REST code=`wps_unavailable` |
| 本地或上游 I/O | 502 固定消息 |
| 未知异常 | 500 固定消息 |

REST WPS 错误可含脱敏 upstream_status 数字，但不得含上游 body、URL 或 ID。

## 15. WPS 控制请求公共契约

1. base/account URL 必须 HTTPS、可信 kdocs.cn host、无 userinfo/query/fragment/额外危险 path。
2. 禁止自动 redirect。
3. 控制请求带当前 Cookie，可选 Referer/Origin。
4. JSON body 使用 application/json；响应必须在上限内且是 JSON object。
5. 每个响应先处理 Set-Cookie，再解析正文。
6. 第一次 401：检测响应轮换、外部 refresh 或 WPS grant；成功后重读凭据、更新 JSON CSRF，最多重试一次。
7. refresh grant 为 account origin 的 `/passport/secure/api/grant_token`，使用 refresh_token grant 形状。
8. 状态检查是否允许触发 refresh 由 D-02 单独决定。

### 15.1 已支持控制端点

| 能力 | 方法与路径 |
| --- | --- |
| islogin | GET account `/api/v3/islogin` |
| 空间候选 | GET `/3rd/plus/groups/v1/companies/<tenant>/users/self/groups/private` |
| 列表 | GET `/3rd/drive/api/v5/groups/<group>/files` |
| folder | POST `/3rd/drive/api/v5/files/folder` |
| rename | PUT `/3rd/drive/api/v3/groups/<group>/files/<file>` |
| move | POST `/3rd/drive/api/v5/files/batch/task/move` |
| delete | POST `/3rd/drive/api/v5/files/batch/task/delete` |
| task progress | GET `/3rd/drive/api/v5/files/batch/task/progress` |
| native file copy | POST `/3rd/drive/api/v3/groups/<group>/files/batch/copy` |
| upload precheck | GET `/3rd/drive/api/v5/files/upload/pre_check` |
| normal instruction | PUT `/3rd/drive/api/v5/files/upload/create_update` |
| multipart init/part | POST/PUT `/3rd/drive/api/v5/files/upload/block` |
| multipart merge | POST `/3rd/drive/api/v5/files/upload/block/merge` |
| final register | POST `/3rd/drive/api/v5/files/file` |
| download resolve | GET `/api/v3/office/file/<file>/download` |

请求字段和类型必须逐项以 Python client 和脱敏 fixture 为准。表中存在端点不代表可以扩大到批量、跨 group 或覆盖场景。

## 16. Session import 与旧登录助手兼容

`POST /api/v1/session/import` 必须继续被当前 `wps_login.py` 调用：

1. 使用适配器 Basic Auth。
2. 不接受 redirect 后的导入。
3. body 最大 512 KiB。
4. cookies 必须为非空数组，最多 256 项。
5. 服务端重新校验 name/value/domain/path，不信任 helper。
6. 只接受 WPS 允许域，必须得到 csrf 和 rtk。
7. workspace ID 满足 `[A-Za-z0-9._-]{1,256}`。
8. spaces 最多 128，名称唯一且满足安全规则。
9. 只有批准的 auto 配置允许更新映射。
10. Cookie/CSRF 成对原子替换；workspace 后续写入失败不能形成静默成功。
11. 成功后热切换 root/mount 并清缓存，不重启。
12. 成功 JSON 只含 status、cookie_count 和可选 workspace=`updated`。

## 17. Secret 与状态文件

| 文件 | 默认位置 | 上限/规则 |
| --- | --- | --- |
| Cookie | `/etc/wps-adapter/secrets/wps-cookie` | 最大 4 MiB，0600 |
| CSRF | `/etc/wps-adapter/secrets/wps-csrf` | 最大 4 MiB，0600 |
| workspace | `/etc/wps-adapter/secrets/wps-workspace.json` | 16 KiB，最多 128 spaces，0600 |
| web settings | `/etc/wps-adapter/secrets/web-settings.json` | 16 KiB，name 256 字符/1024 bytes，0600 |
| multipart checkpoint | `/var/lib/wps-adapter/uploads/<hash>.json` | 不含正文/凭据，0600 |

生产 Linux 必须要求绝对路径、真实私有父目录、root 或服务 uid owner、非 symlink 普通文件、无 group/world 权限。写入使用同目录临时文件、fsync 和原子 rename。状态文件可在运行中热加载。

## 18. 签名对象存储

1. 签名 URL 必须 HTTPS。
2. host 必须等于或属于配置的可信 WPS 对象存储 suffix。
3. 端口只能省略或 443。
4. 拒绝 userinfo、fragment、host 混淆和 redirect。
5. 使用与 WPS control 完全独立的 HTTP client。
6. 绝不带 WPS Cookie、CSRF 或适配器 Authorization。
7. 普通对象控制响应和 merge XML 分别有严格大小上限。

## 19. 上传状态机

### 19.1 共同前置

- 先检查声明长度、目标冲突和全局上传/磁盘预算。
- 边读边 spool 并计算 MD5、SHA-1、SHA-256。
- 默认内存阈值 8 MiB，之后落临时磁盘。
- 实际长度必须等于声明长度。
- 上传结束或失败清理 spool、槽和预留。

### 19.2 普通上传

1. pre_check。
2. create_update 获取 signed PUT 指令。
3. 从 spool 开头上传对象；失败重新取签名并有限重试。
4. 要求对象 200 和 ETag。
5. POST file 登记正式文件。
6. 登记成功后才向客户端报告成功。

### 19.3 Multipart

1. 默认 50 MiB 起用，期望片 10 MiB，单片硬上限 64 MiB。
2. multipart overwrite 暂不支持。
3. block init 获取 upload_id/key/store/limit。
4. 每片计算 hex MD5 和 Base64 Content-MD5，获取并验证 signed 指令，再 PUT 并保存 ETag。
5. 可选 checkpoint 只保存会话与已完成片，不保存正文。
6. 仅已确认的失效状态允许重建 session，且必须清空旧 parts。
7. merge 指令必须是已确认 XML 形状，拒绝 DTD/entity。
8. 合并后 POST file 登记，成功才删 checkpoint。

## 20. 下载状态机

1. 用 metadata 确认 file，并取得 id/size/etag/link_id。
2. `link_id` 优先作为 cid，配置 cid 只作后备。
3. 调 download resolve；缺省形态 403 时只用已观察 direct=true 重试一次。
4. 验证 signed URL 后使用独立 client。
5. full/range 内容按块转发，不整体缓存。
6. 客户端取消关闭上游并释放下载槽。
7. 内容完整性由长度、Range metadata 和端到端 SHA-256 测试保证。

## 21. 暂不支持与禁止推断

- 跨空间 MOVE/COPY。
- folder 的 WPS 原生 COPY。
- COPY/MOVE 覆盖既有目标。
- multipart overwrite。
- 跨目录同时移动并改名。
- 多范围响应。
- 持久或分布式 DAV lock。
- 进程退出后不重新提供源正文的真正断点续传。
- 未确认的快速上传成功路径、取消/清理 API、原子冲突 API。

## 22. 契约完成门禁

- [ ] 每个 REST 正式路由和别名至少有成功/失败测试。
- [ ] 每个 DAV 方法都有 raw HTTP 黑盒测试。
- [ ] path、Destination、Depth、Range、If-Range、Overwrite、LOCK parser 有边界矩阵。
- [ ] WPS fixture 检查 method/path/query/JSON 字段/顺序/重试。
- [ ] signed fixture 证明凭据隔离和 redirect 拒绝。
- [ ] Python/Go 状态码、关键头、JSON/XML 语义、body hash 无未批准差异。
- [ ] 成功、失败、取消路径均证明资源释放。
- [ ] 日志与测试产物 secret 扫描为零。
