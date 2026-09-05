# Go 重构计划 01：当前系统全景

## 1. 文档目的

本文只描述当前 Python 版本已经存在的结构和行为，不描述 Go 的具体实现代码。

本文有四个目标：

1. 让后续执行者在修改任何代码以前知道每个模块负责什么。
2. 让后续执行者能沿着一次请求追到 WPS 控制接口和对象存储。
3. 让后续执行者知道哪些状态会写入磁盘、哪些状态只存在于内存。
4. 把源码、文档和测试之间已经发现的不一致提前列出，防止重构时无意改变行为。

本文对应的当前版本为 `0.9.8`，版本号证据见 `src/wps_adapter/__init__.py:3` 和 `pyproject.toml:7`。

## 2. 项目用途和运行形态

项目把一个已授权的 WPS 企业云盘会话转换为三种本地访问方式：

| 访问方式 | 默认入口 | 当前职责 |
| --- | --- | --- |
| 浏览器文件管理页 | `/` | 浏览、上传、下载、新建文件夹、重命名、移动、删除、修改显示名称、查看 WPS 状态 |
| WebDAV | `/dav/` | 向桌面、移动端、NAS 和同步客户端提供文件协议兼容层 |
| REST | `/api/v1/` | 向网页和脚本提供路径型 JSON 接口及文件流接口 |
| 进程健康检查 | `/healthz` | 只确认适配器进程可响应，不访问 WPS |

当前服务是单进程、每连接一个线程的 Python HTTP/1.1 服务。运行时不依赖第三方 Python 包，`pyproject.toml:11-12` 要求 Python 3.11 或更高版本并声明空依赖列表。

服务并不长期保存云盘文件正文。目录元数据会短时缓存在内存中；上传正文会为了计算 WPS 所需校验和而短时进入内存或临时文件；下载正文从 WPS 对象存储按块转发给调用方。

当前存在两层互相独立的目录缓存：服务端 `WpsStorage` 的 WPS 元数据缓存默认 2 秒，网页 `web.py` 中的浏览器内存缓存默认 30 秒。网页缓存只保存 `GET /api/v1/entries` 返回的条目元数据，不保存文件正文、Cookie、CSRF 或签名对象存储 URL；它不是 HTTP 响应缓存，也不改变 REST/WebDAV 的服务端缓存契约。

## 3. 仓库中与服务有关的文件

### 3.1 核心运行模块

| 文件 | 大致规模 | 当前职责 | 主要调用者 | 主要下游 |
| --- | ---: | --- | --- | --- |
| `src/wps_adapter/__main__.py` | 159 行 | 命令行解析、读取环境变量、组装应用、启动服务 | Python 模块入口、console script | client、storage、server、login command |
| `src/wps_adapter/server.py` | 1775 行 | HTTP/1.1、REST、WebDAV、Basic Auth、同源写保护、Range、DAV 锁、错误映射 | `__main__.py` | storage、settings、login cookie parser、web renderer |
| `src/wps_adapter/storage.py` | 801 行 | 路径解析、名称冲突、WPS ID 查找、元数据缓存、多空间路由、COPY 中继 | server | client、provider |
| `src/wps_adapter/client.py` | 2589 行 | WPS 控制 API、Cookie/CSRF、401 续期、对象存储上传下载、multipart | storage、status、login 类型 | WPS 账号服务、WPS 云盘服务、对象存储、状态文件 |
| `src/wps_adapter/provider.py` | 104 行 | 统一条目模型、领域错误、抽象存储协议 | server、storage、client | 无 |
| `src/wps_adapter/workspace.py` | 328 行 | 单空间和多空间配置文件校验、热加载、原子持久化 | client、server、storage | 工作区 JSON 文件 |
| `src/wps_adapter/settings.py` | 209 行 | 浏览器显示名称校验、热加载、原子持久化 | `__main__.py`、server | web settings JSON 文件 |
| `src/wps_adapter/login.py` | 1548 行 | 隔离 Chrome 登录、Cookie 筛选、空间发现和验证、凭据同步 | login command、session import parser | Chrome CDP、WPS、SSH、适配器导入接口 |
| `src/wps_adapter/login_command.py` | 315 行 | 登录命令参数、交互问答、目标选择、错误输出 | `__main__.py`、独立脚本生成器 | login |
| `src/wps_adapter/web.py` | 975 行 | 以内嵌字符串保存浏览器页面的 HTML、CSS、JavaScript | server | 浏览器 REST 请求 |

### 3.2 辅助模块和生成物

| 文件或目录 | 性质 | 重构时的处理原则 |
| --- | --- | --- |
| `wps_login.py` | 由两个登录源模块拼接生成的独立脚本 | 不要手工修改；第一阶段可继续保留 Python 版本 |
| `tools/build_login_script.py` | 生成并校验 `wps_login.py` | 只要登录助手仍保留 Python，就继续作为发布校验的一部分 |
| `src/wps_adapter/har.py` | HAR 结构检查和保守脱敏 | 不在常驻服务请求链中；不能误删安全研究能力 |
| `tools/wps_probe.py` 等 | WPS 请求研究和安全探测工具 | 不能作为生产服务依赖；真实凭据不得进入测试样本 |
| `tests/` | 单元、集成和 HTTP 黑盒性质测试 | Go 重构前应提取语言无关用例，而不是只翻译断言语法 |
| `deploy/` | systemd、Docker、Compose 模板 | 最终必须改为运行 Go 产物，同时保留路径、用户和权限语义 |
| `scripts/` | 一键安装、卸载脚本 | 用户入口，切换后仍应保持相同操作步骤和回滚能力 |

## 4. 命令入口

命令入口注册于 `pyproject.toml:30-31`，实际实现位于 `src/wps_adapter/__main__.py:76-159`。

| 命令 | 当前行为 | 网络访问 | 退出行为 |
| --- | --- | --- | --- |
| `--version` | 输出当前版本 | 无 | 成功退出 |
| `serve` | 组装完整应用并启动 HTTP 服务 | 按客户端请求访问 WPS | Ctrl-C 返回 0；配置或监听失败返回 1 |
| `check-config` | 构造并验证配置，输出配置摘要 | 不访问 WPS | 成功返回 0；无效配置返回 1 |
| `login` | 启动本地隔离 Chrome 登录和同步流程 | 访问官方 WPS，并按选择同步到目标 | 成功返回 0；取消或安全错误返回 1 |

`serve` 默认监听 `127.0.0.1:54321`。当 bind 不是 `127.0.0.1`、`localhost` 或 `::1` 且 Basic Auth 完全没有启用时，入口会拒绝启动，证据见 `src/wps_adapter/__main__.py:94-99,132`。

启动成功后会向标准输出打印监听地址、WebDAV 地址和 REST 地址。这里只包含适配器地址，不包含 Cookie、CSRF 或签名 URL，证据见 `src/wps_adapter/__main__.py:140-145`。

## 5. 应用组装顺序

当前组合根是 `src/wps_adapter/__main__.py:30-73`。后续重构必须先理解以下顺序，因为后面的对象持有前面对象的状态引用：

1. `WpsClientConfig.from_env()` 读取 WPS、凭据、工作区、上传和状态探测配置。
2. 如果配置允许自动工作区或已有工作区文件，则创建 `WorkspaceState`。
3. 如果配置了 Cookie 文件、CSRF 文件或外部刷新命令，则创建 `FileCredentialSource`。
4. 创建根显示名称设置对象 `WebSettings`，并读取保存值或环境变量回退值。
5. 创建一个基础 `WpsDriveClient`。
6. 从 `WorkspaceState.spaces` 读取挂载集合；没有 workspace 时集合为空。
7. 无条件创建 `MultiSpaceStorage`，即使只选择了一个空间。
8. `MultiSpaceStorage` 为每个 mount 派生一个固定 group 的子 `WpsDriveClient` 和 `WpsStorage`。
9. 创建 `BasicAuth`、`DavLockStore` 和 `AdapterApplication`。
10. `create_server()` 创建 `AdapterHTTPServer`，随后 `serve_forever()` 开始接受连接。

重要所有权关系：

| 所有者 | 持有的状态 | 生命周期 |
| --- | --- | --- |
| `AdapterHTTPServer` | 应用引用、连接 semaphore、请求 timeout | 整个服务进程 |
| `AdapterApplication` | storage、auth、DAV lock store、协议限制、web settings | 整个服务进程 |
| `MultiSpaceStorage` | 当前 mounts、每空间 storage 映射、根显示名 | 整个服务进程，可因文件变化重建子对象 |
| 每个 `WpsStorage` | 元数据缓存、上传槽、下载槽 | mount 未变化期间 |
| 每个 `WpsDriveClient` | 401 刷新锁、status cache/singleflight、spool 预留计数 | 对应 client 生命周期 |
| 每个 HTTP handler | 当前 socket、请求头、读写流 | 单连接内的请求处理 |

## 6. 请求总调用链

### 6.1 所有请求共有的前置处理

一次请求先经过以下阶段：

1. Python HTTP server 解析请求行和请求头。
2. handler 拒绝 `Transfer-Encoding`、重复 `Content-Length` 和不允许携带正文的方法。
3. socket 使用全局请求超时。
4. `/healthz` 直接绕过 Basic Auth；其余接口按配置认证。
5. 写方法执行 Origin 或 Referer 同源检查。
6. 根据 path 判断浏览器页面、REST、WebDAV 或未知路由。
7. 协议层验证 path、query、Destination、Depth、Range、JSON 或 XML。
8. 协议层调用 storage，不直接拼接 WPS 私有请求。
9. storage 把人类路径解析为 WPS entry ID 和 parent ID。
10. client 读取本次请求使用的最新凭据并调用 WPS。
11. 结果被转换为 REST JSON、WebDAV XML、响应头或文件流。
12. 错误在 handler 统一映射，不能把上游正文直接返回给调用方。

### 6.2 列目录和元数据

1. server 从 REST query 或 DAV path 得到远端绝对路径。
2. `split_remote_path()` 做一次严格 URL 解码和组件校验。
3. 多空间层用第一段名称选择 mount，并把剩余路径交给对应 `WpsStorage`。
4. `WpsStorage` 从虚拟根开始，逐级列父目录并按名称精确匹配。
5. 每个父目录的完整分页结果可在内存缓存默认 2 秒。
6. client 调用 WPS v5 文件列表接口，并将 WPS 字段归一化为 `RemoteEntry`。
7. REST 返回条目数组；PROPFIND 再生成固定 DAV 属性集合。

网页当前目录成功加载后，会从这次返回的条目中按原顺序筛选 `kind=folder` 的直接子文件夹，最多安排 24 个后台 `entries` 请求，最多同时执行 2 个请求。有效的网页缓存命中会直接用于打开目录；预取失败只丢弃该缓存项，不能把失败结果写成空目录。刷新会强制重读当前目录并开始新的预取 generation，写操作和 WPS 重新连接会清理网页缓存；旧请求即使稍后完成，也不能覆盖新导航或重新建立已失效的缓存。

证据：`src/wps_adapter/storage.py:237-305`、`src/wps_adapter/client.py:1384-1541`、`src/wps_adapter/server.py:866-1072`。

### 6.3 下载

1. server 先经目录列表解析目标元数据。
2. 目标必须是 file；folder 不进入对象存储流程。
3. server 根据客户端 Range 和 entry size 计算 offset 和 length。
4. storage 获取一个下载槽，超时则返回忙碌错误。
5. client 调 WPS 下载解析接口获取短期签名 URL。
6. client 严格检查签名 URL 的 scheme、host suffix、端口和凭据部分。
7. client 向对象存储发 GET；这个请求不带 WPS Cookie。
8. server 按配置块大小将对象响应写给客户端。
9. 客户端断开、写失败、正常 EOF 或异常路径最终都关闭对象响应并释放下载槽。

证据：`src/wps_adapter/server.py:762-827`、`src/wps_adapter/storage.py:375-397`、`src/wps_adapter/client.py:2466-2561`。

### 6.4 普通上传

1. server 要求 `Content-Length`，并在读取正文前检查声明长度上限。
2. storage 解析 parent，列目录并检查同名冲突和 overwrite 规则。
3. storage 获取上传槽。
4. client 边读请求正文边写 spool，同时计算 MD5、SHA1 和 SHA256。
5. spool 在默认 8 MiB 内存阈值内驻留内存，超过阈值后完整转入临时文件。
6. client 先调用 upload pre-check。
7. 文件小于 multipart 阈值时，client 获取普通上传签名指令。
8. client 从 spool 开头向对象存储 PUT；失败重试前重新获取签名指令并重新 seek。
9. 对象存储返回 ETag 后，client 向 WPS 注册正式文件。
10. 成功 mutation 清空目录缓存；任意退出路径释放上传槽并关闭、删除 spool。

证据：`src/wps_adapter/storage.py:322-373`、`src/wps_adapter/client.py:2258-2464`。

### 6.5 multipart 上传

1. 前四步与普通上传相同，因此大文件也先完整 spool 和计算整体哈希。
2. client 初始化 WPS block upload，读取 upload ID、key、store 和服务端限制。
3. client 计算最终 part size；单片内存硬上限为 64 MiB。
4. 每一片先读入 bytes、计算 MD5，再从 WPS 获取该片签名指令。
5. client 验证指令中的 method、body type、Content-MD5、Content-Type 和预期状态码。
6. client 向对象存储 PUT 该片，保存 ETag，并可写 checkpoint。
7. 全部分片完成后，client 获取 merge 指令。
8. client 把 WPS 提供的 XML 发给对象存储，解析合并响应 ETag。
9. client 向 WPS 注册最终文件；成功后删除 checkpoint。

证据：`src/wps_adapter/client.py:1977-2256`。

### 6.6 重命名、移动、删除和复制

| 操作 | storage 的前置判断 | WPS 或中继行为 |
| --- | --- | --- |
| 重命名 | 不能改 root；同目录不能有同名其他项 | 调 WPS v3 单条目 PUT |
| 移动 | 不能移 root、不能移入自己；目标父目录必须存在且无同名项 | 提交 WPS v5 batch move task 并轮询成功 |
| 删除 | 不能删 root | 提交 WPS v5 batch delete task 并轮询成功 |
| 同名普通文件复制 | 目标不存在、同空间、目标 basename 与源相同 | 优先使用已确认的 WPS v3 batch copy |
| 改名文件复制 | 目标不存在、同空间 | 下载后重新上传 |
| 文件夹复制 | 目标不存在、同空间、不能复制到自己后代 | 按 Depth 有界递归创建和中继文件 |

任何成功 mutation 都会清空对应 `WpsStorage` 的全部目录缓存。文件夹 COPY 中途失败会尽力删除本次新建的目标根，但清理失败不会覆盖原始异常。

网页层还会在这些 mutation 成功后清理自己的 30 秒目录缓存，并强制重新加载当前目录。服务端缓存失效和浏览器缓存失效必须分别实现，不能因为浏览器命中缓存就跳过 WPS 写后的一致性处理。

证据：`src/wps_adapter/storage.py:430-668`、`src/wps_adapter/client.py:1771-1975`。

### 6.7 状态探测

1. `/api/v1/status` 读取凭据和当前工作区。
2. 缺文件、空 Cookie 或空 group 时返回 `not_configured`，不应把进程健康误报为 WPS 已连接。
3. client 先调用账号服务 `islogin`。
4. 登录检查成功后，对当前 root 做一次 count 为 1 的目录列表。
5. 结果被归类为 connected、session expired、permission denied、upstream unavailable 或 invalid response。
6. 成功默认缓存 30 秒，失败默认缓存 5 秒。
7. 并发调用共享一次正在执行的探测。
8. 缓存 key 含 Cookie、CSRF、group ID 和 root ID，凭据或工作区变化会使旧结果失效。

证据：`src/wps_adapter/client.py:832-1191`、`src/wps_adapter/server.py:284-308,1276-1280`。

### 6.8 网页设置更新

1. GET settings 只读本地状态，不访问 WPS。
2. PATCH settings 要求 JSON 只有 `name` 一个字段。
3. name 被 trim 并执行长度、字节数和控制字符校验。
4. 新值原子写入 web settings 文件。
5. 应用内根名称和 storage 虚拟根名称立即更新。
6. 下一次网页渲染、REST 根元数据和 DAV displayname 使用新名称。

证据：`src/wps_adapter/settings.py:29-41,117-196`、`src/wps_adapter/server.py:264-282,1281-1286,1438-1447`。

### 6.9 登录和会话导入

1. 登录助手在本地创建隔离 Chrome 临时 profile。
2. Chrome 调试端口只绑定 loopback。
3. 用户只在官方 WPS HTTPS 页面完成登录、SSO、验证码或风控。
4. helper 通过 CDP 读取当前页面 URL 和 Chrome Cookie，不读取或代填密码。
5. helper 只保留允许 WPS 域、同时匹配当前 drive host 的 Cookie。
6. helper 要求 Cookie 中同时存在 `csrf` 和 `rtk`。
7. helper 从当前页面或账号空间发现接口得到候选空间。
8. 每个最终选择必须先通过 WPS 文件列表接口做只读验证。
9. helper 通过适配器 HTTP(S)、SSH stdin 或本地文件三种方式之一同步。
10. server 重新校验 Cookie 域、工作区标识、空间数量和空间名称唯一性。
11. server 先更新 Cookie/CSRF pair，再更新 workspace，并让后续请求热加载新状态。

证据：`src/wps_adapter/login.py:108-286,910-1382,1385-1548`、`src/wps_adapter/server.py:1305-1381`。

## 7. 主要数据模型

### 7.1 远端条目

`RemoteEntry` 定义于 `src/wps_adapter/provider.py:48-60`。

| 字段 | 类型语义 | 来源 | 是否向 REST 暴露 | 特别说明 |
| --- | --- | --- | --- | --- |
| `id` | 字符串 | WPS `id` | 是 | 即使 WPS 返回数字也转字符串 |
| `name` | 字符串 | WPS `fname` 或虚拟根名称 | 是 | 经过路径组件安全校验 |
| `kind` | file、folder、unknown | WPS `ftype` | 是 | unknown 不参与正常路径匹配 |
| `parent_id` | 字符串或空 | WPS `parentid` | 是 | 虚拟根为空 |
| `size` | 非负整数或空 | WPS `fsize` | 是 | 非法值归一为空 |
| `modified_at` | 字符串或空 | WPS `mtime` | 是 | DAV 层尝试按 Unix 秒格式化 |
| `etag` | 安全字符串或空 | WPS `fsha` | 是 | 超长或控制字符值被丢弃 |
| `link_id` | 字符串或空 | WPS `link_id` | 否 | 下载时优先作为 `cid` |
| `raw` | 映射 | 预留 | 否 | 当前正常映射没有保留 WPS 原始正文 |

### 7.2 WPS 状态

`WpsStatus` 定义于 `src/wps_adapter/client.py:637-666`。

| 字段 | 允许值或含义 |
| --- | --- |
| `status` | connected、not_configured、session_expired、permission_denied、upstream_unavailable、invalid_response |
| `wps` | connected、not_configured、session_expired 或 unknown |
| `workspace` | ready、not_configured、permission_denied 或 unknown |
| `account_type` | business、personal 或 unknown |
| `last_checked_at` | Unix 秒或空 |
| `retry_after` | 非负秒数；成功通常为 0，缓存失败时为剩余退避时间 |

### 7.3 工作区挂载

`WorkspaceMount` 定义于 `src/wps_adapter/workspace.py:27-41`。

| 字段 | 约束 | 用途 |
| --- | --- | --- |
| `group_id` | `[A-Za-z0-9._-]{1,256}` | 选择 WPS group |
| `root_id` | 同上，默认 `0` | 选择 group 内映射根 |
| `name` | 非空、无 `/` 和反斜杠、UTF-8 最多 4096 bytes | 作为虚拟根下第一段路径 |

### 7.4 认证和锁

| 模型 | 关键字段 | 是否持久化 |
| --- | --- | --- |
| `WpsCredentials` | Cookie、CSRF | 通过 credential source 文件持久化 |
| `BasicAuth` | username、password 或各自文件路径 | 值由外部 secret 文件持久化 |
| `ActiveLock` | token、canonical path、depth、owner、timeout、monotonic expiry | 否，只在当前进程内 |

## 8. 磁盘状态文件

### 8.1 状态文件总表

| 状态 | 默认路径 | 格式 | 读取时机 | 写入者 | 权限目标 |
| --- | --- | --- | --- | --- | --- |
| WPS Cookie | `/etc/wps-adapter/secrets/wps-cookie` | 单行 Cookie header | 每次 WPS 请求 | session import、登录助手、Set-Cookie rotation | 私有父目录，普通文件 0600 |
| WPS CSRF | `/etc/wps-adapter/secrets/wps-csrf` | 单行 token | 每次写请求所需 | session import、登录助手、Set-Cookie rotation | 同上 |
| 工作区 | `/etc/wps-adapter/secrets/wps-workspace.json` | JSON object | 通过 mtime 按请求热加载 | session import、登录助手 | 私有父目录，普通文件 0600 |
| 网页设置 | `/etc/wps-adapter/secrets/web-settings.json` | JSON object | 通过 mtime 按请求热加载 | REST settings | 私有父目录，普通文件 0600 |
| multipart checkpoint | `/var/lib/wps-adapter/uploads/<hash>.json` | JSON object | multipart 请求开始时 | upload client | 目录 0700、文件 0600 |
| 上传 spool | 系统 temp 或 `WPS_UPLOAD_SPOOL_DIR` | 临时二进制 | 单次上传期间 | upload client | 请求结束自动删除 |

### 8.2 凭据文件规则

规则实现位于 `src/wps_adapter/client.py:63-133,196-424`。

1. 路径必须是绝对路径且不能含 NUL。
2. 父目录的 real path 必须等于 absolute path，禁止借父目录 symlink 跳转。
3. 父路径必须是目录，且 group/world 权限全部关闭。
4. owner 必须是 root 或当前进程 uid。
5. 文件必须是普通文件，不能是 symlink，group/world 权限全部关闭。
6. 单个凭据文件最多读取 4 MiB。
7. Cookie 和 CSRF 值不能包含 HTTP 控制字符。
8. 原子写使用目标同目录临时文件、0600、关闭后 replace。
9. WPS 返回多个 Set-Cookie 时按 cookie name 合并，过期 cookie 删除。
10. 如果 Set-Cookie 包含 csrf，同步更新独立 CSRF 文件。
11. pair replace 的第二次写失败时，代码会尽力恢复旧 Cookie 和旧 CSRF。

### 8.3 工作区文件规则

规则实现位于 `src/wps_adapter/workspace.py:16-318`。

1. 文件最大 16 KiB。
2. 兼容旧格式 `group_id`、`root_id`。
3. 新格式可增加 `spaces` 数组，每项包含 group、root、name。
4. spaces 必须非空且最多 128 项。
5. group 不能重复。
6. 当前 `WorkspaceState` 本身不检查 space name 重复；`MultiSpaceStorage` 构造时会检查。
7. `configured_group_id` 为固定值时，文件中的 top-level group 不覆盖它。
8. `configured_root_id` 为固定值时，文件中的 top-level root 不覆盖它。
9. `spaces` 数组仍会被加载，因此固定 top-level 配置和动态 mounts 可以产生组合行为。
10. 读取时通过 mtime 避免每次都完整解析；mtime 改变后重新校验。
11. 写入使用 0600 临时文件、fsync、replace。

### 8.4 网页设置文件规则

规则实现位于 `src/wps_adapter/settings.py:14-196`。

1. 默认文件路径目前是代码常量，没有对应环境变量。
2. 文件最大 16 KiB。
3. JSON 必须是 object 且必须提供合法 `name`。
4. 名称 trim 后非空，最多 256 个字符、UTF-8 最多 1024 bytes。
5. 名称不能包含 C0 控制字符或 DEL。
6. 文件不存在或为空时使用 `WPS_ROOT_NAME` 回退值。
7. mtime 改变时热加载。
8. 写入使用 0600、fsync、replace。

### 8.5 multipart checkpoint

规则实现位于 `src/wps_adapter/client.py:2010-2045,2066-2075,2251-2255`。

| 字段 | 含义 |
| --- | --- |
| `version` | 当前固定为 1 |
| `identity` | group、parent、name、size、sha1 组成的逻辑身份 |
| `upload_id` | WPS multipart 会话标识 |
| `key` | WPS 对象 key |
| `store` | 对象存储标识 |
| `part_size` | 本次会话分片大小 |
| `parts` | part number 字符串到 ETag 的映射 |

checkpoint 文件名是 identity 的 SHA256。checkpoint 不保存正文，因此重启后调用方仍需重新发送完整文件；代码会重新 spool 正文并跳过已有 ETag 的分片。成功注册文件后删除 checkpoint，失败时可能保留以便后续复用。

## 9. 环境变量完整清单

以下默认值以 `src/wps_adapter/__main__.py:20-71`、`src/wps_adapter/client.py:535-607` 和 `.env.example` 为准。

### 9.1 适配器 HTTP 层

| 变量 | 默认值 | 当前含义 |
| --- | --- | --- |
| `ADAPTER_BIND` | `127.0.0.1` | 监听地址 |
| `ADAPTER_PORT` | `54321` | 监听端口，必须 1 至 65535 |
| `ADAPTER_DAV_PREFIX` | `/dav` | DAV 路由前缀 |
| `ADAPTER_REST_PREFIX` | `/api/v1` | REST 路由前缀 |
| `ADAPTER_USERNAME` | 空 | 适配器 Basic Auth 用户名 |
| `ADAPTER_PASSWORD` | 空 | 适配器 Basic Auth 密码 |
| `ADAPTER_USERNAME_FILE` | 空 | 用户名 secret 文件；设置时优先于直接值 |
| `ADAPTER_PASSWORD_FILE` | 空 | 密码 secret 文件；设置时优先于直接值 |
| `ADAPTER_MAX_CONNECTIONS` | `64` | 同时接受的连接槽数 |
| `ADAPTER_REQUEST_TIMEOUT` | `60` | socket 请求超时秒数 |

### 9.2 WPS 定位和网络

| 变量 | 默认值 | 当前含义 |
| --- | --- | --- |
| `WPS_GROUP_ID` | 空；部署模板常写 `auto` | 固定 group 或从 workspace 文件自动读取 |
| `WPS_ROOT_ID` | `0`；部署模板常写 `auto` | 固定 root 或从 workspace 文件自动读取 |
| `WPS_WORKSPACE_FILE` | `/etc/wps-adapter/secrets/wps-workspace.json` | 工作区状态文件 |
| `WPS_ROOT_NAME` | `WPS Enterprise Drive` | 根显示名称回退值 |
| `WPS_BASE_URL` | `https://365.kdocs.cn` | WPS drive 控制接口 origin |
| `WPS_ACCOUNT_BASE_URL` | 空 | 空时从 drive host 推导 account origin |
| `WPS_OBJECT_STORAGE_HOST_SUFFIX` | `.ag.kdocs.cn` | 允许的签名对象存储 host suffix |
| `WPS_REFERER` | 空 | 可选 WPS 控制请求 Referer |
| `WPS_ORIGIN` | 空 | 可选 WPS 控制请求 Origin |
| `WPS_CID` | 空 | entry 没有 link_id 时的下载 cid 后备值 |
| `WPS_TIMEOUT` | `30` | WPS 和对象请求超时秒数 |

### 9.3 WPS 凭据和刷新

| 变量 | 默认值 | 当前含义 |
| --- | --- | --- |
| `WPS_COOKIE` | 空 | 直接 Cookie 值；不推荐在生产使用 |
| `WPS_CSRF_TOKEN` | 空 | 直接 CSRF 值；不推荐在生产使用 |
| `WPS_COOKIE_FILE` | 空；部署模板指定 secrets 路径 | Cookie 文件 |
| `WPS_CSRF_TOKEN_FILE` | 空；部署模板指定 secrets 路径 | CSRF 文件 |
| `WPS_AUTO_REFRESH` | `true` | 上游 401 时是否尝试 WPS refresh grant |
| `WPS_CREDENTIAL_REFRESH_COMMAND` | 空 | 在 grant 前运行的可选本地凭据刷新命令 |
| `WPS_CREDENTIAL_REFRESH_TIMEOUT` | `30` | 外部刷新命令超时秒数 |

### 9.4 状态、目录和缓存

| 变量 | 默认值 | 当前含义 |
| --- | --- | --- |
| `WPS_STATUS_PROBE_TTL` | `30` | connected 状态缓存秒数，0 表示不缓存 |
| `WPS_STATUS_FAILURE_BACKOFF` | `5` | 失败状态退避秒数 |
| `WPS_LIST_COUNT` | `20` | WPS 单页目录条目数 |
| `WPS_MAX_LIST_ENTRIES` | `10000` | 一个目录最多加载条目数 |
| `WPS_CACHE_TTL` | `2` | 目录元数据缓存秒数 |
| `WPS_MAX_CACHED_FOLDERS` | `1024` | 每个 storage 最多缓存文件夹数 |
| `WPS_MAX_JSON_RESPONSE_BYTES` | `8388608` | 单个 WPS JSON 控制响应最大字节数 |

### 9.5 上传、下载和复制

| 变量 | 默认值 | 当前含义 |
| --- | --- | --- |
| `WPS_UPLOAD_SPOOL_MEMORY` | `8388608` | spool 留在内存的阈值 |
| `WPS_STREAM_CHUNK_SIZE` | `1048576` | 读写文件流的块大小 |
| `WPS_UPLOAD_SPOOL_DIR` | 空 | 空时使用系统临时目录 |
| `WPS_UPLOAD_RESUME_DIR` | 空；部署模板建议 `/var/lib/wps-adapter/uploads` | multipart checkpoint 目录 |
| `WPS_UPLOAD_MIN_FREE_BYTES` | `536870912` | spool 文件系统必须保留的空闲量 |
| `WPS_MAX_UPLOAD_BYTES` | `1073741824` | 单文件上限，0 取消适配器上限 |
| `WPS_UPLOAD_RETRIES` | `2` | 普通签名 PUT 和单片失败后的重试次数 |
| `WPS_UPLOAD_RETRY_DELAY` | `0.5` | 指数退避基础秒数 |
| `WPS_MULTIPART_THRESHOLD` | `52428800` | 达到该大小时走 multipart |
| `WPS_MULTIPART_PART_SIZE` | `10485760` | 期望分片大小，服务端限制可抬高 |
| `WPS_ENABLE_RANGE` | `true` | 是否允许对象存储 Range |
| `WPS_MAX_UPLOADS` | `2` | 每个当前 `WpsStorage` 的上传槽数 |
| `WPS_MAX_DOWNLOADS` | `4` | 每个当前 `WpsStorage` 的下载槽数 |
| `WPS_TRANSFER_WAIT_TIMEOUT` | `30` | 等待上传或下载槽的秒数 |
| `WPS_MAX_COPY_ENTRIES` | `10000` | 单次 COPY 最多展开条目数 |
| `WPS_MAX_COPY_DEPTH` | `64` | COPY 最大递归层级 |

### 9.6 HTTP 控制面资源限制

| 变量 | 默认值 | 当前含义 |
| --- | --- | --- |
| `WPS_MAX_PROPFIND_ENTRIES` | `10000` | 一次 PROPFIND 最多响应条目数 |
| `WPS_MAX_PROPFIND_DEPTH` | `64` | PROPFIND 最大递归层级 |
| `WPS_MAX_LOCKS` | `4096` | 进程内活动 DAV 锁上限 |
| `WPS_MAX_CONTROL_BODY` | `1048576` | 普通 JSON、被丢弃请求体等最大长度 |
| `WPS_MAX_RESPONSE_BODY_BYTES` | `16777216` | 生成的 JSON 或 XML 响应最大长度 |

### 9.7 登录助手环境变量和参数

| 名称 | 类型 | 当前含义 |
| --- | --- | --- |
| `WPS_BROWSER` | 环境变量 | 显式 Chrome 或 Chromium 路径 |
| `WPS_ADAPTER_URL` | 环境变量或参数 | 直接同步的适配器 origin |
| `WPS_ADAPTER_USER` | 环境变量或参数 | 适配器 Basic Auth 用户名 |
| `WPS_ADAPTER_SSH_TARGET` | 环境变量或参数 | SSH 同步目标 |
| `WPS_ADAPTER_SSH_PORT` | 环境变量或参数 | SSH 端口，默认 22 |
| `--login-url` | 参数 | 官方 WPS 登录 URL |
| `--workspace-url` | 参数 | 显式选择具体 WPS 文件夹；省略时选择空间根 |
| `--domain-suffix` | 参数 | Cookie 允许域 suffix |
| `--wait-timeout` | 参数 | 等待人工登录的秒数，默认 300 |
| `--allow-http` | 参数 | 明确允许把凭据经远程明文 HTTP 发送 |
| `--output-dir` | 参数 | 将凭据写到本地绝对目录 |

## 10. 内存状态、并发和资源保护

### 10.1 HTTP 连接

`AdapterHTTPServer` 在 `src/wps_adapter/server.py:355-392` 使用全局 bounded semaphore。

1. 默认最多 64 个已接受连接。
2. 槽满时直接关闭新 socket，不生成 503 响应。
3. 每个请求在线程中处理。
4. handler 线程为 daemon，服务关闭不等待无限阻塞的 handler。
5. 每个 socket 默认 60 秒 timeout。

### 10.2 上传和下载槽

`WpsStorage` 在 `src/wps_adapter/storage.py:136-188` 创建上传和下载 semaphore。

1. 默认每个 `WpsStorage` 最多 2 个上传、4 个下载。
2. 等待默认 30 秒。
3. 等待失败转换为 `ServiceBusyError`，HTTP 层返回 503 和 `Retry-After: 5`。
4. 下载槽直到返回的流被 close 才释放。
5. COPY 中继会先占用下载槽，再尝试占用上传槽。
6. 在当前多空间设计中，每个空间有自己的槽，而不是整个服务共享一组槽。

### 10.3 spool 磁盘预算

`WpsDriveClient` 在 `src/wps_adapter/client.py:1553-1601` 维护预留计数。

1. 小于等于内存阈值的上传不检查临时磁盘。
2. 超过阈值时按完整上传大小而不是溢出部分计算磁盘需求。
3. 需求等于当前完整大小加配置的最小剩余空间。
4. 同一 client 内的并发上传会扣除其他请求已预留量。
5. 请求退出时释放预留计数。
6. 当前每个空间派生独立 client，因此预留计数不是全服务全局值。

### 10.4 状态探测 singleflight

`WpsDriveClient` 在 `src/wps_adapter/client.py:1088-1191` 使用 condition 合并并发状态检查。

1. 一个 client 同时只执行一次探测。
2. 等待者最多等待 WPS timeout 加一秒。
3. 等待超时返回临时 upstream unavailable 和 retry_after 1。
4. mount 重建后新的子 client 拥有新的独立状态缓存。

网页预取队列不属于上述 WPS status singleflight。它由页面自己的有限队列调度；缓存清理通过 epoch 和 generation 使旧请求结果失效，导航 generation 则阻止旧目录请求更新当前页面。

### 10.5 DAV 锁

DAV 锁全部保存在 `AdapterApplication` 的一个 `DavLockStore` 中，因此跨空间共享一个锁集合。锁使用 monotonic clock，到期只在下一次相关操作时清理，不使用后台清理线程。

## 11. 外部系统和信任边界

| 边界 | 可信输入 | 不可信输入 | 当前保护 |
| --- | --- | --- | --- |
| WebDAV/REST 客户端 | 无 | path、query、header、body、连接生命周期 | Basic Auth、同源检查、长度限制、路径校验、锁、错误脱敏 |
| secret 文件 | 管理员选择的路径和值 | 文件替换、symlink、宽权限、超长值 | no-follow、owner/mode、大小、控制字符、原子写 |
| WPS 控制 API | 已观察的 endpoint 和字段结构 | 状态码、JSON、Set-Cookie、文件名称和 ID | HTTPS host 限制、response size、结构校验、名称校验 |
| 签名对象存储 | WPS 返回且通过 host suffix 校验的 URL | URL host、redirect、响应 metadata、正文长度 | HTTPS、host suffix、禁 redirect、不带 Cookie、Range 严格校验 |
| 本地 Chrome CDP | helper 自己启动的 loopback 端口 | WebSocket URL、帧、消息大小 | 只允许 loopback、消息上限、临时 profile |
| SSH 同步 | 用户显式 SSH target 和系统 ssh | 远端路径、shell 参数、secret 内容 | secret 走 stdin，不进入 argv；远端路径限定 secrets 直属文件 |

## 12. WPS 私有接口清单

以下只是当前代码已使用的接口，不表示 WPS 的公开稳定 API。

| 目的 | 方法和路径 | 实现位置 |
| --- | --- | --- |
| 登录状态 | `GET account-origin/api/v3/islogin` | `client.py:884-905` |
| 空间候选发现 | `GET /3rd/plus/groups/v1/companies/<tenant>/users/self/groups/private` | `client.py:945-993`、`login.py:1208-1299` |
| 目录列表 | `GET /3rd/drive/api/v5/groups/<group>/files` | `client.py:1423-1485` |
| 创建文件夹 | `POST /3rd/drive/api/v5/files/folder` | `client.py:1771-1798` |
| 重命名 | `PUT /3rd/drive/api/v3/groups/<group>/files/<file>` | `client.py:1800-1826` |
| 移动 | `POST /3rd/drive/api/v5/files/batch/task/move` | `client.py:1857-1904` |
| 删除 | `POST /3rd/drive/api/v5/files/batch/task/delete` | `client.py:1937-1975` |
| 异步任务进度 | `GET /3rd/drive/api/v5/files/batch/task/progress` | `client.py:1828-1855` |
| 同空间文件复制 | `POST /3rd/drive/api/v3/groups/<group>/files/batch/copy` | `client.py:1906-1935` |
| 上传预检 | `GET /3rd/drive/api/v5/files/upload/pre_check` | `client.py:2345-2359` |
| 普通上传指令 | `PUT /3rd/drive/api/v5/files/upload/create_update` | `client.py:2398-2424` |
| 注册上传文件 | `POST /3rd/drive/api/v5/files/file` | `client.py:2229-2250,2443-2464` |
| multipart 初始化和分片指令 | `POST/PUT /3rd/drive/api/v5/files/upload/block` | `client.py:2051-2185` |
| multipart 合并指令 | `POST /3rd/drive/api/v5/files/upload/block/merge` | `client.py:2187-2227` |
| 下载 URL | `GET /api/v3/office/file/<file>/download` | `client.py:2491-2513` |
| 会话刷新 | `POST account-origin/passport/secure/api/grant_token` | `client.py:1244-1281` |

## 13. 当前异常和日志结构

### 13.1 领域错误

`src/wps_adapter/provider.py:12-45` 定义以下存储层错误：

| 错误 | 语义 |
| --- | --- |
| `InvalidPathError` | 路径不是安全绝对路径或操作目标非法 |
| `EntryNotFoundError` | 路径不存在 |
| `NotFolderError` | 把 file 用于 folder 操作，或下载目标不是 file |
| `AlreadyExistsError` | 创建、移动、复制或默认上传发生名称冲突 |
| `InsufficientStorageError` | 文件、磁盘、条目、深度或响应限制被触发 |
| `ServiceBusyError` | 上传、下载或锁数量达到配置限制 |
| `AmbiguousPathError` | 一个父目录中出现多个同名 WPS 条目 |
| `UnsupportedOperationError` | 当前没有安全确认或非原子风险过高的操作 |

`WpsApiError` 位于 `src/wps_adapter/client.py:446-460`，只保存 operation、上游 status 和粗粒度 category，不保存响应正文或 URL。

### 13.2 日志

1. HTTP access logger 名为 `wps_adapter.http`。
2. 正常请求只记录 method 和去掉 query 的 path。
3. path 内控制字符替换为问号。
4. 不记录请求头、Authorization、Cookie、body 或签名 query。
5. 只有未知异常记录 stack trace，消息固定为 `request failed`。
6. 登录助手打印 Cookie 数量，不打印 Cookie 值、CSRF 或 ID 原值。

证据：`src/wps_adapter/server.py:46,433-438,611-613`、`src/wps_adapter/login.py:1471-1474`。

## 14. 当前部署方式

### 14.1 Native systemd

`deploy/wps-adapter.service` 当前行为：

1. 工作目录 `/opt/wps-adapter`。
2. 读取 `/etc/wps-adapter/wps-adapter.env`。
3. 当前 ExecStart 是 Python module。
4. 服务失败自动重启，间隔 5 秒。
5. `UMask=0077`、PrivateTmp、NoNewPrivileges、ProtectSystem strict、ProtectHome。
6. 仅允许写 `/etc/wps-adapter/secrets` 和 `/var/lib/wps-adapter/uploads`。

### 14.2 Docker

`deploy/Dockerfile` 和 `deploy/docker-compose.yml` 当前行为：

1. 基础镜像默认为 Python 3.12 slim。
2. 复制 source，不安装第三方包。
3. 使用传入的非 root uid/gid。
4. secrets 和 multipart 状态目录从宿主机挂载。
5. drop 所有 Linux capabilities，启用 no-new-privileges。
6. Basic Auth 两个文件额外以只读文件 mount 覆盖。

Go 切换部署时必须保留现有 env 文件、secret 文件、状态目录、uid/gid、umask 和 hardening 意图。用户不能因为语言变化而重新登录或迁移数据格式。

## 15. 测试资产地图

| 测试文件 | 覆盖范围 | 重构用途 |
| --- | --- | --- |
| `tests/test_server.py` | HTTP framing、auth、origin、REST、DAV、Range、锁、session import、网页 | 提取 Python/Go HTTP 黑盒对照 |
| `tests/test_storage.py` | 路径解析、缓存、冲突、move/copy、多层中继 | 建立存储接口契约和 fake WPS |
| `tests/test_smoke.py` | WPS 请求字段、上传、multipart、下载、刷新、状态 singleflight、安全 host | 建立录制式 fake upstream 合同测试 |
| `tests/test_settings.py` | name 校验、持久化、symlink | 状态文件兼容测试 |
| `tests/test_login.py` | Cookie 筛选、空间发现、Chrome、HTTP/SSH 同步 | 确认 Go server 可继续接收旧 helper |
| `tests/test_installers.py` | 安装脚本和部署模板 | Go 发布切换回归 |

关键测试入口：

- `tests/test_server.py:166-752`
- `tests/test_storage.py:112-282`
- `tests/test_smoke.py:153-1344`
- `tests/test_login.py:42-733`

## 16. 已发现的实现歧义和迁移决策点

以下事项不得由执行模型自行猜测。每一项都要先补一条当前 Python 行为测试，再由负责人决定“保持、修复或弃用”。

### 16.1 固定 group 配置可能得到空根

事实：`src/wps_adapter/__main__.py:53-55` 无条件创建 `MultiSpaceStorage`。`WpsClientConfig.from_env()` 在 group/root 都固定且 workspace 文件不存在时可能返回 `workspace=None`，见 `src/wps_adapter/client.py:541-549`。随后 mounts 是空 tuple，虚拟根没有任何空间。

决策要求：

1. 确认手工固定 `WPS_GROUP_ID` 是否仍是受支持部署方式。
2. 若支持，明确它应映射为单空间虚拟目录还是直接映射根。
3. 在决定前，Go 兼容测试以当前行为为观察基线，但不得悄悄宣称空根是设计目标。

### 16.2 status 是否绝对不刷新凭据

事实：账号 `islogin` 明确设置 `retry_on_401=False`，见 `client.py:884-905`；但后续 root list 使用默认可刷新请求，见 `client.py:1058-1079,1311-1349`。

决策要求：

1. 增加“islogin 成功、root list 401”测试。
2. 确认状态接口应该只读，还是允许第二阶段触发 refresh。
3. 文档、Python 基线和 Go 结果必须统一。

### 16.3 资源限额是每空间还是全服务

事实：连接和 lock 是应用全局；上传槽、下载槽、spool 预留和 status cache 是每个派生 storage/client 独立。

风险：选择多个空间后，实际总并发和临时磁盘预留可按空间数增长。

决策要求：

1. 记录当前单空间和多空间基线。
2. 出于 VPS 保护，建议 Go 使用全服务总预算。
3. 若改为全局，必须写成显式安全变更并更新配置说明。

### 16.4 工作区名称校验不完全一致

事实：登录空间发现会拒绝控制字符；`WorkspaceMount` 从本地 JSON 解析时只拒绝空、slash、反斜杠和过长值，见 `workspace.py:35-41`。

决策要求：安全上建议统一拒绝控制字符；这是有意收紧，不应伪装成无行为变化。

### 16.5 Basic Auth 半配置状态

事实：只要四个 auth 配置项中任一个存在，`enabled` 就为真；真正认证要求 username 和 password 都非空，见 `server.py:76-100`。

结果：半配置时公网 bind 检查通过，但所有受保护请求均 401。该状态 fail closed，但运维体验含糊。

决策要求：Go 可在启动时把半配置直接判为配置错误，但需记录为有意改变并更新 `check-config` 测试。

### 16.6 session import 对 auto 的检查与提示不一致

事实：错误消息说导入 workspace 需要 group 或 root 为 auto；实现只检查 `workspace_state is not None`。已有 workspace 文件也会让固定配置创建 state，见 `client.py:541-549` 和 `server.py:1315-1324`。

决策要求：明确固定配置是否允许替换 spaces，以及 top-level 固定 ID 与导入 mounts 的优先级。

### 16.7 COPY 注释和实际能力不一致

事实：`storage.py:130-133` 的类注释仍说没有确认的 WPS server-side COPY；实际 `storage.py:558-578` 已对同名普通文件使用 v3 native copy，且 `tests/test_smoke.py:1005-1028` 有请求合同测试。

决策要求：以实际代码和测试为准，更新迁移说明，不要把所有文件 COPY 都错误实现成中继。

### 16.8 multipart overwrite 的拒绝时机

事实：是否达到 multipart 阈值只在完整读取、spool、计算哈希和 pre-check 以后知道；overwrite 大文件随后返回 unsupported，见 `client.py:2313-2365`。

决策要求：若 Content-Length 已知，Go 可以提前拒绝以节省资源，但这是可观察的时序变化。应明确新行为和状态码，不要一边声称逐字兼容一边提前返回。

### 16.9 status 只验证多空间中的第一个空间

事实：`MultiSpaceStorage.status_root_id` 返回第一个 mount root，基础 client 的 group 也是 top-level 第一选择；其他 mounts 不在 `/status` 中逐个验证。

决策要求：第一版保持单一汇总状态，还是扩展每空间状态。扩展响应属于 API 变化，必须版本化或只添加兼容字段。

### 16.10 workspace、Cookie 和 CSRF 不是一个三文件事务

事实：session import 先替换 credential pair，再持久化 workspace。workspace 写失败时不会自动恢复已更新的 credentials。

决策要求：可以在 Go 中引入更完整事务，但要设计崩溃恢复，并验证旧 helper 收到的错误语义。

### 16.11 文档中的“流式上传”容易误读

事实：HTTP 请求正文按块读取，不会一次性读进一个大 bytes；但为了先算 checksum，完整内容仍会进入 spool 后再上传对象存储。

决策要求：性能目标必须测量内存、临时磁盘和二次 I/O，不能把 Go 重构目标错误写成“取消 spool”。

## 17. 重构边界建议

第一轮建议只替换 VPS 上长期运行的服务，不同时重写登录助手。

理由：

1. 登录助手只在用户需要登录时短时运行，不是性能热点。
2. 它已经处理 Chrome/CDP、Cookie 域、SSH 和交互式错误，风险面与 HTTP 服务不同。
3. 只要 Go 服务保持 session import、secret 文件和 workspace JSON 兼容，旧 helper 可以继续工作。
4. 将服务与 helper 分开迁移能明显缩小首轮验证范围。

建议的目标职责目录仅用于分工，不是强制代码实现：

| 目标区域 | 对应当前职责 |
| --- | --- |
| `cmd/wps-adapter` | CLI、配置加载、信号和 shutdown |
| `internal/httpapi` | REST、health、settings、session import、统一错误 |
| `internal/webdav` | DAV 方法、Depth、Destination、Range、锁 |
| `internal/storage` | path、mount、cache、mutation 规则 |
| `internal/wps` | WPS 控制 API、凭据和 refresh |
| `internal/transfer` | spool、hash、signed object、multipart、预算 |
| `web` | 独立 HTML、CSS、必要 JavaScript |

## 18. 阅读和执行顺序

接手模型在开始 Go 代码以前，必须按以下顺序阅读和确认：

1. 本文全部内容。
2. `02-compatibility-contracts.md` 的所有 MUST 契约。
3. `src/wps_adapter/provider.py`，掌握领域类型和错误。
4. `src/wps_adapter/storage.py:30-81`，掌握路径规则。
5. `src/wps_adapter/server.py:409-723`，掌握 framing、认证和错误。
6. `src/wps_adapter/server.py:762-1274`，掌握 DAV、Range 和锁。
7. `src/wps_adapter/server.py:1276-1752`，掌握 REST 路由和方法分派。
8. `src/wps_adapter/client.py:499-607,738-1374`，掌握配置、状态、凭据和公共请求器。
9. `src/wps_adapter/client.py:1384-1975`，掌握元数据和普通 mutation。
10. `src/wps_adapter/client.py:1977-2561`，掌握上传和下载。
11. 对应测试；先理解断言，再写语言无关 fixture。
12. `docs/research/findings.md`，只将已标记 observed/reproduced 的形状视作上游证据。

在以上阅读、基线测试和歧义决策完成以前，不应删除 Python 服务，也不应让 Go 服务写真实用户数据。
