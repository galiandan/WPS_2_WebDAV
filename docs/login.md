# Bootstrap Login

这一步在账号所有者自己的电脑上完成。服务器不需要安装 Chrome、Playwright 或图形界面，WPS 密码也只输入在官方 WPS 页面中。

## Requirements

- Chrome 或 Chromium。
- Python `3.11+`。
- 可用的 SSH 客户端和 VPS 登录密钥。
- 已下载本仓库源码。

先确认 SSH 主机指纹和密钥：

```bash
ssh -F /dev/null -i ~/.ssh/id_ed25519 <vps-user>@<vps-host> exit
```

第一次连接时，只有在确认目标地址属于自己服务器后才接受主机指纹。如果密钥路径或远程用户不同，请替换命令中的对应部分。

## Run the helper

在项目目录运行：

```bash
PYTHONPATH=src python3 -m wps_adapter login \
  --ssh-target <vps-user>@<vps-host> \
  --ssh-identity ~/.ssh/id_ed25519
```

助手会启动一个临时隔离的 Chrome 窗口：

1. 只在官方 WPS 页面中登录自己的账号。
2. 正常完成学校 SSO、扫码、验证码或二次验证。
3. 看到云盘页面后，回到终端按回车。
4. 等待终端显示凭据已通过 SSH 更新。

助手只读取这个临时 Chrome 配置，只保留匹配 WPS 云盘域名的 Cookie，并要求存在 `rtk` 和 `csrf`。Cookie 值不会显示、不会进入命令参数，也不会写入仓库。临时浏览器配置在流程结束后删除。

## Why a helper is needed

适配器网页和 WPS 网页属于不同源；浏览器页面也不能读取 HttpOnly Cookie。因此不能通过 iframe 或普通 JavaScript 从 WPS 页面“拿出”登录态。助手使用 Chrome 本地 DevTools Protocol 读取浏览器自身已经保存的会话，登录动作仍完全由官方 WPS 页面执行。

## Verify

同步完成后服务无需重启：

```bash
curl -u <adapter-user> \
  'https://<adapter-host>/api/v1/entries?path=/'
```

curl 会提示输入适配器 Basic Auth 密码。不要把密码写在命令中。

## Troubleshooting

### 找不到 Chrome

确认本机安装了 Chrome/Chromium，或显式指定路径：

```bash
PYTHONPATH=src python3 -m wps_adapter login \
  --browser /path/to/chrome \
  --ssh-target <vps-user>@<vps-host>
```

### 找不到 `rtk`

关闭临时窗口并重新运行助手，确认已在官方 WPS 页面完成登录，而不是停留在登录页或只打开分享链接。

### SSH 同步失败

先手动运行 Requirements 中的 SSH 命令确认密钥、主机指纹和权限。若密钥有口令，先把密钥加入本机 `ssh-agent`。不要通过 HTTP 上传 Cookie，也不要把 Cookie 粘贴到聊天或命令行。
