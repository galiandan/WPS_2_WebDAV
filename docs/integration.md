# WPS Adapter 对接文档

本文记录当前适配器的对接方式和明天的部署验证步骤。文档不包含 Basic Auth 密码、WPS Cookie、CSRF、refresh token 或对象存储签名 URL。

## 1. 当前状态

| 项目 | 状态 |
| --- | --- |
| VPS 地址 | `<vps-host>` |
| 对外端口 | `54321` |
| 网页入口 | `http://<vps-host>:54321/` |
| WebDAV 入口 | `http://<vps-host>:54321/dav/` |
| REST 入口 | `http://<vps-host>:54321/api/v1/` |
| 健康检查 | `http://<vps-host>:54321/healthz` |
| 适配器账号 | `<adapter-user>` |
| WPS 企业空间 | 已配置为本人的测试空间和测试目录 |
| 5005 端口 | 已停用，不再使用 |

今天新增的本地代码尚未同步到 VPS。VPS 线上服务仍以今天开始前的版本为准；明天部署完成后，下面标为“新版本”的能力才会在线生效。

本地代码已通过 42 项标准库测试：

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

## 8. Cookie、CSRF 和真正的自动续期

当前能确认的事实只有：WPS 网页请求使用 Cookie，会写请求携带 CSRF 字段；没有确认可复现的 refresh token、刷新 URL 或无交互登录流程。

新版本的凭据行为：

1. 每次访问 WPS 前重新读取 `/etc/wps-adapter/secrets/wps-cookie` 和 `/etc/wps-adapter/secrets/wps-csrf`。
2. 上游返回 `401` 时检查凭据文件是否已经被替换，并自动重试一次。
3. 可以通过 `WPS_CREDENTIAL_REFRESH_COMMAND` 配置 root 管理的本地刷新助手；该助手必须按照真实抓包得到的流程更新两个文件，适配器不会替它猜测 WPS 登录协议。
4. 没有刷新助手时，WPS 会话真正过期仍需要在本人浏览器重新登录，再原子替换 secret 文件。

因此，“自动读新 Cookie 和恢复请求”已经具备；“适配器自己完成 WPS 登录并取得新会话”仍待新的真实抓包，不能宣称已经解决。

## 9. 明天部署顺序

今天不执行以下操作，明天按顺序执行：

1. 备份当前 VPS 服务状态和配置文件路径，不读取或打印 secret 内容。
2. 把本地 `src/`、`pyproject.toml` 和 `deploy/` 同步到 `/opt/wps-adapter`。
3. 安装 `deploy/wps-adapter-hardening.conf` 和 `deploy/wps-adapter-hardening.env` 到 `/etc/systemd/system/wps-adapter.service.d/` 与 `/etc/wps-adapter/`。
4. 运行 `PYTHONPATH=src python3 -m wps_adapter check-config`。
5. `systemctl daemon-reload` 后重启 `wps-adapter.service`。
6. 先检查本机 `/healthz`，再从外部检查 54321。
7. 用中性测试文件验证列表、上传、下载、Range、COPY、LOCK/UNLOCK 和 100 MiB 分片上传。
8. 验证失败后先看 `journalctl -u wps-adapter` 的错误摘要；日志不应出现 Cookie、CSRF、完整 URL 或文件内容。

## 10. 明天的验收清单

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
