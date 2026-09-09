# WPS 登录与持久化

登录助手只在账号所有者自己的电脑上运行。它不会读取日常浏览器配置，不会处理 WPS 密码，也不需要在 VPS 上安装 Chrome、Playwright 或图形界面。

登录助手已经打包成一个独立的 `wps_login.py` 文件，不需要为了获取 Cookie 而 clone 整个项目。

## Requirements

- Python `3.11+`。
- Chrome 或 Chromium。
- 一个独立的 `wps_login.py` 文件。
- 选择 SSH 连接方式时，还需要系统自带的 `ssh` 命令；选择 HTTP/HTTPS 时不需要 SSH。

HTTP/HTTPS 直接同步不需要 SSH。远程 HTTPS 是推荐方式；没有域名或证书时，远程 HTTP 也可以使用，但必须在向导中确认风险，或在命令行加 `--allow-http`。HTTP 会明文传输 Cookie、Basic Auth 和文件请求。

## Interactive flow

直接下载并运行单文件助手：

```bash
curl -fL --progress-bar --connect-timeout 10 --max-time 120 'https://ghfast.top/https://raw.githubusercontent.com/galiandan/WPS_2_WebDAV/main/wps_login.py' -o wps_login.py && python3 wps_login.py
```

如果 `ghfast.top` 无法访问，把命令中的 `ghfast.top` 替换为 `gh-proxy.com`。如果已经下载过脚本，也可以只运行 `python3 wps_login.py`。

如果已经 clone 了项目，也可以直接运行仓库根目录中的 `wps_login.py`。若仓库是 Private，GitHub Raw 地址需要相应访问权限。

脚本会依次询问 VPS 地址、连接方式和连接信息。连接方式有三种：

1. SSH 私钥：输入 SSH 用户名、端口和私钥路径。
2. SSH 密码：输入 SSH 用户名和端口；登录完成后由系统 `ssh` 提示密码。
3. HTTP/HTTPS：输入适配器地址、Basic Auth 用户名和隐藏输入的密码。

也可以把 HTTP/HTTPS 地址和用户名直接作为参数：

```bash
python3 wps_login.py \
  --adapter-url https://<adapter-host> \
  --adapter-port 18080 \
  --adapter-user <adapter-user>
```

没有域名或证书时：

```bash
python3 wps_login.py \
  --adapter-url http://<vps-host>:18080 \
  --adapter-user <adapter-user> \
  --allow-http
```

脚本会先提示输入适配器 Basic Auth 密码，密码不会显示。随后它会打开一个临时隔离的 Chrome 窗口：

1. 只在官方 WPS 页面登录自己的账号。
2. 正常完成学校 SSO、扫码、验证码或二次验证。
3. 默认不需要手动切换文件夹。WPS 登录后可能自动恢复上次打开的文件夹，即使该页面显示无权访问，脚本也会忽略这个文件夹。
4. 登录完成后，脚本会先获取全部可见企业空间及其名称，然后自动关闭临时浏览器；输入 `1,2` 选择网页要显示的多个空间，输入 `all` 选择全部空间。无法获取空间名称时会停止同步，避免把群组 ID 当作文件夹名称。
5. 脚本只从已选空间中选择一次 WebDAV 根目录所属空间，然后显示该空间可访问的文件夹。输入 `0` 使用当前目录，输入文件夹序号进入子目录，输入 `b` 返回上一级；输入 `s` 或直接回车跳过目录选择。跳过后暂时使用该空间根目录，登录完成后可在网页设置中选择真正的 WebDAV 根目录。
6. 脚本验证选定空间和当前根目录，验证失败不会覆盖 VPS 上已有凭据。
7. 如果要挂载具体子文件夹，也可以使用 `--workspace-url`；脚本会直接打开并校验该地址，具体用法见下方。网页中的“设置 → WebDAV 存储位置”可以在之后随时重新选择目录。
8. 脚本只保留匹配 WPS 云盘域名的 Cookie，并通过 HTTP 或 HTTPS 发送到适配器；SSH 方式也会同步工作区文件。
9. 适配器原子更新 `wps-cookie`、`wps-csrf` 和 `wps-workspace.json`，服务无需重启。

脚本不会显示 Cookie 值，也不会把 Cookie 放入命令参数、URL、日志或仓库。临时浏览器配置在流程结束后删除。

## Target folder (optional)

省略 `--workspace-url` 时，脚本会在关闭浏览器后让你选择网页空间，再从这些空间中选择一个 WebDAV 根目录所属空间和存储文件夹。输入 `0` 映射当前目录；选择子文件夹后，WebDAV 只从该文件夹开始显示；输入 `s` 或直接回车则暂时映射空间根目录，之后可在网页设置中选择。WPS 自动恢复的旧浏览器文件夹不会被自动使用。网页始终保留所有选中的空间，从 `/A/`、`/B/` 等空间根目录浏览。

之后也可以直接在网页右上角“设置”中修改。网页选择器会从当前 WPS 空间的原始根目录开始浏览，即使当前已经映射到子文件夹，也可以切换到同级或其他子文件夹。保存后 `/dav/` 地址不变，文件不会被移动或复制，只有适配器的映射位置改变。

若要使用命令行指定具体文件夹，从 WPS 页面复制类似下面的官方地址：

```text
https://365.kdocs.cn/space/<企业ID>/<群组ID>/<文件夹ID>
```

运行时添加：

```bash
python3 wps_login.py \
  --workspace-url 'https://365.kdocs.cn/space/<企业ID>/<群组ID>/<文件夹ID>' \
  --adapter-url https://<adapter-host> \
  --adapter-port 18080 \
  --adapter-user <adapter-user>
```

指定后，登录助手只接受同一企业、群组和文件夹的页面；如果地址无权访问或登录后跳到了其他位置，脚本会超时失败，不会悄悄改用其他目录。

## SSH fallback

如果 VPS 暂时没有 HTTPS 反向代理，可以确认 HTTP 风险后直接同步，也可以继续通过 SSH 标准输入同步：

```bash
ssh -F /dev/null -i ~/.ssh/id_ed25519 <vps-user>@<vps-host> exit

python3 wps_login.py \
  --ssh-target <vps-user>@<vps-host> \
  --ssh-identity ~/.ssh/id_ed25519
```

第一次连接时，只有在确认目标地址属于自己服务器后才接受主机指纹。SSH 方式也不会把 Cookie 放入命令参数；建议使用安装器选择的服务用户连接，若使用 root，助手会尽量保留已有凭证文件的所有者。

## Local output

只想把凭据写到本机测试目录时，可以使用绝对路径：

```bash
python3 wps_login.py --output-dir /absolute/path/to/secrets
```

目录会设置为 `0700`，凭据文件会设置为 `0600`。
成功后目录中还会有 `wps-workspace.json`，其中保存群组 ID、根目录 ID 和 WPS 返回的空间名称，不保存 Cookie。

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

首次同步得到的 `rtk` 会保存在 VPS secret 文件中，群组和目录选择保存在 `wps-workspace.json`。默认目录是企业云盘根目录 `0`；选择子文件夹后会保存具体文件夹 ID 和显示路径。适配器遇到 WPS `401` 时，会按已经观察到的 `grant_token` 刷新流程更新轮换 Cookie，并重试原请求。只有 WPS 撤销刷新票据、要求重新登录或登录策略改变时，才需要再次运行助手。

## Web login security

网页登录仍使用安装时创建的唯一 Basic Auth 账号。WebDAV 客户端、REST 客户端和网页的账号密码不分离；2FA 和 Passkey 只增加网页登录方式，不会让 Windows、手机、NAS 等 WebDAV 客户端支持交互式二次验证。

登录网页后打开“设置 → 登录安全”即可选择：

1. 启用 TOTP 两步验证。把页面显示的密钥添加到验证器应用，输入当前 6 位验证码确认。随后页面一次性显示 8 个恢复码；恢复码只保存哈希，必须离线保存原文。
2. 添加 Passkey。浏览器会调用系统的指纹、人脸、设备 PIN 或安全密钥。注册和登录要求浏览器提供 WebAuthn 安全上下文，生产环境应使用 HTTPS 域名。

2FA/Passkey 状态文件是 `/etc/wps-adapter/secrets/auth-settings.json`，权限与其他私密状态文件相同。不要手工编辑；忘记验证器时使用尚未使用的恢复码关闭 2FA，再重新配置。Passkey 可以在网页安全设置中逐个删除。若所有 Passkey 都被删除，密码登录仍然可用。

如果使用纯 HTTP 访问公网 IP，TOTP 仍可工作，但密码、验证码和会话本身都会经过明文连接；Passkey 通常会被浏览器禁用。没有域名和证书时建议只在可信网络使用 HTTP，或先配置 HTTPS 反向代理。

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

### HTTP/HTTPS 同步失败

确认适配器地址、端口、Basic Auth 账号和密码正确，并确认 VPS 已部署当前版本的 `/api/v1/session/import` 接口。HTTP 模式需要加 `--allow-http` 或在向导中确认风险；不要把 Cookie 粘贴到聊天或命令行。

### SSH 同步失败

先手动运行 SSH 主机检查命令确认密钥、主机指纹和权限。若密钥有口令，先把密钥加入本机 `ssh-agent`。
