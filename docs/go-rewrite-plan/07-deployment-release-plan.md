# Go 部署、升级、回滚与发布计划

> 本文只提供可执行的中文步骤和验收标准，不提供实现代码。
> 本文假设当前开发电脑没有 Go、Git、Docker、Bash 或 Node.js 环境，并假设最终用户不应在 VPS 上安装 Go 编译器。
> 执行模型必须保留现有 secret、端口、Basic Auth、登录助手和 WebDAV 使用方式；任何改变这些用户操作的方案都要先由负责人批准。

## 1. 本文目标

1. 在 Windows 开发电脑上建立可重复的 Go 开发环境。
2. 为 Linux VPS 生成不依赖 Python 的预编译 Go 服务。
3. 保留 Native 和 Docker 两条部署路径。
4. 保留 systemd 和无 systemd 的便携后台模式。
5. 保留当前一键安装器的下载校验、secret 保护、失败回滚和健康检查能力。
6. 让 Go 版本可以和 Python 版本并行验收，并能在一次服务切换内回滚。
7. 建立从提交、CI、跨架构构建、校验清单到发布推广的完整链路。
8. 第一轮继续发布独立的 Python 登录助手；只移除 VPS 长期服务的 Python 依赖。

## 2. 当前电脑的环境基线

截至 2026-09-05，在当前 PowerShell 会话中只读检查得到：

| 工具 | 当前状态 | 对计划的影响 |
| --- | --- | --- |
| Python | 已有 3.14.7 | 可继续运行现有测试和登录助手 |
| winget | 可用 | 可用于安装 Go 和 Git |
| Go | PATH 中不存在 | 开始 Go 实现前必须安装 |
| Git | PATH 中不存在 | 无法运行发布清单生成器 |
| Docker | PATH 中不存在 | 本机暂时不能构建或验证容器 |
| Bash | PATH 中不存在 | 本机暂时不能执行安装器语法检查 |
| Node.js/npm | PATH 中不存在 | 目标前端不需要安装；不要为拆分网页引入 |

当前工作目录还没有可见的 .git 元数据，表现为解压后的源码目录。现有 tools/build_release_manifest.py 在第 21 至 30 行调用 Git 的已跟踪文件列表，因此即使 Python 可用，没有 Git 元数据也不能可靠生成发布清单。

## 3. 三种环境必须分开理解

### 3.1 Windows 开发电脑

负责：

1. 阅读和修改 Go、HTML、CSS、JavaScript。
2. 运行快速 Go 单元测试。
3. 运行保留的 Python 回归测试。
4. 可选运行浏览器 E2E。

不应承担：

1. 生产 Linux 二进制的最终可信构建。
2. systemd 真机验收。
3. Linux 文件权限和符号链接安全的最终验收。

### 3.2 CI Linux 构建环境

负责：

1. 固定版本 Go 编译。
2. Linux amd64 和 arm64 产物。
3. Go 测试、竞态测试和静态检查。
4. Python 参照测试与跨语言黑盒测试。
5. Docker 镜像构建。
6. 发布校验和、来源信息和产物上传。

### 3.3 最终 Linux VPS

只需要：

1. 运行预编译 Go 二进制，或运行 Docker。
2. 保存 /etc/wps-adapter 配置和 secret。
3. 保存 /var/lib/wps-adapter/uploads。
4. 运行反向代理和现有 WebDAV 客户端。

最终 VPS 不应需要：

1. Go。
2. Git。
3. Python 服务运行时。
4. Node.js。
5. 前端构建工具。

如果账号所有者继续使用 wps_login.py，本地个人电脑仍需要 Python 3.11 以上和 Chrome；这不等于 VPS 仍依赖 Python。

## 4. 当前部署事实来源

| 内容 | 当前文件与行号 | 迁移时必须保留 |
| --- | --- | --- |
| Python 包与 CLI 入口 | pyproject.toml:5 和 pyproject.toml:30 | 版本、serve、check-config 的用户语义 |
| CLI 默认绑定和端口 | src/wps_adapter/__main__.py:76 | 默认 127.0.0.1 与 54321 |
| 非本地绑定要求认证 | src/wps_adapter/__main__.py:94 | Go 必须 fail closed |
| 服务启动和退出 | src/wps_adapter/__main__.py:132 | Go 增加优雅退出但不改入口 |
| 全部环境变量 | .env.example:1 | 名称和默认值第一版不变 |
| systemd 单元 | deploy/wps-adapter.service:1 | 用户、环境、重启和硬化 |
| 低内存硬化参数 | deploy/wps-adapter-hardening.env:1 | 数值保持一致 |
| 当前 Python 镜像 | deploy/Dockerfile:1 | 改为 Go 多阶段构建 |
| Compose 权限和挂载 | deploy/docker-compose.yml:1 | 端口、UID/GID、secret 和上传目录 |
| Native 安装器 | scripts/install-native.sh:1 | 安全安装与回滚基线 |
| Docker 安装器 | scripts/install-docker.sh:1 | Docker daemon、镜像和容器切换基线 |
| 卸载器 | scripts/uninstall.sh:1 | 默认保留配置和凭据 |
| 当前 CI | .github/workflows/test.yml:1 | 扩展而非立即删除 Python 验证 |
| 源码清单生成器 | tools/build_release_manifest.py:1 | 供应链校验基线 |
| 登录脚本生成器 | tools/build_login_script.py:1 | 第一轮继续保留 |
| 用户部署文档 | docs/deployment.md:1 | 所有操作说明同步更新 |
| 集成验收顺序 | docs/integration.md:19 | 发布后真机验证顺序 |
| 已有迁移方向 | docs/language-migration.md:130 | 本文细化该阶段计划 |

## 5. 当前 Native 安装器必须等价保留的行为

### 5.1 安装目标

1. 应用目录为 /opt/wps-adapter。
2. 配置目录为 /etc/wps-adapter。
3. secret 目录为 /etc/wps-adapter/secrets。
4. multipart 恢复目录为 /var/lib/wps-adapter/uploads。
5. 环境文件为 /etc/wps-adapter/wps-adapter.env。
6. systemd 单元为 /etc/systemd/system/wps-adapter.service。
7. 无 systemd 模式的 PID 和日志位于 /etc/wps-adapter。
8. Go 迁移不得改变 secret 路径。

### 5.2 依赖发现

当前安装器在 scripts/install-native.sh:63 识别 apt、dnf、microdnf、tdnf、yum、apk、pacman、zypper 和 xbps-install。Go 版仍需保留这些发行版入口，但不再安装 Python。

迁移后 Native 最少依赖应为：

1. CA 证书。
2. curl 或 wget。
3. tar。
4. sha256sum。
5. find 和基础 shell 工具。
6. systemd、runuser 或 su 中至少一种可用启动方式。

### 5.3 下载与校验

当前行为位于 scripts/install-native.sh:159 和 scripts/install-native.sh:620：

1. 只接受 HTTPS 下载地址。
2. 设置连接超时、总超时、重试和最大下载大小。
3. 先尝试自定义地址。
4. 再尝试两个国内代理。
5. 最后尝试 GitHub 直连。
6. 下载的是固定 40 位提交归档。
7. 提取前先检查 tar 条目类型和路径。
8. 拒绝绝对路径、父目录跳转、符号链接、硬链接、设备和 FIFO。
9. 校验 release-manifest.txt 自身 SHA-256。
10. 校验实际文件集合没有额外文件或缺失文件。
11. 对清单中每个文件执行 SHA-256 校验。

Go 预编译产物必须达到相同或更强的校验强度。

### 5.4 参数与交互

当前参数位于 scripts/install-native.sh:338：

1. --port。
2. --bind。
3. --group-id。
4. --root-id。
5. --adapter-user。
6. --run-user。
7. --source-ref。
8. --source-manifest-sha256。
9. --help。

当前密码只允许隐藏交互输入，不接受命令行参数。Go 安装器不得增加明文密码参数。

### 5.5 配置与 secret

当前行为位于 scripts/install-native.sh:534 和 scripts/install-native.sh:661：

1. 升级读取并保留旧端口、绑定、群组、根、workspace、Cookie、CSRF 和 Basic Auth 文件路径。
2. 默认 group 和 root 为 auto。
3. secret 路径必须是绝对路径。
4. secret 必须是 secret 目录下的直接普通文件。
5. 拒绝符号链接、特殊文件、路径重复和不安全文件名。
6. 新 secret 文件权限为 0600。
7. secret 目录权限为 0700。
8. 默认使用执行 sudo 的原用户运行服务。
9. 升级不覆盖已有有效 secret。

### 5.6 服务与回滚

当前行为位于 scripts/install-native.sh:748：

1. 记录旧服务是否运行、是否启用。
2. 备份旧应用、环境、单元、硬化配置和 secret。
3. 切换开始后任一步失败都触发 rollback。
4. rollback 恢复旧应用和非秘密配置。
5. rollback 恢复凭据文件存在性和权限。
6. rollback 恢复服务启用和运行状态。
7. 新服务启动后执行本机 /healthz。
8. 健康检查失败视为安装失败。

## 6. 当前 Docker 安装器必须等价保留的行为

1. 应用和 secret 仍保存在宿主机。
2. 自动识别并安装 Docker。
3. 可通过 systemd、OpenRC 或 SysV 启动 Docker daemon，见 scripts/install-docker.sh:212。
4. 当前先构建新镜像，再停止旧 Native 服务，见 scripts/install-docker.sh:759。
5. 发现正在运行的 Native 服务时，必须显式给出 --replace-native。
6. 同名容器只有带项目 managed 标签才可替换。
7. 旧容器先改名保留，新容器健康后再删除。
8. 新容器以执行 sudo 的原用户 UID/GID 运行。
9. 丢弃全部 Linux capabilities。
10. 启用 no-new-privileges。
11. secret 目录可写，以支持 Cookie/CSRF 同目录原子轮换。
12. Basic Auth 两个具体文件再次以只读挂载覆盖。
13. multipart 恢复目录可写。
14. 容器端口由宿主绑定和同值容器端口映射。
15. 新容器必须通过本机 /healthz。
16. 任一步失败恢复旧容器、旧 Native 服务、旧应用目录和配置。

## 7. Windows 环境准备逐微步

### 阶段 W0：保护当前源码

1. 不删除、不移动、不覆盖当前解压目录。
2. 记录当前目录的绝对路径。
3. 记录现有文件数量和 release-manifest.txt 的 SHA-256。
4. 检查是否存在用户未提交的本地配置、secret、HAR 或抓包。
5. 不把这些敏感文件复制进新 Git 工作区。
6. 当前目录没有 .git 时，不假设它等于远端 main。
7. 在安装 Git 后，从正式远端另建一个同级 clone。
8. 用文件级差异工具对比当前目录与 clone。
9. 只把确认属于源码的差异逐项迁入 clone。
10. 在负责人确认前，不对当前目录执行 git init、reset 或清理。

### 阶段 W1：安装 Git

1. 使用 winget 的 Git.Git 包，或从 Git 官方站点安装 Git for Windows。
2. 不使用来源不明的便携压缩包。
3. 保留 Git Bash 组件，用于本地 shell 脚本语法检查。
4. 选择让 Git 命令可从 PowerShell 使用。
5. 完成安装后关闭旧终端并打开新 PowerShell。
6. 验证 git 命令可用并记录版本。
7. 验证 Git Bash 可启动。
8. 验证检出 shell 脚本后仍保持 LF 行尾。

### 阶段 W2：安装 Go

1. 由负责人在 go.mod 中先确定项目支持的 Go 主次版本。
2. Windows 安装同一主次版本，不使用任意旧版本。
3. 使用 winget 的 GoLang.Go 包，或 Go 官方安装包。
4. 完成安装后关闭旧终端并打开新 PowerShell。
5. 验证 go 命令可用。
6. 记录 go version、GOOS、GOARCH 和 GOROOT。
7. 确认 GOOS 为 windows，GOARCH 与电脑实际架构一致。
8. 不全局修改 GOPATH 到项目目录。
9. 不把 Go 缓存、临时文件或编译产物提交到仓库。

### 阶段 W3：建立正确工作区

1. 使用 Git clone 得到带 .git 的正式工作区。
2. 确认远端 URL 指向项目正式仓库。
3. 确认当前分支和基准提交。
4. 检查 git status。
5. 检查 .editorconfig 使用 UTF-8、LF 和四空格。
6. Go 文件最终由 gofmt 决定缩进，不手工强制四空格。
7. 确认现有 Python 测试在迁入任何 Go 代码前通过。
8. 确认 tools/build_release_manifest.py --check 可运行。
9. 如果清单检查失败，先确认工作区版本，不自动重写清单掩盖差异。

### 阶段 W4：可选安装 Docker

1. 只有需要本地容器调试时才安装 Docker Desktop。
2. 先确认 Windows 虚拟化和 WSL2 条件。
3. Docker 不是开始 Go 代码的前置条件。
4. 安装后验证 docker version 和 docker info。
5. 验证 Linux container 模式。
6. 不把本机真实 /etc/wps-adapter secret 挂入测试容器。
7. 本机没有 Docker时，由 CI 完成容器门禁，不跳过发布前 CI。

### 阶段 W5：前端与浏览器工具

1. HTML、CSS 和原生 JavaScript无需 Node.js。
2. 不运行 npm init。
3. 不新增 package.json，只为“看页面”没有必要。
4. 浏览器 E2E 可以在 CI 的隔离开发环境安装所需驱动。
5. E2E 依赖不得进入 Go 二进制或生产镜像。

## 8. Go 产物规范

### 8.1 第一轮支持平台

负责人在首个实现提交前确定并记录：

1. Linux amd64 必须支持。
2. Linux arm64 建议同一首发支持。
3. armv7、386、FreeBSD、macOS 和 Windows 服务端不在首发承诺内，除非有真实测试机器。
4. Windows 只作为开发环境，不代表 Windows 服务部署已经支持。

### 8.2 编译约束

1. 服务二进制采用 CGO 关闭的纯 Go 构建，除非后续依赖明确要求 CGO。
2. 二进制必须嵌入 web/index.html、web/style.css 和 web/app.js。
3. 二进制必须能输出版本、提交号和构建时间的非敏感摘要。
4. 构建必须启用 trimpath，避免泄露 CI 绝对路径。
5. 同一标签的产物不得被覆盖。
6. 不把调试 secret 或真实 WPS fixture 编入二进制。
7. 产物在最低支持 Linux 环境做启动验证。

### 8.3 发行包内容

每个平台压缩包只包含明确文件：

1. wps-adapter 可执行文件。
2. LICENSE。
3. 一份最小运行说明。
4. 该产物的构建元数据或版本文本。

网页已经嵌入二进制，不应在发行包里再维护第二份可编辑网页。

### 8.4 命名

统一采用可机器解析的命名：

1. 项目名称。
2. 语义版本。
3. GOOS。
4. GOARCH。
5. 压缩格式。

命名规则一旦首发，不得在安装器中使用模糊匹配寻找资产；安装器必须构造唯一文件名。

## 9. 版本来源与兼容版本

当前版本同时出现在：

1. src/wps_adapter/__init__.py:3。
2. pyproject.toml:7。
3. CHANGELOG.md。
4. 安装器固定提交。

迁移步骤：

1. 新增唯一的人类可编辑版本来源，推荐使用仓库根 VERSION 文件。
2. Go 构建读取该版本并在构建时注入提交号。
3. Python 登录助手生成器读取同一版本，或在迁移期增加一致性检查。
4. pyproject.toml 在 Python 登录助手仍存在时继续保留。
5. CI 检查 VERSION、Go 输出、Python 包版本和 changelog 一致。
6. 不允许执行模型在多个文件手工写不同版本后继续发布。
7. Go 的 HTTP Server 头和 /healthz version 字段保持现有语义。

## 10. Native 预编译发布设计

### 10.1 为什么不能在 VPS 现场编译

1. 用户明确没有 Go 环境。
2. 最终部署目标是低配 VPS。
3. 现场编译会增加内存、磁盘、时间和供应链依赖。
4. 现场下载任意 Go 模块会让构建不可重复。
5. Native 安装器必须下载 CI 已构建的目标架构产物。

### 10.2 架构识别

安装器先做：

1. 确认内核为 Linux。
2. 读取 uname 机器架构。
3. 把 x86_64 映射为 amd64。
4. 把 aarch64 或 arm64 映射为 arm64。
5. 未明确支持的架构立即停止，并列出支持值。
6. 不在未知架构上尝试运行另一个二进制。

### 10.3 下载对象

安装器下载：

1. 固定版本的目标平台压缩包。
2. 固定版本的产物校验清单。
3. 可选的签名或来源证明。

下载候选仍按：

1. 管理员显式 HTTPS 地址。
2. 已批准的国内代理。
3. GitHub Release 直连。

不得回退到可变 latest URL 后不校验内容。

### 10.4 校验顺序

1. 校验下载协议是 HTTPS。
2. 校验响应大小不超过合理上限。
3. 校验校验清单自身的固定 SHA-256。
4. 从清单中只选精确平台文件名。
5. 校验压缩包 SHA-256。
6. 解压前检查条目名称、类型和数量。
7. 拒绝绝对路径、父目录跳转、链接和特殊文件。
8. 解压后确认只有允许文件。
9. 确认可执行文件不是符号链接。
10. 执行新二进制 --version。
11. 版本不匹配立即停止。
12. 执行新二进制 check-config；该步骤不得访问 WPS。

### 10.5 Native 目录切换

1. 在 /opt 的安全临时目录中准备新版本。
2. 临时目录与最终目录位于同一文件系统，以便原子目录切换。
3. 新二进制归服务用户所有，只有必要执行权限。
4. 不把 secret 复制到应用目录。
5. 停服务前完成下载、校验、解压、版本和配置检查。
6. 记录旧应用目录和单元状态。
7. 停止旧服务。
8. 把旧应用目录移动到明确的回滚目录。
9. 把新目录移动为 /opt/wps-adapter。
10. 安装双版本兼容的 systemd 单元。
11. 启动 Go 服务。
12. 验证进程、/healthz 和受认证 /api/v1/status。
13. 失败则恢复旧目录和旧单元，并启动旧服务。
14. 成功后仍保留一个明确的上一版本回滚包，直到观察窗口结束。
15. 不回滚 Cookie、CSRF 和 workspace 到旧副本；这些状态可能已被新会话轮换。

## 11. CLI 兼容要求

Go 二进制必须保留：

1. 顶层 --version。
2. serve 子命令。
3. serve 的 --bind。
4. serve 的 --port。
5. check-config 子命令。
6. 成功退出码 0。
7. 配置、绑定或启动失败退出码非 0。
8. check-config 不发网络请求。
9. 启动日志输出监听地址、WebDAV 前缀和 REST 前缀，但不输出 secret。
10. Ctrl+C 和 SIGTERM 都触发有界优雅关闭。

第一轮不要求 Go 二进制实现 login 子命令。仓库根 wps_login.py 继续作为登录入口，文档必须明确这一点。

## 12. systemd 迁移逐微步

### 12.1 先更新卸载识别

这是强制顺序：

1. 先让 scripts/uninstall.sh 同时识别现有 Python ExecStart 和新 Go ExecStart。
2. 校验必须是两个明确允许的完整形状，不能放宽为任意可执行文件。
3. 同时更新便携模式的进程识别。
4. 为旧 Python 单元和新 Go 单元各增加测试。
5. 先发布该双版本卸载器。
6. 只有用户已能安全卸载两种版本后，才切换安装器写入 Go 单元。

### 12.2 新单元必须保留

1. Description 保持可识别。
2. After 和 Wants 继续依赖 network-online.target。
3. Type 继续为 simple。
4. User 和 Group 继续由安装器替换为实际运行用户。
5. WorkingDirectory 继续为 /opt/wps-adapter。
6. EnvironmentFile 继续读取 /etc/wps-adapter/wps-adapter.env。
7. ExecStart 改为 /opt/wps-adapter 中明确的 Go 二进制 serve。
8. 删除 PYTHONPATH。
9. Restart 继续为 on-failure。
10. RestartSec 继续为 5 秒。
11. UMask 继续为 0077。
12. PrivateTmp、NoNewPrivileges、ProtectSystem、ProtectHome 继续保留。
13. ReadWritePaths 继续只开放 secret 和上传恢复目录。
14. 服务不以 root 运行，除非用户明确以 root 执行安装且没有普通 sudo 用户。

### 12.3 Go 关闭行为

1. systemd 发送 SIGTERM 后，Go 停止接受新连接。
2. 在限定时间内等待在途请求。
3. 到期后取消剩余 WPS 和对象存储请求。
4. 关闭下载响应体。
5. 清理请求级临时上传文件。
6. 释放上传、下载和连接槽。
7. 不删除持久 multipart 检查点。
8. 正常 SIGTERM 退出不应触发无限重启。
9. systemd TimeoutStopSec 必须大于应用优雅关闭期限。

### 12.4 单元验收

1. daemon-reload 无错误。
2. enable --now 成功。
3. systemctl status 显示 Go 二进制路径。
4. 服务进程 UID/GID 正确。
5. /healthz 成功。
6. /api/v1/status 未认证返回 401。
7. 认证后状态响应不含敏感字段。
8. journalctl 不含查询参数值、Cookie、CSRF 或签名 URL。
9. 重启后进程内 WebDAV 锁按文档失效。
10. 配置错误时服务快速失败，不进入重启风暴。

## 13. 无 systemd 便携模式迁移

当前 Python 启动逻辑位于 scripts/install-native.sh:298。

迁移步骤：

1. 保留 PID 文件和日志文件路径。
2. 从环境文件导出变量后直接 exec Go 二进制 serve。
3. 删除 PYTHONPATH 和 Python 解释器发现。
4. pid_is_adapter 同时识别旧 Python 命令和新 Go 绝对路径。
5. 不只凭进程名杀进程；仍检查完整命令形状。
6. 首次过渡版本同时支持停止旧 Python 和新 Go。
7. 启动后确认 PID 仍存活。
8. 再执行 /healthz。
9. 启动失败打印日志最后有限行数。
10. 保持“无 systemd 不自动注册开机启动”的当前说明，除非另开功能任务。

## 14. Dockerfile 迁移逐微步

### 阶段 D0：冻结当前容器契约

1. 记录当前镜像用户、工作目录、入口、环境和挂载。
2. 记录当前镜像大小、启动时间和常驻内存。
3. 记录 Docker 下 secret 原子轮换成功。
4. 记录上传超过内存阈值后的临时文件行为。
5. 记录容器停止时在途下载和上传行为。

### 阶段 D1：建立 Go 多阶段构建

1. 构建阶段使用项目固定 Go 版本。
2. 固定基础镜像标签，正式发布再固定镜像摘要。
3. 先复制 go.mod 和 go.sum 以利用依赖缓存。
4. 再复制 Go 源码和 web 资源。
5. 关闭 CGO。
6. 构建 Linux 目标二进制。
7. 运行阶段不包含 Go 工具链和源码。
8. 第一版运行阶段优先选择具有可靠 CA 证书和用户管理的最小 Debian 运行镜像。
9. 只有在 TLS、DNS、时区、MIME、临时目录和非 root 测试全部通过后，才考虑 scratch。
10. 运行镜像必须能验证 WPS HTTPS 证书。
11. 创建 /var/lib/wps-adapter/uploads。
12. 确保请求级临时目录对容器用户可写。
13. 保留 APP_UID 和 APP_GID 构建参数，或改为运行时数字用户但保持 Compose 契约。
14. ENTRYPOINT 只启动 Go 服务的 serve 子命令。

### 阶段 D2：运行镜像安全

1. 容器以宿主传入的非 root UID/GID 运行。
2. 不需要 shell 才能运行服务。
3. 不把编译器、包缓存或测试 fixture 带入最终层。
4. 不把 .env、secret、captures、HAR、PCAP 带入构建上下文。
5. 保持 cap-drop ALL。
6. 保持 no-new-privileges。
7. 根文件系统只读属于后续可选强化；启用前必须为临时上传明确可写挂载。
8. 通过镜像扫描检查操作系统包和 Go 标准库漏洞。

### 阶段 D3：镜像验收

1. 镜像启动不需要 Python。
2. 容器内找不到 Python 不应影响服务。
3. --version 输出与发布版本一致。
4. /healthz 成功。
5. 前端三个资源成功。
6. Basic Auth 文件只读。
7. Cookie、CSRF、workspace 和 web-settings 可原子替换。
8. multipart 恢复目录可写。
9. 请求级临时 spool 可创建并清理。
10. 容器重启后读取同一 secret，无需重新登录。

## 15. Docker Compose 迁移

当前契约位于 deploy/docker-compose.yml:7。

逐步修改：

1. build.context 继续指向仓库根。
2. dockerfile 继续指向 deploy/Dockerfile。
3. 把 Python BASE_IMAGE 参数替换为明确的 Go builder 和 runtime 镜像参数。
4. 保留 APP_UID 和 APP_GID。
5. 保留 restart unless-stopped。
6. 保留运行时 user。
7. 保留 cap_drop ALL。
8. 保留 no-new-privileges。
9. 保留 /etc/wps-adapter/wps-adapter.env。
10. 容器内 ADAPTER_BIND 继续强制为 0.0.0.0。
11. 保留宿主绑定、宿主端口和容器端口相同的映射。
12. 保留整个 secret 目录可写挂载。
13. 保留上传恢复目录可写挂载。
14. 保留 Basic Auth 用户名和密码文件的只读覆盖挂载。
15. 自定义 Basic Auth 文件名时继续要求同步修改 Compose 挂载。
16. 增加容器健康检查前，先确认不会把 WPS status 当成进程健康。
17. Compose healthcheck 只能访问 /healthz，不访问 WPS。

## 16. Docker 安装器迁移

### 16.1 过渡方案

第一轮推荐继续由安装器：

1. 下载固定提交源码归档。
2. 校验源码清单。
3. 用 Go 多阶段 Dockerfile 构建镜像。
4. 宿主不安装 Go，Go 工具链只存在于构建容器。

这样可以保留当前固定提交与源码清单模型，同时满足宿主无 Go 环境。

### 16.2 必须修改的位置

1. 把 LOCAL_BASE_IMAGE 的 Python 名称改为 Go builder 名称。
2. 把 WPS_ADAPTER_DOCKER_BASE_IMAGE 拆成或替换为含义明确的 builder/runtime 镜像配置。
3. 把国内 Python 镜像候选换成 Go builder 和运行镜像候选。
4. 所有自定义镜像仍应来自负责人允许的 registry。
5. 输出文案不再说“Python 基础镜像”。
6. 构建成功仍必须发生在停止旧服务之前。
7. 容器标签继续记录项目 managed=true 和不可变应用版本。
8. 入口进程检查改为 Go。
9. rollback 继续恢复旧容器和 Native 服务。
10. 安装完成文案继续给出网页、WebDAV、用户和 secret 目录。

### 16.3 后续可选方案

CI 稳定后可发布多架构镜像，并让安装器按固定 digest 拉取。采用前必须完成：

1. amd64 和 arm64 manifest 验证。
2. 镜像 digest 固定。
3. 国内网络可达性或自定义 registry 方案。
4. 镜像签名或来源证明。
5. 与本地多阶段构建结果的行为对照。
6. 旧版离线或镜像不可达回滚路径。

首版不要同时切换后端语言、Dockerfile 和镜像分发模型三个变量。

## 17. 配置兼容迁移

### 17.1 环境变量分组

第一版保留 .env.example 中全部名称和默认值：

1. WPS 上游与工作区：
   - WPS_GROUP_ID。
   - WPS_ROOT_ID。
   - WPS_ROOT_NAME。
   - WPS_BASE_URL。
   - WPS_ACCOUNT_BASE_URL。
   - WPS_OBJECT_STORAGE_HOST_SUFFIX。
   - WPS_REFERER。
   - WPS_ORIGIN。
   - WPS_CID。
2. 凭据文件：
   - WPS_COOKIE_FILE。
   - WPS_CSRF_TOKEN_FILE。
   - WPS_WORKSPACE_FILE。
3. 超时、状态与列表：
   - WPS_TIMEOUT。
   - WPS_STATUS_PROBE_TTL。
   - WPS_STATUS_FAILURE_BACKOFF。
   - WPS_LIST_COUNT。
   - WPS_MAX_LIST_ENTRIES。
   - WPS_CACHE_TTL。
   - WPS_MAX_CACHED_FOLDERS。
4. 上传和下载：
   - WPS_UPLOAD_SPOOL_MEMORY。
   - WPS_STREAM_CHUNK_SIZE。
   - WPS_UPLOAD_SPOOL_DIR。
   - WPS_UPLOAD_RESUME_DIR。
   - WPS_UPLOAD_MIN_FREE_BYTES。
   - WPS_MAX_UPLOAD_BYTES。
   - WPS_UPLOAD_RETRIES。
   - WPS_UPLOAD_RETRY_DELAY。
   - WPS_MULTIPART_THRESHOLD。
   - WPS_MULTIPART_PART_SIZE。
   - WPS_ENABLE_RANGE。
5. 刷新：
   - WPS_AUTO_REFRESH。
   - WPS_CREDENTIAL_REFRESH_COMMAND。
   - WPS_CREDENTIAL_REFRESH_TIMEOUT。
6. 资源上限：
   - WPS_MAX_UPLOADS。
   - WPS_MAX_DOWNLOADS。
   - WPS_TRANSFER_WAIT_TIMEOUT。
   - WPS_MAX_COPY_ENTRIES。
   - WPS_MAX_COPY_DEPTH。
   - WPS_MAX_PROPFIND_ENTRIES。
   - WPS_MAX_PROPFIND_DEPTH。
   - WPS_MAX_LOCKS。
   - WPS_MAX_CONTROL_BODY。
   - WPS_MAX_JSON_RESPONSE_BYTES。
   - WPS_MAX_RESPONSE_BODY_BYTES。
7. 服务：
   - ADAPTER_USERNAME_FILE。
   - ADAPTER_PASSWORD_FILE。
   - ADAPTER_BIND。
   - ADAPTER_PORT。
   - ADAPTER_DAV_PREFIX。
   - ADAPTER_REST_PREFIX。
   - ADAPTER_MAX_CONNECTIONS。
   - ADAPTER_REQUEST_TIMEOUT。

### 17.2 加载规则

1. Go 服务读取进程环境，不自行把任意文件当 shell 执行。
2. systemd 和安装器负责加载 wps-adapter.env。
3. Docker 使用 --env-file 或 Compose env_file。
4. 布尔值、整数、浮点、端口、URL、路径和 host suffix 在启动时集中校验。
5. check-config 使用相同解析器。
6. 配置错误只显示变量名和规则，不回显敏感值。
7. 非本地 bind 时必须确认 Basic Auth 用户名和密码都有效。
8. 第一版不得重命名环境变量来“更符合 Go 风格”。

## 18. secret 与状态文件兼容

1. Go 直接读取旧 wps-cookie。
2. Go 直接读取旧 wps-csrf。
3. Go 直接读取旧 wps-workspace.json，包括单空间旧格式和多空间格式。
4. Go 直接读取旧 web-settings.json。
5. Go 直接读取旧 adapter-username 和 adapter-password。
6. Linux 上继续要求私有父目录、普通文件、非符号链接、正确 owner 和 0600。
7. Cookie/CSRF 刷新使用同目录临时文件和原子替换。
8. workspace 和 web settings 同样使用原子替换。
9. Docker secret 目录必须可写才能完成替换。
10. Basic Auth 两个文件本身不需要由服务写。
11. 升级和回滚都不要求重新登录。
12. 回滚程序读取当前最新 secret，不恢复可能已经失效的旧 Cookie。

## 19. 安装流程逐步门禁

每种安装方式都按以下顺序，不能提前停旧服务：

1. 解析和校验参数。
2. 确认 root 权限。
3. 确定实际运行用户和组。
4. 验证目标目录不是异常符号链接或特殊文件。
5. 读取旧非敏感配置和 secret 路径。
6. 确认端口合法。
7. 下载新产物或源码。
8. 完成所有 SHA-256 和归档安全校验。
9. 完成新二进制版本检查。
10. 生成候选环境文件。
11. 执行新二进制 check-config。
12. 准备候选 systemd 单元或容器。
13. 备份旧应用、单元和非秘密配置。
14. 记录旧服务/容器是否运行和启用。
15. 安装或构建新产物。
16. 直到此处全部成功后才停止旧服务。
17. 原子切换应用或容器。
18. 启动新服务。
19. 检查进程存活。
20. 检查 /healthz。
21. 使用 Basic Auth 检查 /api/v1/status。
22. 失败时自动恢复旧服务。
23. 成功时打印网页、WebDAV、运行用户和 secret 路径。
24. 不在输出中打印密码、Cookie、CSRF、工作区 ID 或签名 URL。

## 20. Python 与 Go 灰度

### 20.1 灰度前准备

1. Python 服务保持原端口。
2. Go 服务使用另一个仅本机可达端口。
3. 不让两个服务同时对同一测试目录执行写操作。
4. 只读对照可以使用相同 secret，但关闭会改变凭据的额外刷新路径。
5. 写操作对照使用专用测试目录。
6. 必要时为影子服务使用权限相同的 secret 副本，避免两个进程竞争原子轮换。
7. 反向代理暂不指向 Go。

### 20.2 对照顺序

沿用 docs/integration.md:21 的顺序：

1. /healthz。
2. /api/v1/status。
3. REST 根目录列表。
4. WebDAV Depth 0。
5. WebDAV Depth 1。
6. 小文件上传。
7. 下载并校验 SHA-256。
8. 新建文件夹。
9. 重命名。
10. 移动。
11. 删除本次测试对象。
12. 单 Range 下载。
13. COPY。
14. LOCK 和 UNLOCK。
15. 递归 PROPFIND。
16. 大文件 multipart。
17. 客户端中断上传和下载。
18. Cookie refresh。
19. 多空间根目录。
20. 一个完整客户端同步周期。

### 20.3 切流

1. 先停止影子 Go。
2. 记录 Python 服务和配置状态。
3. 停止 Python。
4. 用生产配置启动 Go。
5. 只把反向代理或端口切向 Go。
6. 执行健康、认证状态和只读检查。
7. 在专用测试目录执行一个小型写闭环。
8. 观察日志、内存、临时磁盘和连接。
9. 观察至少一个完整 Cookie refresh 周期。
10. 在负责人设定的稳定窗口内保留 Python 应用和单元备份。

## 21. 回滚计划

### 21.1 自动安装失败回滚

1. 停止失败的新 Go 进程或容器。
2. 恢复旧应用目录或旧容器名称。
3. 恢复旧 systemd 单元和硬化配置。
4. 恢复旧非秘密环境文件。
5. 保留当前 Cookie、CSRF、workspace 和 web-settings。
6. 恢复旧服务启用状态。
7. 启动旧 Python 服务。
8. 检查 /healthz。
9. 检查受认证 status。
10. 报告失败阶段，不打印 secret。

### 21.2 发布后人工回滚

出现以下任一情况立即回滚：

1. 文件下载校验和不同。
2. 上传后远端内容不同。
3. WebDAV 客户端目录层级变化。
4. 多空间被错误路由。
5. Range 长度或 Content-Range 错误。
6. 401 refresh 使凭据损坏。
7. 日志出现 Cookie、CSRF、ID、文件路径或签名 URL。
8. 客户端断开后连接或临时文件持续存在。
9. 内存峰值高于已批准基线。
10. 安装器无法自动恢复旧服务。

人工回滚只切换程序、镜像和服务单元，不回滚远端 WPS 数据，也不覆盖当前 secret。

### 21.3 回滚演练

每个候选版本必须实际演练：

1. Python 升 Go 成功。
2. Go 启动失败自动恢复 Python。
3. Go 运行后人工恢复 Python。
4. Docker 新容器失败恢复旧容器。
5. Docker 替换 Native 失败恢复 Native。
6. 无 systemd 模式失败恢复旧 PID 进程。
7. 回滚后无需重新登录。

## 22. 卸载器迁移

当前卸载器在 scripts/uninstall.sh:124 精确校验 Python systemd 单元，这是重要保护，但会拒绝未来 Go 单元。

逐步修改：

1. 同时接受旧 Python ExecStart 和新 Go ExecStart。
2. 同时接受旧 PYTHONPATH 行存在和新单元不含该行。
3. 继续校验 Description、WorkingDirectory 和 EnvironmentFile。
4. Go 单元必须匹配固定绝对二进制路径。
5. 便携 PID 检查同时支持两种进程。
6. Docker 仍只删除 managed 标签容器。
7. 默认仍保留 /etc/wps-adapter/wps-adapter.env 和 secret。
8. --purge 才删除配置、Cookie、CSRF、Basic Auth、workspace 和 web settings。
9. --remove-image 才删除项目镜像。
10. 不删除 Docker 软件。
11. 不删除 WPS 云盘远端文件。
12. 若引入上一版本回滚目录，卸载提示必须列出并安全删除它；默认保留策略由负责人明确。
13. 增加旧 Python、Go Native、Go Docker、混合残留和未知同名对象测试。
14. 在切换 Go systemd 单元之前先发布此版本。

## 23. CI 迁移计划

当前 .github/workflows/test.yml:10 只有 Python 3.11 至 3.14 单元测试、compileall 和两个安装器的 Bash 语法检查。

### 23.1 PR 快速门禁

每个 PR 执行：

1. 检出完整 Git 历史或至少可得到提交号。
2. 安装 go.mod 指定的 Go 版本。
3. 检查 gofmt 无差异。
4. 运行 go vet。
5. 运行 go test ./...。
6. 在 Linux 运行 Go race 测试。
7. 保留 Python 3.11 至 3.14 测试，直到 Python 服务退役。
8. 运行 Python compileall。
9. 检查 standalone wps_login.py 与生成器一致。
10. 检查源码 release-manifest.txt 一致。
11. 对 install-native.sh、install-docker.sh 和 uninstall.sh 全部执行 Bash 语法检查。
12. 检查 web/index.html、style.css 和 app.js 都被嵌入且清单包含。
13. 运行 Python/Go 语言无关 HTTP 契约测试。
14. 运行浏览器前端 E2E。
15. 构建 Docker 镜像并启动 /healthz。
16. 检查 Docker 最终层不含 Go 工具链、Python、源码或测试 secret。
17. 检查 Git 差异没有生成物漂移。

### 23.2 较慢门禁

在主分支或夜间执行：

1. 大目录 PROPFIND。
2. 并发上传和下载。
3. 客户端断开。
4. multipart 失败恢复。
5. Docker 下原子凭据轮换。
6. systemd 虚拟机安装、升级、回滚和卸载。
7. 无 systemd Linux 安装。
8. amd64 和 arm64 真实或仿真启动。
9. 内存、临时磁盘和 goroutine 泄漏检查。
10. 依赖漏洞和镜像扫描。

### 23.3 CI 安全

1. 默认 permissions 继续最小化为 contents read。
2. PR 测试不使用真实 WPS secret。
3. 发布 job 才获得写 Release 所需的最小权限。
4. fork PR 不获得发布 secret。
5. 测试日志不打印环境文件。
6. 上传失败时不把临时测试工作区作为公开 artifact。
7. release artifact 不包含 captures、HAR、PCAP 或用户文件。

## 24. 发布清单与供应链

### 24.1 保留源码清单

当前 release-manifest.txt 是源码归档的逐文件 SHA-256 清单，生成器位于 tools/build_release_manifest.py。

迁移时：

1. 继续把所有已跟踪源码文件列入清单。
2. 新增 Go 源文件和 web 资源后重新生成。
3. 安装器脚本仍按当前设计排除在应用清单之外时，文档必须解释 bootstrap 边界。
4. 生成器必须运行在真实 Git checkout。
5. 清单生成后再次运行 --check。
6. 不手工编辑某一行哈希。

### 24.2 新增二进制产物清单

源码清单不能替代编译产物校验。另建发布产物校验清单，包含：

1. 每个平台压缩包精确文件名。
2. 每个平台压缩包 SHA-256。
3. Docker 镜像 digest。
4. 源提交完整 40 位 SHA。
5. Go 版本。
6. 构建工作流标识。

安装器内固定该产物清单自身的 SHA-256，再从清单校验目标包。

### 24.3 可重复性

1. 固定 Go 版本。
2. 固定 go.mod 和 go.sum。
3. 固定基础镜像摘要。
4. 使用 trimpath。
5. 不把本地路径或不稳定时间写入影响哈希的内容；如果记录构建时间，明确它会影响可重复性。
6. 同一源提交在独立 job 构建并比较哈希，或至少记录差异原因。
7. 发布后禁止替换同名资产。

### 24.4 签名与证明

第一版 SHA-256 是最低门槛。后续增加：

1. GitHub artifact attestation。
2. 二进制签名。
3. 容器签名。
4. SBOM。
5. 依赖许可证清单。

任何签名方案都不能让安装器在验证失败时静默回退到不验证。

## 25. 正式发布逐微步

### 阶段 R0：冻结候选

1. 选择唯一候选提交。
2. 确认工作区无未提交文件。
3. 确认版本号和 changelog。
4. 确认兼容性决策已记录。
5. 确认 Python 与 Go 全部契约测试。
6. 确认前端 E2E。
7. 确认 Native、Docker、systemd 和卸载演练。

### 阶段 R1：生成源相关产物

1. 重新生成 wps_login.py。
2. 检查生成结果一致。
3. 重新生成 release-manifest.txt。
4. 检查源码清单一致。
5. 创建不可变版本标签。
6. 记录完整提交 SHA。

### 阶段 R2：构建

1. 从标签重新检出干净工作区。
2. 构建 linux/amd64。
3. 构建 linux/arm64。
4. 对每个二进制执行 --version。
5. 对每个平台包执行解压和启动冒烟。
6. 构建 Docker 镜像。
7. 启动 Docker /healthz。
8. 运行镜像安全扫描。

### 阶段 R3：清单和上传

1. 生成二进制产物 SHA-256 清单。
2. 写入源提交和 Go 版本元数据。
3. 生成可选签名和证明。
4. 上传所有产物。
5. 上传后重新下载到隔离 job。
6. 再次校验清单。
7. 再次执行二进制 --version。
8. 发布资产只读，不覆盖。

### 阶段 R4：推广安装器

安装器与应用版本解耦，避免“修改安装器常量又改变被构建提交”的循环：

1. 应用 Release 成功后，再创建单独推广提交。
2. 同时更新 Native 和 Docker 安装器的固定版本、提交或清单摘要。
3. 更新 README 一键安装说明。
4. CI 用新的 raw 安装器实际安装刚发布版本。
5. 安装器测试成功后合并推广提交。
6. 保留上一稳定版的版本、摘要和回滚说明。
7. 推广失败不删除已发布资产，只让默认安装器继续指向上一稳定版。

### 阶段 R5：发布后验证

1. 从 README 公布的原始命令开始，不使用开发者本地快捷方式。
2. 在全新 amd64 VPS 安装 Native。
3. 在全新 arm64 VPS 安装 Native。
4. 在全新 VPS 安装 Docker。
5. 从现有 Python Native 升级到 Go Native。
6. 从现有 Python Docker 升级到 Go Docker。
7. 从 Native 切换 Docker。
8. 执行卸载默认保留凭据。
9. 重新安装并确认无需重新登录。
10. 执行 --purge 只在专用测试机验证。

## 26. 文档迁移清单

### 26.1 README.md

更新：

1. 版本号。
2. Native 不再需要 VPS Python。
3. 本地 wps_login.py 仍需要 Python。
4. Native 和 Docker 一键安装命令。
5. Go 服务不改变网页、WebDAV 和 REST 地址。
6. 卸载与回滚。
7. 当前限制。
8. 开发测试命令增加 Go。

当前关键位置为 README.md:9、README.md:15、README.md:44、README.md:64、README.md:141 和 README.md:190。

### 26.2 docs/deployment.md

更新：

1. 主机要求。
2. Native 预编译包。
3. 手工安装的 --version 和 check-config。
4. systemd ExecStart。
5. Docker builder/runtime 镜像。
6. 升级、灰度和回滚。
7. 日志和健康检查。
8. 卸载。

当前关键位置为 docs/deployment.md:51、docs/deployment.md:71、docs/deployment.md:102、docs/deployment.md:141、docs/deployment.md:176 和 docs/deployment.md:193。

### 26.3 deploy/README.md

更新：

1. Dockerfile 不再描述 Python 标准库应用。
2. Native 模板改为 Go 二进制。
3. Compose 镜像参数。
4. secret 可写目录和 Basic Auth 只读覆盖继续说明。
5. 安装器固定产物校验。

### 26.4 docs/login.md

第一轮只澄清：

1. Python 仅用于账号所有者电脑上的登录助手。
2. Go VPS 服务继续接受 POST /api/v1/session/import。
3. 同步后无需重启 Go 服务。
4. SSH 写入路径不变。
5. 不把“服务已改 Go”误写成“登录助手也不需要 Python”。

### 26.5 docs/api.md 和 docs/integration.md

1. 只有实际外部行为改变时才更新 API。
2. 实现语言变化本身不应改变接口文档。
3. 增加 Go/Python 对照验证说明。
4. 保留现有状态码、WebDAV 方法和验收顺序。

### 26.6 其他文件

1. CHANGELOG.md 记录 Go 服务首发和已批准差异。
2. CONTRIBUTING.md 增加 Go 环境、测试和格式化步骤。
3. SECURITY.md 增加 Go 依赖和二进制报告信息。
4. .gitignore 增加 Go 二进制、coverage 和临时测试目录。
5. .dockerignore 继续排除 .env、secret、抓包，并增加本地 Go 构建输出。
6. pyproject.toml 在登录助手仍由 Python 维护期间不要删除。

## 27. 部署测试矩阵

| 维度 | 最低覆盖 |
| --- | --- |
| 架构 | linux/amd64、linux/arm64 |
| Native init | systemd、无 systemd |
| Docker init | systemd、OpenRC、至少一次 SysV |
| 安装来源 | GitHub 直连、一个国内代理、自定义 HTTPS 地址 |
| 安装类型 | 全新、原地升级、失败回滚、默认卸载、purge |
| 服务来源 | Python Native 到 Go Native、Python Docker 到 Go Docker、Native 到 Docker |
| 网络入口 | 127.0.0.1、0.0.0.0 加 Basic Auth、HTTPS 反代 |
| secret | 首次空文件、已有凭据、凭据轮换、自定义直接文件名 |
| 文件传输 | 小文件、超过内存 spool、multipart、Range、断开 |
| 前端 | /、/web、/web/、CSS、JavaScript、Basic Auth |

每个格子不一定单独一台机器，但必须有可追溯测试记录，不能只写“手工验证过”。

## 28. 资源和性能验收

1. 记录 Python 和 Go 空闲 RSS。
2. 记录 64 个空闲连接时 RSS。
3. 记录默认 2 上传和 4 下载时 RSS、CPU 和临时盘。
4. 记录大目录 PROPFIND 峰值内存。
5. 记录客户端断开到上游连接关闭的时间。
6. 记录 SIGTERM 到进程退出的时间。
7. 记录 Native 启动时间。
8. 记录 Docker 启动时间。
9. Go 内存峰值高于 Python 基线时停止推广并分析。
10. 单文件速度受 WPS 限制时如实记录，不能把迁移宣传为固定倍数加速。

## 29. 执行顺序

低能力执行模型必须严格按以下顺序，一次只完成一项：

1. 安装并验证 Git。
2. 建立带 .git 的正式工作区。
3. 安装并验证固定版本 Go。
4. 运行当前 Python 基线测试。
5. 冻结外部契约和性能基线。
6. 建立 go.mod、版本来源和最小 --version。
7. 建立 check-config。
8. 完成 Go 服务功能阶段，不改生产安装器。
9. 完成前端静态资源嵌入。
10. 完成跨语言黑盒测试。
11. 先修改卸载器使其双版本兼容。
12. 测试旧 Python 卸载不回归。
13. 更新 systemd 模板为候选 Go 单元。
14. 更新 Native 安装器下载预编译包。
15. 测试 Native 全新安装。
16. 测试 Python Native 升 Go。
17. 测试失败自动回滚。
18. 更新 Dockerfile 为多阶段 Go 构建。
19. 更新 Compose。
20. 更新 Docker 安装器。
21. 测试 Python Docker 升 Go。
22. 测试 Docker 失败回滚。
23. 扩展 CI。
24. 建立二进制产物清单。
25. 构建候选 Release。
26. 在隔离 VPS 完成部署矩阵。
27. 更新全部文档。
28. 创建正式 Release。
29. 单独更新安装器固定版本和摘要。
30. 从 README 命令做发布后安装。
31. 观察稳定窗口。
32. 稳定后才考虑移除 VPS Python 安装兼容代码。

## 30. 每一步固定回报格式

执行模型每完成一步都要报告：

1. 本步唯一目标。
2. 修改的文件。
3. 产物名称和版本。
4. 保留的旧兼容路径。
5. 执行的测试。
6. 测试结果。
7. 是否接触真实 secret。
8. 是否停止过现有服务。
9. 是否验证自动回滚。
10. 是否需要负责人做决定。
11. 下一步唯一动作。

任何涉及服务停止、容器替换、purge 或删除回滚包的步骤，都要在执行前再次列出精确目标。

## 31. 明确禁止事项

1. 不在 VPS 现场 go build 作为 Native 正常安装流程。
2. 不从可变 main 或 latest 下载后跳过摘要。
3. 不把 Go 迁移与新的认证系统一起做。
4. 不改变 secret 路径。
5. 不把真实 Cookie 放入 CI。
6. 不把前端资源从远端 CDN 加载。
7. 不因 Go 默认支持 chunked 上传就放宽现有 Content-Length 契约。
8. 不先删除 Python 服务再构建或下载 Go。
9. 不在新 Go 单元发布前忘记更新卸载器识别。
10. 不用宽泛进程名匹配杀死未知进程。
11. 不在回滚时恢复旧 Cookie 覆盖新 Cookie。
12. 不让 Docker 最终镜像包含编译器和源码。
13. 不删除 wps_login.py，除非未来有单独、完整的登录助手迁移计划。
14. 不宣称 Windows 生产服务受支持，除非完成独立部署测试。

## 32. 最终完成定义

只有同时满足以下条件，才允许声明部署发布迁移完成：

1. 当前 Windows 开发机有可验证的 Git 和固定版本 Go 环境。
2. 仓库是有 .git 元数据的正式工作区。
3. CI 可生成 linux/amd64 和 linux/arm64 预编译产物。
4. Go 二进制不依赖 Python、Node.js 或现场 Go 工具链。
5. 网页静态资源已嵌入二进制。
6. Native 一键安装保留当前参数、secret 和安全校验。
7. Docker 使用 Go 构建产物并保留 UID/GID、挂载和安全选项。
8. systemd 单元不再包含 Python 或 PYTHONPATH。
9. 无 systemd 模式可启动、停止和识别 Go 进程。
10. 卸载器同时安全处理旧 Python 和新 Go 安装。
11. 升级失败能自动恢复旧服务。
12. 发布后能在一次服务切换内人工回滚 Python。
13. 回滚不要求重新登录、不覆盖当前 secret。
14. 源码清单和二进制产物清单均通过校验。
15. CI 包含 Go、Python参照、契约、前端、Docker 和 shell 门禁。
16. Native 与 Docker 的全新安装、升级、回滚和卸载矩阵通过。
17. README、deployment、deploy README、login、API、integration、contributing 和 changelog 已同步。
18. 最终用户在 VPS 上不需要 Go、Python 服务运行时或 Node.js。
