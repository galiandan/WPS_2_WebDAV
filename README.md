# WPS 2 WebDAV

把你有权访问的 WPS 云盘接入 WebDAV，同时提供一个无需额外前端依赖的网页文件管理器和 REST 接口。

~~~text
WPS 云盘 -> Go 适配器 -> 网页 / WebDAV / REST
~~~

当前长期运行服务是 Go 单二进制。项目仍是实验性适配器，不是 WPS 官方软件。它只适用于你自己的账号和你有权限访问的数据。

## 你能得到什么

- 浏览器网页：多级目录树导航、浏览、搜索、排序、列表/网格视图、拖放上传、上传速度、图片/PDF 在线预览，以及纯文本在线浏览（TXT、日志、Markdown、JSON 等，支持中文编码切换）、下载、新建文本文件、新建文件夹、重命名、移动、复制和删除。
- 受限分享：为文件或目录创建带有效期、可选提取码的只读链接，随时撤销；访客页面独立于管理界面，不需要适配器账号。
- ZIP 浏览：查看压缩包目录并下载单个文件，使用有上限的 Range 读取，不解压到服务器目录。
- 多用户目录权限：管理员在设置中创建成员、选择独立目录并授予上传/删除权限；成员使用自己的网页或 WebDAV 账号，搜索、任务与登录安全设置相互隔离。
- 丰富预览：图片网格缩略图、音视频播放器、同目录播放列表、进度记忆和本地字幕；Markdown 渲染、代码高亮、原文切换及目录 README 说明均使用内嵌资源。
- 后台任务中心：批量复制/移动/删除提交给服务端执行，关闭网页后继续；支持逐项进度、取消后续项目、重试明确失败或未开始的项目。重启后标记中断，不自动重放。
- 离线下载：填写公网 HTTP(S) 文件地址和保存名称，由 VPS 下载后上传到当前 WPS 目录；关闭网页后继续，任务中心显示下载和上传进度，同名不覆盖。
- 后台 ZIP：选择文件/目录后提交“后台打包”，完成后从任务中心下载；生成结果保留 1 小时，原有即时打包下载仍可使用。
- 文本编辑：在文本预览中点击“编辑文件”，支持中文编码读取，以 UTF-8 保存；冲突或失败保留当前页面草稿。
- 批量管理：勾选文件或文件夹、Shift 连选，批量复制/移动/删除并逐项显示结果；支持所选文件和目录流式打包下载为 ZIP。
- 目录上传：选择或拖入文件夹，保留目录结构；拖放支持空目录，失败项目可重试，取消后不会继续创建后续目录。
- 全局文件搜索：按空间或所有已选空间建立文件名/路径索引，按文件类型筛选；显示索引范围、更新时间和未完成原因，不下载文件正文。
- WebDAV：Windows、Linux、macOS、手机、NAS、同步软件和其他 WebDAV 客户端。
- REST：脚本化列目录、上传、下载、创建文件夹、重命名、移动、复制、删除和状态检查。
- 多个 WPS 空间：登录后按实时显示的空间名称选择网页要显示的一个、多个或全部空间；再从其中选择唯一的 WebDAV 根目录。
- 个人 WPS 网盘：登录助手会自动识别个人账号，复用 OpenList 已验证的个人接口映射；个人端使用 `drive.wps.cn`，不需要手填空间 ID。
- 资源保护：上传/下载并发、临时磁盘、目录递归、响应大小和大目录读取都有上限。
- 不需要浏览器扩展；登录只需要一个独立的 wps_login.py 文件。

## 三步开始

### 1. 在 Linux VPS 安装

先准备一台能访问 WPS 的 Linux VPS，并放行你要使用的端口。下面以 54321 为例，端口可以替换成任意未占用端口。

Native（推荐，运行时不需要 Docker）：

~~~bash
set -o pipefail; curl -fL --progress-bar --connect-timeout 10 --max-time 120 'https://gh-proxy.com/https://raw.githubusercontent.com/galiandan/WPS_2_WebDAV/main/scripts/install-native.sh' | sudo bash -s -- --port 54321
~~~

Docker：

~~~bash
set -o pipefail; curl -fL --progress-bar --connect-timeout 10 --max-time 120 'https://gh-proxy.com/https://raw.githubusercontent.com/galiandan/WPS_2_WebDAV/main/scripts/install-docker.sh' | sudo bash -s -- --port 54321
~~~

安装器会显示阶段进度和下载进度，并在首次安装时询问 WebDAV、REST 和网页共用的 Basic Auth 用户名和密码。密码不会显示，请记住它；网页登录直接使用这组凭据。

Native 安装器不要求 VPS 预装 Go：它会先从国内加速的 GitHub Release 下载对应 Linux 架构的预编译静态二进制。只有预编译文件下载失败、无法执行或当前架构没有资产时，才会从源码现场编译；这时优先使用主机已有的 Go 1.25+，否则临时下载 Go 工具链，完成后删除。服务运行时只使用编译好的 Go 二进制，不需要 Python、Node.js 或 Go 运行时。

Docker 安装器也会先下载预编译二进制并制作最小运行镜像；只有二进制或运行镜像制作失败时，才从国内 Docker 镜像获取 Go 构建镜像并现场编译。最终容器只包含服务二进制和 CA 证书，你的个人电脑不需要安装 Docker。Buildx 可用时使用 Buildx 构建，旧发行版会自动使用兼容构建器。

安装器默认只访问命令中显示的国内加速地址；网络不可用时不会静默切换到其他地址。预编译资产不可用时会明确显示“回退到源码现场编译”，也不会执行项目归档、二进制或 Go 工具链的哈希校验。不要把未知网页中的安装命令直接交给 root。

安装完成后会打印实际端口、网页地址和 WebDAV 地址。服务默认使用执行 sudo 的当前用户运行，不会强制创建名为 wps-adapter 的 Linux 用户。

安装器会把配置、凭据、断点数据、日志和运行文件集中在部署目录：如果执行安装命令时当前目录是 `/`、`/root`、`/home` 或用户主目录，部署目录为 `/opt/wps-adapter`；在其他明确的工作目录执行时，部署目录就是当前目录。目录结构为 `config/`、`data/`、`logs/` 和 `runtime/`。systemd 注册单元仍位于系统规定的 `/etc/systemd/system/`。

预编译 Release 默认使用 `v1.6.0`。如果你维护自己的 Release 镜像，可在安装命令前设置 `WPS_ADAPTER_BINARY_BASE_URL`（目录地址，文件名由安装器追加）和 `WPS_ADAPTER_BINARY_RELEASE_TAG`。预编译资产名称为 `wps-adapter-linux-amd64`、`wps-adapter-linux-arm64` 等；当前没有对应资产时会自动进入源码回退路径。

### 2. 在自己的电脑登录 WPS

只下载一个登录脚本，不需要 clone 整个项目：

~~~bash
curl -fL --progress-bar --connect-timeout 10 --max-time 120 'https://gh-proxy.com/https://raw.githubusercontent.com/galiandan/WPS_2_WebDAV/main/wps_login.py' -o wps_login.py && python3 wps_login.py
~~~

电脑需要 Python 3.11+、Chrome 或 Chromium。脚本会询问：

1. VPS 地址或域名。
2. 连接方式：SSH 私钥或 SSH 密码。
3. SSH 用户、端口、私钥路径（私钥方式）和 VPS 部署目录。

随后脚本打开临时隔离的官方 WPS 登录窗口。只在这个官方窗口中完成登录、SSO、扫码、验证码或二次验证。登录完成后：

1. 脚本自动读取当前账号能看到的全部 WPS 空间。
2. 浏览器自动关闭，回到终端选择网页要显示的空间，例如输入 `1,2`，或输入 `all` 选择全部空间。
3. 脚本只让你从已选空间中选择一次 WebDAV 根目录所属空间和文件夹。例如选择 A 的 `web` 文件夹；输入 `0` 使用当前目录，输入序号进入子文件夹，输入 `b` 返回上一级；输入 `s` 或直接回车可跳过目录选择，之后在网页“设置 → WebDAV 存储位置”中选择。
4. 脚本验证 WebDAV 目标和所有网页空间，并把 Cookie、CSRF 和工作区配置安全同步到 VPS。

WPS 登录后自动恢复的旧文件夹不会被误当成目标目录。网页会把选中的空间显示为 `/A/`、`/B/` 等独立文件夹；WebDAV 只有一个根目录，例如 `/dav/` 映射到 `/A/web/`，不会把 B 暴露到 WebDAV。跳过目录时先映射所选 WebDAV 空间的根目录，网页选择器随后可以把 `/dav/` 切换到 A 或 B 中的任意文件夹。切换位置不会移动 WPS 文件，只会改变 `/dav/` 的映射。脚本不会显示 Cookie、CSRF、密码或签名 URL，也不需要手动填写空间 ID、群组 ID 或文件夹 ID。

个人 WPS 网盘使用同样的空间选择和 WebDAV 映射流程。登录后脚本通过 WPS 账号状态接口选择对应接口，再使用个人端 `/api/v3/groups` 获取空间名称；目录、上传、下载、复制和文件登记使用个人端对应路径，移动和删除使用个人端 `/api/v3/groups/<group>/files/batch/move`、`batch/delete`。工作区文件会额外保存 `mode: personal`，服务重启后不会误切回另一套接口。当前个人接口主要依据 OpenList 的公开 WPS 驱动实现，首次接入本人账号时应先用测试目录完成读写验收。

### 3. 打开网页或连接 WebDAV

浏览器：

~~~text
http://<VPS地址>:54321/
~~~

网页打开后会在后台检查项目 Release。正常时，云盘名称下方显示灰色版本号胶囊；发现新版本时版本号会变黄。点击版本号即可打开版本详情，查看当前版本、最新版本和发布页，并可重新检查或立即更新。点击更新后服务会下载当前 Linux 架构的预编译二进制，检查版本后自动替换并重启。配置、Cookie、工作区选择和 WPS 云端文件不会被修改，更新期间不要重复点击按钮。更新检查失败不会影响文件浏览。更新默认使用国内加速地址，也可以通过 `WPS_ADAPTER_UPDATE_API_URL` 和 `WPS_ADAPTER_UPDATE_BASE_URL` 指向你自己的 HTTPS Release 镜像。

WebDAV：

~~~text
http://<VPS地址>:54321/dav/
~~~

网页现在使用内置登录页面，不会再弹出浏览器原生的用户名密码窗口。直接使用安装时设置的适配器用户名和密码登录；这也是 WebDAV 客户端使用的同一组凭据。登录后的网页会话使用 HttpOnly Cookie，服务重启后需要重新登录。

网页支持可选的两步验证和 Passkey。登录网页后打开“设置 → 登录安全”：

- 两步验证使用兼容 TOTP 的验证器应用。启用时先把密钥加入验证器，再输入当前验证码；页面只显示一次恢复码，请离线保存。以后密码登录需要验证码或未使用过的恢复码。
- Passkey 使用浏览器原生 WebAuthn，可添加多个设备并单独删除。启用后登录页会出现“使用 Passkey 登录”。Passkey 通常要求 HTTPS；直接使用 IP 的 HTTP 页面可能被浏览器拒绝，这是浏览器安全策略。
- 两种方式都是可选的，互不强制；Passkey 登录不会改变 WebDAV 客户端仍使用 Basic Auth 的事实。

2FA 和 Passkey 的状态保存在 `/opt/wps-adapter/config/secrets/auth-settings.json`，服务重启后仍然有效。卸载脚本会连同该文件一起删除。不要复制、提交或公开这个文件。

WebDAV 客户端仍使用安装时设置的 Basic Auth 用户名和密码，这是为了兼容 Windows、手机、NAS 和同步软件。自定义端口时，把地址中的 54321 换成实际端口。服务显示 WPS 未连接时，表示适配器进程正常但 WPS 凭据尚未同步、已过期或当前空间无权访问；重新运行 wps_login.py 即可。

修改适配器账号时，直接替换 `/opt/wps-adapter/config/secrets/adapter-username` 和 `/opt/wps-adapter/config/secrets/adapter-password`，网页登录和 WebDAV 会同时使用新凭据，无需重启服务。

手工部署个人 WPS 时可设置 `WPS_MODE=personal`；正常使用登录助手时保持 `WPS_MODE=auto`，由助手写入的 `wps-workspace.json` 自动决定账号类型。

## 登录助手的 SSH 同步

登录助手只允许通过 SSH 私钥或 SSH 密码把凭据写入 `/opt/wps-adapter/config/secrets/`。它不接受适配器 URL、HTTP、HTTPS 或本地输出目录参数，因此 Cookie、CSRF 和工作区数据不会经过 WebDAV/REST 端口传输。

直接运行即可按提示选择：

~~~bash
python3 wps_login.py
~~~

也可以直接使用 SSH 参数：

~~~bash
python3 wps_login.py \
  --ssh-target <vps-user>@<vps-host> \
  --ssh-identity ~/.ssh/id_ed25519 \
  --remote-dir /opt/wps-adapter
~~~

密码登录时使用 `--ssh-password-auth`，SSH 会在传输凭据时安全地提示密码。同步成功后不需要重启服务。

## 服务状态和常用检查

健康检查只表示 Go 进程正在运行，不代表 WPS 已登录：

~~~bash
curl 'http://<VPS地址>:54321/healthz'
~~~

查看 WPS 会话状态：

~~~bash
curl -u <Basic Auth用户名> 'http://<VPS地址>:54321/api/v1/status'
~~~

常见状态：

- connected：WPS 凭据和目标空间可访问。
- not_configured：还没有同步 Cookie、CSRF 或工作区配置。
- session_expired：会话过期，重新运行登录助手。
- permission_denied：当前账号无法访问所选空间或根目录。
- upstream_unavailable：WPS 或对象存储暂时不可达。

服务遇到 WPS 401 时会尝试已确认的 rtk/grant_token 自动续期流程并重试一次。WPS 撤销刷新凭据或改变登录策略后，仍需重新登录。

网页目录不会先等待状态接口；能成功显示目录就说明当前目录请求可用。状态接口只在后台低频检查，偶发网络抖动不会立即把正在使用的页面判定为不可用。WPS 的只读 GET 请求遇到短暂网络错误或 408、502、503、504 时会自动重试一次，写入请求不会重复执行。

## HTTP、HTTPS 和端口

适配器仍支持 HTTP 和 HTTPS 作为网页、REST、WebDAV 的访问协议；HTTP 会明文传输 Basic Auth、WPS 会话和文件内容，不适合直接暴露到公网。登录助手不通过这两个协议同步凭据，只使用 SSH。

有域名时，建议使用 Caddy、Nginx 或其他反向代理提供 HTTPS，并把请求转发到 http://127.0.0.1:<端口>。WebDAV 客户端使用：

~~~text
https://<你的域名>/dav/
~~~

安装器支持自定义端口。例如：

~~~bash
set -o pipefail; curl -fL --progress-bar --connect-timeout 10 --max-time 120 'https://gh-proxy.com/https://raw.githubusercontent.com/galiandan/WPS_2_WebDAV/main/scripts/install-native.sh' | sudo bash -s -- --port 18080
~~~

登录助手中的端口也必须填写 18080，不能继续使用默认的 54321。

## 手工运行和开发

生产服务代码在 go/；前端是 Go embed 的原生 HTML、CSS 和 JavaScript，没有 Node.js 构建步骤。文件浏览器布局移植自 OpenList-Frontend（MIT），将上游 Solid/Hope UI 组件的布局属性转换为原生 DOM/CSS，并对接本项目接口。原始组件、许可证和移植说明见 [go/web/upstream/openlist](go/web/upstream/openlist/README.md)。

本机开发需要 Go 1.25+。国内 Go 模块镜像：

~~~bash
export GOPROXY=https://goproxy.cn,direct
~~~

构建和测试：

~~~bash
cd go
gofmt -w .
go test ./...
go vet ./...
CGO_ENABLED=0 go build -trimpath -o /tmp/wps-adapter ./cmd/wps-adapter
~~~

如果需要从源码包开始，使用国内 GitHub 加速地址下载 main 源码归档，再按 go/README.md 构建。普通用户不需要执行这些步骤。

## 卸载

卸载脚本会自动识别 Native 和 Docker，删除服务、程序、本机配置、Basic Auth、Cookie、CSRF 和工作区文件：

~~~bash
set -o pipefail; curl -fL --progress-bar --connect-timeout 10 --max-time 120 'https://gh-proxy.com/https://raw.githubusercontent.com/galiandan/WPS_2_WebDAV/main/scripts/uninstall.sh' | sudo bash -s --
~~~

如果还安装过 Docker，并希望同时删除本项目镜像：

~~~bash
set -o pipefail; curl -fL --progress-bar --connect-timeout 10 --max-time 120 'https://gh-proxy.com/https://raw.githubusercontent.com/galiandan/WPS_2_WebDAV/main/scripts/uninstall.sh' | sudo bash -s -- --remove-image
~~~

卸载不会删除 Docker 软件，也不会删除 WPS 云盘中的远端文件。没有 Docker 时会自动跳过容器和镜像清理。脚本会要求输入 YES；自动化场景可额外添加 --yes。

## 项目目录

~~~text
go/              Go 生产服务、嵌入式网页和 Go 测试
scripts/         Native、Docker 和卸载脚本
deploy/          systemd 加固文件和 Docker 构建配置
wps_login.py     独立 WPS 登录/凭据同步助手
docs/            使用、接口、架构、部署和研究文档
contract_tests/  脱敏 JSON 契约金标准，供 Go 回归测试读取
~~~

## 当前限制

- 离线下载和后台 ZIP 必须登录适配器账号；离线下载还需上传权限。两者共享独立的传输队列：单个工作线程、最多 16 个排队/执行任务、最多 100 条历史；单任务最多 1 GiB、设 30 分钟执行预算（已进入 WPS 上传的在途请求可能在预算到期后继续，需核对结果），传输临时目录总量最多 2 GiB。离线下载同时受 `WPS_MAX_UPLOAD_BYTES` 限制，设置为 `0` 也不会解除传输任务的 1 GiB 上限。
- 离线下载只接收公网 HTTP(S) 地址，逐次校验 DNS 与重定向并连接校验后的 IP，不接受 URL 中的账号密码或自定义认证头。进入 WPS 上传阶段后不能取消或重试；失败、中断或超时后先检查目标目录。服务重启不会自动继续未完成任务。
- 传输记录默认为网页设置同目录的 `transfers.json`（`WPS_TRANSFERS_FILE`），以 `0600` 私有文件保存。源 URL 可能含查询参数凭据，只在该任务记录中保留供受控重试，不出现在公共任务状态、服务日志或浏览器存储。临时文件位于上传 spool 下的 `transfers/`，可用 `WPS_TRANSFER_DATA_DIR` 指定私有目录；ZIP 结果生成后 1 小时失效，过期后需重新打包。

- 分享默认 7 天、最长 30 天，支持可选提取码；每位用户当前策略最多 100 条有效分享，全局最多 1000 条记录。链接密钥只在创建时显示，不保留明文；撤销、所有者权限/密码变化或目标替换后不可继续使用。正常 WPS Cookie 续期不影响资源绑定。
- 分享访客授权 Cookie 最多有效 1 小时或直到分享过期，服务重启后需用原链接重新打开。分享只提供读取，不允许上传、修改、搜索其他目录或调用管理接口。
- ZIP 浏览支持 Store/Deflate 和 UTF-8 文件名，不支持加密、多卷或链接条目；单文件解压下载最多 128 MiB，需保留 `WPS_ENABLE_RANGE=true`。压缩比、目录大小、请求数和执行时长均有上限。

- 最多 32 个成员，安装时的管理员账号继续由配置文件管理。成员均有读取权限，可另授上传/删除；覆盖、移动和重命名需要同时具备上传和删除权限。
- 成员目录绑定实际 WPS 空间、接口环境和目录 ID。目录被替换、空间被重新映射时需管理员重新选择；Cookie 正常续期不影响目录授权。权限或密码修改会使旧网页会话失效，已开始的远端操作可能继续完成。
- 用户在各类后台队列中只能查看当前账号、当前目录策略下的记录；修改目录/权限后不会显示旧策略任务，排队项目执行前会再次验证。搜索最多缓存 8 个成员服务，每个成员索引最多 5000 项/4 MiB，所有账号同时最多运行一次索引扫描。

- 音视频仅支持浏览器能解码的格式，无服务端转码。播放进度保存在当前浏览器；本地 VTT/SRT 字幕不上传 WPS。
- 缩略图支持 JPEG、PNG、GIF 首帧；源文件最多 8 MiB、800 万像素、单边 8192 像素，超限显示原文件图标。服务端只同时生成一张缩略图。
- Markdown/代码富文本最多 256 KiB、10000 个元素，使用可中止的后台渲染，复杂内容自动回退原文。相对图片只读取适配器内已有安全图片；远程图片不自动加载，HTML 源码按文字显示。

- 后台复制/移动/删除每次最多 100 项、同时最多 16 个排队/执行任务，单个工作线程执行；保存最近 100 条记录。进度按所选项目计数，单个文件夹内部不显示字节进度。取消不会回滚已经完成的项目；当前项目可能继续完成，后续项目停止。记录无法落盘时停止启动新任务。
- 复制/移动/删除记录默认存于网页设置同目录的 `tasks.json`（可通过 `WPS_TASKS_FILE` 指定），权限为 0600。记录含文件路径和结果，不含 WPS Cookie 或签名 URL。账号、工作区、源文件或目标目录变化会拒绝旧任务；不确定是否已生效的项目必须先核对远端结果。
- 文本编辑最多 2 MiB。保存前重新读取完整内容并核对版本，保存后复读确认；服务重启或登录凭据变化后需重新读取。WPS 覆盖接口没有原子条件写入能力，外部客户端恰好在检查与写入之间修改仍可能竞争。新草稿只保留在当前页面。

- 全局搜索索引保存在内存，重启、适配器内文件写入或登录凭据/工作区变化后需重新建立；WPS 官网等外部变更需手动更新索引。单次扫描最多 50000 项、遍历 5000 个目录、64 层、32 MiB 元数据和约 10 分钟（当前目录请求结束后检查时限），遇到上限或读取失败会明确标记结果不完整。
- 批量复制/移动沿用当前存储边界，不支持跨空间移动或复制，也不会覆盖同名目标；每次最多选择 100 项。
- 即时 ZIP 每次最多选择 100 项，递归最多 10000 项、64 层和 10 GiB 文件数据。下载中断或源文件变化时需重新下载，不支持流式 ZIP 断点续传。后台 ZIP 沿用选择与遍历上限，但生成结果必须符合更小的 1 GiB 传输任务上限，ZIP 目录等开销也计入大小。
- 文件夹上传保留结构并对已有目录请求合并确认；文件同名处理沿用上传队列。目录选择器能否枚举空目录取决于浏览器，拖放在支持目录枚举的浏览器中保留空目录。单次最多 10000 个文件/文件夹。

- WPS 私有接口可能变化，项目不承诺长期兼容；个人端路径和字段参考了 OpenList WPS 驱动，并通过独立的个人模式测试覆盖。
- 上传请求需要 Content-Length，暂不接受 HTTP chunked request body。
- 大文件失败后会在当前请求内有限重试；跨进程断点恢复仍属于实验性能力。
- 文件夹 COPY 使用流式中继；LOCK 是当前进程内的兼容锁，服务重启后失效。
- 网页可以同时显示多个 WPS 空间；WebDAV 始终只有一个根目录，并且可以映射到已选空间中的任意一个文件夹。
- 网页目录切换优先立即显示浏览器缓存，并在后台刷新最新内容；首次访问没有缓存时仍会显示加载状态。
- 网页一键更新依赖可访问的 HTTPS Release 镜像；如果当前部署目录不可写或没有对应架构的 Release 资产，服务会保留当前版本并显示失败原因。
- 递归 PROPFIND、递归 COPY、上传和下载都受深度、条目、并发和磁盘预算限制。

## 安全

不要把以下内容提交到 GitHub、Issue、聊天或日志：

- WPS Cookie、rtk、CSRF、refresh token；
- WebDAV/REST Basic Auth 密码；
- 签名对象存储 URL；
- 原始 HAR、PCAP 和真实文件内容。

详见 SECURITY.md、docs/login.md 和 docs/deployment.md。项目采用 GNU GPL v3，见 LICENSE。
