# Go 重写代码审查报告

- 审查日期：2026-09-07
- 审查基线：`rewrite` 分支提交 `a20aab7`（docs: add 07-deployment-release-plan and refresh contract re-run results）
- 审查范围：`go/` 全部 15 个包、116 个文件、约 16355 行非测试 Go 代码，并与 Python 参照实现（`src/wps_adapter/`，main 分支语义）逐段交叉验证
- 审查方式：逐文件通读（非抽样）＋ staticcheck 全量扫描（`all` 检查集）＋ 常见缺陷模式搜索（body 泄漏、`time.After`、goroutine 生命周期、锁拷贝、日志脱敏）＋ 关键行为与 Python 源码逐行对照
- 当时门禁状态：`gofmt` / `go vet` / `go test ./...`（含 `-race`）/ 7 个 fuzz 目标冒烟全绿；Python 参照套件 169/169 全绿；amd64 + arm64 编译通过

本文定位：记录对 `go/` 代码本身的审查结论，作为修复工作清单使用。逐任务迁移证据见
`go/MIGRATION-LOG.md`，迁移完成度结论见 `docs/go-rewrite-plan/PROGRESS.md`。
每个问题标注状态（待修复 / 已修复 / 已拒绝），修复时请同步更新。

---

## 问题总表

| 编号 | 级别 | 位置 | 摘要 | 状态 |
| --- | --- | --- | --- | --- |
| R-1 | P1 | `internal/wps/multipart.go:381,416-430` | multipart 会话重建后 `partInfos` 不清空，合并请求携带死会话旧分片 | 已修复（2026-09-07） |
| R-2 | P1 | `internal/httpserver/server.go:51-55`、`internal/wps/signed.go:144` | 传输阶段无超时，僵死对端可无限期占满上传/下载槽 | 已修复（2026-09-07） |
| R-3 | P2 | `internal/httpserver/upload.go:148-163` | `limitedUploadBody.remaining` 永不递减（值接收者，SA4005） | 已修复（2026-09-07） |
| R-4 | P2 | `cmd/wps-adapter/main.go:30,46-48` | `--version` 不输出提交号，`commit` 变量为死代码（U1000） | 已修复（2026-09-07） |
| R-5 | P3 | `internal/httpserver/dav_write.go:103-106` | Destination 端口非法时错误文案与 Python 分歧 | 待修复 |
| R-6 | P3 | `internal/httpserver/dav_write.go:351-356` | LOCK Timeout 超长数字：Go 钳制到上限，Python 抛 400 | 待修复 |
| R-7 | P3 | `internal/workspace/state.go:237-274`（Python 同病） | 重复 group 的 workspace 导入写入后使状态文件永久解析失败 | 待修复（双端） |
| R-8 | P3 | `internal/httpserver/slot_gate_test.go:71` 等（仅测试） | 测试忽略 Read 错误；非规范 header 键写法（SA1008/SA4006） | 待修复 |

级别定义：P0 可被利用/数据损坏/必然崩溃；P1 确定的功能 bug 或安全/可用性弱点；
P2 边界条件缺陷、死代码陷阱、发布契约缺口；P3 低风险偏差与卫生问题。

---

## P1 — 应当优先修复

### R-1 multipart 会话重建后 `partInfos` 不清空，合并请求携带死会话的旧分片

- 位置：`go/internal/wps/multipart.go:381`（`partInfos` 声明在分片循环外）、
  `go/internal/wps/multipart.go:403-430`（重试与重建分支）、
  `go/internal/wps/multipart.go:456-503`（`reinitializeMultipart` 只重置 `state.parts` 与检查点）
- 参照行为：Python 在重建时明确执行 `completed.clear()` **和** `part_infos.clear()`
  （`src/wps_adapter/client.py:2181-2182`），随后从分片 1 重新开始。
- 问题：Go 的会话重建分支只清空 `state.parts` 并保存检查点，`partInfos` 原样保留；
  `sessionReset` 后 `partNumber` 回到 1 重新上传，`partInfos` 变成
  「旧会话分片 1..k-1 + 新会话分片 1..N」的重复列表，merge 请求体的
  `part_infos` 含有死会话的 etag 和重复的 part_number，合并必然失败甚至错拼。
- 触发条件：multipart 上传中途某分片指令返回 400/404/410 且此前已有分片成功
  （生产默认配置 `WPS_UPLOAD_RESUME_DIR` 非空即满足前置）。现有测试未覆盖，
  推测测试场景在分片 1 就触发重建（此时 `partInfos` 为空）。
- 证据：Python `client.py` 分片重试循环（2100-2195 行）与 Go `multipart.go`
  逐段对照；重建分支 Go 侧无任何对 `partInfos` 的写入。
- 修复方向：在重建分支（`if sessionReset`）中执行 `partInfos = partInfos[:0]`，
  并补一条「分片 k>1 时触发会话重建」的回归测试钉住 merge 请求体形状。
- 修复记录（2026-09-07）：按修复方向实施。`multipart.go` 重建分支
  `partInfos = partInfos[:0]`；新增回归测试
  `TestMultipartSessionResetDropsPartInfosOfTheDeadSession`
  （part 1 成功、part 2 指令 404 触发重建，逐字断言 merge 请求体只含新
  会话 u2 的分片）。修复前该测试精确复现缺陷：merge body 携带
  `{"etag":"etag-1","part_number":1}`（死会话）且 part_number 重复。

### R-2 传输阶段完全没有超时，僵死对端可无限期占满上传/下载槽

- 位置：
  - `go/internal/httpserver/server.go:51-55`：`http.Server` 只设
    `ReadHeaderTimeout` 与 `IdleTimeout`，请求体读取与响应写没有期限；
  - `go/internal/wps/signed.go:144`：对象存储请求用 `http.NewRequest` 构造
    （不带 context），且 signed transport 仅有 `ResponseHeaderTimeout`，
    body 阶段无任何期限，客户端断连的取消也不会传播到上游读。
- 参照行为：Python 用 `ADAPTER_REQUEST_TIMEOUT` 的 socket 超时约束**每一次**
  收发操作；慢速或僵死的对端在 60 秒内被内核切断。
- 问题：`server.go` 注释自己承认「body 与响应写是 handler 的责任」，但没有任何
  handler 设置这些期限。上游对象存储 body 卡死时，`sendDownload` 的拷贝循环
  阻塞在 `stream.Read` 内（`r.Context()` 检查在每次读之前执行，永远轮不到）；
  慢速客户端同理可无限期持有上传槽。上传/下载槽是有限资源（默认 2/4），
  数个僵死请求即可让全部传输不可用。
- 后果级别：可用性回退（相对 Python），非数据损坏。
- 修复方向：
  1. signed 对象流包一层 per-read deadline（timeout reader），并把
     `r.Context()` 贯穿到 signed 请求；
  2. 入站 body（上传 spool、控制体读取）包整体或 per-read 时限；
  3. 补一条契约测试：上游 body 停止发送时，下载槽在有限时间内释放。
- 备注：`copy.go:131-134` 注释声称「两个等待都受 transfer budget 约束」只对
  槽位**获取**成立；槽位**持有**时长仍是无界的。
- 修复记录（2026-09-07）：采用 per-operation deadline 方案（逐字对应
  Python `connection.settimeout` 的"每次收发操作"语义，慢而不断的传输
  不受影响）。新增 `internal/opdeadline` 包：包装 `net.Conn`，在每次
  Read/Write 前重新武装 deadline。接入两处：
  1. `httpserver.Listen` 的 `slotListener` 用 `RequestTimeout` 包装每个
     接受的客户端连接（覆盖请求体读取与响应写两个停滞面）；
  2. `wps.NewSignedTransport` 的 `DialContext` 用配置 timeout 包装每个
     拨号连接（覆盖上游 body 读、body 写与 header 等待）。
  测试三层：`opdeadline` 包 3 个单测（net.Pipe 停滞读/写按期失败且不
  毒化连接；间隙大于超时窗口的流动交换不中断）；
  `httpserver` 端到端 `TestStalledRequestBodyIsCutAtTheRequestTimeout`、
  `TestFlowingResponseIsNotCutByTheRequestTimeout`（总时长 2.4s > 超时
  1s 的流动响应必须存活，防将来误改成整请求 deadline）；
  `wps` 端到端 `TestSignedTransportCutsStalledUpstreamBody`、
  `TestSignedTransportKeepsFlowingUpstreamBodyAlive`。
  控制面 client 本有 `http.Client.Timeout` 绝对期限（响应体有界且小），
  维持原状。

---

## P2 — 建议修复

### R-3 `limitedUploadBody.remaining` 永不递减（值接收者赋值丢失）

- 位置：`go/internal/httpserver/upload.go:143-163`，staticcheck SA4005 实锤。
- 问题：`func (l limitedUploadBody) Read` 用值接收者，156 行
  `l.remaining -= int64(read)` 写在副本上，`remaining` 恒等于声明的
  Content-Length，152-154 行的截断 clamp 永远不生效。
- 当前影响：net/http 会把 identity 编码的请求体限制在 Content-Length 内，
  因此没有可观察的错误行为；`ErrUnexpectedEOF → io.EOF` 的断连映射仍然有效。
- 风险：类型文档（139-142 行）声称的"declared-size 截断"是假的，属于
  复用即踩的陷阱——一旦将来把它包在无界 reader（如 chunked 语义的适配层）
  上，框架行为会静默改变。
- 修复方向：改为 `func (l *limitedUploadBody) Read`，或删除无效字段、
  在注释中如实记录"仅做断连清洗，不截断"。
- 修复记录（2026-09-07）：改为指针接收者，`remaining` 成为与 Python
  `_LimitedReader`（`server.py:311-329`，`self.remaining -= len(chunk)`）
  一致的活状态，截断 clamp 生效；两处调用点改传
  `&limitedUploadBody{...}`。新增单测
  `TestLimitedUploadBodyClampsToTheDeclaredLength`（超声明长度读取被
  clamp 且不再触达 source）与 `TestLimitedUploadBodyMapsUnexpectedEOFCleanly`
  （断连映射保留）。staticcheck SA4005 消除。

### R-4 `--version` 不输出提交号，`commit` 变量为死代码

- 位置：`go/cmd/wps-adapter/main.go:29-31`（`var commit = "unknown"`）、
  `go/cmd/wps-adapter/main.go:46-48`（`--version` 只打印 `version`）。
  staticcheck U1000 证实 `commit` 全程序无人读取。
- 问题：细纲 `docs/go-rewrite-plan/07-deployment-release-plan.md` §8.2 要求
  二进制能输出版本、提交号和构建时间的非敏感摘要；§24.2 要求发布产物记录
  源提交。发布流水线按注释用 `-ldflags "-X main.commit=..."` 注入后值被静默
  丢弃，发布物与源提交的对应关系在运行时不可验证。
- 修复方向：`--version` 输出 `version + commit`（需与 Python CLI 输出契约
  对照确认允许的格式），或删除该变量并同步修正 07 细纲的产物要求。
- 修复记录（2026-09-07）：按 07 §8.2.3 实施三字段摘要：`--version` 输出
  `0.9.8 commit=<hash> build_time=<UTC时间>`（首个 token 保持裸版本号，
  兼容 §10.4 安装器"版本不匹配立即停止"的前缀比较）；新增 `buildTime`
  注入点，构建注释补 `-trimpath`（§8.2.4）。进程级测试
  `TestVersionReportsTheBuildSummary` 锁定输出形状；`go/README.md`
  命令段同步。staticcheck U1000 消除。

---

## P3 — 低级别偏差与卫生问题

### R-5 Destination 端口非法时错误文案与 Python 分歧

- 位置：`go/internal/httpserver/dav_write.go:103-106`。
- 现象：`url.Parse("http://h:bad/")` 直接返回错误，Go 走解析失败分支回答
  `"Destination must point inside the WebDAV path"`；Python 的 `urlsplit(...).port`
  抛 ValueError，被 `_destination_dav_path` 捕获后回答
  `"Destination host or port is invalid"`（`src/wps_adapter/server.py:1228-1232`）。
  Go 代码里存在该文案（114 行）但触发条件是 Host 头非法，与 Python 的
  触发面（Destination 端口非法）不同。
- 修复方向：在 `destinationDavPath` 里把「Destination 整体解析失败」与
  「Host/Port 形状非法」分开判定，对齐 Python 文案。

### R-6 LOCK Timeout 超长数字：Go 钳制，Python 报 400

- 位置：`go/internal/httpserver/dav_write.go:342-365`（`lockTimeoutHeader`）。
- 现象：Go `strconv.ParseInt` 溢出（约 19 位以上数字）时按 ErrRange 钳到
  `maxTimeout` 正常建锁；Python `int()` 在超过 `int_max_str_digits`（4300 位）
  时抛 ValueError → 400。仅对恶意超长头可达，属理论边界。
- 修复方向：在解析前限制数字位数（如 ≤4300）以对齐 Python，或记录为
  已知可接受偏差交负责人追认。

### R-7 重复 group 的 workspace 导入会"投毒"状态文件（Go 与 Python 同病）

- 位置：`go/internal/workspace/state.go:237-274`（`Update` 不预检重复 group）；
  `go/internal/workspace/state.go:372-421`（`parseSpaces` 在重载时拒绝重复 group）。
- 现象：经 `POST /api/v1/session/import` 提交两个同 group 的 space 时，
  `Update` 逐个 `NewMount` 校验（名称唯一、标识符合法）但**不查重复 group**，
  文件写入成功、请求返回 200；下次 mtime 重载时 `parseSpaces` 以
  `"workspace spaces contain duplicate groups"` 拒绝，此后每次工作区访问
  都报错，直到手工删除状态文件。Python `workspace.py` 的 `update()` 同样
  只在重载路径（`_apply_file_payload_locked`）查重复，行为完全一致——
  这是**继承自参照实现的共同弱点**，不算迁移偏差。
- 修复方向：双端都在 `update`/`Update` 持久化之前校验 group 唯一；
  若保持行为一致优先，则至少在文档中标注该投毒路径。

### R-8 测试卫生（staticcheck 测试侧发现）

- `internal/httpserver/slot_gate_test.go:71`：`c3.Read(buf)` 的 `err` 被忽略
  （SA4006），应断言其为超时错误，防止连接提前关闭让断言变哑。
- `dav_test.go`（120/152/266）、`dav_write_test.go:362`、`download_test.go`
  （238/569）、`propfind_test.go:172`、`range_test.go`（224/283/318）、
  `router_test.go:287` 共 11 处以非规范键 `"ETag"`/`"DAV"` 直接写
  `http.Header` map（SA1008）。生产代码这样写是刻意的（保持 Python 线上
  拼写），但测试里应统一用 `http.CanonicalHeaderKey` 或加注释，避免将来
  有人改用 `header.Get` 读取时静默失配。
- `internal/mimetypes/mimetypes.go:27` 的 SA4006 经人工核实为误报
  （`base` 在循环回边被读取），无需处理。

---

## 审查中排除的疑点（查证后无问题）

以下疑点在审查中被提出并逐条查证排除，记录于此避免重复排查：

1. **路由前缀 `/` 的 `//` 边界**：Go `davPath`/`restRoute` 与 Python
   `_dav_path`/`_rest_route` 结构逐字一致（`server.py:639-648`），边界行为
   是忠实镜像。
2. **session import 的 `space.name` 非字符串**：Python
   `WorkspaceMount.__post_init__` 同样抛
   `WorkspaceConfigError("space.name is invalid")` → 400，文案一致。
3. **本地凭据写失败映射为 `WpsAPIError`**：Python
   `_do_rest_session_import` 本来就 `raise WpsApiError("store imported credentials")`
   （`server.py:1476`），Go 的映射是正确镜像。
4. **signed URL 后缀校验**：`evil-ag.kdocs.cn` 类同形后缀无法绕过
   （`.ag.kdocs.cn` 边界判断正确）；cookie/authorization/csrf 头在 signed
   通道被硬拒，凭据不可能到达对象存储 host。
5. **cache 代际机制**：Invalidate 递增 generation，迟到加载只会落入旧
   generation 的 key 被丢弃；joiner 直接接收 inflight 结果的窗口与 Python
   单飞实现一致，属参照语义。
6. **budget 槽位配对**：`sync.OnceFunc` 保证 release 恰好一次；`slotConn`
   用 `sync.Once` 保证连接槽在关闭时恰好释放一次（D-09）。
7. **`copy.go:222` 忽略 `writer.Delete` 错误**：对应 Python best-effort
   删除语义，注释已明示，非缺陷。
8. **Move/Delete 任务轮询用 `context.Background()`**：与 Python 一致
   （Python 客户端断连后同样轮询到完成），轮询器本身支持 ctx（`task.go`），
   供未来调用方使用。
9. **JSON 契约层（`pyjson.go`）**：有序对象、`json.Number` 保真、
   `ensure_ascii` 转义（含代理对）、重复键"首个位置 + 末个值"均与 CPython
   `json` 模块对齐。
10. **内部字段防泄漏**：`RemoteEntry.MarshalJSON` 强制 `Public()` 形状，
    `Raw` 上游字段不会出网；`Credentials.String/GoString` 脱敏；错误类型
    按构造只携带操作名与状态码。
11. **日志面**：全项目仅 4 处日志输出，内容分别为脱敏路径、panic 栈、
    脱敏错误串与上传告警；无 `time.After` 循环泄漏；goroutine 仅 2 处且
    均有界（copy 取消 watcher、serve 错误通道）。

---

## 修复优先级建议

1. **R-1**（一行修复 + 回归测试，直接对齐 Python）；
2. **R-2**（需设计 body 阶段超时策略：对象流 per-read deadline + ctx 贯穿 +
   契约测试"上游卡死时槽位最终释放"）；
3. **R-3 / R-4** 顺手修复（机械改动）；
4. **R-5 / R-6** 修文案或记录为待追认偏差；
5. **R-7** 与负责人确认是否双端一起在写前校验；
6. **R-8** 随下一次测试改动一并清理。

修复完成后请在本文总表更新状态，并按 `go/MIGRATION-LOG.md` 的惯例补充
证据（测试名、对照结果）；涉及外部可见行为变化的（R-5、R-6）需按
`02-compatibility-contracts.md` 流程交负责人追认。

## 修复后门禁复验（2026-09-07，R-1..R-4 完成）

- `gofmt` / `go vet` 干净；staticcheck 生产代码仅剩 `mimetypes.go:27`
  已核实的 SA4006 误报（R-8 记录的测试侧发现维持原状）；
- `go test ./...`（14 个含测试包，新增 `opdeadline`）与 `-race` 全绿；
- 7 个 fuzz 目标各 15s 冒烟全绿；
- amd64/arm64 `-trimpath` 交叉编译通过，嵌入资源在产物中确认，
  `--version` 实跑输出注入的 commit 与 build_time；
- Python 参照套件 169/169 全绿；Python 侧契约测试 119/119 全绿；
- Go 侧契约套件（对照工具从当前工作树自动重建 contractsrv 并运行）：
  112 场景逐字一致 + 3 approved_change + 1 plan_specified + 3 recorded
  deviation，`unapproved=0`，与修复前分类完全相同（R-2 的超时行为未
  改变任何契约可观察结果）。
