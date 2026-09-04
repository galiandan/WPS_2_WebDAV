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

直接下载并运行单文件助手。下载失败时会依次尝试国内加速节点和 GitHub 直连：

```bash
LOGIN_RAW_URL="https://raw.githubusercontent.com/galiandan/WPS_2_WebDAV/main/wps_login.py"
LOGIN_FILE="$(mktemp -t wps-login.XXXXXX)"
LOGIN_DOWNLOADED=0
for URL in "https://gh-proxy.com/$LOGIN_RAW_URL" "https://ghfast.top/$LOGIN_RAW_URL" "$LOGIN_RAW_URL"; do
  : > "$LOGIN_FILE"
  if curl --fail --show-error --location --progress-bar --connect-timeout 10 --max-time 60 --retry 1 --max-filesize 5242880 --proto '=https' --proto-redir '=https' --tlsv1.2 "$URL" -o "$LOGIN_FILE"; then
    LOGIN_DOWNLOADED=1
    break
  fi
done
if (( LOGIN_DOWNLOADED )); then
  mv "$LOGIN_FILE" wps_login.py
else
  rm -f "$LOGIN_FILE"
  echo '登录助手下载失败' >&2
  exit 1
fi

python3 wps_login.py
```

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
3. 登录完成后，在同一个窗口进入想要挂载的企业云盘文件夹。脚本只接受类似 `/space/<企业ID>/<群组ID>/<文件夹ID>` 的官方 WPS 地址；停留在登录页或空间首页时会继续等待。
4. 脚本自动读取当前页面地址和有效会话，不需要回终端按回车，并从地址中取得群组 ID 和根目录 ID。
5. 脚本只保留匹配 WPS 云盘域名的 Cookie，并通过 HTTP 或 HTTPS 发送到适配器；SSH 方式也会同步工作区文件。
6. 适配器原子更新 `wps-cookie`、`wps-csrf` 和 `wps-workspace.json`，服务无需重启。

脚本不会显示 Cookie 值，也不会把 Cookie 放入命令参数、URL、日志或仓库。临时浏览器配置在流程结束后删除。

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
成功后目录中还会有 `wps-workspace.json`，其中只保存群组 ID 和根目录 ID，不保存 Cookie。

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

首次同步得到的 `rtk` 会保存在 VPS secret 文件中，当前选中的群组和目录保存在 `wps-workspace.json`。适配器遇到 WPS `401` 时，会按已经观察到的 `grant_token` 刷新流程更新轮换 Cookie，并重试原请求。只有 WPS 撤销刷新票据、要求重新登录或登录策略改变时，才需要再次运行助手。

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
