# 自动登录并同步凭据

这一步只需要在你自己的电脑上做。VPS 不需要安装 Chrome、Playwright 或图形界面；WPS 密码也只会输入在官方 WPS 页面里。

## 开始前

电脑上需要有：

- Chrome 或 Chromium 浏览器。
- Python 3.11 或更高版本。
- 能正常 SSH 登录 VPS 的密钥。
- 项目目录，例如 `<project-dir>`。

先在终端确认 SSH 主机指纹和密钥都正常。这个命令只执行远程退出，不会改动项目：

```bash
ssh -F /dev/null -i ~/.ssh/id_ed25519 <vps-user>@<vps-host> exit
```

第一次连接如果询问是否信任主机指纹，只有确认地址确实是自己的 VPS 后才输入 `yes`。如果密钥位置不同，把 `-i` 后面的路径换成自己的密钥路径。

## 登录并同步

在项目目录打开终端，运行：

```bash
cd <project-dir>
PYTHONPATH=src python3 -m wps_adapter login \
  --ssh-target <vps-user>@<vps-host> \
  --ssh-identity ~/.ssh/id_ed25519
```

Windows PowerShell 可以这样运行：

```powershell
cd C:\path\to\WPS_2_WebDAV
$env:PYTHONPATH = "src"
python -m wps_adapter login --ssh-target <vps-user>@<vps-host>
```

随后会出现一个单独的 Chrome 窗口：

1. 只在这个官方 WPS 窗口中登录自己的 WPS 账号。
2. 如果出现学校 SSO、扫码、验证码或二次验证，按 WPS 页面正常完成。
3. 看到云盘页面后，回到刚才的终端按一次回车。
4. 等终端显示“已通过 SSH 更新 VPS 凭据”。

助手只读取这个临时 Chrome 配置中的 Cookie，且只保留匹配 WPS 云盘的 Cookie。它必须找到 `rtk` 和 `csrf` 才会同步；Cookie 值不会显示在终端，也不会写入命令行参数。登录结束后临时 Chrome 配置会自动删除。

## 检查结果

同步完成后不需要重启 VPS 服务。执行下面的命令测试列表：

```bash
curl -u <adapter-user> 'http://<vps-host>:54321/api/v1/entries?path=/'
```

curl 会提示输入适配器密码。不要把密码直接写在命令里，也不要把终端输出中的 Cookie、CSRF 或错误详情发到聊天。

以后 WPS 会话正常续期时，适配器会自己调用已经确认的 `grant_token` 流程。只有 WPS 撤销了 `rtk` 或要求重新登录时，才需要重新运行本页命令。

## 常见提示

### 没有找到 Chrome

请确认 Chrome 已安装，然后重新运行。也可以显式指定浏览器路径，例如：

```bash
PYTHONPATH=src python3 -m wps_adapter login \
  --browser /usr/bin/google-chrome-stable \
  --ssh-target <vps-user>@<vps-host>
```

### 没有找到 `rtk`

不要手工复制 Cookie。关闭窗口后重新运行助手，并确认已经在官方 WPS 页面完成登录，而不是停留在登录页或只打开了分享链接。

### SSH 同步失败

先重新执行“开始前”的 SSH 命令，确认密钥和主机指纹正常；如果密钥有口令，先将密钥加入本机的 `ssh-agent`，再运行登录助手。不要把 Cookie 作为命令参数或通过 HTTP 上传到适配器。
