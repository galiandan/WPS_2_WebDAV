# WPS 登录与持久化

登录助手只在账号所有者自己的电脑上运行。它不会读取日常浏览器配置，不会处理 WPS 密码，也不需要在 VPS 上安装 Chrome、Playwright 或图形界面。

## Requirements

- Python `3.11+`。
- Chrome 或 Chromium。
- 已下载本仓库源码。

HTTPS 直接同步不需要 SSH。公网适配器必须有 HTTPS 反向代理；登录助手会拒绝把 Cookie 发到远程明文 HTTP。当前只有本机回环地址允许 HTTP 测试。

## Recommended flow: direct HTTPS sync

在项目目录运行：

```bash
python3 wps_login.py
```

脚本会依次询问适配器 HTTPS 地址、适配器用户名和适配器密码（密码不会显示）。也可以把地址和用户名直接作为参数：

```bash
python3 wps_login.py \
  --adapter-url https://<adapter-host> \
  --adapter-user <adapter-user>
```

脚本会先提示输入适配器 Basic Auth 密码，密码不会显示。随后它会打开一个临时隔离的 Chrome 窗口：

1. 只在官方 WPS 页面登录自己的账号。
2. 正常完成学校 SSO、扫码、验证码或二次验证。
3. 登录完成并出现有效会话后，脚本自动检测，不需要回终端按回车。
4. 脚本只保留匹配 WPS 云盘域名的 Cookie，并通过 HTTPS 发送到适配器。
5. 适配器原子更新 `wps-cookie` 和 `wps-csrf`，服务无需重启。

脚本不会显示 Cookie 值，也不会把 Cookie 放入命令参数、URL、日志或仓库。临时浏览器配置在流程结束后删除。

## SSH fallback

如果 VPS 暂时没有 HTTPS 反向代理，可以继续通过 SSH 标准输入同步：

```bash
ssh -F /dev/null -i ~/.ssh/id_ed25519 <vps-user>@<vps-host> exit

python3 wps_login.py \
  --ssh-target <vps-user>@<vps-host> \
  --ssh-identity ~/.ssh/id_ed25519
```

第一次连接时，只有在确认目标地址属于自己服务器后才接受主机指纹。SSH 方式也不会把 Cookie 放入命令参数。

## Local output

只想把凭据写到本机测试目录时，可以使用绝对路径：

```bash
python3 wps_login.py --output-dir /absolute/path/to/secrets
```

目录会设置为 `0700`，凭据文件会设置为 `0600`。

## Why a helper is needed

适配器网页和 WPS 网页属于不同源；关键的 `rtk` Cookie 还是 HttpOnly，普通 JavaScript、iframe 和书签脚本都不能读取。助手使用 Chrome 本地 DevTools Protocol 读取临时浏览器自己保存的会话，登录动作仍完全由官方 WPS 页面执行。

## Verify

同步完成后服务无需重启：

```bash
curl -u <adapter-user> \
  'https://<adapter-host>/api/v1/entries?path=/'
```

curl 会提示输入适配器 Basic Auth 密码。不要把密码写在命令中。

## Persistent refresh

首次同步得到的 `rtk` 会保存在 VPS secret 文件中。适配器遇到 WPS `401` 时，会按已经观察到的 `grant_token` 刷新流程更新轮换 Cookie，并重试原请求。只有 WPS 撤销刷新票据、要求重新登录或登录策略改变时，才需要再次运行助手。

## Troubleshooting

### 找不到 Chrome

确认本机安装了 Chrome/Chromium，或显式指定路径：

```bash
python3 wps_login.py \
  --browser /path/to/chrome \
  --adapter-url https://<adapter-host> \
  --adapter-user <adapter-user>
```

### 找不到 `rtk`

确认临时窗口中已经完成官方 WPS 登录并进入云盘，而不是停留在登录页或只打开分享链接。某些账号登录完成后需要等待几秒，脚本会自动继续等待。

### HTTPS 同步失败

确认适配器地址使用 `https://` 且证书有效，确认适配器 Basic Auth 账号和密码正确，并确认 VPS 已部署当前版本的 `/api/v1/session/import` 接口。不要通过 HTTP 上传 Cookie，也不要把 Cookie 粘贴到聊天或命令行。

### SSH 同步失败

先手动运行 SSH 主机检查命令确认密钥、主机指纹和权限。若密钥有口令，先把密钥加入本机 `ssh-agent`。
