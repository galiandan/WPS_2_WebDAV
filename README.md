# WPS 2 WebDAV

将你自己有权访问的 WPS 企业云盘映射为 WebDAV、REST API 和网页文件管理器。

```text
WPS 企业云盘 -> WPS 2 WebDAV -> WebDAV / REST / 网页
```

当前版本：`0.9.7`，项目仍处于实验性阶段。

## 项目定位

WPS 相关接口不是公开稳定 API。本项目只根据本人账号的真实网页请求进行适配，不是 WPS 官方客户端，也不保证 WPS 服务变更后仍然兼容。

项目的设计目标是：

- VPS 不长期保存文件，上传和下载尽量直接流经 WPS。
- 不要求用户安装浏览器扩展、Playwright 或项目依赖。
- 通过独立的 `wps_login.py` 在用户自己的电脑上完成 WPS 登录。
- 适配器保留 WPS `rtk`，在上游返回 `401` 时按已观察的流程自动续期。
- 用 WebDAV 兼容 Windows、Linux、手机、NAS 和同步工具。

## 已实现功能

- 文件列表和文件夹浏览
- 创建文件夹
- 上传、覆盖上传和下载
- 删除、重命名和移动
- 50 MiB 以上文件自动使用分片上传
- 流式下载和单范围 `Range` 下载
- WebDAV `PROPFIND`、`GET`、`HEAD`、`PUT`、`MKCOL`、`DELETE`、`MOVE`、`COPY`、`LOCK`、`UNLOCK`
- `Depth: 0`、`1`、`infinity`，带条目数和深度保护
- 网页拖动上传和上传速度显示
- 网页内直接修改云盘显示名称，所有客户端统一生效
- Native 和 Docker 两种部署方式
- 自定义监听端口
- 一个统一的 Native/Docker 卸载脚本

当前 WebDAV 限制：`COPY` 通过适配器执行下载再上传，不是 WPS 服务端复制；`MOVE` 和 `COPY` 暂不覆盖已有目标；锁只在当前适配器进程内有效。

## 安全边界

只使用自己的 WPS 账号和自己有权限访问的企业空间。项目不会绕过权限、SSO、验证码、风控或租户隔离。

以下内容绝不能提交到 GitHub、Issue、聊天或日志：

- WPS Cookie、`rtk` 和 CSRF
- 适配器 Basic Auth 密码
- 签名对象存储 URL
- 原始 HAR、PCAP 或真实文件内容

公网部署优先使用 HTTPS。没有域名或证书时也支持 HTTP，但 Cookie、Basic Auth 和文件内容都会明文传输，只适合可信网络。

## 快速开始

### 1. 部署 VPS

VPS 需要常见 Linux 发行版、root 或 `sudo` 权限、一个未被占用的端口。Native 安装器支持常见的 Debian、RHEL、Alpine、Arch、openSUSE 和 Void 系发行版；没有 systemd 时会使用便携后台模式。

下面两种方式只选一种。

Native：

```bash
set -o pipefail; curl -fL --progress-bar --connect-timeout 10 --max-time 60 --retry 1 'https://gh-proxy.com/https://raw.githubusercontent.com/galiandan/WPS_2_WebDAV/main/scripts/install-native.sh' | sudo bash -s -- --port 54321
```

Docker：

```bash
set -o pipefail; curl -fL --progress-bar --connect-timeout 10 --max-time 60 --retry 1 'https://gh-proxy.com/https://raw.githubusercontent.com/galiandan/WPS_2_WebDAV/main/scripts/install-docker.sh' | sudo bash -s -- --port 54321
```

自定义端口时，把最后的 `54321` 改成目标端口，例如 `--port 18080`。如果 `gh-proxy.com` 无法访问，将 URL 中的 `gh-proxy.com` 替换为 `ghfast.top`。

安装器会显示阶段进度和下载进度，并校验固定版本的文件清单。首次安装会询问适配器 Basic Auth 用户名和密码；密码不会显示。安装器默认使用执行 `sudo` 的当前用户运行服务。

Docker 安装器如果检测到正在运行的 Native 服务，会拒绝覆盖。确认切换时：

```bash
set -o pipefail; curl -fL --progress-bar --connect-timeout 10 --max-time 60 --retry 1 'https://gh-proxy.com/https://raw.githubusercontent.com/galiandan/WPS_2_WebDAV/main/scripts/install-docker.sh' | sudo bash -s -- --port 54321 --replace-native
```

### 2. 登录并同步 WPS

服务端不需要安装 Chrome。登录助手只需要在你自己的电脑上运行：Python `3.11+`、Chrome/Chromium，以及选择 SSH 方式时的系统 `ssh` 命令。

下载并运行独立脚本：

```bash
curl -fL --progress-bar --connect-timeout 10 --max-time 60 --retry 1 'https://gh-proxy.com/https://raw.githubusercontent.com/galiandan/WPS_2_WebDAV/main/wps_login.py' -o wps_login.py && python3 wps_login.py
```

向导会询问 VPS 地址、SSH 或 HTTP/HTTPS 连接方式、端口和适配器账号。随后会打开一个临时隔离的 Chrome 窗口：

1. 只在官方 WPS 页面登录自己的账号。
2. 完成学校 SSO、扫码、验证码或二次验证。
3. 默认不需要手动切换文件夹。WPS 可能自动恢复上次打开的文件夹，即使该页面显示“无权访问”，脚本也会忽略这个旧文件夹。
4. 脚本从当前官方页面读取企业和群组上下文，并默认保存企业云盘根目录 `root_id=0`。
5. Cookie、CSRF 和工作区信息会自动同步到 VPS，服务无需重启。

脚本不会打印 Cookie，也不会把 Cookie 放进命令参数、URL 或日志。

没有域名或证书时可以使用 HTTP，但必须明确允许明文传输：

```bash
python3 wps_login.py \
  --adapter-url http://<VPS-IP>:54321 \
  --adapter-user <适配器用户名> \
  --allow-http
```

有权限的子文件夹可以通过 `--workspace-url` 指定。地址必须是你从 WPS 页面复制的具体文件夹地址：

```bash
python3 wps_login.py \
  --workspace-url 'https://365.kdocs.cn/space/<企业ID>/<群组ID>/<文件夹ID>' \
  --adapter-url https://<适配器地址> \
  --adapter-user <适配器用户名>
```

指定后，脚本会直接打开并校验这个文件夹；如果登录后跳到了其他位置，不会悄悄选择别的目录。

### 3. 访问服务

安装完成后，端口以安装器输出为准：

```text
网页：   http://<VPS-IP>:54321/
WebDAV： http://<VPS-IP>:54321/dav/
REST：   http://<VPS-IP>:54321/api/v1/
健康检查：http://<VPS-IP>:54321/healthz
```

网页、WebDAV 和 REST 共用适配器 Basic Auth。

先验证连接：

```bash
curl -u <适配器用户名> \
  'http://<VPS-IP>:54321/api/v1/entries?path=/'
```

`curl` 会隐藏询问密码。若返回 `WPS 未连接` 或上游 `401`，先重新运行 `wps_login.py`；新凭据写入后不需要重启服务。

## 网页文件管理器

浏览器打开：

```text
http://<VPS-IP>:<端口>/
```

登录后可以浏览目录、进入文件夹、拖动上传、查看上传进度和速度、下载、创建文件夹、重命名、移动和删除。点击右上角的齿轮按钮即可修改云盘显示名称，不需要命令行；保存后会立即更新当前页面，并在服务重启后继续保留。

## WebDAV

WebDAV 地址：

```text
http(s)://<服务器地址>:<端口>/dav/
```

在 Windows、Linux、手机或 NAS 客户端中选择 WebDAV，填写该地址以及适配器 Basic Auth 账号密码。建议先连接测试目录，再接入正式数据。

常用方法：

| 方法 | 作用 |
| --- | --- |
| `PROPFIND` | 列目录和读取属性 |
| `GET` / `HEAD` | 下载文件和读取元数据 |
| `PUT` | 上传或覆盖文件 |
| `MKCOL` | 创建文件夹 |
| `DELETE` | 删除文件或文件夹 |
| `MOVE` | 重命名或移动 |
| `COPY` | 复制文件或文件夹 |
| `LOCK` / `UNLOCK` | 进程内写锁兼容 |

## REST 示例

所有远端路径都以 `/` 开头：

```text
GET    /api/v1/entries?path=/
GET    /api/v1/metadata?path=/folder/file.txt
GET    /api/v1/download?path=/folder/file.txt
PUT    /api/v1/upload?path=/folder/file.txt
POST   /api/v1/folders?path=/folder
PATCH  /api/v1/entries?path=/folder/file.txt
DELETE /api/v1/entries?path=/folder/file.txt
```

上传：

```bash
curl -u <适配器用户名> \
  -H 'Content-Type: application/octet-stream' \
  --upload-file ./local-file.bin \
  'http://<VPS-IP>:54321/api/v1/upload?path=/remote-file.bin'
```

下载：

```bash
curl -u <适配器用户名> \
  -o ./local-file.bin \
  'http://<VPS-IP>:54321/api/v1/download?path=/remote-file.bin'
```

重命名：

```bash
curl -u <适配器用户名> \
  -X PATCH \
  -H 'Content-Type: application/json' \
  --data '{"name":"new-name.txt"}' \
  'http://<VPS-IP>:54321/api/v1/entries?path=/old-name.txt'
```

完整接口、状态码和 WebDAV 头部说明见 [`docs/api.md`](docs/api.md)。

## 卸载

统一卸载脚本会同时检查 Native 和 Docker 安装。默认删除服务、应用代码和本项目管理的 Docker 容器，但保留配置及凭据，方便以后重新安装：

```bash
set -o pipefail; curl -fL --progress-bar --connect-timeout 10 --max-time 60 --retry 1 'https://gh-proxy.com/https://raw.githubusercontent.com/galiandan/WPS_2_WebDAV/main/scripts/uninstall.sh' | sudo bash -s --
```

删除配置、Cookie、CSRF、Basic Auth 和工作区文件时，明确添加 `--purge`：

```bash
set -o pipefail; curl -fL --progress-bar --connect-timeout 10 --max-time 60 --retry 1 'https://gh-proxy.com/https://raw.githubusercontent.com/galiandan/WPS_2_WebDAV/main/scripts/uninstall.sh' | sudo bash -s -- --purge
```

卸载 Docker 镜像需要额外添加 `--remove-image`。自动化执行可以添加 `--yes`，但 `--purge --yes` 会不可恢复地删除本机凭据，请确认目标服务器后再使用。

卸载脚本不会删除 Docker 软件本身，也不会删除 WPS 云盘上的远端文件。

## 配置

安装器会生成下面的文件；网页首次保存云盘名称后，还会生成 `web-settings.json`：

```text
/etc/wps-adapter/wps-adapter.env
/etc/wps-adapter/secrets/wps-cookie
/etc/wps-adapter/secrets/wps-csrf
/etc/wps-adapter/secrets/wps-workspace.json
/etc/wps-adapter/secrets/web-settings.json
/etc/wps-adapter/secrets/adapter-username
/etc/wps-adapter/secrets/adapter-password
```

常用变量：

| 变量 | 默认值 | 作用 |
| --- | --- | --- |
| `WPS_GROUP_ID` | `auto` | 登录助手自动识别企业群组 |
| `WPS_ROOT_ID` | `auto` | `auto` 使用工作区文件，默认根目录为 `0` |
| `WPS_ROOT_NAME` | `WPS Enterprise Drive` | 网页首次使用时的默认显示名称；网页修改后以页面保存值为准 |
| `WPS_WORKSPACE_FILE` | `/etc/wps-adapter/secrets/wps-workspace.json` | 群组和根目录选择 |
| `WPS_AUTO_REFRESH` | `true` | 上游 `401` 后自动续期 |
| `WPS_MULTIPART_THRESHOLD` | `52428800` | 分片上传阈值，单位字节 |
| `WPS_MAX_UPLOAD_BYTES` | `1073741824` | 单次上传上限，`0` 表示不限制 |
| `WPS_MAX_UPLOADS` | `2` | 并发上传上限 |
| `WPS_MAX_DOWNLOADS` | `4` | 并发下载上限 |
| `ADAPTER_BIND` | 安装器默认 `0.0.0.0` | 监听地址 |
| `ADAPTER_PORT` | `54321` | 监听端口 |
| `ADAPTER_USERNAME_FILE` | `/etc/wps-adapter/secrets/adapter-username` | Basic Auth 用户名文件 |
| `ADAPTER_PASSWORD_FILE` | `/etc/wps-adapter/secrets/adapter-password` | Basic Auth 密码文件 |

完整配置模板见 [`.env.example`](.env.example)。不要把密钥直接写入公开配置或 shell 历史。

最简单的修改方式是打开网页后点击右上角齿轮，输入新的云盘名称并点击“保存”。名称会写入 `/etc/wps-adapter/secrets/web-settings.json`，因此对其他浏览器、WebDAV 和 REST 根目录元数据也保持一致。这个操作不修改 WPS 远端文件夹名称。

如果还没有打开网页，也可以把 `WPS_ROOT_NAME` 作为首次默认名称写入配置：

```dotenv
WPS_ROOT_NAME="我的学校云盘"
```

网页保存的名称优先于 `WPS_ROOT_NAME`；删除网页设置文件后才会回退到配置中的默认值。

## 当前限制

- WPS 私有接口可能随时变化，项目不承诺长期兼容。
- 上传需要 `Content-Length`，暂不接受 HTTP chunked request body。
- 大文件失败后只在当前请求内有限重试，进程退出后的跨请求续传尚未实现。
- `COPY` 会经过 VPS 中继，速度和临时空间取决于 VPS 与 WPS 的网络。
- `LOCK` 是进程内兼容锁，服务重启后失效。
- WPS 撤销 `rtk`、要求重新登录或改变登录策略时，需要重新运行登录助手。

## 文档与开发

- [`docs/deployment.md`](docs/deployment.md)：部署、升级、回滚和服务管理
- [`docs/login.md`](docs/login.md)：登录助手、HTTP/HTTPS 和 SSH 同步
- [`docs/api.md`](docs/api.md)：REST、WebDAV 和状态码
- [`docs/integration.md`](docs/integration.md)：Windows、NAS 和验收流程
- [`docs/architecture.md`](docs/architecture.md)：组件和数据流
- [`docs/research/`](docs/research/)：抓包记录、实验结论和安全边界
- [`SECURITY.md`](SECURITY.md)：安全问题报告

运行测试：

```bash
PYTHONPATH=src python3 -m unittest discover -s tests -v
python3 -m compileall -q src tests
python3 tools/build_login_script.py --check
python3 tools/build_release_manifest.py --check
git diff --check
```

项目不依赖第三方 Python 包。真实 WPS 实验必须只使用自己的测试目录；原始 HAR 不要提交到仓库。

## License

本项目采用 [GNU General Public License v3.0 or later (GPL-3.0-or-later)](LICENSE) 发布。WPS 商标、服务和接口归其各自权利人所有；本项目不代表 WPS 官方立场。
