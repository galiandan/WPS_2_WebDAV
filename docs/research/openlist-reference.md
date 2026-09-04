# OpenList 借鉴总览

> 文档类型：外部参考重构、实验流程和功能索引
>
> 更新时间：2026-09-04
>
> 适用版本：当前 `main` 分支

## 1. 文档目标

本项目研究 WPS 企业云盘网页端行为，并把本人账号能够正常执行的文件操作适配为 REST 和 WebDAV。OpenList 只提供公开的实现参考，不能替代 WPS 官方文档，也不能替代本人账号的抓包和重放验证。

原来混在一份长文档中的内容现在按功能拆分：

| 功能 | 独立文档 | 当前结论 |
| --- | --- | --- |
| 登录状态预检 | [`01-login-status-preflight.md`](01-login-status-preflight.md) | 本地状态接口已实现；`islogin` 仍属于外部候选 |
| 企业空间与目录发现 | [`02-enterprise-space-discovery.md`](02-enterprise-space-discovery.md) | 当前使用已保存工作区；自动发现接口待验证 |
| 上传、下载与分片 | [`03-upload-download-multipart.md`](03-upload-download-multipart.md) | 普通和 100 MiB 分片流程已观察并复现 |
| WebDAV 适配 | [`04-webdav-adapter-design.md`](04-webdav-adapter-design.md) | 基本方法、流式下载、Range、COPY/LOCK 已有适配或设计 |

## 2. 参考来源与边界

用户所说的 OneList 应为 OpenList。相关公开来源：

- 项目主页：https://github.com/OpenListTeam/OpenList
- WPS 驱动：https://github.com/OpenListTeam/OpenList/tree/main/drivers/wps
- WPS 驱动文档：https://doc.oplist.org/guide/drivers/wps

允许借鉴的是接口发现思路、对象模型、错误处理经验和兼容性设计。不能直接复制外部项目代码、凭空扩大权限、扫描其他用户数据，或把历史逆向接口称为稳定官方契约。本项目许可证为 GPL-3.0-or-later；外部项目许可证和来源必须单独核对。

所有实验必须满足：

- 只使用账号所有者自己的 WPS 会话和测试目录；
- 不绕过密码、验证码、SSO、风控、权限和租户隔离；
- 不把 Cookie、CSRF、refresh token、签名 URL、原始 HAR 或文件内容提交到仓库；
- 发现接口不代表获得授权，成功请求也不代表对所有租户都兼容。

## 3. 证据等级

| 标记 | 含义 | 可以做什么 |
| --- | --- | --- |
| `external-reference` | 只在 OpenList 或其他公开资料中看到 | 只能形成研究假设 |
| `candidate` | 外部资料给出了可尝试的请求形状 | 只能在本人测试目录中验证 |
| `observed` | 本人账号网页或客户端抓包观察到 | 记录请求，不自动进入默认行为 |
| `reproduced` | 本项目用本人账号重放成功并有测试覆盖 | 可以进入默认实现，但要保留错误处理 |
| `inferred` | 根据多个请求推导，未直接验证 | 不能作为 API 承诺 |
| `unsupported` | 已知不应采用或证据不足 | 不实现、不静默回退 |

每条结论应在 [`findings.md`](findings.md) 中记录来源、时间、账号范围、请求形状、响应摘要、是否重放以及脱敏方式。

## 4. 重构后的研究流程

### 第一步：定义最小目标

先只验证“列目录、上传小文件、下载小文件”。创建文件夹、重命名、移动、删除和 WebDAV 方法在基础链路稳定后逐项加入。大文件分片单独使用可识别的测试文件验证，不能和普通上传混为一个接口。

### 第二步：准备隔离测试数据

在本人有权限的企业云盘目录创建专用测试文件夹。文件名带日期和用途，例如 `adapter-probe-YYYYMMDD`。不使用真实隐私文件，不在日志或提交中保存文件内容。实验记录可保存必要的群组 ID、父目录 ID、文件 ID，但公开文档应尽量脱敏。

### 第三步：浏览器抓包

打开 WPS 网页端开发者工具，在 Network 中只保留 Fetch/XHR，勾选 Preserve log。执行一个动作后立即停止并导出 HAR，不要导出整个浏览历史。优先记录请求方法、路径、参数名、JSON 字段类型、响应结构、对象存储临时 URL、上传指令、ETag 和任务 ID。

### 第四步：脱敏和分类

先把 Cookie、CSRF、refresh token、`AccessKeyId`、`Signature`、`Policy`、对象存储路径、用户信息和文件内容替换为占位符，再写入研究记录。网页实际看到的是 `observed`，OpenList 代码看到的是 `external-reference` 或 `candidate`。

### 第五步：单请求重放

只用本人账号、本人测试目录和短期凭据重放。先做只读请求，再做可逆写操作，最后做删除。每次只改变一个变量，例如父目录、文件名或分片号。记录 HTTP 状态和脱敏后的响应形状，不把浏览器整段 Cookie 粘进代码或命令历史。

### 第六步：实现最小原型

按“控制 API -> WPS 返回的动态对象存储指令 -> 文件登记或任务完成”的边界实现。对象存储 URL、方法、Header、ETag/key 来源必须以 WPS 返回的指令为准，不能把某一次 URL 永久写死。下载由适配器打开临时地址并流式转发，Cookie 不发送到对象存储域名。

### 第七步：加入协议层和回归测试

先让 REST API 通过，再映射到 WebDAV。每加入一个操作，至少补一个正常路径和一个失败路径测试，并验证权限错误、会话过期、重复名称、断线、短读、Range、任务失败和响应大小限制。测试不能依赖真实 Cookie 或真实 WPS 网络。

### 第八步：发布和回滚

更新功能文档、`findings.md`、CHANGELOG、单元测试和发布清单后再提交。安装器必须固定到包含该功能的完整提交及对应清单摘要。发布前运行：

```bash
PYTHONPATH=src python3 -m unittest discover -s tests -v
python3 -m compileall -q src tests tools wps_login.py
python3 tools/build_login_script.py --check
python3 tools/build_release_manifest.py --check
bash -n scripts/install-native.sh scripts/install-docker.sh scripts/uninstall.sh
git diff --check
```

出现 WPS 接口变化时，优先关闭或回退该功能并保留原有已验证路径，不要静默切换到未经验证的个人版或其他群组接口。

## 5. 当前实现基线

```text
网页 / WebDAV / REST
          |
     Adapter Server
          |
      WpsStorage
          |
      WpsDriveClient
          |
  WPS 控制 API + 临时对象存储地址
```

当前已实现或已复现的能力包括：v5 目录列表和建文件夹、v3 重命名、v5 任务移动/删除、普通和分片上传、流式下载、单范围 Range、WebDAV `PROPFIND`/`GET`/`PUT`/`MKCOL`/`DELETE`/`MOVE`，以及本地登录状态预检。细节和证据见四份功能文档与 [`findings.md`](findings.md)。

## 6. 四个方向的取舍

### 6.1 登录状态预检

OpenList 提供 `GET https://account.kdocs.cn/api/v3/islogin` 这一候选思路。本项目将它限制为只读状态检查，不把它当作 refresh token，也不把成功响应当作目标目录有权限。当前 `/api/v1/status` 会区分未配置、会话过期、权限失败、无效响应和上游故障。

### 6.2 企业空间发现

OpenList 的企业群组接口只能作为候选。企业账号可能出现个人接口结果不完整的情况，因此自动发现必须用当前账号新 HAR 验证；在验证前继续使用登录助手得到的工作区和用户明确选择的目录。

### 6.3 动态上传下载

OpenList 的实现说明了“控制接口返回动态指令”的重要性，但其完整缓存文件的方式不符合本项目低内存、尽量不长期存储文件的目标。本项目使用有限内存、临时 spool、磁盘空间保护和流式对象转发。

### 6.4 WebDAV 兼容

OpenList 的文件对象模型可供参考，但 WPS 当前账号实际观察到的移动和删除是 v5 异步任务接口，不能用 v3 候选接口直接替换。COPY、LOCK、`Depth: infinity`、Range 和任务失败语义必须分别验证和限制，不能只因为客户端发来了请求就宣称完整支持。

## 7. 后续推进规则

后续每个功能单独推进：先在对应文档补充本人账号实验记录，再修改代码和测试，最后更新发布清单并发布一个可回滚版本。不要把多个未验证接口一次性接入默认流程。
