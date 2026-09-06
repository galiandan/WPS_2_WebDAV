# Go 重写进度总览

本文件只记录**粗粒度进度**：已经完成什么、下一步是什么。
具体实现要求、请求/响应逐字段细节和验收标准，一律以本目录的详细细纲
（`00-README.md` ~ `09-python-retirement-plan.md`）和 `go/MIGRATION-LOG.md`
的逐任务证据为准，此处不重复。

最后更新：2026-09-06

## 分支布局

- `rewrite`：Go 重写的唯一工作线。所有重写相关内容（`go/` 模块、
  `go/MIGRATION-LOG.md`、本目录细纲）都在这条分支上，每个任务一个
  `BXXX <描述>` 提交并推送。
- `main`：保持为完整、可直接运行的 Python 参照实现（仅保留 `go/web`
  三个前端资产文件，因为 Python 的 `web.py` 默认从该目录读取）。
  重写期间不在 main 上开发。

## 已完成

| 阶段 | 内容 | 任务 |
| --- | --- | --- |
| 阶段 0 冻结现场 | 工作区状态、Linux 参照环境、性能基线、兼容性决策 D-01..D-09 | B000–B003 |
| 阶段 1 契约测试 | 黑盒测试基础设施、HTTP/auth/framing、REST、WebDAV、WPS fixture 契约 | B100–B104 |
| 阶段 2 Go 骨架 | module 初始化、配置结构、生命周期与信号 | B200–B202 |
| 阶段 3 领域模型与本地状态 | 模型/错误分类、安全文件读取、原子写、workspace 状态、web settings、凭据源 | B300–B305 |
| 阶段 4 WPS 只读客户端 | 双 HTTP client、JSON 请求器、登录状态检查、entry 解析、列表分页 | B400–B404 |
| 阶段 5 路径/缓存/预算/存储 | 路径解析与 href、全进程资源预算、元数据缓存、单空间 Storage、多空间 | B500–B504 |
| 阶段 6 HTTP 基础 | 显式路由器、中间件顺序、错误映射表、settings 接口、session import | B600–B604 |
| 阶段 7 只读接口 | REST status/list/metadata、OPTIONS/HEAD、PROPFIND Depth 0/1 与 infinity | B700–B703 |
| 阶段 8 下载 | 签名下载地址、完整流式 GET、Range 与 If-Range | B800–B802 |
| 阶段 9 低风险写操作 | 创建文件夹、重命名、异步任务轮询、移动、删除 | B900–B904 |
| 阶段 10 普通上传（进行中） | 请求正文与 spool：411/507 帧序、上传槽与 spool 预算、流式 MD5/SHA-1/SHA-256、长度失配、各错误注入点清理 | B1000 |

当前 Go 侧门禁基线：`gofmt`/`go vet` 无差异，`go test ./...` 全绿，
交叉构建 linux amd64/arm64、windows amd64、darwin arm64 通过；
Python 参照套件（169）与 contract_tests（119）保持全绿。

## 下一步（按细纲顺序）

1. **阶段 10 剩余**（`04-backend-migration-steps.md` §11）：
   - B1001 pre_check 与冲突语义（REST 默认不覆盖、DAV PUT 默认覆盖、
     仅在 overwrite + 已观察 403 时继续、不先删后传）
   - B1002 create_update 与对象 PUT（精确字段、签名 URL 校验、
     无 Cookie 流式发送、指数退避重试、ETag 规范化）
   - B1003 文件登记（登记失败脱敏告警、spool 与资源必清理）
   - 完成条件：0B、阈值边缘、覆盖、重试、磁盘不足、断连、登记失败
     全覆盖；上传后下载 hash 一致
2. **阶段 11 multipart 上传与检查点**：B1100 检查点格式 → B1101
   初始化与分片大小 → B1102 单片上传 → B1103 session 失效恢复 →
   B1104 merge 与登记
3. **阶段 12 COPY 与 DAV LOCK**：B1200–B1204（原生/中继/文件夹
   COPY、Lock Store、LOCK/UNLOCK 协议；此阶段补验各写路由的锁检查）
4. **阶段 13 完整服务整合**：B1300 组装依赖 → B1301 接入静态前端 →
   B1302 与 Python 全量对照 → B1303 全量静态与并发检查
5. **阶段 14 部署、灰度与发布**：按 `07-deployment-release-plan.md`
   与 `08-executor-checklist.md` 执行，全部签字后才允许切换默认服务

## 遗留提醒

- 所有者侧门禁仍未执行：真实 WPS 专用目录的人工验证、浏览器 E2E
  （M203/M205）、前端 F0 截图与 FE-02 追认等，按细纲留到对应阶段。
- 任何任务开始前先读对应细纲小节；完成后 MIGRATION-LOG 记录证据、
  门禁全绿再提交推送；未完成前置任务不开后续任务。
