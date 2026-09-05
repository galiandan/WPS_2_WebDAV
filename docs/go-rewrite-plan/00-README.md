# WPS 2 WebDAV：Go 重构总纲索引

> 文档状态：实施纲要，尚未开始重构。
>
> 生成日期：2026-09-05。
>
> 目的：让接手能力有限的执行模型也能按小步骤完成迁移，而不是要求它一次读懂并重写整个项目。

## 1. 一句话结论

推荐把 VPS 上长期运行的 Python 服务逐阶段迁移为 **Go 单二进制 + 原生 HTML + CSS + JavaScript**，第一轮继续保留现有 `wps_login.py` 作为本地登录助手，并始终用当前 Python 实现和语言无关黑盒测试校验兼容性。

这里必须澄清：用户原话中的 `ccs` 按上下文理解为 `CSS`；网页文件管理器不能只靠 HTML/CSS 实现上传进度、拖放、状态请求、重命名、移动和删除，所以必须保留少量原生 JavaScript。它不需要 Node.js、React 或 Vue。

## 2. 本文档集只做什么

本文档集只提供：

- 当前项目的完整功能地图。
- 必须保持的协议和安全契约。
- 建议的 Go 包和文件职责。
- 从环境准备到灰度上线的逐步任务。
- 前端拆分、测试、部署、发布和回滚细纲。
- 给后续执行模型使用的固定工作方式和总检查表。

本文档集没有：

- 安装 Go/Git/Docker 等环境。
- 编写 Go、HTML、CSS 或 JavaScript 实现。
- 修改当前 Python 服务行为。
- 访问真实 WPS 账号或 secret。
- 替换 Native/Docker 生产部署。

## 3. 为什么不直接一次重写

这个项目虽然以 Python 标准库实现，但它不是一个简单的 HTTP 转发脚本。它同时包含：

- Basic Auth 和浏览器同源写保护。
- REST 与 WebDAV 两组外部协议。
- 路径到 WPS 对象 ID 的逐层解析。
- 单空间和多空间虚拟挂载。
- 元数据缓存、分页、重复 ID 保护。
- 网页端短期目录缓存、直接子文件夹后台预取和导航竞态保护。
- Cookie/CSRF 安全文件、热加载和 401 后刷新。
- 对 WPS 私有控制接口的严格请求形状。
- 与签名对象存储隔离的上传/下载流。
- 普通上传前 spool 与三种摘要计算。
- multipart 初始化、逐片签名、合并、登记和检查点。
- 单范围下载、If-Range 和 416。
- 异步 move/delete task 轮询。
- 非原子的 COPY 中继和进程内 DAV LOCK。
- systemd、无 systemd 便携模式、Docker、一键安装、升级和回滚。
- 本地隔离 Chrome 登录、空间发现和凭据同步。

一次性翻译几千行代码很容易得到“能启动但会丢行为”的版本。此总纲要求先冻结行为，再逐层替换，每层都能停下和回滚。

## 4. 文档地图

| 顺序 | 文件 | 用途 | 何时阅读 |
| --- | --- | --- | --- |
| 00 | 本文件 | 总范围、导航、阶段依赖、本机事实 | 每次新模型接手首先读 |
| 01 | `01-current-system.md` | 当前仓库、模块、数据流、配置和已知歧义 | 第一次理解项目 |
| 02 | `02-compatibility-contracts.md` | REST/WebDAV/WPS/认证/路径/状态码硬契约 | 写任何对应功能前 |
| 03 | `03-target-architecture.md` | Go 目录、包职责、依赖、并发和安全设计 | 建 Go 骨架前 |
| 04 | `04-backend-migration-steps.md` | 后端 B000 起的逐任务实施清单 | 每次后端开发 |
| 05 | `05-frontend-plan.md` | HTML/CSS/原生 JS 拆分和 UI 验收 | 前端阶段 |
| 06 | `06-testing-risk-gates.md` | 单元/黑盒/E2E/race/fuzz/性能/故障门禁 | 每阶段验证 |
| 07 | `07-deployment-release-plan.md` | 本机环境、Native/Docker/CI/发布/回滚 | 环境及部署阶段 |
| 08 | `08-executor-checklist.md` | 弱模型执行规则、模板和总进度表 | 每轮开始与结束 |
| 09 | `09-python-retirement-plan.md` | Go 登录助手、剩余工具与最终零 Python 路线 | Go 服务稳定一个发布周期后 |

相对路径均以仓库根目录为起点：

```text
D:\WPS_2_WebDAV-main\WPS_2_WebDAV-main
```

## 5. 推荐阅读方式

### 5.1 项目负责人

按 00 -> 01 -> 02 -> 03 阅读，然后重点确认 `03-target-architecture.md` 中 D-01 至 D-09 的兼容性决定。负责人不需要逐行读每个实施任务，但必须批准存在行为差异的门禁。

### 5.2 第一次接手的执行模型

按 00 至 08 全部阅读一次。之后回到 `08-executor-checklist.md`，只选择最前面未完成的小任务。`09-python-retirement-plan.md` 等 Go 服务稳定后再读和执行。不要直接从 WPS 上传或 WebDAV COPY 开始。

### 5.3 后续继续同一任务的模型

先读 00、08、上一轮交接记录和当前任务对应专题。检查 Git diff 后继续；不要假设前一个模型已经正确完成未打勾项目。

## 6. 推荐技术决定

| 项目 | 决定 | 原因 |
| --- | --- | --- |
| 服务端语言 | Go | 网络 I/O、并发、单二进制、部署和长期维护综合成本合适 |
| HTTP 基础 | Go `net/http` | 标准库成熟，但必须显式保持当前 framing/WebDAV 行为 |
| WebDAV | 项目自己的薄协议层 | 通用 handler 很可能改变 Depth、PROPFIND、覆盖和 LOCK 语义 |
| 前端 | HTML + CSS + 原生 JavaScript | 当前功能需要动态请求，无需前端框架或构建链 |
| 静态资源 | 编译时嵌入 Go 二进制 | 最终用户仍只部署一个服务文件 |
| 登录助手 | 第一轮保留 Python | 它是一次性本地工具，不是常驻性能热点，且 Chrome/CDP/SSH 安全边界复杂 |
| 数据库 | 不使用 | 当前状态适合权限受限的小 JSON/secret 文件 |
| HTTPS | 反向代理终止 | 保持现有部署模型，避免扩大首轮范围 |
| WPS 接口 | 只移植 observed/reproduced 行为 | 私有接口不稳定，不能由模型猜测 |
| 迁移方式 | 双端口对照、阶段切换 | 每层可测、可停、可回滚 |

## 7. 本机实际环境盘点

在编写本纲要时只做了只读探测，没有安装软件。

已检测到：

- Windows `10.0.26200.0`。
- `python.exe`，版本 `3.14.7`。
- `winget.exe`。
- `wsl.exe`，但尚未确认是否已经安装可用的 Linux 发行版。
- Windows 自带 `curl.exe`。

当前命令路径中未检测到：

- `go`。
- `git`。
- `bash`。
- `node` / `npm`。
- `docker`。

因此当前无法读取 Git 分支/提交/dirty 状态，也不能把 Windows 测试结果当作 Linux 发布基线。详细安装和验证步骤放在 `07-deployment-release-plan.md`。

注意：本项目的最终前端不需要 Node/npm；只有在团队明确选择 Playwright 的 Node runner 时，才为 E2E 测试安装 Node。最终用户不安装这些开发工具。

## 8. 当前测试实测结果

在 Windows 上使用：

```powershell
$env:PYTHONPATH='src'
python -m unittest discover -s tests -v
```

实际运行 148 项：

- 128 项通过。
- 4 项 failure。
- 16 项 error。

该结果不是“业务已有 20 个回归”。已确认的主要平台原因包括：

- 缺少 Bash，安装器 shell 测试无法启动。
- 缺少 Git，release manifest builder 无法运行。
- Windows 没有 POSIX `socket.MSG_DONTWAIT`。
- Windows 临时目录、owner/mode 与 Linux `0700/0600` 假设不同。
- Windows 创建 symlink 需要额外权限。
- SSH/绝对 secret 路径测试以 Linux `/etc/wps-adapter` 为目标。

另外，`multipart checkpoint` 恢复用例在本机也报错，不能未经复核就归因于 Windows。阶段 B001 必须在 Ubuntu CI/WSL 中重新运行并单独判断它是仓库状态、测试 fixture 还是实现问题。

所以真正的起点是：先建立 Ubuntu 参照基线，再开始 Go。不要修改断言来强行让 Windows 全绿。

## 9. 总体阶段与依赖

| 阶段 | 主要产物 | 必须先完成 | 允许进入下一阶段的条件 |
| --- | --- | --- | --- |
| 0 现场 | Git/测试/性能/决策记录 | 无 | Linux Python 基线可信 |
| 1 契约 | 语言无关黑盒与 WPS fixture | 阶段 0 | Python 可作为协议 oracle |
| 2 前端拆分 | 独立 index/style/app，仍由 Python 服务 | 契约基础 | UI E2E 无行为回归 |
| 3 Go 基础 | module、config、安全文件、状态 | 0/1 | 单元、race、双向文件兼容通过 |
| 4 只读核心 | status、list、path、cache、多空间 | 3 | fixture 和黑盒只读全绿 |
| 5 HTTP/DAV | auth、REST、PROPFIND、HEAD | 4 | 状态/头/JSON/XML 对照通过 |
| 6 下载 | signed URL、流式 GET、Range | 5 | hash、取消、206/416 全绿 |
| 7 写操作 | folder、rename、move、delete | 6 | fixture 与专用目录低频验证 |
| 8 普通上传 | spool/hash/retry/register | 7 | 内容 hash、故障清理全绿 |
| 9 multipart | checkpoint/part/merge/register | 8 | 100 MiB 往返 hash 一致 |
| 10 COPY/LOCK | 原生/中继/Depth/进程锁 | 9 | 失败残留和并发语义通过 |
| 11 整合 | 嵌入前端、完整 Go 服务 | 2/10 | 全量对照无未批准差异 |
| 12 部署 | Native/Docker/CI/release | 11 | 安装升级回滚演练通过 |
| 13 灰度 | 双端口、专用真实目录 | 12 | 安全/资源/兼容观察通过 |
| 14 切换 | Go 默认入口，Python 回滚保留 | 13 | 观察期完成且可一次切回 |

任何阶段都不能以“后面一起补测试”为理由越过门禁。

## 10. 最重要的项目事实

接手模型必须能复述以下事实：

1. `/healthz` 只表示进程活着，不能访问 WPS。
2. `/api/v1/status` 才检查 WPS 会话与映射根，并返回脱敏状态。
3. 除 health 外的网页、REST、DAV 都受可选 Basic Auth 保护。
4. 浏览器写请求有 Origin/Referer 同源保护；curl/WebDAV 无两者时允许。
5. REST 上传默认不覆盖，DAV PUT 当前默认覆盖。
6. MOVE/COPY 目标已存在时不会先删；Overwrite F 为 412，T 当前为 501。
7. 多空间根是适配器虚拟目录，不在 WPS 创建同名目录。
8. 跨空间 MOVE/COPY 不支持。
9. 上传必须先 spool 并计算 MD5、SHA-1、SHA-256，不能简单直通。
10. 小文件和 multipart 都必须经过对象存储后再向 WPS 登记。
11. signed object 请求永不带 WPS Cookie，并且必须限制到可信 HTTPS host。
12. Range 只支持单范围；上游未真正返回匹配 206 时必须失败。
13. LOCK 是当前进程内兼容锁，重启后消失。
14. Cookie/CSRF/workspace/settings 使用权限受限文件、热加载和原子替换。
15. WPS API 是私有且不稳定的，只能按已确认证据移植。
16. 网页在成功加载当前目录后，只预取返回结果中的直接子文件夹，最多 24 个，最多同时发起 2 个目录请求。
17. 网页目录缓存的 TTL 为 30 秒，只保存 `entries` 元数据；刷新、写操作、重新连接和目录切换中的旧请求不能污染当前页面。

## 11. 当前已知的高风险歧义

这些问题不能交给能力较弱的模型凭感觉解决：

- 固定 group/root、无 workspace 文件时，当前无条件 MultiSpace 组合可能出现空根。
- status 文档说不刷新，但根列表路径可能仍触发刷新。
- 多空间当前可能把每空间 2/4 的传输上限放大成总计 N 倍。
- REST query 与业务路径可能发生二次 URL 解码。
- 半配置 Basic Auth 可能通过公网 bind 检查，却使所有请求 401。
- workspace import 对 auto 的实际判断与报错文案不完全一致。
- workspace mount 名的控制字符限制不一致。
- PROPFIND 忽略请求 XML 并返回固定属性，通用 Go WebDAV 库可能改变它。
- 超过最大连接时当前直接断开，不返回 503。

推荐解决方向已经列在 `03-target-architecture.md` D-01 至 D-09，但必须先用 Python 特征测试和负责人签字固定。

## 12. 交付物清单

重构最终应该交付，而非本次纲要已经交付：

- Go 源码、go.mod/go.sum。
- 独立 HTML/CSS/JavaScript 静态资源。
- 保留的独立 Python 登录助手。
- 语言无关黑盒契约测试和脱敏 WPS fixture。
- Go 单元、集成、race、fuzz 测试。
- 浏览器 E2E。
- Linux amd64/arm64 可执行文件和校验和。
- 多阶段最小 Docker 镜像。
- 更新后的 systemd/便携模式/安装/卸载脚本。
- 更新后的 CI、release manifest、README、架构、API、部署和回滚文档。
- Python 与 Go 同环境性能报告。
- 灰度记录与负责人最终签字。

## 13. 开始实施前的最小确认

项目负责人先确认：

- [ ] 接受第一轮保留 Python 登录助手。
- [ ] 接受前端使用原生 JavaScript。
- [ ] 接受先建立 Linux/黑盒基线，不立即写 Go 全功能。
- [ ] 接受 WPS 未确认能力继续标记 unsupported。
- [ ] 接受生产切换前保留 Python 快速回滚。
- [ ] 对 D-01 至 D-09 逐项作出决定。

完成这些确认后，执行模型从 `04-backend-migration-steps.md` 的 B000 开始，不从目录中任意挑选“看起来简单”的功能。
