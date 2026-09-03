# WPS Enterprise Cloud Adapter

这个仓库用于研究并实现一个面向本人 WPS 企业云盘账号的适配器。目标链路是：

```text
WPS 企业云盘 -> Adapter -> WebDAV / REST -> Windows / Linux / 手机 / NAS
```

当前可运行版本已经接通从本人抓包确认的列目录、流式下载、普通上传、创建目录、删除、重命名和移动流程，并增加了 WebDAV COPY、锁定、递归 PROPFIND、单范围下载和传输保护。程序不会在导入时访问 WPS；只有收到对应请求时才访问上游。

## 边界

- 只使用本人账号在网页端或官方客户端已经能够执行的操作。
- 不绕过权限、验证码、访问控制、租户隔离或文件分享限制。
- 不尝试枚举 ID、扫描接口、重放他人请求或访问其他用户数据。
- 原始 HAR 可能包含 Cookie、Bearer token、签名下载 URL 和文件内容，只保存在本机，不能提交到仓库或直接分享。
- 每个结论都要有一个可重复的本人账号实验作为依据；猜测会明确标注为猜测。

## 已实现

- `PROPFIND`：目录和子项，支持 `Depth: 0/1/infinity`，递归深度和条目数有上限保护。
- 浏览器文件管理页：打开服务根路径即可操作文件和文档。
- `GET` / `HEAD`：文件元数据和流式下载；GET 支持单个 `Range: bytes=...`。
- `PUT`：新文件上传和覆盖更新；适配器只在上传期间使用内存或临时 spool，完成后删除。
- 大文件分片上传：达到阈值后使用已观察的 block/multipart 流程；默认阈值为 50 MiB、分片为 10 MiB。
- 普通上传和单个分片失败后的有限重试；临时 spool 受文件大小和磁盘余量保护。
- `COPY`：文件和文件夹通过流式中继复制，支持 `Depth: 0/1/infinity`。
- `LOCK` / `UNLOCK`：适配器进程内的短期独占写锁，写操作会校验锁令牌。
- 上传、下载并发限制，避免低内存 VPS 被大量并发传输拖垮。
- `DELETE`：删除文件或文件夹；等待 WPS 异步删除任务完成。
- 重命名：REST `PATCH` 和 WebDAV 同目录 `MOVE`。
- REST 的列表、元数据、下载和上传接口。
- REST 和 WebDAV 的删除接口。
- Cookie/CSRF 从环境变量或本机 secret 文件读取。
- Cookie 文件替换后的动态读取；`401` 时可检测文件变化并重试一次；可选调用管理员配置的本地刷新助手。
- `/healthz`、适配器 Basic Auth、systemd 部署骨架。

## 尚未实现

WPS 的跨目录同时改名、快速上传成功路径和真正的 Token 刷新协议尚未由本人账号的独立抓包确认，因此没有伪造 WPS 刷新 API。大文件分片协议已经从本人账号的 100 MiB 上传中观察并由 VPS 适配器真实回放成功；当前增加的是单个请求内的失败重试，不是进程重启后的无条件续传。跨目录同时改名及其他未确认的 WPS 操作会返回 `501`，不会猜测接口。COPY 是适配器层的下载/上传中继，不代表 WPS 提供了服务端 COPY。

## 本地运行

项目只依赖 Python 标准库，不需要安装额外工具。先复制配置模板：

```bash
cp .env.example .env
```

在 `.env` 中填写自己的 `WPS_GROUP_ID`、`WPS_ROOT_ID` 和 secret 文件路径。然后在当前 shell 中加载配置并检查：

```bash
set -a
. ./.env
set +a
PYTHONPATH=src python3 -m wps_adapter check-config
PYTHONPATH=src python3 -m wps_adapter serve
```

默认地址：

```text
网页:   http://127.0.0.1:54321/
WebDAV: http://127.0.0.1:54321/dav/
REST:   http://127.0.0.1:54321/api/v1/
健康检查: http://127.0.0.1:54321/healthz
```

详细接口、VPS 安装和明天的验收步骤见 [docs/api.md](docs/api.md)、[docs/deployment.md](docs/deployment.md) 和 [docs/integration.md](docs/integration.md)。

## 目录

```text
README.md                    项目入口和安全边界
.env.example                 不含凭据的配置模板
deploy/
  wps-adapter.service        systemd 服务单元
captures/                    本地私密 HAR，仅用于实验，始终被 Git 忽略
docs/
  00-scope-and-safety.md       授权边界和敏感信息处理
  01-capture-plan.md           浏览器抓包和最小实验计划
  integration.md               WebDAV/REST 对接、部署和验收清单
  findings.md                  已验证发现的唯一事实记录
  request-record-template.md   单个请求/实验的记录模板
src/wps_adapter/
  har.py                       HAR 读取、摘要和初步脱敏
  provider.py                  远端存储接口和安全错误类型
  storage.py                   WPS ID 与路径解析、短时元数据缓存
  server.py                    WebDAV/REST HTTP 服务
  web.py                      浏览器文件管理页
  __main__.py                  服务入口和配置检查
tools/                         无第三方依赖的抓包辅助工具
tests/                         标准库单元测试
```

`__pycache__/`、`.pyc`、本地环境文件和原始网络抓包都是生成或敏感材料，不属于项目交付内容，已由 `.gitignore` 排除。

## WPS 研究边界

- 只使用本人账号在网页端或官方客户端已经能够执行的操作。
- 不绕过权限、验证码、访问控制、租户隔离或文件分享限制。
- 不枚举 ID、扫描接口、重放他人请求或访问其他用户数据。
- 原始 HAR 可能包含 Cookie、Bearer token、签名下载 URL 和文件内容，只保存在本机，不能提交到仓库或直接分享。
- 每个结论都要有一个可重复的本人账号实验作为依据；猜测会明确标注为猜测。

## 认证安全

不要把 Cookie、CSRF、Basic Auth 密码、预签名对象存储 URL、完整 cURL 或原始 HAR 发到聊天、Issue 或 Git。推荐使用 `WPS_COOKIE_FILE` 和 `WPS_CSRF_TOKEN_FILE`；Cookie 失效时只在本机/VPS 替换文件。适配器不会把 WPS Cookie 转发给对象存储，也不会打印响应正文。

已验证事实记录在 [docs/findings.md](docs/findings.md)，抓包工具说明在 [docs/01-capture-plan.md](docs/01-capture-plan.md)。
