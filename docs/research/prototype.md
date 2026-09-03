# 当前原型状态

当前 Python 原型已经实现或接入了以下已经从本人账号观察到的操作：

- `GET /3rd/drive/api/v5/groups/<group-id>/files` 列指定父目录。
- `GET /api/v3/office/file/<file-id>/download` 获取短期下载地址，再直接流式读取对象存储响应。
- 普通文件上传：`pre_check` -> `PUT /3rd/drive/api/v5/files/upload/create_update` -> 签名对象存储 `PUT` -> `POST /3rd/drive/api/v5/files/file`。WPS 需要先计算校验和，因此适配器使用短时内存/临时文件 spool，不保存长期副本。
- 大文件分片上传：达到默认 50 MiB 阈值后，使用 `POST/PUT /3rd/drive/api/v5/files/upload/block` 获取会话和每片上传指令，直接向签名对象存储地址上传各片，再调用 `POST .../block/merge` 完成合并，最后登记文件。默认分片大小为 10 MiB；分片内容只在短时 spool 和当前内存片段中存在。
- 覆盖更新沿用普通上传序列；观察到 `create_update` 的覆盖字段、同一文件 ID 的最终响应和 `fver` 增加。适配器在覆盖模式下计算 `md5`，允许观察到的预检查 `403` 继续进入上传流程。
- 删除：`POST /3rd/drive/api/v5/files/batch/task/delete` 提交任务，再轮询 `GET /3rd/drive/api/v5/files/batch/task/progress?taskuuid=...`；只有任务成功完成才向调用方返回成功。
- 重命名：`PUT /3rd/drive/api/v3/groups/<group-id>/files/<file-id>`，JSON 使用 `fname` 和 `csrfmiddlewaretoken`，响应直接返回更新后的对象。
- 移动：`POST /3rd/drive/api/v5/files/batch/task/move` 提交源父目录、目标父目录和文件 ID，再轮询 `GET /3rd/drive/api/v5/files/batch/task/progress?taskuuid=...`；只有任务成功完成才向调用方返回成功。
- WebDAV `PROPFIND`、`GET`、`HEAD`、`PUT` 和 REST 列表/元数据/下载/上传。
- 同源浏览器文件管理页：目录浏览、上传进度、下载、新建文件夹、重命名、移动和删除。
- WebDAV 和 REST 删除，REST 重命名，WebDAV 同目录重命名，以及保留原名的跨目录移动。
- 远端路径到 WPS 文件夹 ID 的解析，以及带 TTL 的元数据缓存。
- `/healthz`、适配器 Basic Auth 和 systemd 单元模板。
- 本地 `login` 助手：使用临时隔离 Chrome 打开 WPS 官方页面，自动检测本人登录完成后，可通过 HTTPS 同步包含 `rtk`/`csrf` 的凭据；SSH 仍作为备用通道。

上传代码仍属于实验性实现：普通上传、覆盖更新和大文件分片均已有真实回放；大文件分片已在 VPS 上用本人账号的 100 MiB 专用测试文件完成上传和下载校验。失败续传、分片覆盖和 WPS 刷新成功响应的真实账号验收仍待完成。

## 代码入口

核心类位于 `src/wps_adapter/client.py`：

- `WpsClientConfig` 保存基础地址、账号刷新地址、企业空间 ID、可选 Referer/Origin、`cid` 和 Cookie/CSRF 来源。
- Cookie 和 CSRF 可以直接从环境变量读取，也可以从本机文件动态读取；文件更新后下一次请求会使用新值。文件来源默认开启 WPS SDK `grant_token` 续期，并原子保存响应中的 Set-Cookie。
- `WpsDriveClient.list_entries()` 返回 `ListPage`，不会把完整响应原文放入 `RemoteEntry.raw`。
- `WpsDriveClient.open_download()` 先调用 WPS API，再访问返回的签名地址；Cookie 只发给 WPS API，不转发给对象存储。
- `WpsDriveClient.download_to()` 按块写入调用方提供的二进制文件对象，不把整文件读入内存。

## 凭据原则

Cookie 和 CSRF 值只能在运行适配器的本机/VPS secret store 中提供，不能写入仓库、命令行参数、日志或 HAR。首次部署可以使用本地 `login` 助手自动同步完整浏览器会话（包括 `rtk`）；适配器可以自动续期 WPS 会话，但密码、SSO、验证码和风控仍由官方 WPS 页面处理。

网页端不能直接嵌入并读取 WPS 登录态：不同源策略和 HttpOnly Cookie 会阻止适配器页面取得凭据。`login` 助手因此在账号所有者的电脑上启动一个临时 Chrome 配置，用户在官方 WPS 窗口中登录，按回车后助手只选取匹配 `365.kdocs.cn` 的 WPS Cookie，再通过 SSH 写入远端 secret 文件。

调用 `WpsDriveClient` 的列表、下载、上传、创建目录、删除、重命名和移动方法才会访问网络；`python -m wps_adapter check-config` 不访问网络。测试使用本地假响应和回环 HTTP 服务。

## 本地只读探针

`tools/wps_probe.py` 可以在账号所有者自己的机器上验证列表和下载。它会隐藏式询问 Cookie，Cookie 不会打印；不要把命令输出之外的输入内容发给别人。

更适合初学者的是 `tools/wps_har_probe.py`：它从本机 HAR 自动读取 Cookie，不要求手工复制 Cookie，也不会打印 Cookie 或签名 URL。

```bash
cd <project-dir>
python3 tools/wps_har_probe.py captures/R-01.har list
python3 tools/wps_har_probe.py captures/R-01.har download --output /tmp/wps-probe-download.bin
```

两条命令都只会重放 HAR 中已经观察到的本人账号读请求。HAR 必须在重新登录后的新会话中导出，并且只保存在本机。

如果 Chrome 导出的 HAR 不包含 Cookie，使用 `tools/wps_curl_probe.py`。在 Network 中右键已登录的 `files` 或 `download` 请求，选择“复制 -> 复制为 cURL (bash)”，然后先运行：

```bash
python3 tools/wps_curl_probe.py list
```

把 cURL 粘贴到终端后按 `Ctrl-D`。下载时使用：

```bash
python3 tools/wps_curl_probe.py download --output /tmp/wps-probe-download.bin
```

cURL 只在本机内存中使用，工具只打印结果数量或下载字节数。不要把 cURL 文本、终端输入或 HAR 发到聊天中。

列出一个文件夹：

```bash
cd <project-dir>
python3 tools/wps_probe.py list --group-id <own-group-id> --parent-id <own-folder-id>
```

下载一个文件：

```bash
python3 tools/wps_probe.py download --group-id <own-group-id> --file-id <own-file-id> --output /tmp/wps-probe-download.bin
```

如果服务端要求浏览器捕获中的可选参数，再分别加 `--observed-options` 或 `--direct-external`；`--cid` 只填写本人本地捕获到的值。不要在命令行参数中放 Cookie，避免进入 shell 历史。

## 未覆盖能力

快速上传成功路径、跨目录同时改名和进程退出后的分片续传仍未确认。大文件分片流程已观察并由适配器在 VPS 上回放成功；适配器现在会对普通上传和单个分片做有限重试，失败后重新获取签名地址。进程退出后的取消/清理和分片覆盖仍待验证。WebDAV 的 COPY、锁、递归 PROPFIND、单范围下载和并发/磁盘保护已在适配器层实现；COPY 不代表 WPS 有服务端 COPY API。上游 `401` 会先尝试 WPS SDK 的 `grant_token` 刷新并持久化轮换 Cookie；如果 `rtk` 缺失或已撤销，可从账号所有者的电脑运行 `python3 -m wps_adapter login` 重新建立会话。适配器不会自动代填密码或处理 SSO、验证码和风控。
