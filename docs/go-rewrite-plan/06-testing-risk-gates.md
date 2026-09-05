# 06 测试、风险门禁与回滚标准

> 本章只定义测试、证据、验收门禁和回滚规则，不包含 Go 实现代码。
>
> 本章中的“必须”表示不满足时不得进入下一阶段；“建议”表示可以延期，但必须登记负责人、原因和补测日期。

## 1. 目标与不可妥协原则

Go 重写的首要目标不是让新服务“看起来能用”，而是在可重复证据下证明它没有改变现有用户依赖的行为，并且在并发、内存、临时磁盘、客户端断开和上游故障时比当前实现更可预测。

执行模型必须遵守以下原则：

1. 先测量 Python，再测量 Go。没有同机、同数据、同上游条件下的 Python 数据，不得声称 Go 更快或更省内存。
2. 先冻结协议，再重写实现。任何因 Go 标准库默认行为产生的状态码、路径解码、重定向、压缩或连接处理变化，都必须视为潜在回归。
3. 正确性优先于性能。文件内容、目标路径、空间隔离、凭据安全或覆盖语义出现一项错误，即使性能提高也必须停止发布。
4. 单元测试不能代替黑盒测试。跨语言对照必须把 Python 和 Go 当作两个独立进程，只通过 HTTP 请求和脱敏的模拟上游交互进行比较。
5. 模拟上游不能代替真实 WPS。模拟测试用于高频回归；真实 WPS 只做低频、低并发、专用目录内的最终确认。
6. 真实 WPS 测试不得使用隐私文件，不得把 Cookie、CSRF、`rtk`、Basic Auth 密码、签名 URL、真实文件内容、企业 ID、群组 ID 或用户 ID 写入仓库、测试报告或日志。
7. 不允许通过删除、跳过、放宽或仅在 Go 端改写失败测试来获得绿色结果。确需改变契约时，必须先记录旧行为、风险、迁移说明、回滚方式和明确批准。
8. 每个阶段都必须可以在一次服务切换内回到 Python，且回滚不得要求用户重新登录、重新选择空间或重新创建凭据文件。

## 2. 当前测试基线与本机结果解释

### 2.1 仓库现状

当前仓库共有 6 个主要测试文件和 148 个 `test_*` 用例。测试重点如下：

| 测试文件 | 当前重点 | 当前局限 |
| --- | --- | --- |
| `tests/test_server.py` | HTTP、REST、WebDAV、Basic Auth、Range、锁、请求体限制 | 上游存储为内存假对象；只覆盖少量请求组合 |
| `tests/test_storage.py` | 路径到 ID 的解析、缓存、覆盖、移动、复制 | WPS client 为假对象；没有真实并发与大目录 |
| `tests/test_smoke.py` | WPS 请求形状、上传下载、分页、刷新、脱敏 | 主要使用预制响应；没有真实 TLS、DNS 和对象存储 |
| `tests/test_login.py` | 登录助手、Cookie 筛选、空间选择、SSH/HTTP 同步 | 大量 mock；POSIX 权限断言不适合直接在普通 Windows 环境判断 |
| `tests/test_installers.py` | 安装器文本、防护条件、清单生成 | 多数是字符串断言；不是完整安装、升级和回滚测试 |
| `tests/test_settings.py` | 网页设置持久化、权限和符号链接 | 依赖 POSIX 文件模式和符号链接语义 |

当前 CI 只在 Ubuntu 上使用 Python 3.11、3.12、3.13、3.14 运行单元测试、字节码编译和 shell 语法检查，见 `.github/workflows/test.yml:11-28`。当前 CI 没有以下项目：

- Go 构建、`go test`、race detector、fuzz、静态分析和漏洞检查；
- Python 与 Go 的黑盒差异测试；
- 浏览器端到端测试和移动视口截图检查；
- Windows、macOS、NAS 或真实 WebDAV 客户端测试；
- 真实 WPS 的低频回放；
- 吞吐、延迟、CPU、内存、文件描述符和临时磁盘基准；
- 断网、慢连接、短读、客户端取消、磁盘不足和进程重启故障注入；
- Native、Docker、systemd、OpenRC、SysV 和反向代理的实际安装测试。

### 2.2 这台电脑的实际检查结果

本次只读审计使用的是 Windows 和 Python 3.14.7。结果必须按下面方式解释：

1. 直接运行测试发现命令但没有设置 `PYTHONPATH=src` 时，`wps_adapter` 无法导入。这是仓库采用 `src/` 布局造成的启动方式问题，不是核心业务用例失败。
2. 按项目要求设置 `PYTHONPATH=src` 后，共发现并运行 148 项测试。
3. 本机结果为 128 项通过、4 项失败、16 项错误。
4. `python -m compileall -q src tests wps_login.py` 在本机通过。
5. 本机没有 `go`、`node`、`npm`、`git`、`bash`、`docker`、`gcc` 和 `g++`，因此不能在本机完成 Go、前端、发布清单、shell 安装器和容器测试。

### 2.3 4 项失败和 16 项错误的归类

这些结果不能直接描述为“项目有 20 个业务缺陷”。它们主要反映当前 Windows 环境不满足项目测试的 POSIX 前提：

| 类别 | 数量或示例 | 原因 | 处理要求 |
| --- | --- | --- | --- |
| 缺少 `bash` | 安装器 IPv6/bind 检查报错 | 测试显式启动 `bash` | 在 Ubuntu CI 或 WSL/Linux 开发环境运行 |
| 缺少 `git` | release manifest 检查失败 | builder 调用 `git ls-files` | 在有 Git 的规范环境运行 |
| POSIX `0600/0700` 语义不同 | 本地凭据、workspace、settings 多项失败 | Windows `st_mode` 与 POSIX 权限模型不同 | 权限安全测试以 Linux 为发布门禁；Windows 另写 ACL 测试，不混用断言 |
| 普通 Windows 无符号链接权限 | settings 和 credential symlink 用例错误 | 创建 symlink 需要额外权限或开发者模式 | Linux 必跑；Windows 可在具备权限的独立任务中补跑 |
| Windows socket 常量差异 | 客户端断开探测错误 | `socket.MSG_DONTWAIT` 在当前 Windows Python 不存在，代码见 `src/wps_adapter/server.py:440-452` | 记录为当前 Python 的平台边界；Go 必须用请求 Context 设计跨平台取消测试 |
| Windows 临时目录模式 | workspace/settings/credential 读取被拒绝 | 当前实现要求父目录私有且 owner 合法 | Linux 私有临时目录中重跑；不得通过删掉安全校验解决 |
| multipart checkpoint 用例 | 恢复用例在本机错误 | checkpoint 的 owner/mode 校验与 Windows 模式不一致，导致状态不被复用 | Linux 上验证实际恢复流程；Go checkpoint 明确只承诺目标部署平台 |
| SSH 远端 writer 路径用例 | 1 项失败 | Windows 临时路径不满足远端 `/etc/wps-adapter/secrets` 路径约束 | 在 Linux fixture 中使用明确的远端路径模型 |

### 2.4 迁移开始前的规范基线门禁

在编写 Go 业务功能前，必须在干净的 Linux 环境完成一次规范基线：

1. 使用仓库声明支持的 Python 3.11、3.12、3.13 和 3.14 分别运行全部 148 项测试。
2. 记录每个 Python 版本的通过、失败、跳过数量以及总耗时。
3. 运行源码和测试字节码编译检查。
4. 运行登录脚本生成一致性检查和发布清单一致性检查。
5. 运行 shell 语法检查。
6. 保存 CI 运行链接或脱敏日志摘要，禁止只保存“通过”截图。
7. 若规范 Linux 环境不是 148/148 通过，先修复或形成经批准的基线例外；不得把未知的 Python 失败带入 Go 对照阶段。
8. 当前 Windows 的 128/148 结果只进入环境说明，不作为发布通过率，不用于降低 Linux 门禁。

## 3. 测试资产与证据目录

后续实现应建立下列测试资产。路径名称可以按仓库最终结构微调，但职责不得混在一起：

| 资产 | 用途 | 禁止事项 |
| --- | --- | --- |
| 协议用例清单 | 为每个 HTTP/WebDAV/REST 行为分配稳定编号 | 不把测试意图只写在测试函数名里 |
| 脱敏上游 fixture | 保存 WPS 和对象存储的请求字段形状、响应结构与错误 | 不保存真实 Cookie、URL 查询签名、对象 ID 或正文 |
| Python golden | 保存当前 Python 对同一输入的规范化输出 | 不保存动态 `Date`、随机锁 token 原值等不可比较字段 |
| Go candidate 结果 | 保存 Go 对相同用例的规范化输出 | 不得覆盖 Python golden |
| 差异报告 | 列出状态码、头、JSON/XML、正文哈希和上游请求差异 | 不允许只输出“不同”而不指出字段 |
| 性能基线 | 保存环境、配置、轮次、样本和原始指标 | 不允许只保存平均值或只跑一次 |
| 资源报告 | 保存 RSS、heap、goroutine/thread、FD、临时盘和连接数 | 不允许仅在请求结束后采样 |
| 真实 WPS 验收记录 | 保存实验编号、时间、动作、脱敏结果和清理状态 | 不保存原始 HAR 或真实凭据 |
| 风险登记表 | 记录未解决风险、严重度、负责人、门禁和到期时间 | 不得用“以后处理”代替具体门禁 |

每个测试用例编号使用稳定前缀，例如：

- `HTTP-*`：HTTP framing、认证、连接和公共响应；
- `REST-*`：REST 路由与 JSON 契约；
- `DAV-*`：WebDAV 方法、XML、锁和条件头；
- `WPS-*`：WPS 控制请求与响应解析；
- `OBJ-*`：签名对象存储上传下载；
- `STATE-*`：Cookie、CSRF、workspace、settings 和 checkpoint；
- `RES-*`：并发、内存、磁盘、FD 和取消；
- `UI-*`：浏览器端到端行为；
- `DEPLOY-*`：Native、Docker、服务管理和回滚；
- `REAL-*`：真实 WPS 的低频验收。

## 4. 跨语言黑盒契约的比较规则

### 4.1 三层结果必须同时比较

每个黑盒用例必须同时比较以下三层，缺一层不得判定通过：

1. 客户端可见层：HTTP 状态码、响应头、响应体、连接是否关闭以及完成时间。
2. 上游交互层：发往 WPS 和对象存储的 method、host 类别、path 模板、query 名称、JSON 字段、字段类型、请求顺序、重试次数和是否带 Cookie。
3. 资源生命周期层：响应体、上游连接、文件句柄、临时 spool、checkpoint、并发槽和锁是否在成功、失败与取消后释放。

### 4.2 必须逐字比较的内容

以下内容默认逐字或逐数值比较：

- HTTP 状态码；
- `Content-Type`；
- 已知长度响应的 `Content-Length`；
- `Content-Range` 和 `Accept-Ranges`；
- `DAV` 和 `Allow`；
- `WWW-Authenticate`；
- `Retry-After`；
- `Location` 的路径与百分号编码；
- `ETag` 的引号和内容；
- REST JSON 中的字段名称、值类型、错误 `code` 和路径；
- 下载文件的总长度与 SHA-256；
- WPS 请求 method、path、query 名称、JSON 字段名称及字段类型；
- multipart 分片编号、大小、MD5、`Content-MD5`、ETag 顺序和 merge XML 结构；
- 重试次数和“最多重试一次/有限次数”的边界。

### 4.3 规范化后比较的内容

以下内容先规范化再比较：

- JSON 忽略空白和对象键顺序，但不忽略数组顺序、缺失字段、额外字段或数字/字符串类型差异；
- XML 按命名空间、元素层级、属性和值比较，忽略序列化器选择的 namespace 前缀和无意义空白；
- HTTP 头名称大小写不敏感，但重复头的数量和值顺序仍需保留；
- URL host 比较时按 DNS 名大小写不敏感，path、query 值和百分号编码按契约比较；
- 随机锁 token 替换为稳定占位符后，仍检查 scheme 为 `opaquelocktoken:` 且同一流程内引用一致；
- 动态时间转换成“存在、格式正确、误差在允许范围”后比较。

### 4.4 明确忽略或允许变化的内容

只有下列动态字段可以不做逐字比较，并且测试仍需验证它们不泄密：

- HTTP `Date` 的具体秒值；
- 随机 lock token 的具体 UUID；
- 临时文件随机后缀；
- 本地动态端口；
- 性能用例中的精确耗时。

`Server` 头不得直接忽略。应做安全断言：只允许批准的产品名和版本格式，不得泄露 Python、Go 小版本、操作系统或内部库信息。如果 Go 有意移除 Python 当前附带的运行时标识，应记录为安全改进，而不是无说明地更新 golden。

### 4.5 契约变更审批规则

当 Python 与 Go 不同时，执行模型必须按以下顺序处理：

1. 先确认输入、fixture、环境和配置完全相同。
2. 再确认差异是否来自动态字段或序列化无关差异。
3. 若是 Go 标准库默认行为造成的差异，优先显式配置 Go 以匹配既有契约。
4. 若当前 Python 行为明确有安全或协议缺陷，不得悄悄兼容；建立“有意差异”记录，列出旧行为、新行为、受影响客户端、迁移提示和回滚方法。
5. 只有得到项目维护者明确批准后，才能同时更新 Python golden、API 文档和兼容性说明。
6. 不得使用宽泛的正则、删除关键头、忽略整个 JSON 子树或只比较 `2xx` 来掩盖差异。

## 5. Go 单元测试验收矩阵

单元测试可以使用内存输入，但不得以“单元测试通过”替代后文的进程级黑盒测试。

| 编号范围 | 被测单元 | 最少输入集合 | 必须断言 |
| --- | --- | --- | --- |
| `UNIT-CONFIG-*` | 环境变量和默认值 | 缺失、合法最小值、0、负数、超大数、非法布尔、空路径 | 默认值与 `.env.example` 一致；错误指明变量；禁止整数溢出 |
| `UNIT-PATH-*` | 远端路径解析 | `/`、尾斜杠、中文、emoji、空格、`+`、`%`、`%25`、`%2F`、`%252F`、`.`、`..`、双斜杠、反斜杠、NUL、控制字符、非法 UTF-8、4096 字节边界 | 恰好按批准规则解码；拒绝 traversal；字节长度而非字符数限制；REST 与 DAV 差异有明确测试 |
| `UNIT-RANGE-*` | 单范围解析 | `0-0`、`0-N`、`N-`、`-N`、超过末尾、空文件、未知大小、负数、多范围、错单位、空值、超大整数 | offset/length 正确；非法请求稳定映射 416；无溢出 |
| `UNIT-IFRANGE-*` | `If-Range` | 精确 ETag、有无引号、弱 ETag、错误 ETag、日期形式、无 ETag | 与冻结契约一致；不因 Go 自动解析日期而改变当前行为 |
| `UNIT-AUTH-*` | Basic Auth | 无头、错 scheme、非法 base64、非 UTF-8、无冒号、用户名含冒号、空用户名/密码、正确值、轮换文件 | 恒定时间比较；错误为 401；不在日志出现明文 |
| `UNIT-ORIGIN-*` | Origin/Referer/Host | HTTP/HTTPS、默认端口、显式端口、IPv4、IPv6、尾点、大小写、用户名、query、fragment、控制字符、反向代理 Host | 同源写放行；跨源写 403 并关闭连接；非浏览器无头请求保持兼容 |
| `UNIT-DEST-*` | WebDAV `Destination` | 相对路径、绝对 URL、同 host/port、异 host/port、IPv6、凭据、query、fragment、DAV 前缀外、根路径 | 只允许当前适配器 DAV 范围；规范化后仍不能越界 |
| `UNIT-LOCK-*` | 锁存储 | acquire、冲突、Depth 0/infinity、父子路径、refresh、错误 token、expiry、上限、并发 acquire/unlock | token 一致；继承关系正确；到期释放；上限返回稳定错误；无数据竞争 |
| `UNIT-XML-*` | LOCK XML 与 PROPFIND XML | 空体、合法 namespace、owner 嵌套、64 KiB 边界、DOCTYPE、ENTITY、畸形 UTF-8、特殊名称 | 禁止实体；owner 有长度上限；输出正确转义；响应字节预算生效 |
| `UNIT-PAGE-*` | WPS 分页 | 单页、多页、空页、负终点、重复 offset、倒退 offset、重复 ID、超条目、异常字段 | 不死循环；重复 ID 明确失败；不把异常当空目录 |
| `UNIT-META-*` | WPS 元数据映射 | ID 数字/字符串、大整数、file/folder/unknown、负大小、缺字段、坏名称、坏 ETag、mtime 异常 | 不发生精度丢失；不接受危险名称；unknown 不参与路径命中 |
| `UNIT-ERROR-*` | 错误映射 | not found、not folder、already exists、ambiguous、busy、insufficient、unsupported、WPS 401/403/5xx、I/O | 状态码、REST `code`、`Retry-After` 与 Python 契约一致；响应不含上游正文 |
| `UNIT-COOKIE-*` | Cookie 与 Set-Cookie 合并 | 多个头、同名大小写、删除、Max-Age、Expires、quoted value、坏 Cookie、CSRF 轮换 | 顺序和有效值正确；过期删除；Cookie/CSRF 快照一致；不泄密 |
| `UNIT-SECRET-*` | secret 文件 | 绝对/相对路径、缺目录、symlink、非普通文件、硬链接策略、owner、0600/0644、超 4 MiB、换行和控制字符 | Linux fail closed；原子替换；文件和目录权限正确；错误不含值 |
| `UNIT-WORKSPACE-*` | workspace 状态 | 旧单空间、spaces 数组、空/重复 group、重复 name、128 边界、运行中替换、损坏 JSON | 旧格式兼容；切换原子；重复拒绝；缓存 generation 改变 |
| `UNIT-CACHE-*` | 元数据缓存 | hit、miss、TTL=0、过期、1024/1025、group/root 切换、并发冷 miss、上游失败 | 不跨空间污染；有界；并发 miss 按设计 singleflight；失败不缓存为空目录 |
| `UNIT-WEB-CACHE-*` | 浏览器目录缓存与预取调度 | 直接子文件夹筛选、24 个上限、2 个并发、30 秒 TTL、命中、pending 复用、失败、失效、generation | 不预取孙目录；不把失败缓存为空数组；旧请求不能覆盖新导航或新 workspace |
| `UNIT-BUDGET-*` | 全局资源预算 | 单空间、多空间、N-1/N/N+1、等待超时、取消、panic/error 路径 | 上传、下载、控制请求和 spool 是进程级总量；计数绝不为负或泄漏 |
| `UNIT-UPLOAD-*` | 普通上传状态机 | 0 B、1 B、8 MiB 前后、50 MiB 前后、声明长度不符、超 1 GiB、每阶段失败 | 三种 hash 正确；spool 落盘阈值正确；重试重新取签名；登记前不报告成功 |
| `UNIT-MPART-*` | multipart | 最小/最大 part、最大 parts、64 MiB buffer 边界、断点 state、坏 state、过期 session、part 失败、merge 失败 | 分片号/MD5/ETag 顺序正确；checkpoint 原子；完成后删除；不错误复用旧身份 |
| `UNIT-DOWN-*` | 下载状态机 | 正常、resolve 403 后 direct retry、坏签名 URL、对象 200/206/416、短读、长度不符、取消 | 对象存储永不带 WPS Cookie；Range 必须真 206 且 metadata 匹配；任何退出都 close |
| `UNIT-TASK-*` | delete/move task poll | 立即成功、多次 pending、失败、缺字段、超时、重复终态、取消 | 提交不等于成功；只在终态成功后返回；超时有界；取消停止轮询 |
| `UNIT-COPY-*` | COPY 策略 | 同名原生 copy、改名 relay、文件夹 depth 0/1/infinity、目标存在、跨空间、部分失败 | 只在已确认场景用原生 API；不先删旧目标；失败清理仅限本次新建对象 |
| `UNIT-STATIC-*` | 静态资源 | `/`、资源文件、缺失资源、缓存头、MIME、压缩协商、带 query | 内容可重复；用户输入不拼入 HTML/JS；错误不泄露磁盘路径 |

## 6. 进程级 HTTP、REST 与 WebDAV 黑盒矩阵

### 6.1 测试拓扑

黑盒测试必须启动真实服务进程，使用以下拓扑：

```text
测试客户端 -> Python 或 Go 适配器 -> 本地 WPS fixture server -> 本地对象存储 fixture server
```

fixture server 必须具备以下能力：

1. 记录收到的 method、path、query 名、头名称、脱敏后的 JSON/XML 结构、正文长度和正文摘要。
2. 为每个测试按脚本顺序返回预设响应。
3. 能返回慢响应、短响应、错误长度、连接重置、半关闭、3xx、401、403、404、410、429、5xx 和畸形 JSON/XML。
4. 能检查对象存储请求是否错误携带 `Cookie`、`Authorization`、CSRF 或 WPS Referer。
5. 每个测试结束后确认没有收到额外请求；额外重试也必须导致失败。
6. fixture 地址只监听 loopback，随机端口写入当前测试进程，不写入仓库。

### 6.2 HTTP 公共行为矩阵

| 编号 | 请求或场景 | Python 基线 | Go 验收 |
| --- | --- | --- | --- |
| `HTTP-001` | `GET /healthz` | 200 JSON，不访问 WPS，不要求 Basic Auth | 状态、字段类型一致；上游请求数为 0 |
| `HTTP-002` | `GET /healthz?x=1` | 先记录当前行为 | Go 与批准契约一致，不让 query 触发上游 |
| `HTTP-003` | 无认证访问受保护 REST/DAV/UI | 401、`WWW-Authenticate`、`Connection: close`、空体 | 四项全部一致；连接不能继续复用 |
| `HTTP-004` | 正确 Basic Auth | 正常处理 | 不额外访问 secret 之外的文件；日志无明文 |
| `HTTP-005` | 错误或畸形 Basic Auth | 401 | 不 panic、不回显输入、不区分用户名存在性 |
| `HTTP-006` | GET/HEAD/OPTIONS 携带非零正文 | 400 并关闭连接 | 不让 Go 自动读取或忽略后继续复用连接 |
| `HTTP-007` | `Transfer-Encoding: chunked` | 当前明确拒绝 | Go 即使原生支持 chunked 也必须按冻结契约处理 |
| `HTTP-008` | 多个 `Content-Length` | 400 并关闭连接 | 不接受相同值重复头，不产生 request smuggling 差异 |
| `HTTP-009` | 缺失、负数、非数字、溢出长度 | 按基线分别记录 | 状态码和连接关闭行为一致；无整数回绕 |
| `HTTP-010` | body 短于声明长度 | 400 或连接错误 | 不等待超过配置 timeout；不调用写上游 |
| `HTTP-011` | 控制体超过 1 MiB | 413 | 在解析 JSON/XML 前拒绝；关闭连接；RSS 不随声明值分配 |
| `HTTP-012` | 上传声明超过 1 GiB 默认值 | 507 | 读取正文前拒绝；上游和 spool 均为 0 |
| `HTTP-013` | 连接数 63、64、65 | 当前第 65 个连接可能被直接关闭 | 先冻结或批准改为 503；不得无说明改变客户端体验 |
| `HTTP-014` | 60 秒 idle/慢客户端边界 | Python 配置为请求 socket timeout | Go 有等价的 header/body/write/idle 上限；超时后资源归零 |
| `HTTP-015` | 未知路由和未知 method | 记录 Python 的状态、类型和 body | Go 不使用框架默认 HTML 错误覆盖项目契约 |
| `HTTP-016` | 连续 keep-alive 请求和 pipelining | 后一个请求不被断开探测消费 | 顺序正确；断开检测不读取下一请求字节 |
| `HTTP-017` | `OPTIONS` 在根、DAV、REST 和未知路径 | 记录当前 Python 行为 | 有意保留或明确修正；`Allow`/`DAV` 有 golden |
| `HTTP-018` | 日志注入字符、超长 path、query 含 secret 形状 | 路径控制字符被处理，query 不写日志 | 一行一事件；无 Authorization、Cookie、签名 query 和正文 |

### 6.3 REST 契约矩阵

| 编号 | 请求 | 必须覆盖的分支 | 关键验收 |
| --- | --- | --- | --- |
| `REST-001` | `GET /api/v1/status` | connected、not_configured、session_expired、permission_denied、upstream_unavailable、invalid_response | JSON 字段齐全且脱敏；成功 30 秒缓存、失败 5 秒退避、并发 singleflight |
| `REST-002` | `GET /api/v1/settings` | 无设置文件、已有设置、运行中替换 | 不访问 WPS；返回当前显示名 |
| `REST-003` | `PATCH /api/v1/settings` | 合法中文、空值、额外字段、超长、控制字符、跨源 | 原子持久化；非法 400；跨源 403；重启后保持 |
| `REST-004` | `GET entries/list` | 根、空目录、多页、文件路径、缺失、重复名、unknown 类型 | JSON schema 一致；文件当目录为 409；上游异常不伪装空列表 |
| `REST-005` | `GET metadata` | 根、文件、目录、缺失、特殊名称 | ID、name、kind、parent_id、size、mtime、etag 类型一致 |
| `REST-006` | `GET download` | 全量、单范围、If-Range、空文件、取消 | 长度和 SHA-256 一致；Range 头正确；断开释放上游 |
| `REST-007` | `PUT upload` | 新建、`overwrite=false`、`overwrite=true`、目标为目录、重复 query、无长度 | 新建/冲突/覆盖语义一致；默认不覆盖；不先删除旧文件 |
| `REST-008` | `POST folders` | 新建、已存在、父缺失、父为文件、根、多空间虚拟根 | 状态和错误一致；禁止直接写虚拟根 |
| `REST-009` | `DELETE entries/files/delete` | 文件、空目录、非空目录、根、锁定、任务失败 | 根绝不删除；锁为 423；任务成功前不返回 204 |
| `REST-010` | `PATCH entries/files` rename | `name`、`fname`、两者同时、冲突、危险名称、目标锁 | 只接受一个目标字段；新 path 和 entry 一致 |
| `REST-011` | `PATCH` move | `destination`、`parent_path`、跨目录同名、同时改名、跨空间 | 支持边界与 501 一致；源和目标锁都检查 |
| `REST-012` | `POST session/import` | 无认证、合法 Cookie、缺 rtk/csrf、坏域名、256/257 cookies、单/多空间、重复 name、持久化失败 | 不回显 Cookie；credential/workspace 更新顺序可恢复；成功后无需重启 |
| `REST-013` | query path 异常 | 缺省、空、重复、`+`、空格、`%2F`、`%252F`、非法 UTF-8 | 明确记录恰好解码次数；不得越过空间或目录边界 |
| `REST-014` | WPS 错误映射 | 401、403、404、429、5xx、timeout、malformed | 稳定映射 404/502/503 等；只返回批准的错误 code；不转发上游正文 |

### 6.4 WebDAV 契约矩阵

| 编号 | 方法 | 输入矩阵 | 关键验收 |
| --- | --- | --- | --- |
| `DAV-001` | OPTIONS | `/dav`、`/dav/`、子路径、认证有无 | `DAV: 1,2` 与 Allow 方法集合准确；不访问 WPS |
| `DAV-002` | PROPFIND Depth 0 | 文件、目录、虚拟根、特殊名称 | 只返回请求对象；href 编码、displayname、类型、长度、ETag、mtime 正确 |
| `DAV-003` | PROPFIND Depth 1 | 空目录、20/21 项、多页、混合类型 | 返回自身和直接子项；顺序冻结；XML 可解析 |
| `DAV-004` | PROPFIND infinity | 深度 1、64、65；条目 9,999、10,000、10,001；重复 ID；断开 | 限内完整；超限 507；重复 ID 失败；取消停止后续 WPS 请求 |
| `DAV-005` | PROPFIND body | 无 body、`allprop`、`propname`、指定 prop、畸形 XML | 当前 Python 丢弃 body 并返回固定属性；必须先决定兼容或版本化扩展 |
| `DAV-006` | HEAD | 文件、目录、Range、If-Range、未知大小 | 不打开下载正文；状态和头与 GET 规则一致；上游请求数明确 |
| `DAV-007` | GET | 0 B、1 B、1 MiB、100 MiB、未知长度、慢对象存储 | 字节完全一致；已知长度准确；未知长度连接关闭；不缓冲整文件 |
| `DAV-008` | GET Range | `0-0`、`6-10`、`N-`、`-N`、越界、多范围、If-Range 匹配/不匹配 | 206/200/416、Content-Range 和长度完全正确；对象 200 不冒充 206 |
| `DAV-009` | PUT | 新建、覆盖文件、目标目录、缺长度、短体、断开、锁 token | 当前 WebDAV 默认覆盖文件；成功 body/Location/status 与 golden 一致；失败不损坏旧文件 |
| `DAV-010` | MKCOL | 尾斜杠有无、body 0/非 0、已存在、父缺失、锁 | 与当前 body 丢弃和冲突语义一致；不得创建到错误父 ID |
| `DAV-011` | DELETE | 文件、目录、根、锁、上游 task 失败/超时 | 204 仅在确认成功后；根拒绝；无响应体；锁校验正确 |
| `DAV-012` | MOVE | 相对/绝对 Destination、同目录改名、跨目录保留名、同时改名、Overwrite T/F、目标存在、跨空间 | 201/412/501 与 Location 一致；不得先删除已有目标 |
| `DAV-013` | COPY 文件 | 同名跨目录原生、改名 relay、Depth 0/1/infinity、Overwrite T/F、目标存在 | 仅确认的同空间同名文件走原生；relay 内容 SHA-256 一致；覆盖拒绝不删目标 |
| `DAV-014` | COPY 文件夹 | 空目录、两层树、Depth 0/1/infinity、深度/条目上限、中途失败 | 递归范围正确；部分失败只清理本次创建根；残留必须可审计 |
| `DAV-015` | LOCK | 新锁、空 body、合法 owner、Depth 0/infinity、Timeout 秒/Infinite/非法、冲突、锁上限 | 返回 lockdiscovery 与 Lock-Token；最大 24 小时；第 4097 把锁稳定失败 |
| `DAV-016` | LOCK refresh | 正确 If token、错误 token、错误 path、过期 token、并发 refresh | token 不变、expiry 更新；错误不创建新锁 |
| `DAV-017` | UNLOCK | 正确 token、缺头、多 token、错误 token/path、重复 unlock | 成功 204；错误稳定；之后写操作是否放行得到验证 |
| `DAV-018` | 锁影响写操作 | PUT/MKCOL/DELETE/MOVE/COPY/REST rename/move | Depth infinity 锁覆盖后代；源和目标都检查；合法 token 只放行对应锁 |
| `DAV-019` | 不支持方法 | PROPPATCH、REPORT、ACL、任意 method | 状态和响应类型有明确契约；不误调用 WPS |
| `DAV-020` | 客户端兼容名称 | 中文、emoji、`#`、`?`、`%`、空格、极长名称、大小写相似、Unicode 组合形式 | href 可往返；不双解码；不把两个合法名称错误合并 |

## 7. WPS 与对象存储集成 fixture 矩阵

### 7.1 WPS 控制请求

每个 fixture 必须验证请求形状，而不只返回预制响应：

| 编号 | 流程 | fixture 必须检查 | 失败分支 |
| --- | --- | --- | --- |
| `WPS-001` | 列目录 | v5 path、group、parentid、offset、count、排序和可选 query | 401、403、500、超 8 MiB、非对象 JSON、files 非数组 |
| `WPS-002` | workspace 发现 | 只有显式启用才请求；严格解析候选 | 部分坏 item 时整体拒绝，不返回半份结果 |
| `WPS-003` | 状态预检 | 先账号 `islogin`，再最小 workspace list | 未配置不请求；失败按类别缓存；并发仅一组请求 |
| `WPS-004` | 创建目录 | POST path、JSON 字段、CSRF 来源 | result 非 ok、对象缺 id/name、权限失败 |
| `WPS-005` | 重命名 | v3 PUT path、`fname`、CSRF | 冲突、坏对象、401 后 CSRF 更新 |
| `WPS-006` | move | task submit 字段和 progress poll | pending、failed_list、malformed、超时、取消 |
| `WPS-007` | delete | task submit 字段和 progress poll | 同上；禁止重试导致删除其他对象 |
| `WPS-008` | 原生 copy | v3 batch/copy，仅同 group 单文件同名目标 | 响应缺新 ID、重复请求、目标已存在、跨 group 拒绝 |
| `WPS-009` | 401 refresh | 外部 secret 已轮换、grant_token、Set-Cookie 持久化、原请求重试一次 | 无 rtk、grant 失败、Set-Cookie 畸形、第二次 401 |
| `WPS-010` | 响应边界 | Content-Length 正确/缺失/负数/超限、流式超过上限、短读 | 关闭响应；映射 invalid_response；不保留大 buffer |

### 7.2 普通上传完整状态机

普通上传必须按以下检查点逐个故障注入：

1. 客户端正文开始前。
2. spool 尚在内存时。
3. spool 刚超过 8 MiB 转入磁盘时。
4. 三种 hash 完成后、pre_check 前。
5. pre_check 返回前和返回错误时。
6. create_update 返回签名指令前、返回畸形指令时。
7. 对象 PUT 尚未发送、发送一半、已发送完但响应丢失时。
8. 第一次对象 PUT 失败、重新取得新签名 URL 时。
9. 对象 PUT 成功但 WPS `files/file` 登记前。
10. 文件登记失败、返回成功、返回缺字段时。
11. 适配器准备向客户端回响应时客户端已断开。

每个检查点必须断言：

- 是否允许重试以及精确重试次数；
- 新签名 URL 是否重新取得；
- spool 是否关闭并删除；
- 全局磁盘预留是否归还；
- WPS 中是否可能存在未登记对象；
- 旧的覆盖目标是否仍可读；
- 客户端是否收到错误而不是虚假成功；
- 日志是否只有脱敏的阶段名和错误类别。

### 7.3 multipart 完整状态机

multipart 至少覆盖下列顺序：

1. 完整接收客户端文件并计算 SHA-1 身份。
2. 初始化 upload session，校验 `key`、`store`、`upload_id` 和 limit。
3. 根据 min part、max part、max parts 和本地 64 MiB buffer 上限选择 part size。
4. 为每片计算十六进制 MD5 和 Base64 `Content-MD5`。
5. 获取当前片签名 URL。
6. PUT 当前片并取得 ETag。
7. 原子保存 checkpoint。
8. 重启后读取 checkpoint，跳过已确认 part。
9. session 返回 400/404/410 时重新初始化并从第 1 片开始。
10. 生成有序 `part_infos` 和 merge 请求。
11. 提交对象存储 CompleteMultipartUpload XML。
12. 取得 merged ETag 后登记正式文件。
13. 只有登记成功后删除 checkpoint。

边界数据必须至少包含：49 MiB、50 MiB、50 MiB+1 B、100 MiB、part size 恰好 64 MiB、服务端要求超过 64 MiB、part 数恰好 max、part 数超过 max。每个 part 都要在“控制请求失败”“对象 PUT 短写”“响应无 ETag”“checkpoint 写失败”四类故障下验证。

### 7.4 下载与签名 URL

签名下载必须满足：

1. 控制面请求可以带 WPS Cookie；对象存储请求绝对不能带 WPS Cookie、CSRF 或 adapter Basic Auth。
2. URL 必须为 HTTPS，host 必须等于或位于批准的 `.ag.kdocs.cn` 后缀内。
3. 用户名、密码、控制字符、非批准端口策略和非 WPS host 必须在建立连接前拒绝。
4. 自动重定向默认禁止；fixture 返回同 host、跨 host、HTTP downgrade 三类 3xx，均按批准策略断言。
5. Range 请求只接受真实 206，并严格校验起点、终点、总大小和 Content-Length。
6. 对象存储返回 200、错误 Content-Range 或短 body 时，不得把完整文件或错位字节当作合法分片。
7. 客户端断开后，上游 body 必须立即关闭，下载槽和连接计数必须在规定时间内归零。

## 8. Python 与 Go 差异测试执行步骤

每个候选提交按以下顺序执行：

1. 准备一个只含假凭据、假 workspace 和本地 fixture 地址的临时配置目录。
2. 启动 fixture server，清空请求记录。
3. 启动 Python reference，等待 `/healthz` 就绪。
4. 逐一执行单个用例；每个用例前重置 fixture 状态，避免缓存和前一用例污染。
5. 保存 Python 的规范化客户端响应、上游请求序列和资源结束状态。
6. 正常停止 Python，并确认端口、临时文件和进程已清理。
7. 使用同一配置值、同一 fixture 脚本和同一请求字节启动 Go candidate。
8. 重复相同用例，保存 Go 结果。
9. 生成字段级差异报告。报告必须指出“缺少、额外、类型不同、值不同、顺序不同、请求次数不同”中的具体一种。
10. 对允许动态变化的字段执行第 4 节定义的规范化。
11. 任一未批准差异导致任务失败。
12. 最后再运行一轮混合顺序和并发用例，检测缓存、锁、凭据和资源预算的跨请求污染。

差异测试必须提供两种模式：

- 冷态：每个用例重启服务或清空缓存，用于确认第一次请求和初始化行为；
- 热态：同一进程重复请求，用于确认缓存、keep-alive、credential reload、锁和资源复用行为。

## 9. 浏览器端到端验收

前端从 Python 内嵌字符串拆成独立 HTML、CSS 和 JavaScript 后，必须先让 Python 后端提供新静态资源并完成 E2E；不得把“前端拆分”和“Go 切换”放在同一个无法定位问题的发布中。

### 9.1 视口与浏览器

至少运行以下视口：

- 390×844：常见手机竖屏；
- 768×1024：平板或窄窗口；
- 1440×900：普通桌面；
- 1920×1080：宽桌面。

自动化至少覆盖 Chromium。发布宣称兼容其他浏览器前，再加入 Firefox 和 WebKit。每个视口必须保存成功态、空目录态、连接失败态、上传进行态和设置弹窗截图，并执行元素边界检查，保证文字、按钮、表格、面包屑、进度条和弹窗不重叠、不溢出。

### 9.2 UI 操作矩阵

| 编号 | 操作 | 前置状态 | 必须断言 |
| --- | --- | --- | --- |
| `UI-001` | 首次打开 | WPS connected | 标题、空间、状态正确；没有营销页或空白屏 |
| `UI-002` | WPS 未配置/过期/不可用 | status fixture 各状态 | 文案区分进程健康和 WPS 状态；不显示原始上游错误 |
| `UI-003` | 浏览目录 | 单空间、多空间、空目录、多页目录 | 面包屑、返回、刷新、列表和编码路径正确 |
| `UI-004` | 搜索或筛选 | 中文、大小写、特殊字符 | 不修改远端；清除后恢复完整列表 |
| `UI-005` | 普通上传 | 小文件 | 进度从 0 到完成；刷新后出现；下载 hash 相同 |
| `UI-006` | 拖拽上传 | 拖到页面不同区域 | overlay 出现/消失；只上传一次；目录目标正确 |
| `UI-007` | 上传取消 | 读取、等待槽、spool、上游传输阶段 | UI 进入取消/失败态；后端资源释放；无虚假成功 |
| `UI-008` | 上传冲突 | 目标存在 | 默认策略和提示与 REST/WebDAV 文档一致；不会静默覆盖 |
| `UI-009` | 新建目录 | 合法、重复、非法名 | 成功后列表更新；错误可理解且不泄密 |
| `UI-010` | 重命名/移动 | 文件、目录、锁定、跨空间 | 路径正确；不允许的场景有明确错误；不丢失对象 |
| `UI-011` | 删除 | 本次创建的测试对象 | 有明确确认；只删除目标；失败后列表不伪装成功 |
| `UI-012` | 下载 | 小文件、100 MiB、Range 由浏览器触发时 | 内容正确；文件名编码正确；断开后释放资源 |
| `UI-013` | 修改显示名 | 中文、HTML/JS 注入字符串 | 重启后保存；文本被转义；不能执行脚本 |
| `UI-014` | Basic Auth 失效 | 401 | 不无限重试；给出可恢复状态；不把密码写 console |
| `UI-015` | 键盘和可访问性 | 只用键盘 | 焦点可见；主要操作可达；弹窗焦点不逃逸 |
| `UI-016` | 静态资源失败 | CSS/JS 404 或慢响应 | 页面不出现不可理解的重叠；错误可诊断；服务日志无 secret |
| `UI-017` | 目录预取 | 20/24/25 个直接子文件夹、含文件混合 | 只请求直接子文件夹；最多 24 个；最多 2 个并发；不改变当前列表 |
| `UI-018` | 目录缓存命中与失效 | 命中、pending、30 秒过期、刷新、写操作、重新连接、快速导航 | 命中不重复请求；失效后重新请求；旧响应不覆盖新目录；错误不伪装为空 |

浏览器 console 中不得出现未处理异常、Cookie、Authorization、签名 URL、WPS 原始响应或文件正文。网络面板中前端只能访问同源适配器路径，不能直接访问 WPS 私有 API 或签名对象存储 URL。

## 10. Fuzz、race、静态分析与依赖门禁

### 10.1 Fuzz 目标

Go fuzz 至少覆盖以下入口：

1. DAV path 与 REST query path 的解码和规范化。
2. Range 与 If-Range。
3. Destination、Origin、Referer、Host 和 IPv6 authority。
4. Basic Auth base64 输入。
5. Content-Length、Transfer-Encoding 和重复头组合。
6. LOCK XML，特别是 DOCTYPE、ENTITY、深层嵌套、超长 owner 和 namespace 变体。
7. WPS list/status/task/upload JSON 响应。
8. Set-Cookie 合并和过期解析。
9. workspace/settings/checkpoint JSON。
10. 文件名、ETag、mtime 和超大数字。

每个 fuzz 目标必须满足以下不变量：

- 不 panic、不死锁、不无限循环；
- 不分配与声明长度成比例的无界内存；
- 不接受 traversal、非批准 host 或控制字符；
- 无效输入返回受控错误，不访问真实网络；
- 输出不包含 seed 中标记为 secret 的值；
- 同一输入多次执行产生相同分类结果。

每个 pull request 执行固定 seed corpus；主分支定时任务对每个目标至少持续 10 分钟；发布候选对高风险 path、XML、cookie 和 WPS JSON 目标各持续至少 30 分钟。发现 crash 的输入必须脱敏后加入回归 corpus。

### 10.2 Race detector 场景

`go test -race` 必须覆盖：

- 100 个并发状态检查共享 singleflight；
- 同一冷目录的并发 cache miss；
- 浏览器预取完成与导航、刷新、写操作、重新连接同时发生；
- cache expiry 与 rename/delete/upload 同时发生；
- session import 与正在进行的 list/upload/status 同时发生；
- 多空间 mounts 运行中更新；
- 多个空间同时收到 401，只允许一个全局 refresh grant；
- Set-Cookie 更新与普通 credential snapshot 同时发生；
- lock acquire/refresh/unlock/expiry 并发；
- 上传/下载槽的 acquire、timeout、cancel、release 并发；
- spool reservation 与磁盘检查并发；
- multipart checkpoint 保存与进程取消；
- 优雅关闭与活动下载、上传、PROPFIND、task poll 同时发生。

race detector 任一报告都属于发布阻断项。不得用延时、降低并发或在测试中串行化来掩盖竞态。

### 10.3 静态和依赖检查

每个 Go 变更至少执行格式化、全部单元测试、race、vet 和漏洞扫描。依赖门禁要求：

1. 优先标准库，新增第三方依赖必须说明用途、许可证、维护状态和替代方案。
2. 锁定模块版本并校验依赖摘要。
3. WebDAV、XML、认证、日志和 HTTP middleware 依赖升级必须重跑完整协议矩阵。
4. 高危或可远程利用漏洞未解决时不得发布。
5. 不允许为了减少告警关闭 TLS 证书验证、host 验证、symlink 防护或 XML 实体防护。

## 11. 性能基线与验收方法

### 11.1 为什么必须分层测量

本项目大部分时间可能花在 WPS 和对象存储网络等待上。只测真实 WPS 会把网络波动误认为语言性能；只测本地 fake 又不能代表真实吞吐。因此性能测试分三层：

1. 适配器纯本地开销：本地 fixture 无延迟，测 HTTP、JSON、XML、hash、缓存和连接调度。
2. 可控网络开销：fixture 注入固定 RTT、带宽和丢包，测取消、背压、重试和资源峰值。
3. 真实 WPS 低频确认：只验证没有明显退化，不对 WPS 做压力测试。

### 11.2 固定测试环境

Python 和 Go 必须在同一台 Linux 主机或同一规格容器中顺序运行，固定以下条件：

- CPU 配额和核心数；
- 内存上限，至少包含项目目标的约 1.6 GiB、无 Swap 场景；
- 临时目录所在文件系统和可用空间；
- 相同的上传、下载、连接、Depth、缓存和 response 上限；
- 相同的 fixture 二进制、fixture 数据和网络延迟；
- 相同的客户端进程、请求顺序和并发；
- 关闭其他高负载任务；
- 记录操作系统、内核、CPU、内存、Python 版本、Go 版本和 commit。

每组先预热 1 轮，再正式运行至少 5 轮。报告中保存每轮值、中位数、p95、p99、最小和最大，不得只报平均值。Python 与 Go 测试交替执行或交换顺序，避免机器温度和缓存顺序系统性偏向某一方。

### 11.3 工作负载矩阵

| 编号 | 工作负载 | 数据点 | 并发点 |
| --- | --- | --- | --- |
| `PERF-001` | `/healthz` | 10 秒、60 秒持续请求 | 1、8、32、64 |
| `PERF-002` | Basic Auth + 小 JSON | 1,000、10,000 次 | 1、8、32、64 |
| `PERF-003` | 热缓存目录 | 0、20、1,000、10,000 项 | 1、8、32 |
| `PERF-004` | 冷缓存目录分页 | 20、21、1,000、10,000 项 | 1、8、32 同目录 |
| `PERF-005` | PROPFIND Depth 1 | 20、1,000、10,000 项 | 1、4、8 |
| `PERF-006` | PROPFIND infinity | 宽树、深树、混合树；1,000/10,000 项 | 1、2、4 |
| `PERF-007` | 全量下载 | 1 MiB、100 MiB、1 GiB | 1、4、8 |
| `PERF-008` | Range 下载 | 1 B、1 MiB、10 MiB 范围 | 1、4、16 |
| `PERF-009` | 普通上传 | 1 MiB、8 MiB-1 B、8 MiB+1 B、49 MiB | 1、2、4 |
| `PERF-010` | multipart 上传 | 50 MiB、100 MiB、1 GiB | 1、2、4 |
| `PERF-011` | COPY 原生 | 1 MiB、100 MiB 文件 | 1、2 |
| `PERF-012` | COPY relay | 改名文件、两层目录、100 MiB | 1、2 |
| `PERF-013` | 多空间混合 | 1、2、8 个 mount 同时 list/upload/download | 总并发 2、4、8、16 |
| `PERF-014` | 慢客户端 | 每秒读取/写入 64 KiB | 1、4、16 |
| `PERF-015` | 401 风暴 | 32 请求同时得到 401 | 32 |
| `PERF-016` | 网页目录预取 | 0、1、24、25 个直接子文件夹；慢/失败子请求 | 预取请求数、最大活动数和队列完成时间 |
| `PERF-017` | 网页目录缓存 | 30 秒内命中、过期、刷新后 miss、写后 miss | 浏览器命中率、重复 WPS list 请求数和失效延迟 |

### 11.4 每个性能用例必须采集的指标

- 客户端成功率和错误分类；
- 吞吐量；
- 首字节时间；
- 完成延迟 p50、p95、p99；
- 进程 CPU 时间和峰值 CPU；
- idle RSS、峰值 RSS、Go heap 与 GC 次数/暂停；
- Python thread 数或 Go goroutine 数峰值和结束后数量；
- 文件描述符/handle 数峰值和结束后数量；
- 上游活动连接、idle 连接和新建 TLS 连接次数；
- WPS 控制请求数、对象请求数、重试数；
- spool 文件数量、总大小和峰值；
- checkpoint 文件数量；
- 客户端取消到所有资源归零的时间。
- 浏览器目录预取请求总数、同时活动请求峰值、缓存命中/未命中/过期次数和旧响应丢弃次数。

### 11.5 性能通过标准

在没有 Python 基线前，不填写虚构的“快几倍”目标。取得基线后，按以下最低门禁判断：

1. 所有响应、上游请求和文件 hash 先通过正确性矩阵。
2. Go 的任何核心工作负载成功率不得低于 Python。
3. Go 在同一配置下不得出现 Python 没有的 OOM、FD 耗尽、临时盘耗尽、死锁或无限 goroutine 增长。
4. Go 的峰值 RSS 不得高于 Python 基线；高于即按现有迁移文档的回滚标准停止。
5. 本地 fixture 下，Go 的 p95 延迟不得比 Python 慢 10% 以上；若慢，必须有字段级 profile 和批准的正确性/安全性理由。
6. 大文件吞吐不得比 Python 中位数低 5% 以上；真实 WPS 只作为趋势确认，不因单次公网波动失败。
7. 负载结束 30 秒后，goroutine、FD、临时文件和连接数量必须回到稳定基线范围；连续三轮增长视为泄漏。
8. 客户端断开后 5 秒内必须停止无意义的上游传输、释放 slot 并关闭 spool；若上游 API 本身不可取消，必须记录硬 timeout，且不得继续到文件登记阶段。
9. 同一冷目录的 32 个并发请求不得产生 32 份完整分页请求；目标是同 key 合并为 1 份活动请求，等待者共享结果。
10. 多空间场景必须按进程级总预算限制，不能把默认 2 上传/4 下载乘以空间数。
11. 宽目录预取始终不超过 24 个直接子文件夹；同时活动的预取请求始终不超过 2 个。
12. 30 秒内重复打开已预取目录不得产生重复 WPS list 请求；过期、刷新、成功写操作和重新连接必须产生可解释的失效。
13. 预取失败或快速导航后，页面不能出现错误的空目录、旧目录或无限增长的 pending 请求。

## 12. 资源保护与故障注入矩阵

### 12.1 资源阈值边界

每个数值限制都必须测试 `限制-1`、`限制`、`限制+1`：

| 限制 | 当前默认或硬边界 | 必须观察的结果 |
| --- | --- | --- |
| 上传并发 | 2 | 第 3 个等待；30 秒超时后 503；前两个结束后 slot 可复用 |
| 下载并发 | 4 | 第 5 个等待或超时；取消后立即释放 |
| 总连接 | 64 | 第 65 个按冻结契约处理；不能增加 goroutine/FD 泄漏 |
| 单上传大小 | 1 GiB | 超限在读 body 前 507；不建 spool、不访问 WPS |
| 内存 spool | 8 MiB | 边界前在内存；边界后落盘；完成/失败删除 |
| multipart threshold | 50 MiB | 边界前普通上传；边界及之后 multipart |
| multipart part buffer | 64 MiB | 超过直接 507；不得按返回值分配超大 slice |
| spool 空闲保留 | 512 MiB | 低于预算拒绝新上传；health/status/download 仍工作 |
| 单目录条目 | 10,000 | 10,001 返回 507；不返回截断成功 |
| PROPFIND 条目 | 10,000 | 同上；已写出的流式 XML 必须处理失败一致性 |
| PROPFIND/COPY 深度 | 64 | 第 65 层 507；不栈溢出 |
| COPY 条目 | 10,000 | 超限停止；清理范围仅限本次创建对象 |
| 活动锁 | 4,096 | 第 4,097 个 503；过期锁清理后可再建 |
| 控制请求体 | 1 MiB | 超限 413 并关闭连接 |
| WPS JSON 响应 | 8 MiB | 超限失败并关闭上游，不解析部分对象 |
| 生成 JSON/XML | 16 MiB | 超限 507；RSS 有界 |
| 对象响应 body | 1 MiB | 超限失败并关闭连接 |
| merge XML 响应 | 4 MiB | 超限失败并保留可诊断 checkpoint |

### 12.2 故障注入点与预期

| 编号 | 注入故障 | 预期状态 | 必须清理 | 禁止结果 |
| --- | --- | --- | --- | --- |
| `FAULT-001` | WPS DNS 失败 | 502 或规定的 unavailable | 请求、连接、slot | 把它当空目录 |
| `FAULT-002` | WPS connect timeout | 有界错误 | goroutine、socket | 无限等待 |
| `FAULT-003` | WPS TLS 证书错误 | 失败 | socket | 关闭证书验证后重试 |
| `FAULT-004` | WPS 401 | 最多一次刷新和一次原请求重试 | refresh waiter | 每个空间各发一次 grant |
| `FAULT-005` | WPS 403 | 按接口分类；下载 resolve 特定流程可试 direct variant | response body | 对所有 403 无限重试 |
| `FAULT-006` | WPS 429 + Retry-After | 稳定 busy/upstream 错误 | response、slot | 自发高并发重放 |
| `FAULT-007` | WPS 500/502/503 | 稳定 502/503 映射 | response、slot | 返回原始 body |
| `FAULT-008` | JSON 声明长度过大 | 立即拒绝 | response | 分配声明大小 |
| `FAULT-009` | JSON 传输中断 | invalid response | response | 使用部分 JSON |
| `FAULT-010` | 对象存储跨 host redirect | 拒绝 | response、connection | 携 Cookie 跟随 |
| `FAULT-011` | 下载实际 200 但请求 Range | 502 类失败 | object body、download slot | 返回 206 给客户端 |
| `FAULT-012` | 下载 Content-Range 错位 | 失败 | 同上 | 转发错片段 |
| `FAULT-013` | 客户端下载中断 | 不再读上游 | object body、slot | 继续消耗全部远端流量 |
| `FAULT-014` | 客户端上传中断 | 失败 | spool、reservation、slot | 继续登记文件 |
| `FAULT-015` | spool 写满磁盘 | 507 | 临时文件、reservation | 服务崩溃或 health 失效 |
| `FAULT-016` | checkpoint 无写权限 | 明确失败 | temp checkpoint | 留下被误认为有效的半文件 |
| `FAULT-017` | object PUT 成功、登记失败 | 502 类失败并记录孤儿风险 | spool、connection、slot | 向客户端报成功或删除旧覆盖目标 |
| `FAULT-018` | multipart 某片响应丢失 | 按有限策略重试 | part buffer、连接 | 无界产生新 session |
| `FAULT-019` | merge 成功、正式登记超时 | 不确定结果需明确错误 | 本地资源；保留受控 checkpoint | 盲目重复 merge/登记产生副本 |
| `FAULT-020` | move/delete task 永不结束 | timeout | poll timer、request | 提交即返回成功 |
| `FAULT-021` | 递归 COPY 第 N 项失败 | 失败 | 本次新建根的受限清理 | 删除既有目标或源 |
| `FAULT-022` | PROPFIND 中途断开 | 停止遍历 | queue、WPS response | 继续访问剩余目录 |
| `FAULT-023` | session import 写 cookie 成功、写 csrf 失败 | 整组失败并恢复一致快照 | temp files | 其他请求读到长期混合版本 |
| `FAULT-024` | workspace 文件运行中变坏 | 受控错误或继续最后有效快照，按批准设计 | file handles | 切到错误 group/root |
| `FAULT-025` | SIGTERM/容器 stop | 停止接收新请求；在 grace 内结束/取消活动请求 | listener、goroutine、spool | 登记半完成对象或破坏 secret |

### 12.3 资源泄漏判定

每个取消和故障用例结束后重复 100 次，执行以下检查：

1. 活动 HTTP 请求数归零。
2. 上游连接数回到 idle 上限以内。
3. 上传/下载/控制请求 semaphore 当前占用归零。
4. spool reservation 总数和字节数归零。
5. 临时目录没有本用例遗留的 spool；允许保留的 checkpoint 必须有对应失败状态和过期清理规则。
6. FD/handle 数相对开始值没有持续线性增长。
7. goroutine 数在 30 秒内回到允许基线；保存 goroutine dump 仅用于本地诊断且先脱敏。
8. heap 在强制稳定观察窗口后没有随轮次线性增长。
9. fixture 没有仍在发送或接收正文的连接。
10. 日志没有测试 secret marker。

## 13. 真实 WPS 低频验收

### 13.1 前置条件

真实 WPS 测试只有在全部模拟单元、集成、差异、race、fuzz 和资源门禁通过后才能执行。执行前必须满足：

1. 使用测试人员本人有权访问的账号和空间。
2. 在 WPS UI 中建立本轮专用目录，名称包含项目固定前缀、日期和随机短后缀。
3. 目录内只放本轮生成、无隐私、可删除的数据。
4. 本地保存测试对象清单：逻辑名称、随机内容 seed、大小、SHA-256、创建步骤和适配器返回 ID 的脱敏引用。
5. Go 与 Python 使用同一组当前凭据文件，但不同时执行写操作。
6. 默认并发为 1；不得对 WPS 做压力、fuzz、扫描或无界重试。
7. 原始 HAR、Cookie、签名 URL 和文件正文不进入仓库、聊天、Issue 或报告。
8. 删除操作只能针对本轮清单中由测试创建的对象；不能按名称模糊匹配删除。

### 13.2 最小真实验收顺序

| 编号 | 动作 | 验收证据 | 清理要求 |
| --- | --- | --- | --- |
| `REAL-001` | status preflight | connected；日志无 secret；请求次数符合 TTL | 无远端写入 |
| `REAL-002` | 列专用空目录 | REST 和 DAV 都为空；不是权限错误伪装 | 无 |
| `REAL-003` | 上传小文本文件 | 201；WPS UI 可见；下载 SHA-256 一致 | 保留到后续操作完成 |
| `REAL-004` | 新建目录 | WPS UI 可见；父目录正确 | 保留到后续移动测试 |
| `REAL-005` | 重命名文件 | ID/版本语义按当前证据；旧 path 不存在、新 path 可读 | 记录新 path |
| `REAL-006` | 移动文件 | task 完成后才成功；目标父正确；内容不变 | 记录目标 ID/path |
| `REAL-007` | 单范围下载 | 206；Content-Range 正确；片段与本地原文件一致 | 无 |
| `REAL-008` | 同空间同名原生 COPY | 新对象出现；源不变；内容一致；无 VPS 正文中继证据 | 删除复制对象前先记录结果 |
| `REAL-009` | 改名 COPY relay | 新名称正确；完整 hash 一致；spool 被清理 | 删除复制对象 |
| `REAL-010` | LOCK/UNLOCK | 两个真实客户端看到 423/放行变化 | 锁解除 |
| `REAL-011` | PROPFIND Depth 0/1/infinity | 小测试树层级完整；请求数可解释 | 无 |
| `REAL-012` | 100 MiB multipart | 10 MiB 或服务端批准 part；下载完整 SHA-256 一致；checkpoint 删除 | 删除大文件 |
| `REAL-013` | 客户端中断 | 中断后不登记成功、不持续传输；临时资源清理 | 检查是否有远端孤儿/半对象 |
| `REAL-014` | Cookie 自然轮换或一次批准的 refresh | 服务无需重启；原请求最多重试一次；文件原子更新 | 不主动破坏有效账号会话 |
| `REAL-015` | delete 本轮对象 | 任务成功后对象消失；其他对象无变化 | 最后删除专用目录 |

### 13.3 真实 WPS 结果判定

真实测试不得仅以 HTTP 2xx 判定成功。每次写操作必须通过至少两种独立观察确认：

- 适配器返回的结构化结果；
- 随后的无缓存目录/metadata 查询；
- WPS 官方 UI；
- 下载内容长度和 SHA-256；
- task progress 终态。

如果结果不确定，例如对象存储 merge 成功但文件登记响应超时，立即停止后续写测试。先在官方 UI 和只读列表中确认对象状态，不得盲目重试相同写请求。

## 14. WebDAV 客户端与部署兼容矩阵

自动 raw HTTP 契约全部通过后，至少按下表验证用户真实入口：

| 环境 | 最少场景 | 发布要求 |
| --- | --- | --- |
| 浏览器 Chromium | UI 全流程、Basic Auth、上传下载、取消、设置 | 每次发布候选自动运行 |
| `curl` 原始 HTTP | 全部 REST 和关键 DAV method/header | 每次提交自动运行 |
| 一个脚本化 WebDAV 客户端，例如 rclone | list、copy、upload、download、move、delete、lock 能力探测 | 每次发布候选运行 |
| Windows WebDAV 映射 | 连接、浏览、打开、保存、重命名、删除、断线重连 | 宣称 Windows 兼容前人工运行并记录版本 |
| Linux davfs2 或 cadaver | mount/浏览/读写/锁 | 发布候选至少运行一种 |
| macOS Finder | 连接、浏览、复制、保存 | 宣称 macOS 兼容前运行 |
| 实际目标 NAS | 添加远程 WebDAV、定时同步、大文件、失败恢复 | 宣称该 NAS 兼容前按具体型号/版本运行 |
| Nginx 或 Caddy HTTPS 反代 | Host、Destination、Origin、Range、长上传、超时 | 公网推荐路径发布前必跑 |
| Native systemd | 安装、启动、凭据权限、升级、回滚、卸载保留 secret | Linux release 必跑 |
| Docker Compose | non-root UID/GID、只读 auth bind、可写 secret/resume、stop/restart | Docker release 必跑 |

客户端报告必须记录客户端名称、版本、操作系统、服务版本、HTTP/HTTPS、反向代理版本和失败 method/status。不得记录 Authorization、Cookie、完整查询、真实文件名或签名 URL。

## 15. 隐蔽风险登记与专项门禁

| 风险编号 | 代码或文档证据 | 风险 | 必须设置的专项测试/门禁 |
| --- | --- | --- | --- |
| `RISK-001` | `storage.py:136-188,688-700` | 当前每个空间各有上传/下载 semaphore；空间数会放大总并发 | 多空间 1/2/8 mount 资源测试；Go 必须使用进程级共享 budget |
| `RISK-002` | `client.py:776-783` 与 child client 重建 | spool reservation 和 refresh lock 可能按 client 分裂 | 32 请求跨空间 401 只允许一个 grant；总 spool 预算不能乘空间数 |
| `RISK-003` | `storage.py:237-262` | 同一冷目录并发 miss 会产生 cache stampede | 32 并发只产生一个分页序列；等待者取消语义明确 |
| `RISK-004` | `server.py:624-637` + `storage.py:30-60` | REST query 可能由 `parse_qs` 和 `unquote` 双重解码，DAV 只解一次 | `%2F/%252F/%25/+` 差异 golden；修复必须版本化 |
| `RISK-005` | `server.py:866-899,1020-1054` | PROPFIND 先收集完整树再拼完整 XML，峰值内存和取消延迟高 | 10,000 项 RSS、断开、流式写失败测试；Go 不得无界缓存 |
| `RISK-006` | `storage.py:276-291` | 深树每次按 path 从根逐级名称扫描，冷/宽目录可能接近 O(N×depth) | 宽树、深树 profile；Go ID 队列输出顺序须与契约一致 |
| `RISK-007` | `client.py:2258-2375` | 上传不是直接透传；必须先完整 spool 并计算 MD5/SHA1/SHA256 | 8 MiB/50 MiB 边界、磁盘峰值和三 hash golden |
| `RISK-008` | `client.py:1977-2185` | multipart 每片一次性进入内存，checkpoint 有安全和恢复语义 | 64 MiB 边界、重启、损坏 state、过期 session 和 merge 故障矩阵 |
| `RISK-009` | `storage.py:512-663` | 改名或文件夹 COPY 最终会完整 spool，且递归不是事务 | 100 MiB relay disk 峰值、N 项失败清理、旧目标不被删除 |
| `RISK-010` | `docs/research/01-native-copy.md:55-65` | 原生 COPY 仅同空间单文件已确认；目录/覆盖/跨空间未知 | 不得扩大 API 使用范围；真实验收只测已确认组合 |
| `RISK-011` | `server.py:1139-1216` | MOVE/COPY 目标存在时 T 返回 501，F 返回 412，和通用库默认值可能不同 | 状态/body/Location golden；不得让 WebDAV 库自动覆盖 |
| `RISK-012` | `server.py:1020-1072` | 当前忽略 PROPFIND body，固定输出属性 | allprop/propname/prop 用例；兼容或扩展必须明确批准 |
| `RISK-013` | `server.py:355-393` | 连接满时直接关闭 TCP，无 HTTP 503 | 第 65 连接 raw socket 测试；行为改变需文档说明 |
| `RISK-014` | `server.py:440-452` | Python 断开探测依赖平台 socket flag，且不能消费 pipeline | Go Context 取消 + pipelining/half-close 测试；Windows 本机错误须保留记录 |
| `RISK-015` | `client.py:1607-1756,2466-2552` | 签名 URL host、redirect 和 Cookie 隔离是安全边界 | 恶意 host、尾点、子域欺骗、redirect、HTTP downgrade 测试 |
| `RISK-016` | `client.py:197-424` | Cookie/CSRF 是两个文件；单文件原子替换不等于跨文件快照原子 | 并发 reader + 导入/轮换故障；Go 内部使用一致 snapshot，兼容旧文件路径 |
| `RISK-017` | `workspace.py:50-124,275-318` | owner、mode、symlink、原子 replace 是 Linux 安全契约 | Linux 真实文件系统测试；不能用普通 Windows 结果放宽规则 |
| `RISK-018` | `server.py:114-220` | LOCK 只在进程内，重启或多实例失效 | restart 和双实例明确失败/限制测试；部署不得悄悄多副本 |
| `RISK-019` | `storage.py:683-686` | 多空间 status 只用第一个 mount 的 root 做探测 | 首空间正常/失败、后续空间不同状态的产品决策和测试 |
| `RISK-020` | `client.py:1423-1542` | offset 分页期间目录变化可能重复或遗漏 | 页间插入/删除/排序漂移 fixture；重复 ID fail closed |
| `RISK-021` | `server.py:762-864` | If-Range 当前主要按 ETag；Go 标准行为可能自动支持日期或弱 ETag | ETag/date/weak matrix；不得无意改变 200/206 |
| `RISK-022` | Python `mimetypes` 与 Go MIME 表差异 | 同一扩展名可能返回不同 Content-Type | 常见和未知扩展 golden；批准 MIME 表固定在项目内 |
| `RISK-023` | Go `net/http` 默认值 | 自动 chunked、path clean、redirect、gzip、header 和 timeout 行为可能不同 | raw request bytes + fixture request golden；逐项显式配置 |
| `RISK-024` | WPS ID 有数字和字符串形状 | Go `float64` JSON 解码可能损失大整数精度 | 使用 `json.Number`/字符串策略对应测试；超 int64 明确拒绝或保留字符串 |
| `RISK-025` | `docs/research/findings.md:323-327` | refresh 请求形状已观察，但真实轮换仍需低频确认 | 模拟全矩阵后只做一次自然/批准 refresh；失败立即回 Python |
| `RISK-026` | 当前 Basic Auth 和 WPS credential 每次读文件 | 高频请求有额外 stat/open 成本，但也支持运行中轮换 | 性能 profile；若缓存必须测试即时轮换、mtime 相同和原子 snapshot |
| `RISK-027` | 任意 mutation 后大范围 invalidate | 正确但可能制造重复 WPS 列表请求 | 写后并发 list 性能和一致性；优化只能做精确失效并保留 generation |
| `RISK-028` | Python/Go HTTP client 连接复用不同 | TLS 建连次数和上游限流可能改变 | 记录新建/复用连接数；限制 idle pool；不跨 credential/host 复用错误状态 |
| `RISK-029` | settings/root name 进入 HTML、JSON、XML 多种上下文 | 拆前端后易出现 XSS 或转义差异 | 注入字符串同时走 UI、REST、PROPFIND；DOM 中只出现文本，不执行 |
| `RISK-030` | 真实 WPS 私有接口不稳定 | fixture 全绿仍可能在线失败 | 低频 canary、稳定错误分类、Python 快速回滚；不猜测新字段 |

## 16. 分阶段质量门禁

### 门禁 G0：环境与 Python 基线

必须完成：

- Linux Python 3.11~3.14 全部当前测试通过；
- compile、builder、manifest 和 shell 检查通过；
- 记录当前配置默认值与 148 项清单；
- 建立无 secret 的测试数据生成规则；
- 建立 Go、Node/Playwright、Git、Bash 和 Docker 的 CI 环境。

阻断条件：任何规范 Linux 基线失败、测试数不一致、fixture 含真实 secret、无法重现环境。

### 门禁 G1：契约冻结

必须完成：

- 第 6 节 HTTP/REST/DAV 用例全部有稳定编号；
- Python reference 产出客户端响应和上游请求 golden；
- 路径双解码、OPTIONS、PROPFIND body、连接超限、未知方法等模糊行为已有明确决策；
- 动态字段规范化规则经人工审阅；
- 错误状态与文档一致。

阻断条件：只比较状态码、缺少上游请求记录、golden 包含 Cookie/签名 URL、关键行为仍写“以后决定”。

### 门禁 G2：Go 骨架与只读配置

范围仅包括启动、配置、secret、workspace、日志、health、Basic Auth 和静态资源。

必须完成：

- 对应单元、fuzz、race 通过；
- health/auth/framing 黑盒与 Python 一致或有批准差异；
- Linux 文件权限、symlink 和原子替换测试通过；
- 日志 secret marker 扫描为 0；
- SIGTERM 能停止 listener 并清理资源。

阻断条件：为了启动方便把 secret 放环境或命令行、公开 bind 无认证、路径/权限 fail open。

### 门禁 G3：Go 只读 WPS 路径

范围包括 status、list、metadata、workspace/multi-space、缓存和 PROPFIND。

必须完成：

- `WPS-001` 到 `WPS-003`；
- `REST-001`、`REST-004`、`REST-005`；
- `DAV-001` 到 `DAV-005`；
- 0/20/21/1,000/10,000 项目录与 Depth 64/65；
- cache stampede、group/root 切换和多空间隔离 race；
- PROPFIND 峰值内存、响应上限和取消测试。

阻断条件：WPS 错误显示为空目录、跨空间缓存污染、重复 ID 死循环、超限返回截断的 207。

### 门禁 G4：下载与 Range

必须完成：

- `REST-006`、`DAV-006` 到 `DAV-008`、全部 `OBJ` 下载 fixture；
- full、open-ended、suffix、If-Range 和 416 矩阵；
- 签名 URL host/redirect/Cookie 隔离安全用例；
- 客户端断开、对象短读和慢读资源测试；
- 1 MiB/100 MiB/1 GiB 性能对照。

阻断条件：任何字节/hash 不同、对象存储收到 Cookie、200 被包装成 206、取消后继续完整下载。

### 门禁 G5：普通上传与 multipart

必须完成：

- 第 7.2 和 7.3 节每个检查点故障注入；
- 8 MiB 和 50 MiB 阈值两侧测试；
- 全局 spool reservation、多空间总并发和磁盘不足测试；
- checkpoint restart、损坏、过期 session 和清理；
- 100 MiB 内容 hash 对照；
- 上传期间 SIGTERM 和客户端取消。

阻断条件：旧文件被提前删除、登记失败却报成功、spool/reservation 泄漏、multipart part 超内存上限、重试产生无界 session。

### 门禁 G6：写操作、COPY 与 LOCK

必须完成：

- folder、rename、move、delete 的全部 REST/DAV 矩阵；
- task success/failure/timeout/cancel；
- COPY 原生适用条件和 relay 全矩阵；
- 部分树失败与受限清理；
- LOCK acquire/refresh/expiry/restart/race；
- 覆盖目标保持不变的 hash/ID/版本证据。

阻断条件：跨空间写入、错误清理源或旧目标、任务提交即报成功、进程本地锁被宣称为持久锁。

### 门禁 G7：前端与客户端兼容

必须完成：

- 先在 Python 后端完成全部 `UI-*`；
- 再在 Go 后端重复同一 E2E；
- 四个视口无重叠、无 console error、无 secret；
- 目录预取命中、过期、失败、刷新、写后失效和快速导航竞态测试通过；
- 网络记录证明预取最多 2 个并发、每次最多 24 个直接子文件夹，且不访问 WPS 私有域名；
- 至少一个脚本化 WebDAV 客户端和 Windows 映射测试；
- Nginx 或 Caddy HTTPS 路径通过；
- 静态资源缓存和升级后版本一致。

阻断条件：只能通过浏览器手工点击、移动端元素遮挡、路径编码不同、前端直接接触 WPS Cookie/签名 URL。

### 门禁 G8：性能、部署与灰度

必须完成：

- 第 11 节全部本地性能矩阵；
- 峰值 RSS、FD、goroutine、spool 和取消门禁；
- Native 与 Docker fresh install、upgrade、rollback、uninstall-preserve-secret；
- Linux amd64 和 arm64 构建与 smoke；
- 第 13 节真实 WPS 最小验收；
- Go 使用不同端口灰度，Python 保持可启动；
- 至少观察一次凭据轮换、一次 100 MiB multipart、一次失败重试和一次客户端中断。

阻断条件：性能结果不可复现、Go RSS 高于 Python、安装需要用户装 Go/Node、回滚需重新登录、真实 WPS 状态不确定。

## 17. 立即停止、回滚与发布撤销条件

出现以下任一情况，不得继续扩大灰度，必须立即把流量切回 Python：

1. 上传后下载的长度或 SHA-256 与源文件不同。
2. Range 起点、终点、总大小或内容任何一项错误。
3. 文件、目录、虚拟空间的 path、name、parent ID 或 group 路由错误。
4. 跨空间 MOVE/COPY 被错误放行。
5. 已存在目标在未明确允许时被覆盖、删除或变为不可访问。
6. move/delete/copy/upload 在 WPS 未确认终态成功时向客户端返回成功。
7. Cookie、CSRF、`rtk`、Basic Auth、签名 URL、真实对象 ID 或文件正文出现在日志、错误响应、指标 label、trace、core dump 上传或测试报告中。
8. 对象存储请求携带 WPS Cookie、adapter Authorization 或 CSRF。
9. 客户端断开后仍持续显著读取/写入上游，或继续进行正式文件登记。
10. 临时 spool、checkpoint、FD、goroutine、锁或 semaphore 出现持续泄漏。
11. Go 峰值 RSS 高于同条件 Python 基线，或在 1.6 GiB 无 Swap 环境触发 OOM。
12. 第 65 个连接、并发超限、磁盘不足或 response 超限导致整个服务失去 health 响应。
13. 401 并发触发多个不受控 refresh grant，造成 `rtk` 轮换覆盖或会话失效。
14. credential、workspace 或 settings 更新后出现混合版本、权限放宽、symlink 跟随或无法恢复。
15. PROPFIND/COPY 超限仍返回截断成功，或断开后继续遍历大量目录。
16. Go 自动 redirect、path clean、gzip、chunked 或默认 WebDAV 行为突破当前安全边界。
17. 真实 WPS 私有接口返回未识别结构，而 Go 仍继续写入。
18. Native/Docker 升级覆盖或删除 `/etc/wps-adapter/secrets`。
19. 回滚不能在一次服务重启内完成，或回滚后要求重新登录。
20. 任何无法解释且可能影响用户数据的 Python/Go 差异。

### 17.1 回滚操作原则

回滚流程必须预先演练并满足：

1. 停止把新请求路由到 Go。
2. 对活动 Go 写请求设置有限 grace；到期后取消，不允许无限等待。
3. 保存脱敏的请求编号、阶段、错误类别和资源计数，不保存 secret。
4. 切回原 Python service/镜像和原端口配置。
5. 继续使用原有 Cookie、CSRF、Basic Auth、workspace 和 web-settings 文件，不恢复旧 Cookie 备份覆盖新会话。
6. 检查 `/healthz` 和 `/api/v1/status`，再做只读目录与一个已知测试文件 hash 检查。
7. 只处理清单中本轮测试创建的远端对象；状态不确定时先只读确认，不自动批量删除。
8. 暂停 Go 灰度，建立缺陷记录和最小复现；未经完整门禁不得再次上线。

## 18. 每次门禁的输出模板

执行模型完成一个门禁时，必须提交一份简短但可核验的结果记录，至少包含：

| 字段 | 内容要求 |
| --- | --- |
| 门禁编号 | G0 到 G8 |
| commit | Python reference 与 Go candidate 的完整 commit |
| 环境 | OS、架构、CPU/内存限制、Python/Go/Node/Docker 版本 |
| 配置摘要 | 只写非秘密限制值；secret 路径可写，值不可写 |
| 用例统计 | 总数、通过、失败、跳过；跳过逐项说明 |
| 差异统计 | 未批准差异必须为 0；批准差异附决策链接 |
| 性能摘要 | 轮次、p50/p95/p99、吞吐、RSS、FD、临时盘 |
| 安全摘要 | secret marker 扫描、redirect/host/XML/path fuzz 结果 |
| 资源摘要 | cancel 时间、goroutine/thread、slot、spool、checkpoint |
| 真实 WPS | 若未到 G8 写“未执行”；执行后只写脱敏实验编号和结果 |
| 遗留风险 | 风险编号、严重度、负责人、到期门禁 |
| 结论 | 只能是“通过”“阻断”“已回滚”，不得写含糊的“基本通过” |

最终发布只有在 G0 至 G8 全部为“通过”、未批准差异为 0、P0/P1 风险为 0、回滚演练成功后才允许进行。性能提升是发布价值的一部分，但协议正确、数据安全、凭据安全和可回滚性始终拥有否决权。
