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
