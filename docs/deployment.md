# Deployment

本文说明如何在常见 Linux VPS 上部署适配器。项目不需要第三方 Python 包；可以选择 Native 或 Docker。示例中的 `<vps-host>`、`<vps-user>` 和路径都要替换为自己的值。

## One-command install

下面两个脚本都可以通过一行命令启动。首次运行会通过当前终端询问适配器 Basic Auth 用户名/密码和监听端口；WPS 群组和根目录默认写入 `auto`，由登录助手从官方 WPS 当前页面地址识别。`[]` 中的值是默认值，直接回车即可使用。适配器密码不会出现在命令行参数中。服务默认使用执行 `sudo` 的当前用户，可以通过 `--run-user USER` 显式指定。

```bash
set -o pipefail; curl -fL --progress-bar --connect-timeout 10 --max-time 60 --retry 1 'https://gh-proxy.com/https://raw.githubusercontent.com/galiandan/WPS_2_WebDAV/main/scripts/install-native.sh' | sudo bash -s -- --port 18080
```

上面是 Native 安装。把最后的 `18080` 换成你想使用的端口即可。

Docker：

```bash
set -o pipefail; curl -fL --progress-bar --connect-timeout 10 --max-time 60 --retry 1 'https://gh-proxy.com/https://raw.githubusercontent.com/galiandan/WPS_2_WebDAV/main/scripts/install-docker.sh' | sudo bash -s -- --port 18080
```

安装脚本会从脚本内固定的 40 位 Git 提交归档下载代码，并校验归档内置的 SHA-256 文件清单，不要求 VPS 已安装 `git`；可用 `--source-ref` 和对应的 `--source-manifest-sha256` 指定另一个完整提交号。Native 会识别 `apt`、`dnf`、`yum`、`apk`、`pacman`、`zypper` 和 `xbps-install`，有 systemd 时注册服务，没有 systemd 时使用便携后台模式。Docker 会使用这些包管理器安装 Docker，并识别 systemd、OpenRC 和 SysV service。两种方式使用同一套 `/etc/wps-adapter/secrets/`，但同一台机器只能让一种方式占用某个端口。脚本会把服务进程和凭据文件设置为当前用户；若直接以 root 执行，root 就是当前用户。

如果是从原生切换到 Docker，需要显式确认停用原生服务：

```bash
set -o pipefail; curl -fL --progress-bar --connect-timeout 10 --max-time 60 --retry 1 'https://gh-proxy.com/https://raw.githubusercontent.com/galiandan/WPS_2_WebDAV/main/scripts/install-docker.sh' | sudo bash -s -- --port 18080 --replace-native
```

如果 `gh-proxy.com` 无法访问，把命令中的 `gh-proxy.com` 替换为 `ghfast.top`。安装器运行后会从固定提交归档下载项目，并自行校验文件清单。

建议先下载脚本检查内容，再执行；不要把未知来源的内容直接通过管道交给 root。国内加速节点只用于传输，项目归档会按固定清单校验。安装器内部的所有下载都有连接超时和总超时，并会在候选地址之间自动回退。

安装器会按 `[当前阶段/总阶段]` 输出进度。下载安装器和源码归档时会显示进度；Docker 会优先从国内镜像获取 Python 基础镜像，并在构建镜像时持续显示逐层构建输出。若某个地址无响应，会在超时后自动切换，不会无限卡住。

手动使用 Compose 且 Docker Hub 访问不稳定时，可在构建前指定镜像：

```bash
export WPS_ADAPTER_DOCKER_BASE_IMAGE=docker.m.daocloud.io/library/python:3.12-slim
```

手动使用 Docker Compose 时，`/etc/wps-adapter/wps-adapter.env` 只会注入容器，不能替代 Compose 的宿主端口变量。自定义端口时先执行：

```bash
export ADAPTER_BIND=0.0.0.0
export ADAPTER_PORT=18080
export WPS_ADAPTER_UID="$(id -u)"
export WPS_ADAPTER_GID="$(id -g)"
docker compose -f /opt/wps-adapter/deploy/docker-compose.yml up -d --build
```

## 1. Prepare the host

建议使用普通系统用户运行服务；一键脚本默认使用执行 `sudo` 的当前用户。公网部署优先配置 HTTPS 反向代理；没有域名或证书时也可以使用 HTTP，但只适合可信网络，因为认证信息和文件内容会明文传输。

确认主机满足：

- Native 模式需要 Python `3.11+`；安装器会尝试通过系统包管理器安装。Docker 模式不要求宿主机安装 Python。
- Bash、`tar`、`find`、`sha256sum`，以及 `curl` 或 `wget`。安装命令使用 `sudo bash`，极简系统如果没有 Bash，需要先按该系统方式安装 Bash。
- 安装器覆盖常见发行版的包管理器；未列出的定制发行版仍可能需要手工提供 Python/Docker 和服务管理方式。
- 能访问 WPS 和对象存储域名。
- 临时上传文件所在磁盘有足够空间。
- 默认只接受 WPS 返回的 `*.ag.kdocs.cn` 签名对象存储地址；如果你的企业区域返回了不同但可信的 WPS 对象存储后缀，再显式设置 `WPS_OBJECT_STORAGE_HOST_SUFFIX`。
- 单个文件夹默认最多读取 `10000` 个条目，避免异常大的目录耗尽 VPS 内存。
- 并发上传会共享临时盘预留预算；预算不足时返回 `507`，而不是继续占满磁盘。
- 单次上传默认不超过 1 GiB；如果确实需要更大的文件，先评估 VPS 临时盘空间，再设置 `WPS_MAX_UPLOAD_BYTES=0` 或更大的值。
- 适配器生成的单个 JSON/XML 响应默认不超过 16 MiB；超大目录响应会返回 `507`，避免目录元数据耗尽内存。
- 进程内 WebDAV 锁默认最多保留 4096 把，超过时返回 `503`，避免异常客户端无限堆积锁状态。

安装器从归档中读取 `release-manifest.txt`，并只接受清单中列出的普通文件。修改 `--source-ref` 时必须同时提供该提交对应的清单 SHA-256；不要随意复制其他版本的摘要。

## 2. Install the source

在 VPS 上将仓库放到 `/opt/wps-adapter`。例如：

```bash
sudo git clone https://github.com/galiandan/WPS_2_WebDAV.git /opt/wps-adapter
cd /opt/wps-adapter
PYTHONPATH=src python3 -m wps_adapter --version
```

升级时先备份 systemd 单元和非秘密配置，再更新代码。不要用仓库文件覆盖 `/etc/wps-adapter/secrets/`。手工安装 systemd 单元时，请把 `User=` 和 `Group=` 改为实际服务用户；一键安装脚本会自动完成这一步。

## 3. Create secret files

手工部署时先确定服务用户；下面示例使用当前登录用户。一键安装器会自动完成同样的所有者和权限设置：

```bash
SERVICE_USER="$(id -un)"
SERVICE_GROUP="$(id -gn)"
sudo install -d -o "$SERVICE_USER" -g "$SERVICE_GROUP" -m 700 /etc/wps-adapter/secrets
sudo install -o "$SERVICE_USER" -g "$SERVICE_GROUP" -m 600 /dev/null /etc/wps-adapter/secrets/wps-cookie
sudo install -o "$SERVICE_USER" -g "$SERVICE_GROUP" -m 600 /dev/null /etc/wps-adapter/secrets/wps-csrf
sudo install -o "$SERVICE_USER" -g "$SERVICE_GROUP" -m 600 /dev/null /etc/wps-adapter/secrets/wps-workspace.json
sudo install -o "$SERVICE_USER" -g "$SERVICE_GROUP" -m 600 /dev/null /etc/wps-adapter/secrets/adapter-username
sudo install -o "$SERVICE_USER" -g "$SERVICE_GROUP" -m 600 /dev/null /etc/wps-adapter/secrets/adapter-password
```

优先在账号所有者自己的电脑上运行 [`login.md`](login.md) 中的登录助手。配置 HTTPS 反向代理后，助手可以通过受 Basic Auth 保护的接口直接写入 `wps-cookie`、`wps-csrf` 和 `wps-workspace.json`；没有 HTTPS 时可以确认风险后使用 HTTP，或使用 SSH 备用方式。不需要把 Cookie 粘贴进命令行。

适配器 Basic Auth 的用户名和密码分别写入 `adapter-username`、`adapter-password`。这些文件只允许服务用户读取。不要把 WPS 密码、Cookie 或 Basic Auth 密码放入 `.env`、Git、Issue 或聊天。

## 4. Configure the service

```bash
sudo cp /opt/wps-adapter/.env.example /etc/wps-adapter/wps-adapter.env
sudo chmod 600 /etc/wps-adapter/wps-adapter.env
sudoedit /etc/wps-adapter/wps-adapter.env
```

至少设置：

```dotenv
WPS_GROUP_ID=auto
WPS_ROOT_ID=auto
WPS_WORKSPACE_FILE=/etc/wps-adapter/secrets/wps-workspace.json
WPS_COOKIE_FILE=/etc/wps-adapter/secrets/wps-cookie
WPS_CSRF_TOKEN_FILE=/etc/wps-adapter/secrets/wps-csrf
ADAPTER_USERNAME_FILE=/etc/wps-adapter/secrets/adapter-username
ADAPTER_PASSWORD_FILE=/etc/wps-adapter/secrets/adapter-password
ADAPTER_BIND=127.0.0.1
ADAPTER_PORT=18080
```

`ADAPTER_PORT` 可以改成任意未被占用的端口。`auto` 表示登录助手从当前官方 WPS 企业云盘地址识别企业和群组，并默认使用企业云盘根目录 `0`；只有登录助手显式使用 `--workspace-url` 时才会选择具体文件夹。也可以把两个变量改成固定 ID 做手工部署。低内存 VPS 建议保留模板中的并发、spool 和磁盘空间保护参数。

检查配置不会访问 WPS：

```bash
cd /opt/wps-adapter
set -a
. /etc/wps-adapter/wps-adapter.env
set +a
PYTHONPATH=src python3 -m wps_adapter check-config
```

## 5. Install service (Native manual deployment)

Native 一键安装器已经自动处理本节。只有手工部署或没有使用一键安装器时，才需要按下面的 systemd 步骤执行；没有 systemd 的系统应使用一键安装器的便携后台模式，并将日志写在 `/etc/wps-adapter/wps-adapter.log`。

```bash
sudo install -m 644 /opt/wps-adapter/deploy/wps-adapter.service \
  /etc/systemd/system/wps-adapter.service
sudo install -d -m 755 /etc/systemd/system/wps-adapter.service.d
sudo install -m 644 /opt/wps-adapter/deploy/wps-adapter-hardening.conf \
  /etc/systemd/system/wps-adapter.service.d/override.conf
sudo install -m 600 /opt/wps-adapter/deploy/wps-adapter-hardening.env \
  /etc/wps-adapter/wps-adapter-hardening.env
sudo systemctl daemon-reload
sudo systemctl enable --now wps-adapter
systemctl status wps-adapter --no-pager
curl http://127.0.0.1:18080/healthz
```

查看不包含 Cookie、Token、完整 URL 或文件内容的日志：

```bash
sudo journalctl -u wps-adapter -n 100 --no-pager
```

## 6. Reverse proxy

让反向代理终止 TLS，并将请求转发到 `http://127.0.0.1:<port>`。保留适配器 Basic Auth；不要在代理访问日志中记录 `Authorization` 头、查询参数或请求体。登录助手的 HTTPS 凭据导入也必须经过这条 TLS 入口。WebDAV 客户端使用：

```text
https://<vps-host>/dav/
```

如果只通过 SSH 隧道使用，可以保持服务绑定 `127.0.0.1`，无需开放公网端口。

## 7. Upgrade and rollback

升级前执行：

```bash
sudo cp /etc/systemd/system/wps-adapter.service \
  /etc/systemd/system/wps-adapter.service.before-upgrade
sudo cp /etc/wps-adapter/wps-adapter.env \
  /etc/wps-adapter/wps-adapter.env.before-upgrade
```

更新代码后重新安装 service 文件、执行 `systemctl daemon-reload` 和 `systemctl restart wps-adapter`，再检查 `/healthz`。回滚只恢复代码和非秘密配置；不要从 Git 或备份中恢复旧 Cookie。

## 8. Session expiry

服务遇到 WPS `401` 时会先检查 secret 是否被手动替换，然后尝试已确认的 `grant_token` 刷新流程并重试一次。若 `rtk` 已被撤销或 WPS 要求重新登录，在账号所有者自己的电脑上重新运行 [`login.md`](login.md) 的登录助手，通过 HTTPS、确认过风险的 HTTP 或 SSH 方式更新凭据。服务无需因凭据同步而重启。

## 9. Uninstall

Native 和 Docker 共用一个卸载脚本。默认会停止并删除适配器服务、应用代码和本项目管理的 Docker 容器，但会保留 `/etc/wps-adapter/wps-adapter.env` 以及 `/etc/wps-adapter/secrets/`，便于以后重新安装：

```bash
set -o pipefail; curl -fL --progress-bar --connect-timeout 10 --max-time 60 --retry 1 'https://gh-proxy.com/https://raw.githubusercontent.com/galiandan/WPS_2_WebDAV/main/scripts/uninstall.sh' | sudo bash -s --
```

如果确定不再保留本机配置和凭据，添加 `--purge`。如果还要删除本项目 Docker 镜像，添加 `--remove-image`：

```bash
set -o pipefail; curl -fL --progress-bar --connect-timeout 10 --max-time 60 --retry 1 'https://gh-proxy.com/https://raw.githubusercontent.com/galiandan/WPS_2_WebDAV/main/scripts/uninstall.sh' | sudo bash -s -- --purge --remove-image
```

脚本会要求输入 `YES` 确认；自动化执行时可以添加 `--yes`。卸载脚本不会删除 Docker 软件，也不会删除 WPS 云盘上的远端文件。如果 Docker daemon 当前不可用，脚本会拒绝执行，启动 Docker 后重新运行即可。
