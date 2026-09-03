# WPS Adapter 对接文档

本文记录当前适配器的对接方式、线上部署状态和后续验收事项。文档不包含 Basic Auth 密码、WPS Cookie、CSRF、refresh token 或对象存储签名 URL。

## 1. 当前状态

| 项目 | 状态 |
| --- | --- |
| VPS 地址 | `<vps-host>` |
| 对外端口 | `54321` |
| 线上版本 | `0.4.0` |
| 网页入口 | `http://<vps-host>:54321/` |
| WebDAV 入口 | `http://<vps-host>:54321/dav/` |
| REST 入口 | `http://<vps-host>:54321/api/v1/` |
| 健康检查 | `http://<vps-host>:54321/healthz` |
| 适配器账号 | `<adapter-user>` |
| WPS 企业空间 | 已配置为本人的测试空间和测试目录 |
| 5005 端口 | 已停用，不再使用 |

2026-09-03 已将 `0.4.0` 部署到 VPS。该版本增加了本地 Chrome 登录助手、WPS SDK `grant_token` 自动续期和 Set-Cookie 持久化；线上业务读写、Range、COPY、LOCK 和 100 MiB 分片上传仍需要使用本人适配器账号进行一次完整验收，不能用未认证的健康检查代替。

本地代码已通过 50 项标准库测试：

```bash
cd <project-dir>
PYTHONPATH=src python3 -m unittest discover -s tests -v
```

## 2. 登录方式

所有访问都使用适配器自己的 Basic Auth。执行 curl 时只写用户名，让 curl 提示输入密码：

```bash
curl -u <adapter-user> 'http://<vps-host>:54321/api/v1/entries?path=/'
```

不要把密码写在命令行中，否则可能进入 shell 历史或进程列表。也不要把 WPS Cookie、CSRF 或完整 curl 请求发到聊天、工单或 Git。

健康检查不需要 Basic Auth：

```bash
curl 'http://<vps-host>:54321/healthz'
```

### 2.1 一键建立 WPS 会话

适配器网页不能直接读取 WPS 登录 Cookie：两个网页不同源，而且 `rtk` 是 HttpOnly Cookie。请在账号所有者自己的电脑上、项目目录中运行下面的命令；它会启动一个临时隔离的 Chrome 窗口，打开官方 WPS 页面。用户只在该窗口中完成登录，看到云盘页面后回到终端按回车，助手会通过 SSH 将凭据写入 VPS：

```bash
cd <project-dir>
PYTHONPATH=src python3 -m wps_adapter login \
  --ssh-target <vps-user>@<vps-host> \
  --ssh-identity ~/.ssh/id_ed25519
```

运行前先确保本机已安装 Chrome/Chromium，并且已经手动确认过 VPS 的 SSH 主机指纹。助手不需要 Playwright、浏览器插件或 VPS 图形界面，不会显示 Cookie 值，也不会把 WPS 密码发送给适配器。它只保留匹配 WPS 云盘域名的 Cookie，并要求同时存在 `rtk` 和 `csrf`；同步完成后服务无需重启，下一次请求就会读取新会话。

如果只想把凭据写入本机某个受保护目录，可把 `--ssh-target` 改为 `--output-dir /absolute/path`；两个选项不能同时使用。

## 3. REST 接口

REST 的 `path` 必须是以 `/` 开头的远端路径，路径中的中文和空格应由 curl 或客户端进行 URL 编码。

```text
GET    /api/v1/entries?path=/
GET    /api/v1/metadata?path=/folder/file.txt
GET    /api/v1/download?path=/folder/file.txt
PUT    /api/v1/upload?path=/folder/new.txt
POST   /api/v1/folders?path=/folder/new-folder
DELETE /api/v1/entries?path=/folder/file.txt
PATCH  /api/v1/entries?path=/folder/file.txt
```

上传示例：

```bash
curl -u <adapter-user> \
  -H 'Content-Type: application/octet-stream' \
  --upload-file ./local.txt \
  'http://<vps-host>:54321/api/v1/upload?path=/local.txt'
```

REST 上传默认不覆盖同名文件。明确需要覆盖时加 `overwrite=true`：

```bash
curl -u <adapter-user> \
  -H 'Content-Type: application/octet-stream' \
  --upload-file ./local.txt \
  'http://<vps-host>:54321/api/v1/upload?path=/local.txt&overwrite=true'
```

重命名：

```bash
curl -u <adapter-user> -X PATCH \
  -H 'Content-Type: application/json' \
  --data '{"name":"renamed.txt"}' \
  'http://<vps-host>:54321/api/v1/entries?path=/local.txt'
```

移动到另一个目录并保留原名：

```bash
curl -u <adapter-user> -X PATCH \
  -H 'Content-Type: application/json' \
  --data '{"parent_path":"/archive"}' \
  'http://<vps-host>:54321/api/v1/entries?path=/local.txt'
```

## 4. WebDAV 接口

新版本支持以下方法：

| 方法 | 用途 | 关键说明 |
| --- | --- | --- |
| `OPTIONS` | 查询能力 | 声明 `DAV: 1,2` |
| `PROPFIND` | 列目录和读取属性 | 支持 `Depth: 0/1/infinity` |
| `GET` | 下载 | 支持单范围 `Range` |
| `HEAD` | 获取文件属性 | 不返回文件内容 |
| `PUT` | 上传或覆盖文件 | 要求 `Content-Length` |
| `MKCOL` | 新建文件夹 | 目标路径以文件夹名结尾 |
| `DELETE` | 删除文件或文件夹 | 等待 WPS 删除任务完成 |
| `MOVE` | 重命名或移动 | 使用 `Destination` 头 |
| `COPY` | 复制文件或文件夹 | 新版本使用流式下载/上传中继 |
| `LOCK` | 创建或刷新写锁 | 新版本为适配器进程内锁 |
| `UNLOCK` | 释放写锁 | 使用 `Lock-Token` 头 |

查看目录：

```bash
curl -u <adapter-user> -X PROPFIND \
  -H 'Depth: 1' \
  'http://<vps-host>:54321/dav/'
```

上传文件：

```bash
curl -u <adapter-user> -T ./local.txt \
  'http://<vps-host>:54321/dav/local.txt'
```

重命名或移动：

```bash
curl -u <adapter-user> -X MOVE \
  -H 'Destination: http://<vps-host>:54321/dav/renamed.txt' \
  'http://<vps-host>:54321/dav/local.txt'
```

复制文件：

```bash
curl -u <adapter-user> -X COPY \
  -H 'Destination: http://<vps-host>:54321/dav/local-copy.txt' \
  -H 'Depth: 0' \
  'http://<vps-host>:54321/dav/local.txt'
```

复制文件夹时使用 `Depth: infinity`；适配器会递归创建目标文件夹并逐个中继文件。没有确认的 WPS 服务端 COPY 接口，因此复制速度和耗时通常会比同一云盘内的服务端复制慢。

## 5. Range 断点下载

新版本支持单个字节范围。适配器会把 Range 传给 WPS 返回的预签名对象地址，并要求对象存储确实返回 `206`：

```bash
curl -u <adapter-user> \
  -H 'Range: bytes=0-1048575' \
  -o ./part.bin \
  'http://<vps-host>:54321/dav/large.bin'
```

成功响应应包含：

```text
HTTP/1.1 206 Partial Content
Accept-Ranges: bytes
Content-Range: bytes 0-1048575/<total-size>
Content-Length: 1048576
```

多范围请求暂不支持；无效或超出文件大小的范围返回 `416`。如果 WPS 对某类文件没有正确返回 `206`，适配器会报错，不会把完整文件误当成断点片段。

## 6. 锁定行为

锁是适配器本地的 WebDAV 兼容层，目的是让 Office、NAS 或同步程序能够完成“先锁定、再写入”的流程。它不会调用未经抓包确认的 WPS 锁接口。

锁默认最长 24 小时，只在当前 Python 进程内有效；重启服务后锁会消失。创建锁后，后续写请求需要在 `If` 头中带回返回的 `Lock-Token`：

```text
If: (<opaquelocktoken:由服务返回的令牌>)
```

没有令牌的写请求会返回 `423 Locked`。客户端结束编辑后应发送：

```text
UNLOCK /dav/document.docx
Lock-Token: <opaquelocktoken:由服务返回的令牌>
```

## 7. 大文件和资源保护

新版本的默认保护参数如下：

| 参数 | 默认值 | 作用 |
| --- | ---: | --- |
| `WPS_MAX_UPLOADS` | `2` | 同时上传数 |
| `WPS_MAX_DOWNLOADS` | `4` | 同时下载数 |
| `WPS_UPLOAD_SPOOL_MEMORY` | `8388608` | 每个上传在内存中保留的 spool 上限，8 MiB |
| `WPS_UPLOAD_MIN_FREE_BYTES` | `536870912` | 临时文件所在文件系统保留的最小空间，512 MiB |
| `WPS_UPLOAD_RETRIES` | `2` | 普通上传或单个分片的重试次数 |
| `WPS_MAX_COPY_ENTRIES` | `10000` | 一次 COPY 最多处理的对象数 |
| `WPS_MAX_COPY_DEPTH` | `64` | COPY 递归最大层数 |
| `WPS_MAX_PROPFIND_ENTRIES` | `10000` | 一次递归 PROPFIND 最多返回的对象数 |
| `WPS_MAX_PROPFIND_DEPTH` | `64` | 递归 PROPFIND 最大层数 |

上传超过内存 spool 上限后会使用请求级临时文件，操作完成或失败时自动清理。服务不会把云盘文件作为长期缓存保存。并发传输超过限制时返回 `503`；空间、大小或递归保护触发时返回 `507`。

大文件分片上传仍使用本人账号抓包确认的 WPS block/multipart 流程。当前增加的是同一请求内的失败重试：失败分片会重新申请签名地址并从该分片重传。进程退出后的跨请求续传、分片取消和清理接口仍没有被 WPS 抓包确认。

## 8. Cookie、CSRF 和自动续期

本轮从 WPS 账号 SDK 的公开脚本中确认了无交互续期请求：

```text
POST https://account.kdocs.cn/passport/secure/api/grant_token
Content-Type: application/json

{"grant_type":"refresh_token"}
```

该请求依赖浏览器的 `rtk` Cookie。浏览器 Cookie 元数据表明 `rtk` 的作用域为 `.kdocs.cn`、路径为 `/passport/secure`、HttpOnly 且为持久 Cookie；因此从普通云盘列表请求复制 Cookie 时可能看不到它，首次初始化必须从本人浏览器 Cookie 存储中补齐完整会话 Cookie。

`0.4.0` 的凭据行为：

1. 每次访问 WPS 前重新读取 `/etc/wps-adapter/secrets/wps-cookie` 和 `/etc/wps-adapter/secrets/wps-csrf`。
2. 正常 WPS 响应或刷新响应带 `Set-Cookie` 时，自动按 Cookie 名合并并以临时文件加重命名的方式持久化；`csrf` 同步更新到 CSRF 文件。
3. 上游返回 `401` 时，先检查凭据文件是否已被手动替换；没有替换时调用上述 `grant_token`，然后用新的 Cookie 和 CSRF 重试原请求一次。
4. `WPS_AUTO_REFRESH=false` 可关闭自动刷新；`WPS_ACCOUNT_BASE_URL` 可覆盖默认的 `https://account.kdocs.cn`。
5. 没有 `rtk`、刷新票据已撤销或 WPS 要求交互式登录时，服务返回 `503`；适配器不会自动处理密码、SSO、验证码或风控。

这解决的是“在账号所有者本机完成一次官方登录后自动同步，以及已有浏览器会话的无交互续期和服务重启后的 Cookie 持久化”；适配器服务器本身不执行密码登录、SSO、验证码或风控。

## 9. 本次部署记录

已完成：

1. VPS 旧服务单元和环境配置已备份到 root 管理的回退目录；没有读取或打印 secret 内容。
2. `src/`、`pyproject.toml`、`deploy/` 和文档已同步到 `/opt/wps-adapter`。
3. `wps-adapter-hardening.conf` 已安装为 `/etc/systemd/system/wps-adapter.service.d/override.conf`；硬化参数文件已安装为 `/etc/wps-adapter/wps-adapter-hardening.env`。
4. `check-config` 和远端 50 项标准库测试通过；本地共 50 项通过。
5. `systemctl daemon-reload`、重启和 `/healthz` 检查通过，线上版本为 `0.4.0`。
6. 未认证访问 WebDAV/REST 会返回 `401`；5005 没有监听。

待完成：

1. 使用本人适配器账号验证列表、上传、下载、Range、COPY、LOCK/UNLOCK 和 100 MiB 分片上传。
2. 验证失败后查看 `journalctl -u wps-adapter` 的错误摘要；日志不应出现 Cookie、CSRF、完整 URL 或文件内容。

## 10. 后续验收清单

建议所有测试文件都使用统一前缀，例如 `adapter-next-YYYYMMDD-*`，避免误操作已有资料：

- [ ] `GET /api/v1/entries?path=/` 能列出测试目录。
- [ ] WebDAV `PROPFIND Depth: 1` 和 `Depth: infinity` 都能返回 `207`。
- [ ] 上传一个小文件后下载，使用 `cmp` 校验内容一致。
- [ ] 对同一文件请求 `Range: bytes=0-...`，确认返回 `206` 和正确 `Content-Range`。
- [ ] `COPY` 小文件后下载副本并校验内容。
- [ ] `LOCK` 后无令牌写入得到 `423`，带令牌写入成功，随后 `UNLOCK`。
- [ ] 上传一个 100 MiB 中性文件，确认分片流程完成并下载校验。
- [ ] 并发发起超过 2 个上传，确认服务排队或返回 `503`，而不是内存持续上涨。
- [ ] 测试结束后只删除本次创建的测试对象。
