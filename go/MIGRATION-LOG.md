# Go 重写迁移记录

本文件是 `docs/go-rewrite-plan/` 的执行工作记录，只作迁移记录，不进发布产物。
规则见 `docs/go-rewrite-plan/08-executor-checklist.md`：一次只做一个小任务，
测试不过不进入下一任务，每完成任务一个提交。

## B000 记录工作区状态

日期：2026-09-05

- 当前提交：`25c2784e6aebdef4997b84baaa7b61b769552935`（分支 `main`）。
- 工作区状态：干净（`git status --porcelain` 为空），无用户未提交改动。
- 项目版本：`0.9.8`（`pyproject.toml`），`CHANGELOG.md` 仍有 `[Unreleased]` 内容。
- 参照测试入口：`PYTHONPATH=src python -m unittest discover -s tests -v`，
  CI 配置在 `.github/workflows/test.yml`。
- `wps_login.py` 是生成物，来源为 `tools/build_login_script.py`。
- Python 参照实现规模（迁移期保留，不修改）：

| 文件 | 行数 |
| --- | --- |
| src/wps_adapter/client.py | 2598 |
| src/wps_adapter/server.py | 1854 |
| src/wps_adapter/web.py | 1064 |
| src/wps_adapter/storage.py | 857 |
| src/wps_adapter/login.py | 1547 |
| src/wps_adapter/workspace.py | 326 |
| src/wps_adapter/login_command.py | 319 |
| src/wps_adapter/har.py | 457 |
| src/wps_adapter/settings.py | 209 |
| src/wps_adapter/__main__.py | 160 |
| src/wps_adapter/provider.py | 104 |
| src/wps_adapter/__init__.py | 34 |

- 本机工具链：Linux (zen kernel)，`go1.27.0`，git 可用。
- Go 代码放在本仓库 `go/` 目录下，module path 为
  `github.com/galiandan/WPS_2_WebDAV/go`；包结构遵循
  `03-target-architecture.md` 第 4 节（cmd/wps-adapter + internal/*）。

检查：没有源码或配置被修改。

## B001 建立 Linux 参照环境

日期：2026-09-05

本机即 Linux（zen kernel，x86_64），无需 WSL。工具链实测：

- Python `3.14.7`（满足 CI 矩阵 3.11-3.14 的上限）。
- Go `go1.27.0`（`-X:nodwarf5` 本地变体）。
- Git 可用，remote 为 `git@github.com:galiandan/WPS_2_WebDAV.git`。

运行命令与结果：

1. `PYTHONPATH=src python -m unittest discover -s tests`：
   首次运行 155 项，1 failure。
2. `PYTHONPATH=src python -m compileall -q src tests wps_login.py`：通过。
3. `bash -n scripts/install-native.sh scripts/install-docker.sh`：通过。
4. 登录脚本生成检查（`tools/build_login_script.py --check`，由测试套件覆盖）：通过。
5. release manifest 检查（`tools/build_release_manifest.py --check`，由测试套件覆盖）：首次失败。

失败归类（按 B001 要求）：

- `test_release_manifest_matches_its_builder`：**仓库缺陷**，非环境缺失、非平台假设。
  `docs/go-rewrite-plan/` 与 `go/MIGRATION-LOG.md` 在最近提交中加入后，
  生成产物 `release-manifest.txt` 未重新生成。修复方式是运行
  `python tools/build_release_manifest.py` 重新生成（纯生成产物，无行为变化），
  已包含在本任务提交中。未发现 Windows 平台特有的失败（与 00-README 第 8 节
  描述的 Windows 结果不同，Linux 上仅此 1 项失败）。
- 未发现需要单独开修复任务的真实功能缺陷；未顺手修改任何功能。

修复后复跑：`PYTHONPATH=src python -m unittest discover -s tests` →
**155 项全部通过（OK）**。

完成条件达成：已有可信的 Linux Python 参照基线（155 全绿 + compileall + shell
语法 + 两个生成物检查），后续任务以它为协议 oracle。

回滚：本任务仅重新生成 `release-manifest.txt` 并追加本记录；回滚即
`git revert` 本提交。

## B002 记录性能基线

日期：2026-09-05

工具：新增 `go/benchmarks/python_baseline.py`（迁移专用，不进发布产物）。
方法：真实 Python 适配器跑在子进程，WPS 传输层换成进程内 fake upstream
（使用 client 本身提供的测试注入点，未修改参照实现）；父进程经真实
loopback HTTP 驱动并测量。完整数据在
`go/benchmarks/results/python-baseline.json`（同仓库保存，全部为
bench-* 占位数据，无任何真实 ID/Cookie/URL）。

环境：Linux 7.2.2-zen1-1-zen x86_64，Python 3.14.7，16 CPU，15 GiB RAM。
配置：默认参数（list_count=20、cache_ttl=2s、stream_chunk=1MiB、
max_uploads=2、max_downloads=4），仅 WPS_UPLOAD_MIN_FREE_BYTES=0 与
关闭 Basic Auth（loopback 基准）。

主要结果（p50，另见 JSON）：

| 场景 | 数值 |
| --- | --- |
| /healthz keep-alive | 41.0 ms |
| /healthz 新建连接 | 0.15 ms |
| /api/v1/status 冷 | 0.74 ms（fake upstream） |
| /api/v1/status 缓存命中（keep-alive） | 41.0 ms |
| REST 列表 204 条 冷（10 页上游分页） | 3.2 ms |
| REST 列表 204 条 热缓存（keep-alive） | 41.0 ms |
| PROPFIND Depth 1（204 条） | 6.2 ms |
| 下载 8 MiB / 64 MiB | 79 / 98 MiB/s，SHA-256 校验一致 |
| 上传 1 MiB / 8 MiB（spool+三摘要+signed PUT+登记） | 167 / 359 MiB/s，SHA-256 校验一致 |
| RSS 空闲 / 峰值 | 31.4 MiB / 43.3 MiB |
| 上游请求数（全部场景累计） | control list 89 次、islogin 1、object GET 5、PUT 2 |
| 文件描述符（结束后） | 4（无泄漏） |

发现 1（重要，影响所有 keep-alive 小响应）：Python 服务端接受的套接字
未禁用 Nagle（`disable_nagle_algorithm=False`）且响应为多次小段写，
keep-alive 连接上每个小响应稳定多出约 40 ms（41ms vs 新建连接 0.15ms；
在服务端 handler 上设置 TCP_NODELAY 后同场景 0.056 ms，客户端侧设置
无效，证明停顿在服务端）。该行为是当前 Python 可观察行为的一部分；
Go 侧（net/http 默认 TCP_NODELAY）不会复现。**属于潜在行为差异，
是否要求 Go 复现该停顿交由负责人决定（默认建议：不复现，记录为
已批准变更，因为它不是协议语义而是 TCP 交互特征）。**

发现 2（客户端取消释放）：断开检测只在流循环的两个 chunk 之间进行。
- RST 断开（SO_LINGER(0)，写路径立即出错）：上游对象流 138 ms 内释放，
  下载槽立即可复用。
- 干净 FIN 断开（接收缓冲区空）：服务端阻塞在 `sendall`，直到
  `ADAPTER_REQUEST_TIMEOUT`（默认 60 s）写超时才释放上游流；实测
  60.4 s。这是当前真实行为，Go 需要决定是否复现（见 D 系列决定，
  未决定前以保持行为为准）。

未运行项：真实 WPS 专用目录的低频小文件与 100 MiB 文件测试。原因：
执行环境无负责人凭据，且红线禁止执行模型访问真实账号；此项归入
灰度阶段（M1002）由负责人执行。fake upstream 方法论已固定，Go 用
同一 harness 形状对比。

回滚：仅新增 `go/benchmarks/` 与本记录；`git revert` 本提交即可。

## B003 兼容性决策记录（D-01 至 D-09）

日期：2026-09-05

新增 `contract_tests/`（黑盒契约测试目录，同时是 B100 的基础设施）：
真实 Python 服务以子进程运行（生产入口 + 生产环境变量 + 生产 secret 文件
语义），仅上游传输层为进程内 fake；`python -m unittest discover -s
contract_tests -v` 当前 **13 项全部通过**。每个场景的观察结果保存在
`contract_tests/results/DEC-*.json`。

按负责人指示"严格遵守 docs/go-rewrite-plan 指导"，D-01..D-09 采用
`03-target-architecture.md` 第 17 节的推荐决定作为工作决定；下表
"当前行为"列均有自动化测试证据。若负责人日后否决某项，仅需更改对应
决定并调整 Go 侧契约，特征测试本身就是证据链。

| 编号 | 当前行为（证据/测试） | 决定（采纳文档推荐） | 破坏性 |
| --- | --- | --- | --- |
| D-01 | `WPS_GROUP_ID=auto` 且无 workspace 文件：根列表返回 200+空列表（DEC-D01-A）；固定 group：正常单空间（DEC-D01-B） | 不移植"空根成功"；Go 在 auto+未配置时显式报错 | 是（对未配置部署） |
| D-02 | status 根列表 401 会触发凭据刷新（refresh 命令已执行，DEC-D02-A） | status 全流程禁止刷新；Go 对 status 探测关闭 401 重试 | 是（行为收紧） |
| D-03 | 2 空间挂载时 4 个上传同时进入 pre_check，全局 WPS_MAX_UPLOADS=2 被放大（DEC-D03-A） | Go 实现真正进程级全局 ResourceBudget | 是（资源行为收紧） |
| D-04 | REST 业务路径被二次解码：`%2Fweird%252Fname.txt`→404 "weird"；`%25252F` 才命中字面 `%2F` 条目；`+`→空格（DEC-D04-A） | Go 全入口只解码一次；以 DEC-D04 的反向 golden 固定 | 是（修正） |
| D-05 | 仅配用户名文件时 0.0.0.0 可启动且所有请求 401（DEC-D05-A）；完整凭据正常（DEC-D05-B）；完全未配置则拒绝启动（DEC-D05-C） | Go 非本地 bind 必须用户名与密码都有效，启动期失败且错误不含值 | 是（启动期失败提前） |
| D-06 | 固定 WPS_GROUP_ID 但存在 workspace 文件时 session import 可改映射并返回 200（DEC-D06-A/B） | 只有 auto 配置允许改映射；Go 按配置来源判断，不按文件是否存在 | 是（收紧） |
| D-07 | session import 接受含换行的 mount 名并原样返回（DEC-D07-A） | Go 拒绝 mount 名中的控制字符，错误指明配置无效 | 是（安全收紧） |
| D-08 | PROPFIND 忽略请求 body，固定返回完整属性集（含 getlastmodified/getetag）（DEC-D08-A） | 保持：Go 首版固定属性集合，不引入通用 WebDAV 库语义 | 否 |
| D-09 | 超过 max_connections 的 TCP 连接在 accept 后被直接关闭，无任何 HTTP 状态（probe 收到 EOF，DEC-D09-A） | 保持：首个兼容版本维持并记录，后续再评估 503 | 否 |

说明：D-01/02/03/04/05/06/07 的"先修 Python"步骤未在本轮执行（参照实现
保持冻结，避免语言迁移与行为修正混在一个变更里）；Go 按上表"决定"列
实现，B1302 对照时这些差异按"批准变更"归类，归类依据即本表与
`contract_tests/results/`。

安全检查：`results/` 与上游记录仅含 bench-* 占位值；未发现任何真实
凭据/ID/签名 URL。

回滚：仅新增 `contract_tests/` 与本记录；`git revert` 本提交即可。

## B100 黑盒测试基础设施定稿

日期：2026-09-05

`contract_tests/` harness 定稿（B003 期间建立，本任务收尾）：

- `harness.Service`：以子进程启动被测服务；预分配端口；0700 临时目录 +
  0600 secret 文件；就绪信号为子进程 stdout 的 `listening=` 行（不做端口
  探测，避免占用连接槽）；`stop()` 收集子进程 stderr 尾部用于诊断。
- `python_service.py`：走真实 `wps_adapter.__main__.main` 生产入口；
  仅注入 fake 传输层与测试用 web-settings 路径
  （`CONTRACT_WEB_SETTINGS_FILE`，生产默认 `/etc/...` 无法在测试机写入）；
  `CONTRACT_TRACEBACKS=1` 可输出未预期异常栈。
- `fake_upstream.py`：scenario JSON 驱动（路由正则/状态码/延迟/barrier/
  对象内容）；内置 islogin/grant_token/列表/上传/登记/文件夹/重命名/
  任务轮询端点；全部请求写 JSONL 记录；计数原子写 stats。
- `scenario()` 支持 listing/children/objects 覆盖。

检查：`python -m unittest discover -s contract_tests` 与参照套件全绿。

## B101 HTTP/auth/framing 契约

日期：2026-09-05

新增 `contract_tests/test_http_auth.py`，23 项全部通过，证据在
`results/HTTP-HEALTH-*.json`、`results/HTTP-AUTH-*.json`、
`results/HTTP-FRAMING-*.json`。固定要点：

- `/healthz` 无认证、不访问 upstream（upstream 记录为空）；带错误凭据仍 200。
- Basic Auth：缺失/错误/非法 Base64/非 UTF-8/缺冒号/未知 scheme → 401，
  `WWW-Authenticate: Basic realm="wps-adapter"`、`Connection: close`、
  `Content-Length: 0`、空 body；scheme 大小写不敏感；正确凭据 200。
- framing 检查先于认证：Transfer-Encoding、多个 Content-Length、
  GET/HEAD/OPTIONS 带非零/负数/非法长度 body → 400；Content-Length: 0 放行。
- PUT 无 Content-Length → 411；控制 body > 1 MiB → 413；session import
  > 512 KiB → 413；LOCK body > 64 KiB → 413。
- 声明 100 字节只发 10 字节并关闭 → 服务端返回 4xx/5xx（记录实际值），
  绝不返回 201。
- keep-alive 同连接两个请求均 200；401 响应带 `Connection: close`。

## B102 REST 契约

日期：2026-09-05

新增 `contract_tests/test_rest.py`，39 项全部通过，证据在
`results/REST-*.json`。固定要点：

- status schema 六字段；settings GET/PATCH（trim、非空、256 字符上限、
  拒绝控制字符/非字符串/多余字段/非法 JSON）。
- entries/list 别名等价；entry 固定 7 字段；缺失字段 → null
  （fsize 非法 → size null）；path 默认 `/`、空值/多值/相对/穿越 → 400；
  文件上 list → 409；未知路由 → 404 "unknown REST route"。
- metadata/download；download 带 attachment Content-Disposition 与对象
  字节（SHA-256 一致）。
- upload：201 + {path, entry}；对象 PUT 字节 SHA-256 与请求一致；
  已存在同名默认 409 且不发 pre_check；overwrite=true 继续
  pre_check-403 → 201；新名 + pre_check 403 默认 → 502
  （upstream_status=403）；布尔 1/true/yes/on/TRUE → 真，
  0/false/no/off → 假，maybe/多值 → 400。
- folders/folder 别名 201；同名 409。PATCH name/fname/destination/
  parent_path 四种目标、冲突字段 400、空对象 400、改名撞名 409、
  同父移动 no-op（无上游 move 调用）、移动进自身 400、跨目录改名 501。
- delete 204 + 两个别名；根删除 400。
- 上游 500 → 502 {"error","code":"wps_unavailable","upstream_status":500}
  （上游正文不透传）；上游 401（auto_refresh 关闭）→ 503
  code=wps_session_expired + Retry-After: 60。

## B103 WebDAV 契约

日期：2026-09-05

新增 `contract_tests/test_webdav.py`，37 项全部通过，证据在
`results/DAV-*.json`。固定要点：

- OPTIONS（含 DAV 前缀外路径）→ `DAV: 1,2` 与固定 Allow 列表。
- PROPFIND：Depth 0/1/infinity/缺省(=1)；非法 Depth → 400；
  合法值大小写不敏感；D: 命名空间、href 逐段编码、目录 href 以 `/` 结尾、
  固定属性集（displayname/getcontentlength/getcontenttype/getetag/
  getlastmodified/resourcetype）；请求 body 忽略；XML 转义正确；
  前缀外 404。
- GET/HEAD：文件 ETag 带引号、MIME 猜测、Accept-Ranges、no-store、
  Connection: close；目录 GET 409、HEAD httpd/unix-directory + 长度 0。
- PUT（默认 overwrite=true，撞 pre_check 403 继续 → 201）+ Location；
  MKCOL 201/409；DELETE 204、根 400。
- MOVE：Destination 校验（缺 header/query/fragment/userinfo/跨 host/
  跨 port/前缀外 → 400，绝对 URL 同 host+port 允许）；目标存在时
  默认 T → 501、F → 412、非法值 → 400；同路径 MOVE 允许（201）。
- COPY 文件走下载+上传中继（对象字节 SHA-256 一致）；文件夹 Depth 0
  仅建目标根、Depth 1 复制直接子项；目标存在 501/412。
- LOCK：新建 200（已存在资源）/201（lock-null）；If token 刷新保号；
  文件级兄弟锁互不冲突；祖先锁与已有后代锁冲突 → 423；目录 infinity
  锁阻止后代写、带 token 放行；Depth 1 → 400；DOCTYPE/entity → 400；
  Timeout Infinite/超大钳制到 86400（响应体可少 1 秒）、非法 → 400；
  UNLOCK 错 token 409、缺 header 400、成功 204。
- 跨源写保护：带非同源 Origin 的 PUT → 403；同源放行；读请求不受限。

检查：`python -m unittest discover -s contract_tests` → 112 项全绿；
参照套件 155 项全绿（release-manifest.txt 因新增文件重新生成）。

回滚：`git revert` 相应提交即可；契约测试不进入发布产物。

## B104 WPS fixture 契约

日期：2026-09-05

新增 `contract_tests/test_wps_fixtures.py`（7 项，client 级 fixture，全部
通过），证据在 `results/WPS-FIXTURE-*.json`。fake upstream 升级为严格
fixture：对每个请求校验 method、path、query 参数名与值、JSON 字段集合
与类型（violation 记录在 stats，测试断言为零）；对象存储侧记录全部
请求头，出现 Cookie/Authorization/CSRF 即 violation。

- WPS-FIXTURE-001 普通上传：pre_check/create_update/register 各 1 次，
  对象 PUT 只有 Content-Type/Length（无任何凭据），对象字节 SHA-256 与
  请求一致；register 携带 40 位 sha1、64 位 sha256。
- WPS-FIXTURE-002 multipart（2.5×分片）：init 1、part 3、merge 1、
  register 1；每片对象 PUT 的 MD5 与该分片内容一致；分片大小符合
  instruction；成功后 checkpoint 文件清理。
- WPS-FIXTURE-003 download：download_url 解析带固定 support_checksums；
  对象 GET 不带 Cookie/Authorization。
- WPS-FIXTURE-004/005 401 刷新：SDK grant_token（Set-Cookie 轮换落盘，
  旧凭据重试后仍 401）与外部刷新命令（命令执行、无 grant 调用）。
- WPS-FIXTURE-006 状态注入：301/403/404/410/500 均映射为脱敏
  WpsApiError（保留上游状态码，不含正文）。
- WPS-FIXTURE-007 注入：畸形 JSON → invalid_response；超大响应 →
  上限保护；上游延迟超过超时 → unavailable。

阶段 1（语言无关契约）至此完成：黑盒 harness、HTTP/auth/framing、
REST、WebDAV、WPS fixture 五组全部就绪，`python -m unittest discover
-s contract_tests` 共 119 项全绿；参照套件 155 项全绿。允许差异记录：
multipart merge 的 XML 命名空间由客户端按本地名匹配（fixture 用
CompleteMultipartUploadResult 结构）；其余请求形状均为逐字段固定。

回滚：`git revert` 本提交；fixture 不进入发布产物。

## M2/F0 前端拆分基线冻结

日期：2026-09-05

按 05-frontend-plan.md §11 阶段 F0 与 §20 第 1 步执行：不改页面行为，
只记录现状并补充特征测试。

新增特征测试（tests/test_server.py）：
- /、/web、/web/ 三个入口返回完全相同的字节，Content-Type
  text/html; charset=utf-8，Cache-Control: no-store；并逐字符固定当前
  CSP 头值（含 'unsafe-inline'，F4 收紧时更新该断言）。
- Basic Auth 启用时三个入口未认证均 401 + Basic realm="wps-adapter" +
  Connection: close + 空 body。
- render_web_app 对 U+2028 行分隔符在内联脚本 JSON 位置转义为 \u2028。

FE-01..FE-08 负责人决定（采纳 05-frontend-plan.md §8 推荐项，标注待追认）：
- FE-01 改名后左上角品牌文字不更新：采纳“拆分前补测试并修正”，F3 中
  settings 成功后同步更新品牌文字（批准变更候选）。
- FE-02 前端写死 /api/v1/：采纳“第一版明确只保证默认前缀”，不新增
  只读前端配置（待负责人追认）。
- FE-03 迁移旧文档声称存在上传取消：本提交修正
  docs/language-migration.md 措辞；等价阶段不新增取消功能。
- FE-04 同名文件夹上传静默跳过：冻结现状，浏览器 E2E 冻结后再议。
- FE-05 路径不进地址栏：保持，不引入前端路由。
- FE-06 移动靠手写目标路径：保持。
- FE-07 移动端 760px 表格横向滚动：保持。
- FE-08 无浏览器 E2E：本环境无浏览器、Node 与 pip，无法执行 §17 E2E
  与 §11 F0 的四张基线截图（桌面有文件/桌面空目录/移动端有文件/WPS
  未配置）及焦点顺序记录。该项与 M203、M205 一并作为负责人侧门禁，
  须在具备浏览器的环境补齐后才允许关闭 M2 里程碑。

初始加载网络顺序（由 web.py 内联脚本静态读出，REST 契约测试互证）：
渲染根面包屑 → GET /api/v1/status →（仅 connected 时）GET
/api/v1/entries?path=/ → 按返回顺序后台预取直接子文件夹（≤24 个、
并发 ≤2、TTL 30s，仅写浏览器内存缓存）。写操作请求形状已由
contract_tests/test_rest.py 逐项固定：folders POST 空体、entries PATCH
只含 name 或 parent_path、DELETE 204 空响应、upload PUT 原始字节、
settings GET/PATCH。

检查：python -m unittest discover -s tests 全绿；contract_tests 119 项全绿。

回滚：git revert 本提交。

## M2/F1 提取 style.css

日期：2026-09-05

按 05-frontend-plan.md §11 阶段 F1 与 §20 第 3-5 步执行（CSS 机械提取，
Python 参照服务白名单提供）。

- 新增 go/web/style.css：从 WEB_APP_TEMPLATE 的 <style> 块逐字节复制，
  不调整颜色、空格、选择器、断点或尺寸（197 行）。
- web.py：模板 <style> 块替换为固定同源链接
  <link rel="stylesheet" href="/assets/style.css">；新增白名单资源
  装载器 load_web_asset/web_asset_content_type/web_assets_dir——
  文件名必须先命中清单，才允许参与路径拼接；目录解析顺序为
  WPS_ADAPTER_WEB_ASSETS_DIR 环境变量 → 仓库 go/web/。
- server.py：GET/HEAD /assets/<清单名> 经 Basic Auth 后返回资源，
  MIME text/css; charset=utf-8、Cache-Control: no-store；清单外名称
  （含 ../ 穿越、百分号编码变体、子路径）一律 404 并关闭连接，与
  未知 DAV 路由行为一致。
- 桥接不注入任何运行时文本（FE-02 采用“只保证默认前缀”，不新增
  前端配置）。

新增/更新测试：静态资源 5 项（字节一致、MIME/no-store、页面外链、
清单外 404、HEAD 仅元数据、未认证 401）+ 装载器 2 项（白名单拒绝、
目录覆盖生效）。

检查：tests 165 项全绿；contract_tests 119 项全绿；
release-manifest.txt 已重新生成。

回滚：git revert 本提交。

## M2/F2 提取 app.js

日期：2026-09-05

按 05-frontend-plan.md §11 阶段 F2 执行：只机械提取脚本，不改函数名、
调用顺序、状态字段或文案。

- 新增 go/web/app.js（710 行）：内联 IIFE 逐字节复制，含
  "use strict"、目录缓存/预取常量（TTL 30s、并发 2、上限 24）与
  全部交互逻辑。
- 临时保留 rootName 注入（§F2.4）：app.js 内的
  __WPS_ROOT_NAME_JSON__ token 由 Python 桥在响应时用
  _safe_root_name_json 转义替换（render_web_asset），HTML 模板的
  JSON token 随脚本外移自然消失；F3 将删除该替换。
- web.py 模板 <script> 块替换为
  <script src="/assets/app.js" defer></script>；defer 与原“body 末尾
  内联脚本”的执行时点等价（DOM 解析完成后、DOMContentLoaded 前）。
- 白名单新增 app.js → text/javascript; charset=utf-8。
- FE-03 顺带核对：docs/language-migration.md 已在 F0 修正“取消状态”
  措辞；app.js 中无 XMLHttpRequest.abort 调用，等价性保持。

新增/更新测试：原 GET / 字符串断言按 F5.5 拆为页面断言（外链
link/script、无 <style>）与脚本断言（apiRoot、轮询、缓存/预取常量、
连接文案）；新增 JS token 对恶意根名称的转义断言；U+2028 特征测试
目标从 render_web_app 迁至 render_web_asset("app.js")。

流程备注：contract_tests 的证据 JSON 含随机 lock token（DAV-LOCK-001
等），每次运行契约测试后必须重新生成 release-manifest.txt 再提交。

检查：tests 167 项全绿；contract_tests 119 项全绿；manifest 已更新。

回滚：git revert 本提交。

## M2/F3 固定 index.html，根名称改走 settings API

日期：2026-09-05

按 05-frontend-plan.md §11 阶段 F3 与 §20 第 9-10 步执行，对应里程碑
M200（三文件分离完成）与 M202。

- 新增 go/web/index.html（102 行）：页面结构自模板迁出，
  __WPS_ROOT_NAME_HTML__ 全部替换为固定占位文案 "WPS Enterprise
  Drive"；brand-title 增加 id="brand-title"。
- app.js：rootName 改为静态默认值；新增 applyRootName()（统一更新
  document.title、左上角品牌文字、根面包屑/标题/说明——顺带修复
  FE-01 品牌文字不随改名更新的缺口，属批准变更候选）；新增
  initRootName()（GET /api/v1/settings，成功则更新名称，失败显示
  可理解错误并继续用默认名称）；启动改为 boot()：await
  initRootName() 后再首渲染与 load("/")，占位名不会先闪现再翻转。
- web.py：删除 render_web_asset 与 JSON token 替换依赖；新增
  load_web_page()（index.html 不参与 /assets/ 白名单路由，杜绝
  /assets/index.html 旁路）。
- server.py：_handle_web_app 改为直接返回 index.html 字节，不再调用
  current_web_root_name()，页面服务完全不触碰设置文件与存储；CSP
  暂保持不变（F4 收紧）。
- WEB_APP_TEMPLATE/render_web_app 保留为待 F7 删除的遗留参照，仍有
  单测覆盖其自身行为。

新增/更新测试：GET / 与 index.html 字节全等；配置恶意根名称（HTML
标签、引号、&、U+2028、尾随空格）后响应中任何形式均不出现该名称；
app.js 与文件字节全等且无 token；settings PATCH 后 GET / 不再内嵌
新名称（改由 GET /api/v1/settings 返回）。

浏览器侧验证（无闪烁、settings→status→entries 请求顺序、改名后五处
同步更新）依赖真实浏览器，与 M203/M205 一并列入负责人侧门禁。

检查：tests 166 项全绿；contract_tests 119 项全绿；manifest 已更新。

回滚：git revert 本提交。

## M2/F4 收紧内容安全策略

日期：2026-09-05

按 05-frontend-plan.md §11 阶段 F4 执行，对应里程碑 M204。

- index.html 已确认无内联 style、无内联 script（仅
  <script src="/assets/app.js" defer> 外链）。
- CSP 从 script/style 的 'unsafe-inline' 收紧为纯 'self'；img-src
  由 'self' data: 收紧为 'self'（页面不使用 data 图片）；显式
  connect-src 'self'；保留 object-src 'none'、base-uri 'none'、
  frame-ancestors 'none'。
- 网页入口与静态资源响应新增 X-Content-Type-Options: nosniff。
- 特征测试更新为新的逐字符 CSP 值；页面断言新增无 style= 属性、
  无内联 <script>；HEAD 资源断言含 nosniff。

浏览器控制台无 CSP 违规、Basic Auth 挑战页无资源依赖两项依赖真实
浏览器，列入负责人侧门禁（M203/M204 汇总）。

检查：tests 166 项全绿；contract_tests 119 项全绿；manifest 已更新。

回滚：git revert 本提交（安全改动独立成提交，便于单独回退）。

## M2/F5 Python 静态桥收尾与 M2 里程碑状态

日期：2026-09-05

按 05-frontend-plan.md §11 阶段 F5 收尾（F6 Go 嵌入属 B1301 阶段）。

- 桥接能力即最终形态：仅白名单 {style.css, app.js} 参与 /assets/ 路由，
  index.html 仅经 /、/web、/web/ 三入口返回；文件名不参与路径拼接，
  不接受用户输入转换磁盘路径。
- 新增测试：资源响应 no-store 且 Content-Length 与 body 全等（两个
  资源）；POST /assets/* 维持现有 404 兼容结果。
- 发布清单已包含 go/web/index.html、go/web/style.css、go/web/app.js
  三个文件（测试强制校验）。

M2 里程碑状态（08-executor-checklist.md）：
- [x] M200 三文件已从 Python 字符串分离（go/web/）。
- [x] M201 Python 服务白名单提供拆分后的资源。
- [x] M202 根名称经 GET /api/v1/settings 获取；响应 HTML 不含用户
  名称（字节全等断言）；首渲染前 await settings 避免名称闪烁；
  浏览器侧确认列入门禁。
- [ ] M203 桌面/窄屏/键盘/拖放/上传 E2E —— 本环境无浏览器，待负责人
  在具备浏览器的环境执行（§17 最小用例集 30 项）。
- [x] M204 CSP 已移除 unsafe-inline（头级别已验证；控制台无违规待
  浏览器确认）。
- [ ] M205 预取/缓存验收（24 上限、2 并发、30s TTL、命中、失效、导航
  竞态）—— 同样待浏览器环境。

负责人侧待办汇总：四张基线截图（F0）、§17 最小用例集、§15 五视口、
§16 可访问性、FE-02 决策追认、FE-01 修正确认。

检查：tests 168 项全绿；contract_tests 119 项全绿；manifest 已更新。

回滚：git revert 本提交。

## B200 初始化 Go module

日期：2026-09-05

按 04-backend-migration-steps.md B200 执行；目录与依赖方向遵循
03-target-architecture.md §4/§5。

- go.mod：module path 固定为 GitHub 仓库路径
  github.com/galiandan/WPS_2_WebDAV/go（module 根即 go/ 目录）；go
  指令 1.25.0（保守下限，本机工具链 1.27 构建，待负责人确认）。
- cmd/wps-adapter/main.go：三命令形状——--version 输出 0.9.8；
  check-config 输出 "config=ok group_id=pending-login auth=<enabled|
  disabled> dav=<prefix> rest=<prefix>"（骨架不解析 workspace，
  B201 接管真实语义）；serve 支持 --bind/--port（默认取
  ADAPTER_BIND/ADAPTER_PORT 与 Python 相同的 127.0.0.1:54321），
  非本地 bind 且未启用 Basic Auth 拒绝启动（错误文案与 Python 一致），
  监听后输出与 Python 相同的 listening/webdav/rest 两行，SIGINT/
  SIGTERM 优雅退出码 0，监听失败退出码 1。
- 骨架 serve 仅提供 /healthz（JSON 字节与 Python 契约逐字符一致，
  单测固定）与其余路由的 404 "unknown route" 文本回退；未认证挑战、
  REST/DAV 路由属 B5xx 阶段，不提前实现。
- internal/config：骨架级 Load（bind/port/认证四变量/双前缀），前缀
  规范化对齐 _normalise_prefix；错误只含变量名与规则，不回显值。
- internal/app：Application + healthPayload（struct 顺序即 JSON 键序，
  与 Python json.dumps 键序一致）。
- 版本注入位：main.version / main.commit 预留 ldflags -X，README 记录
  fmt/vet/test/race/build/交叉构建命令。
- go/web/ 已在 M2 阶段就位（三前端文件，Python 桥与未来 Go embed
  共用）。

检查：go fmt 无差异、go vet 通过、go test ./... 全绿（config 6 组表
驱动用例 + app 2 项含 healthz 逐字节契约）；serve smoke（listening 行
+ healthz + 404 + 退出）通过；交叉构建 GOOS=windows/linux amd64/
linux arm64 全部产出可执行文件；release-manifest.txt 已更新；Python
参照套件保持全绿。

回滚：git revert 本提交；go/ 内既有 MIGRATION-LOG、benchmarks、web
不受影响。

## B201 实现配置结构

日期：2026-09-05

按 04-backend-migration-steps.md B201 执行；重写 go/internal/config，
新增 go/internal/workspace（加载/校验子集，热加载归 M303）。

- 全部 50 个环境变量建字段/默认值/类型/规则：WPS 客户端 17 项、
  存储 10 项、应用限额 5 项、锁 1 项、适配器网络/认证 8 项、workspace
  3 项、根名称 1 项、serve 专属 2 项。解析顺序与 Python 求值顺序一致
  （刷新命令 → group/root → workspace 文件 → 凭据/URL/数值串 →
  根名称与 web-settings 路径 → client 构造校验 → storage 选项 →
  应用限额 → 网络）。
- 规则分类（与 Python 逐条对齐）：
  必须为正——list_count、max_list_entries、max_cached_folders、
  max_uploads、max_downloads、transfer_wait_timeout、max_copy_entries、
  max_copy_depth、max_propfind_entries、max_propfind_depth、
  max_control_body、max_response_body_bytes、max_locks、
  max_json_response_bytes（且仅当 group 已解析或存在 spaces 时才校验
  storage 组——Python 此时不构造 WpsStorage）；
  允许 0——cache_ttl、status_probe_ttl、status_failure_backoff、
  upload_min_free_bytes、max_upload_bytes；
  允许空——group_id、cookie/csrf 文件、referer/origin/cid、spool
  目录、刷新命令、ADAPTER 四凭据项；
  仅解析不校验——timeout、multipart/spool/chunk/retries/delay 等
  （Python 加载期同样不校验，测试钉住该宽松语义）；
  serve 专属——端口范围、ADAPTER_MAX_CONNECTIONS、
  ADAPTER_REQUEST_TIMEOUT 在 ValidateRuntime/ParseServerRuntime 中
  处理，check-config 不校验（Python 语义）；但 ADAPTER_PORT 的解析
  错误全命令生效（Python 在 parser 构造期即失败）。
- 布尔仅接受 1/true/yes/on、0/false/no/off（含大小写与空白）；空串
  视为错误。浮点镜像 Python float()：空白可剥离、溢出得 ±Inf 而非
  报错、NaN 照单全收（Python 的 NaN 比较恒假，"不为负"检查放行）。
  整数镜像 Python：空白剥离；±Int64 溢出报"out of range"（Python
  无界整数会照收——记录为已记录偏差）。
- WPS_BASE_URL/WPS_OBJECT_STORAGE_HOST_SUFFIX 镜像
  os.environ.get(name, default)：显式置空保留空值并照常校验失败。
  base_url 规则：HTTPS、kdocs.cn 或 *.kdocs.cn（大小写/尾点归一）、
  禁 userinfo/query/fragment/路径；对象 suffix 归一后必须落在
  kdocs.cn。URL/凭据/文件内容永不进入错误文案。
- workspace：标识符 ^[A-Za-z0-9._-]{1,256}$；文件缺失→默认值；仅当
  group/root 为 auto 或文件存在时加载并校验（Python 同款条件，含
  "显式 id + 无文件时不校验标识符"的怪癖测试）；路径必须绝对、父目
  录/文件 0600/0700 且属主为 root 或本用户、拒绝符号链接父目录、
  16KiB 上限；spaces 空/超 128/组重复/名重复/非法标识符/非法名均报
  WorkspaceConfigError 同义错误。OwnedByService 按 unix/windows 拆
  文件（windows 为开发平台跳过，B302 securefile 正式化）。
- BasicAuth.enabled 修正为四者任一非空（B200 骨架曾误用"成对"语义，
  本任务按 server.py BasicAuth.enabled 修正）。
- check-config 输出与 Python 逐字符一致（group_id ready/pending-login
  按 ResolvedGroupID 判定）。

验证：
- go test ./...（config 14 组、workspace 5 组、app 2 组）、
  go test -race ./... 全绿；go fmt/go vet 无差异；三平台交叉构建
  （windows/amd64、linux/amd64、linux/arm64）全部通过。
- Python/Go check-config 同环境对比矩阵 18 场景（成功行逐字符一致、
  失败退出码一致）0 差异，证据 contract_tests/results/
  B201-CHECK-CONFIG-PARITY.json；Go 错误文案按 B201 规则只含变量名与
  规则（Python 文案含字段名/值回显——如 BROKEN_PORT 的 traceback——
  按规范有意不同）。
- Python 参照套件 168 项、契约 119 项保持全绿；manifest 已更新。

已记录偏差（待负责人追认）：整数溢出 Go 报错而 Python 接受无界整数；
失败文案风格（变量名 vs Python 字段名/traceback）；web-settings 与
workspace 的私有性校验在加载期执行（与 Python 一致，非偏差）。

回滚：git revert 本提交。

## B202 生命周期和信号

日期：2026-09-05

按 04-backend-migration-steps.md B202 执行；服务仍是仅 /healthz 的空
HTTP 服务，不访问 WPS。

- 信号处理：SIGINT/SIGTERM 经 signal.NotifyContext 触发停止接收新
  连接，shutdownServer(10s) 有期限优雅排空；期限到达时 server.Close()
  强制关闭残留连接。信号触发的停止一律退出 0（对齐 Python
  KeyboardInterrupt → 0），强制关闭仅向 stderr 打一行 "adapter
  shutdown forced: ..."。
- 启动失败（配置错误、公共 bind 拒绝、监听冲突/失败）退出 1 并输出
  "adapter failed: ..."；启动成功保持两行非敏感监听输出
  （listening=... 与 webdav=... rest=...）。
- 非 loopback bind 且未启用 Basic Auth 拒绝启动（B201 的
  CheckPublicBind，语义与 Python 相同）。
- 关闭顺序：Shutdown/Close 返回后才退出进程，信号通知经 defer stop()
  释放；无 package init 网络行为，check-config 全程无网络请求（配置
  与 workspace 均为本地文件读取）。

进程级测试（cmd/wps-adapter/main_test.go，TestMain 先构建真实二进制，
7 项全绿）：
- 启动输出两行监听地址 + SIGTERM 退出 0；
- SIGINT 退出 0；
- 存活期间 GET /healthz 返回契约 JSON；
- 端口被占用 → 退出 1 + "adapter failed"；
- 0.0.0.0 无凭据 → 退出 1 + "refusing a non-local bind"；
- 0.0.0.0 + ADAPTER_USERNAME/PASSWORD → 正常启动并退出 0；
- 半开连接挂在服务端 → SIGTERM 后在期限内强制关闭并退出 0
  （超时强停路径）。

检查：go fmt/go vet 无差异；go test ./... 与 -race 全绿（cmd 7 项、
config 14 组、workspace 5 组、app 2 组）；Python 参照套件 168 项全绿；
manifest 已更新。

回滚：git revert 本提交。
