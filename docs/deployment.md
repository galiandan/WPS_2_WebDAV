# VPS 部署

当前 VPS `<vps-host>:54321` 已运行 `0.3.0`。本页保留可重复执行的安装/升级步骤；重复部署前先备份 systemd 单元和非秘密配置，并且不要覆盖 `/etc/wps-adapter/secrets/`。

程序只用 Python 标准库，不需要 Docker 或额外 Python 包。下面以 Debian/Ubuntu 风格系统和 `/opt/wps-adapter` 为例；命令中的路径可以按实际目录调整。

## 1. 准备目录和代码

把整个项目目录放到 VPS 的 `/opt/wps-adapter`，并确认以下命令能运行：

```bash
cd /opt/wps-adapter
PYTHONPATH=src python3 -m wps_adapter --version
```

本次部署按当前配置直接由 root 运行 systemd 服务。这样最省事，但隔离性较弱；以后可以把 service 文件中的 `User=root` 改回专用低权限用户。

## 2. 创建 secret 文件

使用 root 创建目录和文件。Cookie 文件写入浏览器当前会话的完整 Cookie 行，必须包含 `rtk`、`kso_sid`、`wps_sid` 等本人会话 Cookie；`rtk` 的浏览器路径是 `/passport/secure`，所以普通云盘列表请求复制出来的 Cookie 可能不包含它。CSRF 文件写入 `csrf` Cookie 的值；如果不单独创建 CSRF 文件，程序也会尝试从 Cookie 行中提取名为 `csrf` 的值。

```bash
sudo install -d -o root -g root -m 700 /etc/wps-adapter/secrets
sudo install -o root -g root -m 600 /dev/null /etc/wps-adapter/secrets/wps-cookie
sudo install -o root -g root -m 600 /dev/null /etc/wps-adapter/secrets/wps-csrf
sudo install -o root -g root -m 600 /dev/null /etc/wps-adapter/secrets/adapter-username
sudo install -o root -g root -m 600 /dev/null /etc/wps-adapter/secrets/adapter-password
```

把值输入这些文件时不要放进 shell 历史，也不要发到聊天或 Git。首次完整 Cookie 初始化后，服务会在上游 `401` 时调用已确认的 WPS `POST /passport/secure/api/grant_token` 刷新流程，并把返回的 Set-Cookie 原子保存回文件；如果 `rtk` 已失效，则需要重新建立本人浏览器会话。

如果未来需要接入企业自有的会话建立脚本，仍可以把它的绝对路径写入 `WPS_CREDENTIAL_REFRESH_COMMAND` 作为 WPS 刷新失败后的外部兜底。服务只会在上游 `401` 时调用一次该命令，标准输出和错误输出会被丢弃；命令必须由 root 管理，并以临时文件加重命名的方式原子替换两个 secret。当前不会自动登录、绕过 SSO 或处理验证码。

service 单元对 `/etc/wps-adapter/secrets` 保留了写权限，专门用于上述 root 管理的刷新助手原子替换凭据；如果未配置刷新助手，适配器本身不会主动写入这些文件。

## 3. 配置非秘密环境变量

```bash
sudo cp /opt/wps-adapter/.env.example /etc/wps-adapter/wps-adapter.env
sudo chmod 600 /etc/wps-adapter/wps-adapter.env
sudoedit /etc/wps-adapter/wps-adapter.env
```

至少修改 `WPS_GROUP_ID` 和 `WPS_ROOT_ID`。`WPS_ROOT_ID` 可以填你本人测试目录的文件夹 ID；填 `0` 表示尝试企业空间根目录。适配器默认只绑定 `127.0.0.1`，Basic Auth 文件仍建议配置，尤其是后面接反向代理或改为公网监听时。

低内存 VPS 建议保留下面的保护参数（`.env.example` 已给出默认值）：`WPS_MAX_UPLOADS=2`、`WPS_MAX_DOWNLOADS=4`、`WPS_UPLOAD_SPOOL_MEMORY=8388608`、`WPS_UPLOAD_MIN_FREE_BYTES=536870912`、`WPS_UPLOAD_RETRIES=2`。这些限制会让并发上传排队或返回 `503`，不会把全部请求同时压进内存。

检查配置不会访问 WPS：

```bash
cd /opt/wps-adapter
set -a
. /etc/wps-adapter/wps-adapter.env
set +a
PYTHONPATH=src python3 -m wps_adapter check-config
```

## 4. 安装 systemd 服务

```bash
sudo install -m 644 /opt/wps-adapter/deploy/wps-adapter.service /etc/systemd/system/wps-adapter.service
sudo systemctl daemon-reload
sudo systemctl enable --now wps-adapter
systemctl status wps-adapter --no-pager
curl http://127.0.0.1:54321/healthz
```

查看不含 Cookie/Token 的服务错误摘要：

```bash
sudo journalctl -u wps-adapter -n 100 --no-pager
```

## 5. 使用

在 VPS 本机先测试：

```bash
curl http://127.0.0.1:54321/api/v1/entries?path=/
```

浏览器页面地址使用 `http://<VPS 地址>:54321/`；WebDAV 客户端连接地址使用 `http://<VPS 地址>:54321/dav/`。如果通过 SSH 隧道访问 VPS，则把本地端口转发到 VPS 的 `127.0.0.1:54321`。直接公网暴露时必须使用 HTTPS 反向代理，并保留 Basic Auth；不要让 Cookie 进入反向代理访问日志。

## 6. 认证失效

如果返回 `503` 且 `upstream_status` 是 `401`，说明 WPS 会话和 `rtk` 刷新票据都失效或被撤销。适配器已先自动尝试 WPS SDK 刷新；仍然失败时，重新在本机浏览器建立本人账号会话，确认新的完整 Cookie 包含 `rtk`，然后只更新 VPS 上的 secret 文件。当前项目没有实现交互式登录、SSO 或验证码流程。
