# Go 重写进度总览

本文件只记录**粗粒度进度**：已经完成什么、下一步是什么。
具体实现要求、请求/响应逐字段细节和验收标准，一律以本目录的详细细纲
（`00-README.md` ~ `09-python-retirement-plan.md`）和 `go/MIGRATION-LOG.md`
的逐任务证据为准，此处不重复。

最后更新：2026-09-07

## 分支布局

- `rewrite`：Go 重写的开发和发布候选线。长期运行服务、Native/Docker
  安装器、部署模板和嵌入式前端都以 Go 为准；Python 只作为参照实现和
  本地登录助手保留。
- `main`：已由 `rewrite` 的经过校验的 Go 文件树替换，作为默认安装入口。
  旧 Python 历史仍保留在合并提交的父历史中，不再作为生产启动入口。

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
| 阶段 10 普通上传 | 请求正文与 spool、pre_check 与冲突语义、create_update 与对象 PUT、文件登记 | B1000–B1003 |
| 阶段 11 multipart 上传与检查点 | 检查点格式、初始化与分片大小、单片上传、session 失效恢复、merge 与登记 | B1100–B1104 |
| 阶段 12 COPY 与 DAV LOCK | 原生单文件 COPY、文件中继 COPY、文件夹 COPY、Lock Store、LOCK/UNLOCK 协议与全路由锁检查 | B1200–B1204 |
| 阶段 13 完整服务整合 | 全量组装（凭据/热 workspace/共享 opener/全局 budget/storage/handlers/server）、Go 嵌入前端三资产与三入口、Python/Go 契约对照、fuzz/构建/冒烟门禁 | B1300–B1303 |
| 阶段 14 Go 部署切换 | Native/Docker/systemd/卸载器改为 Go 服务，安装器自动准备或构建 Go 工具链，发布清单与安装校验保持一致 | R1400 |

当前 Go 侧门禁基线：`gofmt`/`go vet` 无差异，`go test ./...` 全绿
（含 -race）；解析器 fuzz 目标 7 个实机通过；构建目标为 Linux
amd64/arm64（负责人指示：目标平台 Linux，不再产出 windows/darwin
构建）；Python 参照套件（169）与 contract_tests（119）保持全绿。
Python/Go 契约对照：112 场景逐字节一致、3 项批准修正、1 项细纲
规定行为、3 项偏差待负责人追认（见 MIGRATION-LOG B1302 与
contract_tests/results/comparison-report.json）。

## 下一步（按细纲顺序）

1. **发布后的灰度和维护**：由发布者执行真实 WPS 专用目录、浏览器、
   Native/Docker VPS 冒烟和旧版本回滚演练；后续 Go 变更先进入 `rewrite`，
   通过门禁后再提升到 `main`。

## 遗留提醒

- 所有者侧门禁仍未执行：真实 WPS 专用目录的人工验证、浏览器 E2E
  （M203/M205）、fuzz 长跑（CI 定时 10 分钟/发布候选 30 分钟）和三项
  契约偏差追认（见 MIGRATION-LOG B1302）。这些是发布灰度门禁，不是 Go
  服务运行时对 Python 的依赖。
- 任何任务开始前先读对应细纲小节；完成后 MIGRATION-LOG 记录证据、
  门禁全绿再提交推送；未完成前置任务不开后续任务。
