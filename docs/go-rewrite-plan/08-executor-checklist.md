# 后续模型执行手册与总检查表

> 这份文件专门约束执行模型。它不要求模型一次理解整个项目，而是要求模型按固定顺序、小步工作、每步举证。

## 1. 给执行模型的第一条指令

每次开始工作时都遵守下面这段规则：

1. 先读 `docs/go-rewrite-plan/00-README.md`。
2. 再读本次任务明确指定的一个专题文件，不要一次实现多个阶段。
3. 找到最前面未完成的任务编号，只处理这个编号。
4. 在修改前说明：输入文件、计划修改文件、保持不变的外部行为、测试命令。
5. 不知道 WPS 字段、状态码或边界时，停止猜测，回到 Python 源码、现有测试和 `docs/research/` 找证据。
6. 不能找到证据时，把问题标为阻塞并交给负责人，不自行设计私有 API。
7. 修改后运行最小测试和阶段测试，把真实输出摘要写进工作记录。
8. 测试失败就留在当前任务修复；不得先实现后续功能。
9. 不删除 Python 参照实现，不接触真实 secret，不访问非本人 WPS 数据。
10. 只有验收项全部通过，才把任务从 `[ ]` 改成 `[x]`。

## 2. 文档阅读顺序

第一次接手必须按顺序阅读：

1. `00-README.md`：范围、文件导航、阶段顺序。
2. `01-current-system.md`：当前项目到底做什么。
3. `02-compatibility-contracts.md`：哪些行为绝对不能无意改变。
4. `03-target-architecture.md`：Go 目录、边界、依赖方向。
5. `04-backend-migration-steps.md`：后端逐任务执行。
6. `05-frontend-plan.md`：网页拆分与交互验收。
7. `06-testing-risk-gates.md`：测试与风险门禁。
8. `07-deployment-release-plan.md`：环境、Native、Docker、CI、发布和回滚。
9. 本文：统一进度和交接格式。

第一次阅读完成后，不应凭记忆实现。每个任务仍要重新打开对应专题和源文件。

## 3. 权威信息优先级

遇到信息冲突时按以下顺序处理：

1. 已由负责人签字的兼容性决定和语言无关黑盒测试。
2. 当前 Python 自动化测试实际断言。
3. 当前 Python 源码实际行为。
4. `docs/api.md`、`docs/architecture.md`、`docs/research/findings.md`。
5. README 和 CHANGELOG。
6. 通用 WebDAV 经验、第三方库行为或模型自己的知识。

若第 2 与第 3 项冲突，先写特征测试并报告，不能擅自选一个。若源码与文档冲突，记录为迁移决策，不在 Go 中悄悄“顺手修正”。

## 4. 任务状态定义

每个任务只能处于以下状态之一：

| 状态 | 含义 | 是否可开始后续任务 |
| --- | --- | --- |
| 未开始 | 尚未读取任务输入 | 否 |
| 分析中 | 正在读源代码/测试 | 否 |
| 实现中 | 修改范围已明确 | 否 |
| 验证中 | 最小测试已过，正在跑阶段测试 | 否 |
| 完成 | 所有完成条件和记录齐全 | 是 |
| 阻塞 | 缺少必须的人类决定或外部证据 | 否 |

“代码大致写完”“本机看起来能跑”“只有几个测试失败”都不等于完成。

## 5. 每轮开始模板

执行模型在开始任务前必须给出以下信息：

```text
任务编号：
任务名称：
当前状态：分析中
必读文件：
允许修改的文件：
明确不修改的文件：
要保持的外部行为：
计划运行的最小测试：
本任务完成条件：
```

如果无法填写“允许修改的文件”或“完成条件”，说明任务仍过大，必须继续拆分。

## 6. 每轮结束模板

```text
任务编号：
最终状态：完成 / 阻塞
实际修改文件：
行为变化：无 / 已批准变更编号
运行的命令：
通过结果：
失败结果：
未运行的检查及原因：
安全检查：未发现 secret / 发现问题并已停止
回滚方法：
下一允许任务：
```

不得只说“测试通过”。必须写测试范围、数量或关键场景。

## 7. 修改范围规则

### 7.1 可接受的小任务

- 只新增 config 解析和表驱动测试。
- 只实现 Range parser，不接入下载 handler。
- 只移植 WPS create folder fixture 和 client 方法。
- 只拆出 `style.css` 并让 Python 继续提供它。
- 只更新 systemd ExecStart 并运行 service 模板测试。

### 7.2 必须继续拆分的任务

- “完成整个 Go 后端”。
- “把 client.py 全部翻译成 Go”。
- “重写前端并优化 UI”。
- “处理所有 WebDAV 方法”。
- “更新所有部署文件并发布”。

### 7.3 无关改动

发现无关问题时：

1. 记录文件、行和影响。
2. 若不阻塞当前任务，不修改。
3. 添加到后续问题列表。
4. 若它阻塞当前任务，先报告并建立独立任务。

不要为了“代码更漂亮”改名、移动或格式化无关文件。

## 8. 安全红线

出现以下任何内容立即停止，不继续执行命令或提交：

- Cookie、rtk、CSRF、Authorization 或 Basic Auth 密码出现在 diff、日志、fixture、截图或错误正文。
- 完整签名对象存储 URL 出现在输出。
- 真实企业/group/root/file/user/link ID 被写入仓库。
- 原始 HAR、PCAP 或真实文件正文进入仓库。
- signed object 请求携带 Cookie/Authorization/CSRF。
- 测试目标不再明确属于本人账号和专用目录。
- 实现试图绕过验证码、SSO、租户隔离或权限错误。

处理方式：停止、撤销本轮生成的敏感输出、轮换已泄漏凭据、向负责人报告。不要只从 Git diff 删除后假装没有发生。

## 9. Git 和提交规则

本机初始状态可能没有 Git 命令。安装 Git 并确认仓库状态后再执行以下规则：

1. 不重置或覆盖用户已有修改。
2. 每个完成任务对应一个小提交或一个清晰的变更组。
3. 提交前查看精确 diff 和未跟踪文件。
4. 运行格式化后再次确认没有大范围无关变更。
5. 提交消息包含任务编号和行为，例如 `B802 Match single-range behavior`。
6. 不使用交互式 rebase 作为日常步骤。
7. 不提交本机 secret、临时 spool、测试下载、浏览器 profile 或构建产物。
8. 阶段 tag/里程碑只在整阶段门禁通过后创建。

## 10. 总阶段顺序

下列顺序是硬依赖，不得跳跃：

```text
现场与环境
  -> Python/Linux 基线
  -> 语言无关契约
  -> 前端先从 Python 字符串拆出
  -> Go 骨架/config/安全文件
  -> Go WPS 只读 client
  -> path/cache/multi-space/budget
  -> HTTP/REST/WebDAV 只读
  -> download/range
  -> folder/rename/move/delete
  -> 普通 upload
  -> multipart
  -> COPY/LOCK
  -> 完整前端嵌入
  -> Native/Docker/CI
  -> fixture 灰度
  -> 真实 WPS 专用目录灰度
  -> 默认切换
  -> 稳定周期后才考虑移除 Python 服务
```

## 11. 总检查表

### M0 现场和决策

- [ ] M000 已记录 Git 状态、版本和用户已有改动。
- [ ] M001 已在 Ubuntu 建立全绿或逐项解释的 Python 基线。
- [ ] M002 已记录 Python 性能/资源基线。
- [ ] M003 D-01 至 D-09 已有特征测试和负责人决定。
- [ ] M004 已确认第一轮保留 `wps_login.py`。
- [ ] M005 已确认浏览器端允许原生 JavaScript，而非只用 HTML/CSS。

### M1 契约测试

- [ ] M100 黑盒 runner 可切换 base URL。
- [ ] M101 health/auth/framing 场景齐全。
- [ ] M102 REST 正式路由与别名齐全。
- [ ] M103 WebDAV 全方法和关键 header 齐全。
- [ ] M104 WPS control/signed fixture 齐全。
- [ ] M105 所有 fixture 已脱敏并通过 secret 扫描。

### M2 前端拆分

- [ ] M200 `index.html`、`style.css`、`app.js` 已从 Python 字符串分离。
- [ ] M201 Python 服务仍可提供拆分后的资源。
- [ ] M202 根名称改从 settings API 获取且无闪烁/注入。
- [ ] M203 桌面、窄屏、键盘、拖放和上传状态 E2E 通过。
- [ ] M204 CSP 可移除 unsafe-inline。
- [ ] M205 目录预取与浏览器缓存通过 24 个上限、2 个并发、30 秒 TTL、命中、失效和导航竞态验收。

### M3 Go 基础

- [ ] M300 module、CLI、版本和交叉构建完成。
- [ ] M301 所有环境变量与默认值完成。
- [ ] M302 Linux secure file 读取/写入完成。
- [ ] M303 workspace/settings 双向兼容完成。
- [ ] M304 credential/Set-Cookie/refresh race-safe。
- [ ] M305 优雅启动/停止完成。

### M4 Go 只读核心

- [ ] M400 WPS control 与 signed object client 隔离。
- [ ] M401 bounded JSON/错误/单次 401 retry 完成。
- [ ] M402 status cache/backoff/singleflight 完成。
- [ ] M403 entry/list/pagination 完成。
- [ ] M404 path 特殊字符契约完成。
- [ ] M405 全局 budget 在多空间下有效。
- [ ] M406 cache 与 workspace generation 隔离。
- [ ] M407 单/多空间 storage 完成。

### M5 HTTP 只读与下载

- [ ] M500 显式路由和中间件顺序完成。
- [ ] M501 error/response mapping 完成。
- [ ] M502 settings/session import 与旧 helper 兼容。
- [ ] M503 REST status/list/metadata 完成。
- [ ] M504 DAV OPTIONS/HEAD/PROPFIND 0/1/infinity 完成。
- [ ] M505 GET 完整下载和取消完成。
- [ ] M506 Range/If-Range/416 完成。

### M6 写操作

- [ ] M600 folder 完成。
- [ ] M601 rename 完成。
- [ ] M602 task polling 完成。
- [ ] M603 move/delete 完成。
- [ ] M604 REST 与 DAV 冲突/覆盖差异保持。
- [ ] M605 所有 mutation 同源和锁路径检查完成。

### M7 上传

- [ ] M700 spool/hash/长度/磁盘预算完成。
- [ ] M701 pre_check/create_update 完成。
- [ ] M702 signed PUT retry 与 file registration 完成。
- [ ] M703 普通上传失败清理和下载 hash 验证完成。
- [ ] M704 multipart checkpoint 完成。
- [ ] M705 block/part/session rebuild 完成。
- [ ] M706 merge/final registration 完成。
- [ ] M707 100 MiB 上传下载 hash 验证完成。

### M8 COPY 与 LOCK

- [ ] M800 native single-file copy 条件准确。
- [ ] M801 改名文件 relay copy 完成。
- [ ] M802 folder Depth 0/1/infinity 与限制完成。
- [ ] M803 部分失败清理行为有测试。
- [ ] M804 lock store race/expiry/inheritance 完成。
- [ ] M805 LOCK refresh/XML/UNLOCK 完成。

### M9 质量与部署

- [ ] M900 `go test ./...` 全绿。
- [ ] M901 `go test -race ./...` 全绿。
- [ ] M902 `go vet ./...` 全绿。
- [ ] M903 parser fuzz 无崩溃/泄漏。
- [ ] M904 Python 参照测试仍通过。
- [ ] M905 Playwright UI E2E 通过。
- [ ] M906 Linux amd64/arm64 构建和 binary smoke 通过。
- [ ] M907 Docker non-root 和卷权限 smoke 通过。
- [ ] M908 Native systemd 与便携模式安装/升级/卸载/回滚通过。
- [ ] M909 release manifest、校验和、许可证和文档同步。

### M10 灰度与切换

- [ ] M1000 Go 先在不同端口只读运行。
- [ ] M1001 fixture 全量对照无未批准差异。
- [ ] M1002 本人专用目录逐项真实验收。
- [ ] M1003 凭据重新导入/轮换无需重启。
- [ ] M1004 客户端断开后 5 秒内释放资源，或达到负责人批准阈值。
- [ ] M1005 Go 资源指标不劣于批准基线。
- [ ] M1006 回滚演练无需重新登录。
- [ ] M1007 默认入口切换后观察期完成。
- [ ] M1008 旧 Python 服务产物仍可立即恢复。

## 12. 单个功能的验收层级

每个功能按以下顺序验证，不得只做最后一层：

1. 纯 parser/model 单元测试。
2. fake interface 单元测试。
3. 本地 WPS fixture 集成测试。
4. Python/Go HTTP 黑盒对照。
5. race/fuzz/故障注入。
6. Linux 二进制 smoke。
7. 本人专用 WPS 测试目录低频验收。

只有前 6 层通过才允许第 7 层。真实 WPS 验收不是替代自动化测试。

## 13. 遇到测试失败时的固定处理

1. 停止下一任务。
2. 判断失败是否能在不联网的最小场景重现。
3. 判断是新代码、测试、平台假设还是 Python 参照本身的问题。
4. 缩小到一个输入与一个期望差异。
5. 查 `02-compatibility-contracts.md` 和对应 Python 行。
6. 若契约明确，修 Go。
7. 若契约不明确，新增特征测试并请负责人决定。
8. 修复后重跑最小测试、阶段测试和受影响的上层黑盒测试。
9. 记录根因，禁止只改断言迎合错误输出。

## 14. 立即回滚条件

灰度或上线后出现任一条件，不继续观察，立即切回 Python：

- 文件下载 SHA-256 与上传源不一致。
- Range 返回了请求范围外的数据或把完整文件当成 206。
- 目录层级、空间路由或文件名发生错误映射。
- 目标已存在时被意外删除或覆盖。
- Cookie、CSRF、Basic Auth 或签名 URL 进入日志/响应。
- 客户端断开后上游请求、临时文件或资源槽持续不释放。
- 多空间绕过全局资源限制导致 OOM 或临时盘耗尽。
- session refresh 把新的凭据覆盖为旧值。
- 安装/升级破坏现有 secrets、owner 或权限。
- Go 服务无法在一次服务切换内恢复到 Python。

回滚只切换程序/镜像/服务配置，保留 `/etc/wps-adapter/secrets/` 和 `/var/lib/wps-adapter/uploads/`；绝不删除 WPS 远端内容。

## 15. 阻塞问题报告格式

```text
阻塞任务编号：
看到的事实：
证据文件与行号：
最小复现：
当前 Python 结果：
候选决定 A 及影响：
候选决定 B 及影响：
推荐决定及理由：
在负责人决定前已停止的工作：
```

不要只问“接下来怎么办”。负责人必须能看到可比较的选择和证据。

## 16. 完工定义

只有同时满足以下条件，整个重构才算完成：

- 对普通用户，安装、登录、选择空间、网页访问和 WebDAV 地址没有额外步骤。
- 长期服务是 Go 单二进制，最终用户不需要安装 Go、Node.js 或 Python 服务依赖。
- 本地 `wps_login.py` 仍可使用并能无重启同步到 Go 服务。
- 所有公开接口、状态码、重要 header、JSON/XML schema 和资源限制通过契约测试。
- 文件完整性、Range、普通上传、multipart、COPY、LOCK、多空间和 refresh 全部验证。
- 静态前端在桌面和移动端功能完整，无重叠、无注入、无内联脚本依赖。
- Native、Docker、CI、release manifest、升级和回滚均完成。
- 安全日志扫描找不到任何敏感值。
- 性能报告使用同硬件、同 fixture、同限制，可复现且没有降低安全标准。
- 文档中的限制与实际代码一致。
- Python 服务退出有独立计划，不是在 Go 首次可运行时被直接删除。
