# 剩余 Python 与登录助手后续迁移细纲

> 第一轮 Go 服务端迁移不删除 Python 登录助手。本文件说明 Go 服务稳定后，如何选择性完成“最终零 Python”目标。

## 1. 为什么必须后做

1. 登录助手是偶尔运行的本地工具，不是服务端性能瓶颈。
2. 它涉及隔离 Chrome、CDP WebSocket、Cookie 域、SSO、验证码、空间发现、SSH 和 secret 写入，安全面与 WebDAV 服务完全不同。
3. 服务端和登录助手同时重写会让失败来源无法区分。
4. 当前单文件 `wps_login.py` 已可向任何保持 `/api/v1/session/import` 的服务同步。
5. 因此先完成并稳定 Go 服务，再单独迁移登录助手，仍是完整重构的一部分而不是永久放弃。

## 2. 第一轮之后仍存在的 Python

| 区域 | 用途 | 是否影响常驻性能 | 后续建议 |
| --- | --- | --- | --- |
| `wps_login.py` | 最终用户本地登录与同步 | 否 | 若目标零 Python，迁到独立 Go CLI |
| `src/wps_adapter/login.py` | 登录核心源 | 否 | Go helper 稳定后冻结/移除 |
| `login_command.py` | 交互与参数 | 否 | 与 Go CLI 对照迁移 |
| `har.py` | 研究材料脱敏 | 否 | 可保留开发工具或迁 Go |
| `tools/wps_*` | 调试/研究 | 否 | 按实际维护价值逐个决定 |
| build login script | 生成单文件 helper | 否 | Go helper 发布后不再需要 |
| release manifest builder | 发布清单 | 否 | 可迁为 Go 小工具或 CI 脚本 |
| Python tests | 参照契约 | 否 | 至少保留一个稳定周期 |

## 3. 启动本阶段的前置门禁

- [ ] Go 服务完成 `08-executor-checklist.md` M0 至 M10。
- [ ] Go 服务至少经历一个稳定发布周期。
- [ ] 当前 Python helper 向 Go 服务同步已通过 Windows/Linux/macOS 目标平台测试。
- [ ] `/api/v1/session/import` 与 secret/workspace schema 已冻结。
- [ ] Python 服务端回滚产物仍可获取。
- [ ] 登录迁移有独立版本、风险记录和回滚说明。
- [ ] 负责人明确选择“仅移植最终用户 helper”或“所有开发工具也零 Python”。

## 4. 目标产物

建议新增独立命令 `cmd/wps-login`，最终发布为平台二进制：

- Windows amd64/arm64。
- Linux amd64/arm64。
- macOS amd64/arm64。

该命令只负责本地登录和同步，不内嵌到 VPS server 进程，不让 VPS 安装 Chrome。

## 5. 阶段 L0：冻结 Python 登录契约

必读：`src/wps_adapter/login.py`、`login_command.py`、`tests/test_login.py`、`docs/login.md`。

步骤：

- [ ] 列出所有 CLI 参数、环境变量、默认值、互斥规则和退出码。
- [ ] 保存无参数交互问答的顺序和默认选项。
- [ ] 保存 SSH 私钥、SSH 密码、HTTP/HTTPS、本地输出四种路径。
- [ ] 固定远程 HTTP 必须显式 `--allow-http` 的安全门槛。
- [ ] 固定 workspace URL 解析、默认根=0、显式 folder 的区别。
- [ ] 固定 Cookie 筛选、排序、大小、csrf/rtk 必需规则。
- [ ] 固定空间发现、用户选择、逐个只读验证、浏览器关闭后再选择的顺序。
- [ ] 固定错误输出不得包含 secret。
- [ ] 将纯函数场景转为与语言无关 fixture。

完成条件：不启动 Chrome、不访问 WPS也能对比 Python/Go parser 和同步请求。

## 6. 阶段 L1：选择 CDP/WebSocket 方案

Go 标准库没有完整 WebSocket client。负责人必须先评估成熟库，不让模型手写不完整协议。

评估步骤：

- [ ] 比较成熟度、维护状态、许可证、依赖树和安全记录。
- [ ] 确认支持 loopback、消息大小限制、context 取消、ping/close 和 Windows。
- [ ] 确认不会自动把 Cookie 或 header 发到非 loopback host。
- [ ] 记录选型、版本、替代方案和升级责任。
- [ ] 将依赖纳入漏洞扫描和 go.sum。

阻断条件：需要禁用 TLS/host 检查、连接任意远端 CDP、或依赖无人维护。

## 7. 阶段 L2：CLI 和目标配置

一次只移植参数，不启动浏览器：

- [ ] `--login-url`。
- [ ] `--workspace-url`。
- [ ] `--browser`。
- [ ] `--domain-suffix`。
- [ ] `--wait-timeout` 与轮询间隔。
- [ ] `--adapter-url`、port、user、timeout、allow-http。
- [ ] `--ssh-target`、identity、port、远端三个路径、timeout。
- [ ] `--output-dir`。
- [ ] 对应 WPS_BROWSER/WPS_ADAPTER_* 环境变量。
- [ ] 非交互参数缺失时进入相同问答；CI 可关闭交互。

完成条件：同一输入的规范化目标、错误类别和退出码与 Python 一致。

## 8. 阶段 L3：浏览器发现

分平台实现，禁止把路径判断混在一个大函数：

### Windows

- [ ] 显式 `--browser` 优先。
- [ ] 检查当前 Python 支持的 Chrome/Chromium 常见安装位置。
- [ ] 校验候选为普通可执行文件。
- [ ] 路径带空格时通过参数数组启动，不拼 shell 字符串。

### Linux

- [ ] 显式路径优先，再按已批准命令名查 PATH。
- [ ] 不自动安装浏览器。
- [ ] 图形会话不可用时给清晰错误。

### macOS

- [ ] 检查标准 app bundle 可执行路径。
- [ ] 不读取用户日常 Chrome profile。

完成条件：每个平台用 fake executable/path fixture 测优先级和错误，不需要真实登录。

## 9. 阶段 L4：隔离 Chrome 生命周期

1. 创建权限受限的随机临时 profile 目录。
2. 选择空闲 loopback 端口，并防止竞态误连其他服务。
3. 使用独立参数数组启动可见浏览器。
4. 强制 remote debugging 仅在 127.0.0.1。
5. 不指定或复用日常用户数据目录。
6. 等待 CDP endpoint 时受总 timeout 和 context 控制。
7. 收到 Ctrl-C、成功、超时或任何错误都先终止本次浏览器，再删除临时目录。
8. 删除失败不输出 Cookie/profile 内容，只提示路径清理类别。
9. 不让后台浏览器进程在 helper 退出后残留。

完成条件：启动失败、登录超时、Ctrl-C 和正常结束的进程/目录清理测试通过。

## 10. 阶段 L5：CDP 连接安全

- [ ] CDP HTTP discovery 只访问本机 loopback。
- [ ] WebSocket URL 再解析，host 必须仍是 loopback，端口必须是本次启动端口。
- [ ] 禁止 userinfo、fragment、远程地址和重定向。
- [ ] 每帧、每消息和累计 snapshot 有大小上限。
- [ ] request ID 与响应严格匹配。
- [ ] 未识别事件不影响已知响应，但不能无界缓存。
- [ ] context 取消关闭 socket。

完成条件：恶意 discovery/WebSocket URL、超长帧、乱序响应和关闭竞态都有测试。

## 11. 阶段 L6：登录完成检测

1. 打开经过验证的官方 WPS HTTPS URL。
2. 用户自行完成账号、学校 SSO、扫码、验证码和风控。
3. helper 不代填、不读取密码。
4. 轮询当前页面 URL 与 Cookie snapshot。
5. 不要求用户回终端按 Enter。
6. 只有 URL 位于允许的 kdocs.cn 域且 Cookie 条件满足才进入下一步。
7. 自动恢复的旧 folder 不作为默认目标。
8. 显式 workspace URL 时必须留在相同 tenant/group/folder，否则失败。

完成条件：登录页、错误页、恢复旧目录、显式目录匹配/不匹配均有 fixture。

## 12. 阶段 L7：Cookie 筛选

- [ ] 只处理本次隔离 profile 的 Cookie。
- [ ] domain 必须位于允许 suffix 且匹配 drive host。
- [ ] 同名 Cookie 按 exact host、较长 domain/path 和当前 Python规则选择。
- [ ] name/value/domain/path 均有长度与控制字符限制。
- [ ] 拒绝能破坏 Cookie header 的分号、换行等值。
- [ ] 必须存在 rtk 和 csrf。
- [ ] 排序和最终 header 形状与服务端导入契约兼容。
- [ ] 总 snapshot 最大 4 MiB。
- [ ] 日志只显示数量和名称是否齐全，不显示值。

完成条件：测试使用明显虚构 marker，捕获 stdout/stderr 确认 marker 从未出现。

## 13. 阶段 L8：工作区发现、选择和验证

1. 从当前官方 page URL 解析候选 tenant/group/root。
2. 默认 root 强制为 0；只有显式 workspace URL 使用具体 folder。
3. 可选调用当前候选空间发现端点。
4. 严格解析 ID/name，拒绝部分畸形结果，不悄悄接受一半。
5. 只有一个空间时自动选择。
6. 多空间允许单序号、逗号序号和 all。
7. 去重并保持用户看到的顺序。
8. 关闭 Chrome 后再让用户选择，减少 profile 暴露时间。
9. 每个选定空间用已确认 list count=1 做只读验证。
10. 任一验证失败都不覆盖服务器旧凭据。

完成条件：0/1/多空间、重复名/ID、非法输入、权限失败和 fallback 均有测试。

## 14. 阶段 L9：四种同步方式

### HTTPS/HTTP

- [ ] 固定 POST `/api/v1/session/import`。
- [ ] 使用 Basic Auth，不把密码写入 URL或日志。
- [ ] 不自动 redirect。
- [ ] 远程 HTTP 默认拒绝，只有显式确认才允许。
- [ ] 响应最大 1 MiB，严格解析成功 schema。

### SSH 私钥

- [ ] 使用系统 ssh，参数数组，不经过 shell 拼接。
- [ ] secret JSON 从 stdin 发送，不进 argv。
- [ ] 远端目标文件只能位于 `/etc/wps-adapter/secrets/` 直接子级。
- [ ] 保留已有 owner，写临时文件并原子替换。

### SSH 密码

- [ ] 不引入 sshpass。
- [ ] 让系统 ssh 在终端自己提示密码。
- [ ] helper 不读取或保存 SSH 密码。

### 本地输出

- [ ] output-dir 必须绝对路径。
- [ ] 目录 0700、文件 0600。
- [ ] Cookie/CSRF/workspace 原子写。

完成条件：四种方式与 Python 请求/文件 schema 等价，任何错误不泄漏 stdin 内容。

## 15. 阶段 L10：端到端与平台验收

- [ ] Windows Chrome 登录、单空间、HTTPS 同步。
- [ ] Windows 多空间和 SSH 两种方式。
- [ ] Linux Chrome/Chromium 登录和本地输出。
- [ ] macOS Chrome 登录和 HTTPS 同步。
- [ ] SSO、扫码、验证码由人工完成，helper 不干预。
- [ ] Ctrl-C、浏览器关闭、超时、网络断开、错误 Basic、403/500、超大响应。
- [ ] 临时 profile 和 Chrome 子进程清理。
- [ ] 同一测试账号依次使用 Python/Go helper，服务端得到兼容文件。
- [ ] Go helper 导入后服务无需重启且 status connected。
- [ ] 日志和构建产物 secret 扫描为零。

## 16. 阶段 L11：发布切换

1. 首次把 Go helper 作为可选 beta 下载，Python helper 仍是默认回滚。
2. 为每个平台发布校验和、签名和最低系统要求。
3. README 同时列出两个 helper，但明确推荐/回滚路径。
4. 收集一个完整稳定周期的兼容结果。
5. 只有平台矩阵和安全门禁全绿才把 Go helper 设为默认。
6. 再经历一个稳定周期后，才停止发布新的 Python helper。
7. 旧 Python helper 仍可下载一段明确支持期。

## 17. 研究与构建工具处理

逐个分类，不做机械全译：

### 必须迁移或替换

- release manifest 生成：Go/CI 发布必须有跨平台可重复清单。
- 版本注入与 checksum 生成：不能继续要求最终发布机安装 Python。
- 服务端契约 runner：零 Python 目标下迁为 Go 或独立测试容器。

### 可继续保留为开发工具

- HAR 脱敏与摘要。
- curl/HAR/probe 研究工具。
- 旧 Python 行为 oracle。

这些工具不进入运行镜像，不影响终端用户“Go 服务”的性能或依赖。只有负责人要求开发仓库也完全零 Python 时才继续迁移。

### 每个工具的固定迁移步骤

1. 列输入、输出、参数、退出码和敏感数据规则。
2. 建无 secret fixture。
3. 对 Python/Go 输出做语义对照。
4. fuzz parser 和恶意路径/JSON/HAR。
5. 替换 CI 调用，但保留旧命令一个过渡期。
6. 更新文档和 release manifest。
7. 稳定后删除生成脚本，不删除研究证据。

## 18. 最终零 Python 完成条件

只有负责人明确要求“最终用户零 Python”时，以下为必须：

- [ ] VPS 服务只运行 Go 二进制。
- [ ] 网页为嵌入 HTML/CSS/原生 JavaScript。
- [ ] Go 登录 helper 覆盖所有支持平台和同步方式。
- [ ] 用户安装、登录、升级、卸载不调用 Python。
- [ ] Docker runtime 不含 Python。
- [ ] Native 安装不检测或安装 Python。
- [ ] release/checksum 工具不要求发布机 Python。
- [ ] Python helper 有明确回滚支持期和归档校验和。
- [ ] Python 服务源码至少在一个稳定周期后才移除。
- [ ] 文档不再把 Python 写成运行前提。

若负责人只要求“高性能常驻服务”，开发用 HAR 工具和历史参照测试可继续使用 Python，不构成运行时未完成。

## 19. 登录 helper 立即回滚条件

- Cookie 选择错误或漏掉 rtk/csrf。
- helper 连接了非 loopback CDP。
- helper 读取了日常浏览器 profile。
- 临时 profile 或 Chrome 进程未清理。
- workspace 选择了未经验证的空间/目录。
- secret 出现在 argv、URL、日志或错误中。
- HTTP 模式绕过 allow-http 确认。
- SSH 密码被 helper 捕获或保存。
- Go helper 写出的文件让 Python/Go 服务无法读取。

回滚只需恢复使用已归档的 `wps_login.py`；服务器和现有 secret 不需要切换或重新部署。
