# Go 后端逐步迁移细纲

> 本文是执行顺序，不是示例代码。一次只执行一个编号任务。
> 每个任务都必须完成“检查”和“完成条件”，才能勾选并继续。

## 0. 执行规则

### 0.1 每次只做一个小任务

每轮工作固定采用以下流程：

1. 在本文找到最前面未完成的任务编号。
2. 阅读该任务列出的“必读文件”，不要一上来扫描整个仓库。
3. 用一句话复述本任务要保持的行为。
4. 只修改本任务允许的目录或文件。
5. 先运行本任务的最小测试，再运行当前阶段测试。
6. 记录修改文件、测试命令、结果和未解决问题。
7. 只有完成条件全部满足才勾选。
8. 一个任务失败时先修复它，不带着红色测试进入下一任务。

### 0.2 禁止事项

- 不删除或大范围改写 Python 参照实现。
- 不同时移植多个 WPS 操作。
- 不根据函数名猜 WPS 字段，必须逐项对照 Python 源码和脱敏 fixture。
- 不为了让测试通过而放宽路径、host、secret、响应大小或并发限制。
- 不把 Cookie、CSRF、rtk、Basic Auth 密码、签名 URL 或文件正文写入测试快照。
- 不在未完成普通上传前开始 multipart。
- 不在 read-only 契约未通过前实现写操作。
- 不在功能契约未通过前做性能优化。
- 不让 Python 与 Go 两个进程同时对真实账号接收写请求。

### 0.3 每阶段统一完成证据

每个阶段结束必须留下：

- 已完成任务编号。
- 该阶段新增/修改文件列表。
- 单元测试结果。
- 黑盒对照结果。
- `go test -race` 结果（存在并发代码时）。
- 是否访问真实 WPS；若访问，记录脱敏实验编号，不记录原始值。
- 已知差异；没有差异时明确写“无已知差异”。
- 回滚方式。

## 1. 阶段 0：冻结现场，不写 Go 业务

### B000 记录工作区状态

必读：仓库根目录、`README.md`、`pyproject.toml`、`.github/workflows/test.yml`。

步骤：

- [ ] 确认项目版本是 `0.9.8`，并记录 `CHANGELOG.md` 还有 Unreleased 内容。
- [ ] 确认当前分支、提交 ID 和工作区是否已有用户修改。
- [ ] 若 Git 尚不可用，只记录“无法读取 Git 状态”，安装 Git 后再补，绝不假设工作区干净。
- [ ] 保存当前文件清单和各核心文件行数，仅作为迁移记录，不写进发布产物。
- [ ] 确认 `wps_login.py` 是生成物，来源是 `tools/build_login_script.py`。

检查：没有源码或配置被修改。

完成条件：能准确指出当前提交、已有未提交改动、版本和参照测试入口。

### B001 建立 Linux 参照环境

必读：`06-testing-risk-gates.md`、`07-deployment-release-plan.md`。

步骤：

- [ ] 在 Windows 安装 Go 和 Git；Linux/POSIX 测试使用 WSL2 或 CI。
- [ ] 不依赖当前 Windows 的文件 mode/symlink 结果判定 Linux 服务正确性。
- [ ] 在 Ubuntu 环境安装 Python 3.11+、Bash、Git 和必要的进程测量工具。
- [ ] 运行现有 Python 测试、compileall、登录脚本生成检查、release manifest 检查和 shell 语法检查。
- [ ] 若参照测试不全绿，先把失败分类为仓库缺陷、环境缺失或测试平台假设。
- [ ] 不在此阶段顺手修功能；为真实缺陷单独开修复任务。

检查：完整保存命令、通过数、失败数和每个失败的归类。

完成条件：有一个可信的 Ubuntu Python 基线，而不是只使用 Windows 结果。

### B002 记录性能基线

必读：`docs/language-migration.md`、`docs/research/02-large-directory-depth.md`、`docs/research/04-upload-resource-protection.md`。

步骤：

- [ ] 固定同一台 Linux 主机或同一资源限制容器。
- [ ] 固定 CPU、内存、swap、临时盘位置、Python 版本和测试数据大小。
- [ ] 测空闲 RSS、启动时间和不访问 WPS 的 `/healthz` 延迟。
- [ ] 用本地 fake upstream 测列表、PROPFIND、下载、上传和取消。
- [ ] 在本人专用测试目录低频测 1 个小文件与 1 个 100 MiB 文件。
- [ ] 记录吞吐、p50/p95/p99、峰值 RSS、临时盘峰值、文件描述符、上游请求数和取消释放时间。
- [ ] 不做 WPS 并发压测；真实环境只验证完整流程。

检查：数据不含真实文件名、ID、host、Cookie 或签名 URL。

完成条件：后续能在完全相同条件下测 Go，而不是凭感觉比较。

### B003 建立兼容性决策记录

必读：`03-target-architecture.md` 的“必须先由负责人确认的设计决策”。

步骤：

- [ ] 为 D-01 到 D-09 分别建立特征测试。
- [ ] 记录当前 Python 实际结果。
- [ ] 由负责人确认“保持、先修 Python 后移植、或作为有说明的破坏性变更”。
- [ ] 把决定、日期、理由和测试名写回迁移记录。
- [ ] 未确认的项保持阻塞，不让执行模型自行裁定。

检查：每个决定都能指向自动化测试。

完成条件：不存在仅靠口头约定的兼容差异。

## 2. 阶段 1：建立语言无关契约测试

### B100 创建黑盒测试目录

必读：`tests/test_server.py`、`tests/test_storage.py`、`tests/test_smoke.py`。

允许修改：只新增 `contract_tests/` 及测试说明；暂不改 Go/Python业务。

步骤：

- [ ] 定义被测服务地址、Basic Auth、fixture upstream 地址和临时 secret 目录的输入方式。
- [ ] 将测试分成 health/auth、REST、WebDAV、WPS fixture、resource/fault 五组。
- [ ] 测试只通过网络访问服务，不 import Python 内部模块。
- [ ] 所有响应先按协议解析，再比较语义；只有本来要求精确的文本/头才逐字节比较。
- [ ] 为每个场景分配稳定 ID，例如 `HTTP-AUTH-001`，不要依赖执行顺序。
- [ ] 在 README 标出测试是否会修改远端；默认 fixture 测试不得访问真实 WPS。

检查：同一测试命令可指向任意端口的 Python 或未来 Go 服务。

完成条件：测试框架本身不含实现语言假设。

### B101 固定 HTTP/auth/framing 契约

必读：`src/wps_adapter/server.py:355-723`，对应行为见 `02-compatibility-contracts.md`。

步骤：

- [ ] 覆盖 `/healthz` 无认证且不访问 upstream。
- [ ] 覆盖无 Basic、错误 Basic、非法 Base64、非 UTF-8、缺冒号、正确凭据。
- [ ] 核对 401 的 realm、空 body、Content-Length 0 和 Connection close。
- [ ] 覆盖 Transfer-Encoding、多个 Content-Length、负数/非法/缺失长度、短 body。
- [ ] 覆盖 GET/HEAD/OPTIONS 带非零 body。
- [ ] 覆盖控制体、session import、LOCK 的不同大小上限。
- [ ] 用 raw socket 覆盖 keep-alive、关闭和超连接数行为。

完成条件：Python 对这组黑盒测试结果被保存为基线。

### B102 固定 REST 契约

必读：`src/wps_adapter/server.py:1276-1526`、`docs/api.md`。

步骤：

- [ ] 为所有正式路由和兼容别名单独建场景。
- [ ] 对每个接口记录 method、path、query、body、状态、Content-Type、响应 schema。
- [ ] 覆盖 path 默认 `/`、空值、多值、编码字符和非法路径。
- [ ] 覆盖布尔值全部真/假写法、未知值、多值。
- [ ] 覆盖 PATCH 四种目标字段、冲突字段、错误类型和空对象。
- [ ] 覆盖 entry 的 7 个固定公开字段及 null 行为。
- [ ] 覆盖 REST 错误 JSON、WPS 401/其他上游错误和 Retry-After。

完成条件：所有 REST 公开路由都有成功与至少一个失败场景。

### B103 固定 WebDAV 契约

必读：`src/wps_adapter/server.py:838-1274,1528-1752`。

步骤：

- [ ] OPTIONS 检查 DAV 与 Allow。
- [ ] PROPFIND 覆盖 Depth 0、1、infinity、缺失、非法和大小写。
- [ ] 解析 XML 树核对 namespace、href、collection、displayname、length、type、etag、mtime。
- [ ] 固定“忽略 PROPFIND 请求体并返回固定属性集合”的当前行为。
- [ ] 覆盖 GET/HEAD 文件和目录。
- [ ] 覆盖 PUT、MKCOL、DELETE、MOVE、COPY 的成功与冲突状态。
- [ ] 覆盖 Destination 相对/绝对 URL、host、端口、IPv6、query、fragment、userinfo。
- [ ] 覆盖 Overwrite 缺省/T/F/非法。
- [ ] 覆盖 LOCK/UNLOCK 新建、刷新、冲突、过期、继承、上限和非法 XML。

完成条件：Python WebDAV 响应可作为 Go 的协议 oracle。

### B104 固定 WPS fixture 契约

必读：`src/wps_adapter/client.py`、`docs/research/findings.md`、`tests/test_smoke.py`。

步骤：

- [ ] 建立本地 TLS 或受控 HTTP fixture server，不连接真实 WPS。
- [ ] 为列表、status、refresh、folder、rename、move、delete、copy、普通上传、multipart、download 分别建 fixture。
- [ ] fixture 校验 method、path、query 参数名与值、JSON 字段集合与必要类型。
- [ ] 用占位 ID 和中性文件名，不把真实抓包正文直接复制进仓库。
- [ ] signed object fixture 必须验证请求没有 Cookie、Authorization 或 CSRF。
- [ ] 覆盖 3xx、401、403、404、410、500、超时、短读、超大响应和畸形 JSON/XML。
- [ ] 记录允许的字段顺序差异与绝不允许的语义差异。

完成条件：无需 WPS 账号也能复现所有已支持控制流。

## 3. 阶段 2：Go 骨架和生命周期

### B200 初始化 Go module

必读：`03-target-architecture.md` 的最终目录与依赖方向。

步骤：

- [ ] 确定 module path，默认沿用 GitHub 仓库路径；不要临时使用本机目录名。
- [ ] 在 `go.mod` 固定团队确认的最低 Go 版本。
- [ ] 创建 `cmd/wps-adapter` 和最小 `internal/app`、`internal/config` 目录。
- [ ] 第一版骨架只支持 `--version`、`check-config` 和 `serve` 命令形状，不访问 WPS。
- [ ] 版本值先与 Python `0.9.8` 对齐，并预留构建时注入提交信息的位置。
- [ ] 添加格式化、测试、vet 命令说明。

检查：`go fmt`、`go test ./...`、`go vet ./...` 通过。

完成条件：空骨架能构建为 Windows 开发二进制和 Linux amd64/arm64 二进制。

### B201 实现配置结构而不启动服务

必读：`src/wps_adapter/__main__.py:20-84`、`src/wps_adapter/client.py:499-607`、`.env.example`。

步骤：

- [ ] 为每个旧环境变量建立字段、默认值、类型和验证规则。
- [ ] 区分“必须为正数”“允许 0 关闭”“允许空”“必须绝对路径”。
- [ ] 布尔只接受当前 Python 接受的值。
- [ ] DAV/REST 前缀补前导斜线并去尾斜线。
- [ ] URL 与对象存储 suffix 严格限制到 HTTPS/WPS 范围。
- [ ] 配置错误只输出变量名与规则，不输出值。
- [ ] 为边界值、溢出、NaN/Infinity、错误布尔和错误端口写表驱动测试。

检查：同一组环境变量下，Go `check-config` 与 Python 输出语义一致。

完成条件：所有运行环境变量都有测试，没有“稍后再解析”的字符串字段。

### B202 生命周期和信号

必读：`src/wps_adapter/__main__.py:94-155`、部署 service 文件。

步骤：

- [ ] 保持非 loopback 未启用认证时拒绝启动。
- [ ] 实现启动失败退出 1，正常停止退出 0。
- [ ] 保持启动时两行非敏感监听地址输出，或先在契约记录中批准调整。
- [ ] 处理 SIGINT/SIGTERM，停止接收新连接并执行有期限的优雅关闭。
- [ ] 关闭完成后再释放进程资源。
- [ ] 不在 package init 或 `check-config` 中发网络请求。

检查：启动、端口冲突、Ctrl-C、SIGTERM、超时强停都有进程级测试。

完成条件：可可靠启动和停止的空 HTTP 服务存在，但尚未访问 WPS。

## 4. 阶段 3：领域模型、安全文件和本地状态

### B300 移植领域模型与错误分类

必读：`src/wps_adapter/provider.py`、`client.py` 中 WpsStatus/ListPage/UploadOptions。

步骤：

- [ ] 建立 EntryKind 和 RemoteEntry 等最小数据结构。
- [ ] 保留内部 link ID/raw 与公开 REST entry 的隔离。
- [ ] 为 invalid path、not found、not folder、exists、insufficient storage、busy、ambiguous、unsupported 建立可识别错误类别。
- [ ] WPS 错误只保存 operation/category/status，不保存 body 或 URL。
- [ ] 为错误包装后仍可分类写测试。

完成条件：HTTP 层未来不靠错误文本字符串判断状态码。

### B301 实现安全文件读取

必读：`src/wps_adapter/client.py:63-133`、`workspace.py:44-124`、`settings.py:29-114`。

步骤：

- [ ] 分离 Unix 与 Windows 平台文件。
- [ ] Unix 检查绝对路径、父目录、symlink、普通文件、owner、mode、大小、UTF-8。
- [ ] 打开前和打开后都做校验，减少检查与使用之间的竞争窗口。
- [ ] Windows 只用于开发 fixture；不要伪装 POSIX mode 测试已通过。
- [ ] 文件不存在、过大、非 UTF-8、控制字符和权限过宽分别测试。

完成条件：Linux race/symlink 测试通过，secret 内容从不出现在错误或日志。

### B302 实现原子写

必读：`client.py:291-314`、`workspace.py:275-318`、`settings.py:169-196`。

步骤：

- [ ] 只在目标同目录创建临时文件。
- [ ] 设置 0600，完整写入，flush/fsync，原子替换。
- [ ] 失败时清理临时文件，旧目标保持可读。
- [ ] 对 Cookie/CSRF 成对更新增加失败回滚测试。
- [ ] 记录跨平台 rename 语义，Linux 是生产验收标准。

完成条件：模拟每个写入阶段失败都不会留下半截目标或泄漏权限。

### B303 移植 workspace 状态

必读：`src/wps_adapter/workspace.py` 全文。

步骤：

- [ ] 兼容旧 `{group_id,root_id}` 与新 `spaces` schema。
- [ ] 严格验证 ID、空间数量、名称长度、重复 group/name 规则。
- [ ] 按负责人对 D-07 的决定处理控制字符。
- [ ] 实现文件不存在时 auto 尚未登录的状态。
- [ ] 文件 identity/mtime 变化时原子重载；解析失败不应用部分内容。
- [ ] 写入保持旧字段和 `ensure_ascii` 是否属于字节级契约，由 golden 决定。

完成条件：Python 写出的 workspace 可被 Go 读取，Go 写出的可被 Python 读取。

### B304 移植 web settings

必读：`src/wps_adapter/settings.py` 全文。

步骤：

- [ ] 保留固定默认路径和 fallback 优先级。
- [ ] 保留 trim、非空、字符数、UTF-8 字节数和控制字符限制。
- [ ] 支持运行中热加载。
- [ ] 设置写入不访问 WPS。
- [ ] 双向兼容 Python/Go JSON 文件。

完成条件：重启前后名称一致，非法或危险文件 fail closed。

### B305 移植 credential source

必读：`client.py:172-444`。

步骤：

- [ ] 每次 WPS 控制请求前读取当前文件快照。
- [ ] CSRF 文件为空时允许从 Cookie 的 `csrf` 项提取。
- [ ] 解析并合并多条 Set-Cookie，处理 Max-Age/Expires 删除和大小写。
- [ ] Cookie 轮换时同步 CSRF 文件。
- [ ] Cookie/CSRF pair import 第二步失败时尽力恢复旧 pair。
- [ ] 外部 refresh command 无 stdout/stderr 泄漏，受超时限制。
- [ ] 全局串行 refresh；并发测试必须跑 race detector。

完成条件：所有凭据测试只使用虚构值且日志捕获中找不到它们。

## 5. 阶段 4：WPS 控制客户端的只读能力

### B400 创建严格分离的两个 HTTP client

必读：`client.py:139-169,738-787,1300-1374,1607-1769`。

步骤：

- [ ] WPS control client 允许附加当前 Cookie 和可选 Origin/Referer。
- [ ] signed object client 的 API 设计上不接收 Cookie/CSRF。
- [ ] 两者都验证 TLS，都拒绝自动重定向，都有超时和响应关闭规则。
- [ ] 限制 WPS JSON、对象控制正文和 multipart XML 的不同最大字节数。
- [ ] 禁止 client 使用系统或共享 cookie jar 自动携带凭据。

完成条件：fixture 可证明 signed host 收不到任何 WPS/Basic 凭据。

### B401 移植公共 WPS JSON 请求器

步骤：

- [ ] 构造仅允许的 base URL 和编码路径/query。
- [ ] 设置当前必要请求头，不添加浏览器伪装头。
- [ ] 每个响应先处理 Set-Cookie，再有界读取。
- [ ] 只接受 JSON object；数组、标量、空体和超大体为 invalid response。
- [ ] HTTP 错误保存状态，不保存响应体。
- [ ] 401 最多重试一次，重试前重新读取凭据并替换 JSON 中旧 CSRF。
- [ ] 确保重试体可重新读取且大小有界。

完成条件：3xx/401/畸形/超时/过大 fixture 全部通过且无连接泄漏。

### B402 移植登录状态检查

必读：`client.py:832-1191`。

步骤：

- [ ] 缺 workspace 或凭据直接返回 not_configured，不发网。
- [ ] 请求 account `/api/v3/islogin` 并解析粗粒度 account type。
- [ ] 对映射根执行 count=1 的只读列表验证。
- [ ] 按 D-02 决定保证整个 status 路径是否禁止 refresh。
- [ ] 映射六种状态并只返回固定脱敏字段。
- [ ] 成功缓存 30 秒、失败退避 5 秒，key 包含凭据快照+group+root。
- [ ] 并发请求 singleflight；取消一个等待者不破坏其他等待者。

完成条件：状态测试验证请求数、缓存、退避、并发、workspace 权限和无敏感字段。

### B403 移植远端 entry 解析

必读：`client.py:1377-1421`。

步骤：

- [ ] ID 规范成字符串并限制危险值。
- [ ] name 非空、无控制字符、UTF-8 字节上限 4096。
- [ ] kind 只认 file/folder，其余 unknown。
- [ ] size 只接受非负整数。
- [ ] mtime 转为当前公开格式。
- [ ] etag/link_id 分别校验长度和控制字符。
- [ ] raw 只在内部使用，不进入日志或 REST。

完成条件：所有畸形字段单独测试，不能因一个未知 kind 破坏整页解析。

### B404 移植列表和分页

必读：`client.py:1423-1541`、`storage.py:237-262`。

步骤：

- [ ] 精确复刻 v5 list 的 method、path、query 名称和默认顺序。
- [ ] 解析 files、next_offset、next_filter、result。
- [ ] 翻页时保持 WPS 返回的 cursor 规则。
- [ ] 阻止重复 entry ID、重复 cursor、不前进 cursor 和无界页数。
- [ ] 达到 max entries 映射为资源不足，而不是返回截断的成功列表。

完成条件：单页、多页、空页、重复 cursor/ID、超上限 fixture 全通过。

## 6. 阶段 5：路径、缓存、多空间与资源预算

### B500 移植路径解析

必读：`src/wps_adapter/storage.py:27-81`、D-04 决策记录。

步骤：

- [ ] 分开处理 URL transport 解码与业务路径分段。
- [ ] 按已批准决定确保恰好需要的解码次数。
- [ ] 要求绝对路径，规范根和尾斜线。
- [ ] 拒绝空中间段、`.`、`..`、反斜线、NUL、控制字符和超长段。
- [ ] join 时只生成规范业务路径；href 时每段分别 percent encode。
- [ ] 覆盖 `%`、`%25`、`%2F`、`%252F`、`+`、空格、中文、emoji、非法 UTF-8。

完成条件：REST query 与 DAV path 的行为都由 golden 明确，不依赖 Go 默认 URL 清理。

### B501 实现全局 ResourceBudget

步骤：

- [ ] 只由 app 创建一个实例并注入所有空间。
- [ ] 上传、下载、临时盘和连接分别计数。
- [ ] 获取与释放必须可取消、可超时并适用于所有返回路径。
- [ ] 多空间同时请求仍共享 2/4 默认上限。
- [ ] 临时盘预留使用进程内一致视图，并结合实际磁盘可用空间。
- [ ] 暴露的观测值只含数量/字节，不含路径或文件名。

完成条件：N-1/N/N+1 并发与取消测试通过 race detector。

### B502 实现元数据缓存

步骤：

- [ ] 缓存键包含 group/root generation/parent ID。
- [ ] TTL 和最大目录数保持默认值。
- [ ] 冷 miss 同键合并，不同键并行。
- [ ] 淘汰顺序有确定测试。
- [ ] workspace 变化和成功 mutation 清理缓存。
- [ ] 不缓存部分分页或错误结果。

完成条件：并发冷目录只产生一次完整上游分页，切空间后绝无旧 entry。

### B503 实现单空间 Storage

必读：`storage.py:127-510`。

步骤：

- [ ] 从虚拟 root 开始逐层按父 ID+精确名称解析。
- [ ] 0 个匹配为 not found，多个为 ambiguous，文件下钻为 not folder。
- [ ] list/metadata 使用缓存。
- [ ] 上传、创建、重命名、移动、删除先只定义接口和冲突检查，尚不接写 API。
- [ ] 下载打开和关闭与全局下载槽绑定。

完成条件：用 fake WPS client 通过现有 storage 等价测试。

### B504 实现 MultiSpaceStorage

必读：`storage.py:671-800`、workspace schema。

步骤：

- [ ] 虚拟根 ID 和空间虚拟 ID 与旧实现兼容。
- [ ] 根列表只返回配置 mounts，不访问 WPS。
- [ ] 第一段名称选择空间，其余路径交给该空间 storage。
- [ ] workspace 热更新时原子替换路由和 generation。
- [ ] 跨空间 MOVE/COPY 明确 unsupported。
- [ ] 根写入明确拒绝。
- [ ] 按 D-01 决定处理固定单 group、无 workspace 的场景。
- [ ] 所有空间共享 ResourceBudget 和凭据刷新协调器。

完成条件：1、2、128 空间的路由、热更新、重复名和跨空间测试通过。

## 7. 阶段 6：HTTP 基础、设置与 session import

### B600 建立显式路由器

步骤：

- [ ] 不让默认 mux 自动 clean path 或添加不可控重定向。
- [ ] 注册 health、三个网页入口、静态资源、REST 前缀和 DAV 前缀。
- [ ] 保留自定义前缀。
- [ ] 未知路径和未知 method 的结果与 golden 对齐。
- [ ] OPTIONS 当前行为是否不限 DAV 路径，以契约测试为准。

完成条件：route table 测试覆盖尾斜线、encoded slash、自定义前缀和未知路由。

### B601 中间件顺序

固定顺序：连接/请求边界 -> 请求 ID -> 安全日志 -> health 特例 -> Basic Auth -> mutation 同源检查 -> 路由 -> 错误映射。

步骤：

- [ ] health 在认证前处理且不触碰 storage。
- [ ] Basic Auth 每请求热读文件，使用恒定时间比较。
- [ ] 同源保护仅对 mutation 生效；Origin 优先，缺失才看 Referer，两者都无则允许。
- [ ] framing 在业务读取前检查，显式拒绝 Go 已解码的 Transfer-Encoding。
- [ ] panic recovery 只返回固定 500，不回显栈；服务端可记录不含请求数据的栈。

完成条件：中间件顺序测试证明未认证/跨源/超长请求不会调用 storage。

### B602 响应与错误映射

必读：`server.py:522-613,725-760`。

步骤：

- [ ] 统一紧凑 JSON、文本错误换行、Content-Type、Content-Length、no-store。
- [ ] 实现完整领域错误状态表。
- [ ] WPS 401 和其他上游错误使用固定脱敏 JSON code。
- [ ] 流式下载走独立响应路径，不错误添加控制响应 header。
- [ ] 控制响应超过总字节上限返回 507。

完成条件：每个错误类别至少有一个 REST 和一个 DAV golden。

### B603 settings 接口

步骤：

- [ ] GET 返回当前热加载名称。
- [ ] PATCH 只接受且必须恰好有 name 字段。
- [ ] 写入成功后更新虚拟 root，无需重启，不访问 WPS。
- [ ] 并发读写运行 race detector。

完成条件：Python 与 Go 能轮流读写同一个 settings fixture。

### B604 session import

必读：`server.py:1305-1381`、`login.py:181-286,614-746`。

步骤：

- [ ] 限制 body 512 KiB、cookies 1..256。
- [ ] 重新验证 cookie domain/path/name/value，不能信任登录助手已过滤。
- [ ] 必须得到 csrf 和 rtk。
- [ ] 验证 workspace IDs、spaces 上限和显示名称唯一性。
- [ ] 按 D-06 决定只允许 auto 配置更新 workspace。
- [ ] 先在临时/事务计划中验证所有输入，再开始任何文件写入。
- [ ] 成对替换凭据，再写 workspace；任何失败给固定脱敏错误。
- [ ] 成功后 storage 热切换并清缓存，不重启。
- [ ] 响应只返回 status、cookie_count 和可选 workspace 标记。

完成条件：原有 `wps_login.py` 可向 Go fixture 服务同步，并且响应/文件格式兼容。

## 8. 阶段 7：REST 与 WebDAV 只读接口

### B700 REST status/list/metadata

步骤：

- [ ] 先实现 status 和 settings，再实现 entries/list 与 metadata。
- [ ] 保持 path 原输入或规范化输出的现有规则，以 golden 为准。
- [ ] entry 只暴露固定 7 字段。
- [ ] 对文件执行 entries 返回 conflict。
- [ ] 每完成一个 route 就分别跑成功、非法路径、not found、upstream error 场景。

完成条件：REST 只读组对 Python/Go 语义全等。

### B701 WebDAV OPTIONS/HEAD

步骤：

- [ ] OPTIONS 返回精确 DAV 和 Allow 能力。
- [ ] HEAD 文件只用 metadata，不读取对象正文。
- [ ] HEAD 目录返回当前目录 Content-Type 和长度 0。
- [ ] ETag 引号、MIME 和日期格式与 golden 对齐。

完成条件：curl 和至少一个 WebDAV 客户端可完成能力探测。

### B702 PROPFIND Depth 0/1

步骤：

- [ ] 丢弃有界请求体，不解析 prop 选择。
- [ ] Depth 缺省为 1。
- [ ] 生成当前固定属性集与 DAV namespace。
- [ ] href 对每段编码，文件夹以 `/` 结尾。
- [ ] 先实现 0，再实现 1，不提前实现 infinity。
- [ ] 写特殊字符/XML escaping 测试。

完成条件：Depth 0/1 与 Python 解析树相同。

### B703 PROPFIND infinity

步骤：

- [ ] 使用 parent ID 队列/栈遍历，避免每个节点从根重复解析。
- [ ] 保持当前可观察顺序，除非负责人批准差异。
- [ ] 跟踪访问 ID，重复 ID 为上游错误。
- [ ] 在写入 header 前处理“中途超限如何返回 507”的问题；若流式后无法改状态，先用有界 staging 或预遍历方案保证协议正确。
- [ ] 同时限制深度、条目数、输出字节和请求 context。
- [ ] 客户端取消停止后续列表请求。

完成条件：1/1000/10000 条目、深树、循环、超限和断连均有测试。

## 9. 阶段 8：下载与 Range

### B800 签名下载地址解析

必读：`client.py:2466-2561`。

步骤：

- [ ] 请求 download 控制端点，带 support_checksums 和可选 cid。
- [ ] 缺省请求遇 403 时只按已观察流程加 direct=true 重试一次。
- [ ] 只接受 `download_url` 或 `url` 字符串。
- [ ] 在发对象请求前完成 HTTPS/host/port/userinfo/fragment 校验。

完成条件：允许与拒绝 host 的 fixture 全通过，错误不含 URL。

### B801 完整流式 GET

步骤：

- [ ] metadata 先确认 file。
- [ ] 获取全局下载槽。
- [ ] 打开独立 signed client stream。
- [ ] 以配置 chunk size 写客户端，不缓存完整内容。
- [ ] 请求 context 取消时关闭上游。
- [ ] 所有退出路径释放槽和 response body。
- [ ] 已知长度精确设置，未知长度按契约关闭连接。

完成条件：内容 SHA-256 一致；慢客户端、短读、断连无泄漏。

### B802 Range 和 If-Range

必读：`server.py:762-864`、`client.py:1645-1663,2517-2552`。

步骤：

- [ ] 先写 parser 表驱动测试，再接下载。
- [ ] 覆盖 closed/open/suffix 范围、clamp、空文件、未知 size、多范围和溢出。
- [ ] If-Range 仅支持当前 ETag 语义；日期不自行扩展。
- [ ] 上游必须 206，并严格核对 Content-Range、start/end/length。
- [ ] 416 返回 `bytes */N` 或 `*`。
- [ ] HEAD+Range 行为由 golden 固定。

完成条件：所有 Range 场景状态、头和 body hash 与基线一致。

## 10. 阶段 9：低风险 WPS 写操作

通用前置：每个写方法先通过 fixture，再在本人专用测试目录执行一次；每次只创建本轮命名的对象。

### B900 创建文件夹

- [ ] 精确复刻 folder endpoint 和 JSON 字段。
- [ ] storage 先解析父目录并拒绝同名冲突。
- [ ] 成功后清缓存并返回 entry。
- [ ] 测重复名、非法名、权限失败和超时。

### B901 重命名

- [ ] 精确复刻 v3 rename endpoint。
- [ ] 禁止 root；同名返回原 entry；目标冲突不发 WPS 写请求。
- [ ] 成功清缓存。
- [ ] REST 源/目标锁检查留到 LOCK 接入阶段验证。

### B902 异步任务轮询

- [ ] 独立实现 task progress，供 move/delete 共用。
- [ ] 默认间隔、总超时和成功字段与 Python 对齐。
- [ ] context 取消立即停止 sleep/request。
- [ ] finish失败、failed_list、畸形、超时均为脱敏 WPS 错误。

### B903 移动

- [ ] 精确复刻 move JSON。
- [ ] 禁止 root、自身后代、目标非目录和目标同名冲突。
- [ ] 同父目录保持 no-op；跨目录同时改名继续 unsupported。
- [ ] 等 task 真正成功后才返回成功和清缓存。

### B904 删除

- [ ] 精确复刻 delete JSON。
- [ ] 禁止删除虚拟根/空间挂载根。
- [ ] 等 task 成功后再返回 204 和清缓存。
- [ ] 失败/取消不得假装删除成功。

阶段完成条件：REST 与 DAV 的 folder/rename/move/delete 全部黑盒通过；真实测试对象结果人工核对。

## 11. 阶段 10：普通上传

### B1000 请求正文与 spool

必读：`client.py:1553-1605,2258-2375`。

步骤：

- [ ] Content-Length 缺失在读 body 前返回 411。
- [ ] 声明超过 max 时在读 body 前返回 507 并关闭连接。
- [ ] 获取全局上传槽和 spool 预算。
- [ ] 流式计算 MD5/SHA-1/SHA-256 并写内存/磁盘 spool。
- [ ] 实际长度不符失败并关闭连接。
- [ ] 每个错误注入点验证临时文件和预算释放。

### B1001 pre_check 与冲突语义

- [ ] REST 默认不覆盖，DAV PUT 默认覆盖。
- [ ] 只有恰好一个同名 file 才允许 overwrite。
- [ ] 精确发送 pre_check query。
- [ ] 只在 overwrite + 已观察 403 时继续。
- [ ] 不用先删除目标实现覆盖。

### B1002 create_update 与对象 PUT

- [ ] 精确构造已确认字段和类型。
- [ ] 校验 WPS 指令 method、expected code、signed URL 和 store。
- [ ] 从 spool 开头流式发送，设置 Content-Length/Type，不带 Cookie。
- [ ] 对象失败每次重新获取签名 URL，指数退避，最多配置次数。
- [ ] 获取并规范化 ETag；响应体有界。

### B1003 文件登记

- [ ] 精确发送 file registration 字段。
- [ ] 只有 result 成功且 entry 可解析才向客户端返回成功。
- [ ] 登记失败记录“可能存在未登记对象”的脱敏告警，不尝试未知删除 API。
- [ ] 无论登记成功失败都清理 spool 和资源。

阶段完成条件：0B、阈值内文件、阈值边缘、覆盖、重试、磁盘不足、断连、登记失败全覆盖；上传后下载 hash 一致。

## 12. 阶段 11：multipart 上传与检查点

### B1100 检查点格式

必读：`client.py:1996-2075`。

- [ ] 文件名只由脱敏 identity hash 决定。
- [ ] schema version、identity、upload_id、key、store、part_size、parts 全验证。
- [ ] 使用 securefile 原子写、0600、绝对 resume dir。
- [ ] 畸形/错 identity/错 part size 不复用。
- [ ] 检查点不保存正文或凭据。

### B1101 初始化与分片大小

- [ ] 达到 threshold 才进入 multipart。
- [ ] overwrite 继续 unsupported，且记录当前在何时拒绝；后续可优化为更早拒绝但需契约决定。
- [ ] 精确发送 block init。
- [ ] 根据 min/max/max_parts 调整大小。
- [ ] 单片超过 64 MiB 直接拒绝。

### B1102 单片上传

- [ ] 从 spool 精确读取每片，不把全文件放内存。
- [ ] 同时生成 hex MD5 与 Base64 Content-MD5。
- [ ] 严格校验 WPS 返回的 PUT/body_type/header/expect_code。
- [ ] signed PUT 不带 Cookie，收集 ETag。
- [ ] 每片完成后原子更新 checkpoint。
- [ ] 重试时不会跳过未确认成功的片。

### B1103 session 失效恢复

- [ ] 只有已确认的 400/404/410 条件触发重建。
- [ ] 重建后清空旧 parts，不混用 upload_id。
- [ ] 限制重建次数，避免无限循环。
- [ ] 并发同 identity 有互斥测试。

### B1104 merge 与登记

- [ ] 精确发送 merge part_infos。
- [ ] 严格验证返回 POST/data/XML/Content-Type/expect code。
- [ ] 拒绝 DTD/entity，有限读取并解析 ETag。
- [ ] 用合并 ETag 完成 file registration。
- [ ] 只有最终登记成功才删除 checkpoint。

阶段完成条件：100 MiB 的 10 MiB 分片 fixture、重启续点、单片失败、session失效、merge失败、登记失败和断连全部通过；真实一次上传下载 hash 一致。

## 13. 阶段 12：COPY 与 DAV LOCK

### B1200 原生单文件 COPY

- [ ] 只允许同空间、普通文件、目标 basename 与源相同。
- [ ] 精确复刻 v3 batch copy 和唯一 file ID 响应。
- [ ] 任一条件不满足转中继或 unsupported，不谎报目标路径。
- [ ] 目标已存在绝不先删。

### B1201 文件中继 COPY

- [ ] 使用下载槽和上传槽，处理获取顺序以避免互相等待死锁。
- [ ] 源流不整体读入内存；上传仍按正常 spool/hash 流程。
- [ ] 允许复制时改名。
- [ ] 取消同时关闭两侧资源。

### B1202 文件夹 COPY

- [ ] Depth 0 只建目标根。
- [ ] Depth 1 复制/创建直接子项但不深入。
- [ ] infinity 受最大深度/条目限制。
- [ ] 目标根是本请求新建且中途失败时 best-effort 清理。
- [ ] 清理失败只记录脱敏告警，不误删旧目标。
- [ ] 明确非事务性，不承诺完全回滚。

### B1203 Lock Store

必读：`server.py:104-225,919-1009`。

- [ ] token、path、depth、owner、timeout、expires 字段齐全。
- [ ] exact 与 infinity 后代适用规则一致。
- [ ] 单调时间驱动过期，操作时清理。
- [ ] 最大 4096；所有并发访问 race-safe。
- [ ] 锁是进程内的，重启后消失。

### B1204 LOCK/UNLOCK 协议

- [ ] 从 If/Lock-Token 提取当前 token 形状。
- [ ] 限制 body 64 KiB，拒绝 DTD/entity，owner 压空白并截断。
- [ ] Depth/Timeout 默认、范围与错误完全对齐。
- [ ] 新锁、空资源锁、refresh、冲突、unlock 状态正确。
- [ ] 所有 REST/DAV mutation 对源和精确目标检查锁。

阶段完成条件：COPY 深度/失败残留与 LOCK 并发/过期/继承/刷新均通过黑盒和 race 测试。

## 14. 阶段 13：完整服务整合

### B1300 组装依赖

步骤：

- [ ] config -> secure state -> credentials/workspace/settings -> HTTP clients -> global budget/cache -> storage -> handlers -> server，顺序固定。
- [ ] 构造失败时关闭已经创建的资源。
- [ ] `check-config` 只走到本地校验，不构造会联网的后台动作。
- [ ] 所有 child space 共用全局资源与刷新协调器。

### B1301 接入静态前端

依赖：先完成 `05-frontend-plan.md`。

- [ ] 用嵌入资源服务 index/style/app。
- [ ] 三个旧入口保持可用。
- [ ] 动态名称通过 settings API，不再拼 HTML。
- [ ] 静态资源认证、Content-Type、CSP、缓存策略通过 E2E。

### B1302 全量对照

- [ ] 在不同端口启动 Python 和 Go，使用同一只读 fixture 配置。
- [ ] 对每个黑盒场景比较状态、关键头、JSON/XML 语义和 body hash。
- [ ] 写操作使用各自隔离 fixture，不让两个服务争用同一真实状态。
- [ ] 差异逐条分类：实现缺陷、基线缺陷、批准变更。
- [ ] 未批准差异必须归零。

### B1303 全量静态与并发检查

- [ ] `go fmt` 无差异。
- [ ] `go vet ./...` 通过。
- [ ] `go test ./...` 通过。
- [ ] `go test -race ./...` 通过。
- [ ] parser fuzz 达到规定时长且无崩溃。
- [ ] Linux amd64/arm64 构建通过。
- [ ] 二进制启动、health、优雅停止 smoke 通过。

完成条件：Go 服务具备全部旧能力，但尚未替换生产入口。

## 15. 阶段 14：交给部署、灰度和发布

本阶段不在本文重复部署细节，依次执行：

1. `05-frontend-plan.md` 的最终浏览器验收。
2. `06-testing-risk-gates.md` 的完整门禁。
3. `07-deployment-release-plan.md` 的 Native/Docker/CI/发布步骤。
4. `08-executor-checklist.md` 的灰度、回滚演练和最终签字。

只有以下全部为真才能切换默认服务：

- [ ] Python/Go 契约无未批准差异。
- [ ] 真实 WPS 专用目录的列表、小上传、下载、Range、目录、重命名、移动、删除通过。
- [ ] 100 MiB multipart 上传后下载 SHA-256 一致。
- [ ] 至少一次真实凭据轮换/重新导入无需重启。
- [ ] 客户端断连后资源及时释放。
- [ ] Native 与 Docker 均演练升级和回滚。
- [ ] 回滚不要求重新登录、不删除 secrets、不删除 WPS 文件。

## 16. Python 参照实现的退出条件

Go 首次发布后不要立刻删除 Python 服务代码。满足以下条件后才单独规划退役：

- [ ] 至少一个稳定发布周期无严重兼容回归。
- [ ] 支持的 Windows/NAS/WebDAV 客户端矩阵通过。
- [ ] 至少完成一次真实 session refresh 或重新导入生命周期。
- [ ] multipart、COPY、LOCK 和多空间在生产式灰度中使用过。
- [ ] Go 性能报告可复现且没有通过降低安全限制获得优势。
- [ ] 旧版本二进制/镜像、配置和回滚说明仍可获取。
- [ ] 登录助手的 Python 运行要求在 README 中继续明确。

即使服务端 Python 退役，`wps_login.py` 仍可作为独立工具保留，直到另一个经过完整安全和浏览器兼容验收的登录实现取代它。
