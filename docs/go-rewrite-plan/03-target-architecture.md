# Go 重构目标架构

> 本文只定义目标结构、职责边界和设计约束，不提供实现代码。
> 执行模型在开始任何 Go 代码前，必须先读 `00-README.md`、`01-current-system.md` 和 `02-compatibility-contracts.md`。

## 1. 已确定的总方向

1. 只把 VPS 上长期运行的服务端迁移到 Go。
2. 第一轮保留仓库根目录的 `wps_login.py`，继续让它负责本地 Chrome 登录、空间选择和凭据同步。
3. 网页拆成原生 HTML、CSS 和 JavaScript。HTML/CSS 负责结构和样式，JavaScript 负责目录读取、上传进度、拖放、重命名、移动、删除、状态轮询等动态行为。
4. 不引入 React、Vue、Node.js 构建链、数据库、消息队列或微服务。
5. Go 服务编译为单个 Linux 可执行文件；网页静态资源嵌入该文件，部署时不要求用户安装 Go。
6. 第一版保持现有 URL、环境变量、secret 路径、JSON/XML 字段和客户端可见行为。
7. WPS 私有接口只移植仓库中已经观察或重放确认的请求，不自行猜测新端点或字段。
8. Python 服务在整个迁移期都是行为参照和可立即恢复的回滚版本。

## 2. 明确不在第一轮做的事

- 不把本地登录助手改写成 Go。
- 不删除 Python 源码或现有测试。
- 不改变 `/dav/`、`/api/v1/`、`/healthz` 和网页入口。
- 不把 Basic Auth 改成表单登录、JWT 或 OAuth。
- 不实现跨空间 MOVE/COPY。
- 不开放尚未确认的 WPS 文件夹原生 COPY、原子覆盖、快速上传或批量接口。
- 不把 HTTPS 证书管理塞进适配器；公网 HTTPS 仍由 Nginx、Caddy 等反向代理负责。
- 不承诺迁移一定提高单文件速度。WPS 上游延迟可能仍是主要瓶颈。
- 不在真实 WPS 账号上做无界并发压测。

## 3. 目标运行图

```text
浏览器 / WebDAV 客户端 / REST 客户端
                  |
                  v
          Go HTTP Server (HTTP/1.1)
                  |
       +----------+-----------+
       |          |           |
   Web UI       REST       WebDAV
       |          |           |
       +----------+-----------+
                  |
          应用服务与错误映射
                  |
       路径解析 / 多空间 / 元数据缓存
                  |
        进程级 ResourceBudget
                  |
             WPS Client
        +---------+----------+
        |                    |
  WPS 控制请求          签名对象存储
  带 Cookie/CSRF        永不带 WPS Cookie
```

独立登录链路保持为：

```text
wps_login.py -> 隔离 Chrome/CDP -> 官方 WPS 登录
             -> 选择并验证空间
             -> POST /api/v1/session/import 或 SSH
             -> 原子更新原有 secret/workspace 文件
             -> Go 服务热加载，不重启
```

## 4. 建议的最终目录

目录名可以在实施前微调一次；一旦阶段 2 通过，后续模型不得随意改名。

```text
cmd/
  wps-adapter/
    main.go                     只负责 CLI、组装、信号和退出码

internal/
  app/
    application.go              组合所有服务与全局资源
    lifecycle.go                启动、优雅停止、后台任务生命周期
  config/
    config.go                   环境变量读取、默认值、集中校验
    config_test.go
  model/
    entry.go                    RemoteEntry 等纯数据模型
    errors.go                   领域错误类型
  securefile/
    read.go                     有界读取、原子写抽象
    securefile_unix.go          Linux owner/mode/symlink 检查
    securefile_windows.go       Windows 开发环境的明确兼容策略
    securefile_test.go
  credentials/
    source.go                   Cookie/CSRF 快照、热加载与成对替换
    cookies.go                  Set-Cookie 合并、过期删除、CSRF 同步
    refresh.go                  外部刷新命令协调
  workspace/
    state.go                    旧/新 workspace JSON 兼容与热加载
    settings.go                 web-settings.json 管理
  budget/
    budget.go                   全进程连接、上传、下载、spool 预算
  cache/
    metadata.go                 有界 TTL 缓存与同键请求合并
  wps/
    client.go                   WPS 控制面 HTTP 客户端与公共入口
    request.go                  URL、请求头、JSON 上限、401 单次重试
    status.go                   islogin、根目录预检、缓存和退避
    entries.go                  列表、分页、远端字段规范化
    tasks.go                    move/delete 异步任务轮询
    upload.go                   普通上传控制流
    multipart.go                分片、检查点、合并与登记
    download.go                 下载地址解析和 Range 验证
    signed.go                   独立的签名对象存储客户端
  storage/
    path.go                     远端路径解析与编码
    storage.go                  单空间路径到 ID 的业务操作
    multispace.go               虚拟根和空间路由
    copy.go                     原生单文件 COPY 与受限中继
  davlock/
    store.go                    进程内锁、过期、刷新、继承判断
  httpserver/
    server.go                   net/http Server 参数和路由注册
    middleware.go               Basic Auth、同源写保护、日志、限流
    response.go                 JSON/文本错误与状态映射
    rest.go                     REST 路由分发
    webdav.go                   WebDAV 方法分发
    propfind.go                 Depth 遍历和有界 XML 输出
    range.go                    Range/If-Range 解析
    destination.go              Destination/Host 校验
    session_import.go           登录助手兼容入口

web/
  index.html                    语义结构，不放内联业务脚本
  style.css                     布局、响应式、状态样式
  app.js                        原生浏览器交互
  embed.go                      只读嵌入和静态响应元数据

contract_tests/
  README.md                     语言无关黑盒测试说明
  fixtures/                     只含脱敏请求/响应形状
  scenarios/                    HTTP/WebDAV 行为场景

tests/                           现有 Python 测试，迁移期保留
src/wps_adapter/                 现有 Python 参照实现，迁移期保留
wps_login.py                    第一轮继续发布的登录助手
```

## 5. 包职责和依赖方向

### 5.1 允许的依赖方向

1. `cmd/wps-adapter` 可以依赖 `internal/app` 和 `internal/config`。
2. `app` 可以组装所有内部包，但不实现协议细节。
3. `httpserver` 可以依赖 `storage`、`workspace`、`davlock`、`model` 和 `budget`。
4. `storage` 可以依赖 `wps`、`cache`、`budget` 和 `model`。
5. `wps` 可以依赖 `credentials`、`workspace`、`securefile`、`budget` 和 `model`。
6. 底层包不得反向导入 HTTP handler 或 CLI。
7. `web` 只提供嵌入资源；它不依赖 WPS 或 storage。

### 5.2 禁止的依赖方式

- 不允许 handler 直接拼 WPS 请求体。
- 不允许前端知道 Cookie、CSRF、group ID、root ID、签名 URL。
- 不允许 storage 直接写 HTTP 状态码。
- 不允许 WPS client 读取浏览器请求的 Basic Auth。
- 不允许 signed object client 共用带 Cookie 的 transport 或 cookie jar。
- 不允许任何包通过全局可变变量偷偷共享凭据、缓存或并发槽。
- 不允许循环依赖；遇到循环时缩小接口，不要建立“公共杂物包”。

## 6. 每层应拥有的职责

| 层 | 只负责 | 不负责 |
| --- | --- | --- |
| CLI | 参数、版本、组装、退出码、信号 | 业务逻辑、HTTP 响应 |
| config | 读取默认值、类型转换、一次性校验 | 网络调用、secret 内容日志 |
| securefile | 文件安全检查、有界读取、原子替换 | Cookie 语义 |
| credentials | Cookie/CSRF 生命周期、刷新协调 | WebDAV、文件路径 |
| workspace | workspace/settings JSON 兼容与热载 | 调 WPS API |
| WPS client | 已确认上游请求、解析、重试、脱敏错误 | 对外 REST/WebDAV |
| storage | 路径解析、空间路由、冲突规则、缓存失效 | HTTP 头部 |
| budget | 全进程资源额度 | 业务结果缓存 |
| HTTP server | 认证、路由、协议、状态码、取消传播 | WPS 字段细节 |
| web | UI 与同源 REST 调用 | 直接访问 WPS |

## 7. 关键接口边界

这里只描述能力，不规定具体 Go 方法签名。执行模型先写接口测试，再确定最小接口。

### 7.1 Storage 对 HTTP 层提供

- 获取指定路径元数据。
- 列出指定目录的直接子项。
- 创建文件夹。
- 上传到完整目标路径，可显式指定是否覆盖。
- 打开完整或指定范围的下载流。
- 删除非根对象。
- 重命名对象。
- 移动到父目录或完整目标路径。
- 按 Depth 和覆盖策略复制对象。
- 获取状态检查所用的映射根。
- 更新虚拟根显示名称。

### 7.2 WPS Client 对 Storage 层提供

- 分页列出某个 parent ID。
- 创建目录、重命名、移动、删除和单文件复制。
- 普通上传、分片上传与最终文件登记。
- 解析下载 URL 并返回可关闭的流。
- 获取不含敏感数据的连接状态。
- 使用新 workspace 配置完成后续请求。

### 7.3 Credential Source 对 WPS Client 提供

- 读取当前 Cookie/CSRF 快照。
- 检测管理员或登录助手替换的新快照。
- 合并 WPS 返回的多条 Set-Cookie。
- 原子替换 Cookie/CSRF 对。
- 串行执行一次刷新动作。

## 8. 并发模型

### 8.1 进程级预算

Go 目标架构应只有一个共享 `ResourceBudget`，由 `app` 创建并注入所有空间：

- 全进程最大活跃客户端连接数。
- 全进程最大上传数，默认 2。
- 全进程最大下载数，默认 4。
- 全进程临时 spool 已预留字节数。
- WebDAV 活跃锁总数，默认 4096。
- 可选的 WPS 控制请求上限，但首版不新增用户配置项。

这是对当前“每个空间各自拥有上传/下载槽”的风险修正。实施前必须在 Python 基线中记录旧行为，并在发布说明中明确 Go 版改为真正的全局上限。

### 8.2 请求取消

- 浏览器/WebDAV 客户端断开时，由请求 context 向 storage、WPS 控制请求和对象存储流传播取消。
- 取消必须关闭响应体、临时文件和上游连接，并释放所有 semaphore。
- 一个等待同目录缓存结果的请求取消，不得错误取消其他仍在等待的请求。
- 服务退出时先停止接收新请求，再等待在途请求到设定期限，最后取消剩余请求。

### 8.3 单飞与刷新

- 同一冷目录的并发读取合并为一次上游分页请求。
- 状态检查的并发请求合并为一次预检。
- 401 刷新全进程串行，避免旧 rtk 覆盖轮换后的新 rtk。
- 同一 multipart 检查点的恢复不得并发执行两次。

## 9. 缓存设计

1. 缓存只保存元数据，不保存文件正文、Cookie 或签名 URL。
2. 缓存键至少包含 `group ID + root generation + parent ID`，避免空间切换后串数据。
3. 默认 TTL 继续为 2 秒。
4. 默认最多 1024 个目录。
5. 同键 miss 使用 singleflight；不同目录可以并行。
6. 任意成功写操作清理受影响缓存；第一版可以清空全部元数据缓存以换取正确性。
7. workspace 文件 mtime 或内容版本变化时清空全部空间缓存并重建路由。
8. 不缓存上游错误为目录内容；状态接口自己的失败退避单独处理。
9. 浏览器还有独立的短期目录缓存：只保存 `entries` 元数据，TTL 30 秒；它不复用 Go 服务端 `WPS_CACHE_TTL` 的配置值。
10. 网页预取只由 `web/app.js` 调度当前目录的直接子文件夹，单次最多 24 个、最多 2 个并发；Go 后端只需正确提供已有 `GET /api/v1/entries`。
11. 浏览器刷新、成功 mutation 和重新连接必须使对应网页缓存失效；缓存 epoch/generation 用于阻止迟到请求污染新 workspace 或新导航。

## 10. 上传设计约束

上传不是简单的客户端到 WPS 直通，原因是 WPS 在返回签名 URL 前需要内容校验值。

1. 先检查声明的 Content-Length 和全局文件上限。
2. 获取上传槽和临时磁盘预算；等待超时映射为 503。
3. 一边读取请求体，一边计算 MD5、SHA-1、SHA-256，一边写入有界 spool。
4. 小于内存阈值可以保存在内存，超过阈值落到请求级临时文件。
5. 实际读取字节必须与 Content-Length 完全相等。
6. 根据实际总大小选择普通上传或 multipart。
7. 签名对象上传失败时，普通上传必须重新取签名指令再有限重试。
8. 任意返回路径都必须释放 spool、预算和上传槽。
9. 不能将整个大文件转换成一个 `[]byte`。
10. multipart 单片缓冲继续受 64 MiB 硬上限保护。
11. multipart 覆盖在未验证前继续返回不支持。

## 11. 下载设计约束

1. 先通过 metadata 确认目标是文件并得到 ID、size、etag、link_id。
2. Range 解析只接受一个字节范围。
3. 用文件 `link_id` 作为下载解析请求的 `cid`，缺失时才用配置回退。
4. WPS 控制面只用于获取短期签名 URL。
5. 校验签名 URL 是 HTTPS 且 host 落在明确允许的 WPS 对象存储后缀。
6. 对象存储请求使用独立 client，不带 Cookie/CSRF，不自动跟随重定向。
7. 范围请求必须得到 206，并严格核对 Content-Range 和长度。
8. 流式写入客户端；客户端取消立即关闭对象响应。
9. HEAD 不打开对象下载流，只返回已知元数据。

## 12. HTTP 服务器设计约束

- 使用 Go 标准库 `net/http`，但对当前协议行为自行封装。
- 不直接采用通用 WebDAV handler 来替代当前协议层，因为通用库的 Depth、PROPFIND 请求体、覆盖、LOCK 和错误语义可能不同。
- 明确配置 header/read/write/idle 超时和最大 header 大小。
- Go 默认会接受 chunked 请求，适配器入口必须显式执行当前 framing 契约。
- Basic Auth 校验使用恒定时间比较。
- `/healthz` 绕过认证且不访问 storage/WPS；其他公开入口按当前规则认证。
- 所有写方法执行 Origin/Referer 同源校验。
- 日志仅记录方法、无查询参数的路由形状、结果类别、耗时和脱敏请求 ID。
- 不记录 Authorization、Cookie、CSRF、query 值、真实文件名、请求/响应体或签名 URL。

## 13. 静态前端架构

1. `index.html` 只保存固定结构和可访问性标记。
2. 动态根名称不再拼进 HTML/JavaScript，页面加载后通过 `GET /api/v1/settings` 获取。
3. `style.css` 保存现有视觉和响应式规则。
4. `app.js` 保存原生 DOM、Fetch 和 XMLHttpRequest 逻辑。
5. `app.js` 还负责 30 秒目录缓存、直接子文件夹预取队列、2 个并发请求上限和失效 generation；这部分不进入 WPS client 或 Go storage。
6. 三个文件通过 `embed.FS` 编入二进制。
7. 入口 `/`、`/web`、`/web/` 都返回同一个 HTML；静态资源使用固定同源路径。
8. 静态资源同样受 Basic Auth 保护，避免未认证页面产生混乱状态。
9. 拆除内联脚本和样式后收紧 CSP，不再需要 `'unsafe-inline'`；这作为单独安全改动验证。
10. 不在 CSS/HTML 中写使用说明式大段文案；功能通过清晰控件和状态表达。

## 14. 配置与 secret 架构

### 14.1 配置加载

- 所有环境变量只在启动时解析一次，workspace、settings 和凭据文件内容除外。
- 数字、浮点、布尔、URL、host suffix、路径和前缀在启动时集中校验。
- 错误消息只说变量名与规则，不回显 secret 值。
- `check-config` 不发网络请求。
- 原环境变量及默认值详见 `01-current-system.md`，不得自行更名。

### 14.2 热加载状态

- Cookie/CSRF 每次控制请求获取当前安全快照。
- workspace 和 web settings 以文件身份或 mtime 变化触发重读。
- 更稳妥的 Go 实现可比较文件 identity、mtime、size 后重读，但不能只靠周期轮询。
- 解析失败必须 fail closed，不继续使用部分新配置。

### 14.3 Linux 文件安全

- secret 必须使用绝对路径。
- 父目录必须是实际目录，不能通过 symlink 绕转。
- 文件必须是普通文件且不是 symlink。
- 目录和文件不能授予 group/world 权限。
- owner 必须是 root 或当前服务 uid。
- 读取时使用不跟随 symlink 的打开方式，并在打开后再次检查文件元数据。
- 写入使用同目录临时文件、0600、flush/fsync、原子 rename；必要时同步父目录。

## 15. 外部依赖策略

第一版优先只用 Go 标准库：

- `net/http`、`net/url`：HTTP 和 URL。
- `encoding/json`、`encoding/xml`：结构化数据。
- `crypto/*`：摘要和恒定时间比较。
- `io`、`os`、`path/filepath`：流与文件。
- `sync`、`context`：并发和取消。
- `embed`：前端资源。

只有满足以下全部条件才能增加第三方依赖：

1. 标准库实现会显著增加安全风险或重复复杂度。
2. 依赖有明确维护者、许可证兼容 GPL-3.0-or-later、近期安全记录可接受。
3. 依赖不会改变 WebDAV 外部行为。
4. 为其添加版本锁定、许可证记录和漏洞扫描。
5. 在设计记录中写明为何需要、替代方案和移除成本。

## 16. 可观察性

首版只增加有界、脱敏的结构化日志，不新增遥测服务依赖。

建议记录：

- 适配器版本和启动配置的非敏感摘要。
- 请求方法、路由类型、HTTP 状态、持续时间。
- WPS 操作类别、上游状态类别、重试次数。
- 上传/下载槽等待和占用时间。
- 状态缓存命中、目录缓存命中、singleflight 合并次数。
- 临时 spool 使用字节和清理结果，只记录数值。
- 优雅停机是否在期限内完成。

禁止记录：

- 任意 Cookie、rtk、CSRF、Authorization。
- group/root/user/company/file/link ID 的原值。
- query 参数值、文件名、完整远端路径。
- 签名 URL 或其 query。
- WPS 原始响应正文和上传内容。

## 17. 必须先由负责人确认的设计决策

下列项目存在“当前代码、当前文档、理想行为”不完全一致。执行模型不得自行选择；阶段 1 完成特征测试后，由项目负责人逐项勾选。

| 编号 | 问题 | 推荐决定 | 迁移处理 |
| --- | --- | --- | --- |
| D-01 | 固定 group/root 且无 workspace 文件时虚拟根为空 | 先在 Python 修为单空间并冻结测试 | Go 只移植修正后的契约 |
| D-02 | status 根列表可能触发 401 refresh | status 全流程不得刷新 | Python/Go 同步修正并写发布说明 |
| D-03 | 多空间使上传/下载上限按空间倍增 | 改为真正进程级上限 | 记录为资源安全修正 |
| D-04 | REST 路径可能发生二次 URL 解码 | 所有入口只解码一次 | 先加入 `%25/%2F/+` 特征测试，作为兼容修正 |
| D-05 | 半配置 Basic Auth 可公开绑定但所有请求 401 | 非本地 bind 必须用户名和密码都有效 | 启动期失败并给不含值的错误 |
| D-06 | workspace import 的 auto 判断与错误文案不完全一致 | 只有 auto 配置允许改映射 | 先测试并统一 Python 契约 |
| D-07 | workspace mount 名未拒绝控制字符 | 拒绝控制字符 | 安全收紧，错误指明配置无效 |
| D-08 | PROPFIND 忽略请求 body 并固定返回属性 | Go 首版保持固定属性集合 | 不让通用库擅自扩展语义 |
| D-09 | 超连接数当前直接断开 TCP | 首个兼容版本保持并记录，后续再评估 503 | raw socket golden 必须固定 |

负责人确认格式：在每项末尾记录“决定、日期、原因、对应测试名、是否属于破坏性变更”。没有确认不得进入相应实现阶段。

## 18. 目标性能原则

- 正确性、数据不丢失、凭据不泄漏优先于吞吐数字。
- Go 必须在相同 Linux 主机、相同 WPS fixture、相同并发和相同文件上与 Python 比较。
- 首先要求 Go 不劣化峰值内存、临时磁盘和取消释放时间。
- 只有全部契约测试通过后才公布性能数据。
- 不用 `httptest` 的纯内存结果代替真实 socket、磁盘 spool 和 Linux 进程指标。
- 不通过取消上限、关闭校验或减少错误检查来制造更好成绩。

## 19. 架构完成标准

当且仅当以下条件全部满足，才能说“目标架构落地”：

- Go 目录职责与本文一致，没有跨层直接调用。
- 所有旧入口、配置、secret 文件和登录助手兼容。
- WPS 控制 client 与 signed object client 明确隔离。
- 全局资源预算在多空间下仍有效。
- 请求取消能释放上游、临时文件、锁和并发槽。
- Python/Go 黑盒契约对照通过。
- 普通上传、multipart、Range、COPY、LOCK 和 session refresh 均通过相应阶段门禁。
- Native/Docker 都只部署预编译产物，最终用户无需 Go 或 Node.js。
- Python 服务可以在一次服务切换内恢复，并继续读取同一份 secret。
