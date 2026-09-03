# Deployment

本文说明如何在 Debian/Ubuntu 风格的 Linux VPS 上使用 systemd 部署适配器。不需要 Docker 或第三方 Python 包。示例中的 `<vps-host>`、`<vps-user>` 和路径都要替换为自己的值。

## 1. Prepare the host

建议使用专用低权限系统用户运行服务；当前模板默认使用 root 以降低首次部署复杂度。公网部署必须配置 HTTPS 反向代理，不能把带 Basic Auth 的纯 HTTP 端口直接暴露给互联网。

确认主机满足：

- Python `3.11+`。
- systemd。
- 能访问 WPS 和对象存储域名。
- 临时上传文件所在磁盘有足够空间。

## 2. Install the source

在 VPS 上将仓库放到 `/opt/wps-adapter`。例如：

```bash
sudo git clone https://github.com/galiandan/WPS_2_WebDAV.git /opt/wps-adapter
cd /opt/wps-adapter
PYTHONPATH=src python3 -m wps_adapter --version
```

升级时先备份 systemd 单元和非秘密配置，再更新代码。不要用仓库文件覆盖 `/etc/wps-adapter/secrets/`。

## 3. Create secret files

```bash
sudo install -d -o root -g root -m 700 /etc/wps-adapter/secrets
sudo install -o root -g root -m 600 /dev/null /etc/wps-adapter/secrets/wps-cookie
sudo install -o root -g root -m 600 /dev/null /etc/wps-adapter/secrets/wps-csrf
sudo install -o root -g root -m 600 /dev/null /etc/wps-adapter/secrets/adapter-username
sudo install -o root -g root -m 600 /dev/null /etc/wps-adapter/secrets/adapter-password
```

优先在账号所有者自己的电脑上运行 [`login.md`](login.md) 中的登录助手。公网部署并配置 HTTPS 反向代理后，助手可以通过受 Basic Auth 保护的接口直接写入 `wps-cookie` 和 `wps-csrf`；没有 HTTPS 时仍可使用 SSH 备用方式。不需要把 Cookie 粘贴进命令行。

适配器 Basic Auth 的用户名和密码分别写入 `adapter-username`、`adapter-password`。这些文件只允许服务用户读取。不要把 WPS 密码、Cookie 或 Basic Auth 密码放入 `.env`、Git、Issue 或聊天。

## 4. Configure the service

```bash
sudo cp /opt/wps-adapter/.env.example /etc/wps-adapter/wps-adapter.env
sudo chmod 600 /etc/wps-adapter/wps-adapter.env
sudoedit /etc/wps-adapter/wps-adapter.env
```

至少设置：

```dotenv
WPS_GROUP_ID=your-enterprise-group-id
WPS_ROOT_ID=0
WPS_COOKIE_FILE=/etc/wps-adapter/secrets/wps-cookie
WPS_CSRF_TOKEN_FILE=/etc/wps-adapter/secrets/wps-csrf
ADAPTER_USERNAME_FILE=/etc/wps-adapter/secrets/adapter-username
ADAPTER_PASSWORD_FILE=/etc/wps-adapter/secrets/adapter-password
ADAPTER_BIND=127.0.0.1
ADAPTER_PORT=54321
```

`WPS_ROOT_ID=0` 表示尝试企业空间根目录；也可以填自己测试目录的文件夹 ID。低内存 VPS 建议保留模板中的并发、spool 和磁盘空间保护参数。

检查配置不会访问 WPS：

```bash
cd /opt/wps-adapter
set -a
. /etc/wps-adapter/wps-adapter.env
set +a
PYTHONPATH=src python3 -m wps_adapter check-config
```

## 5. Install systemd

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
curl http://127.0.0.1:54321/healthz
```

查看不包含 Cookie、Token、完整 URL 或文件内容的日志：

```bash
sudo journalctl -u wps-adapter -n 100 --no-pager
```

## 6. Reverse proxy

让反向代理终止 TLS，并将请求转发到 `http://127.0.0.1:54321`。保留适配器 Basic Auth；不要在代理访问日志中记录 `Authorization` 头、查询参数或请求体。登录助手的 HTTPS 凭据导入也必须经过这条 TLS 入口。WebDAV 客户端使用：

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

服务遇到 WPS `401` 时会先检查 secret 是否被手动替换，然后尝试已确认的 `grant_token` 刷新流程并重试一次。若 `rtk` 已被撤销或 WPS 要求重新登录，在账号所有者自己的电脑上重新运行 [`login.md`](login.md) 的登录助手，通过 HTTPS 导入或 SSH 备用方式更新凭据。服务无需因凭据同步而重启。
