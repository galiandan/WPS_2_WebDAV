# OpenList 借鉴路线

> 更新时间：2026-09-04
>
> 文档性质：外部参考和后续开发计划，不是 WPS 官方 API 文档。

## 1. 当前状态

登录状态预检、企业空间发现、登录前工作区验证和基础 WebDAV 能力已经实现，相关旧规划文档已移除。分片上传检查点续传也已完成并删除对应任务文档；当前只保留四项未完成优化。

## 2. 优先级

| 优先级 | 功能 | 原因 | 文档 |
| --- | --- | --- | --- |
| P0 | 原生 COPY 剩余范围 | 文件夹复制和覆盖语义仍未完成 | [`01-native-copy.md`](01-native-copy.md) |
| P0 | 大目录分页与 `Depth: infinity` 剩余优化 | 影响 NAS、Nextcloud 和同步软件的目录浏览稳定性 | [`02-large-directory-depth.md`](02-large-directory-depth.md) |
| P1 | 上传并发、缓存和资源保护 | 保护 1.6 GiB 内存 VPS，避免大文件并发导致 OOM 或磁盘耗尽 | [`04-upload-resource-protection.md`](04-upload-resource-protection.md) |
| P1 | 重复文件策略 | 统一 WebDAV、REST、网页上传面对同名文件时的行为 | [`05-duplicate-file-policy.md`](05-duplicate-file-policy.md) |

执行顺序固定为：先补实验记录和设计文档，再做本人账号的最小抓包验证，之后才改代码、补测试、更新发布清单并推送。

## 3. OpenList 借鉴边界

OpenList 的 WPS 驱动可以提供历史接口路径、字段形状、分页模型和兼容性经验，但它不是当前企业租户的契约。来自 OpenList 的内容标记为 `external-reference` 或 `candidate`；只有当前账号网页抓包属于 `observed`，只有本项目用当前账号重放成功才属于 `reproduced`。

任何候选接口都必须满足以下条件才能进入默认实现：

1. 只使用当前账号正常拥有权限的数据；
2. 在专用测试目录中完成低风险验证；
3. 记录请求和响应结构，并删除 Cookie、CSRF、Token、签名 URL 和文件内容；
4. 有失败回退，不因候选接口失效而破坏已有能力；
5. 有自动化测试覆盖正常、权限、会话过期、超时和异常响应。

## 4. 统一研究步骤

### 4.1 准备

为每项功能建立独立测试文件或目录，使用带日期的名称。先写出目标、输入、预期结果和失败后的清理方式，不使用真实隐私文件。

### 4.2 抓包

在 WPS 网页端 Network 中只观察一个动作。记录方法、路径、查询参数名、请求字段类型、响应结构、任务 ID、ETag 和对象存储指令。一个动作完成后立即停止并导出 HAR，不提交原始 HAR。

### 4.3 脱敏

把 Cookie、CSRF、refresh token、`AccessKeyId`、`Signature`、`Policy`、对象 ID、用户信息和文件内容替换为占位符。公开记录只描述结构，不提供可重放凭据。

### 4.4 重放

从只读请求开始，再做可逆写操作，最后做删除。每次只改变一个变量。确认 WPS 返回的状态码、任务完成状态和实际文件结果，不凭 HTTP 200 推断操作已经完成。

### 4.5 原型与测试

控制 API、对象存储临时 URL 和 WebDAV 协议层分开实现。适配器不长期存储文件，不把 WPS Cookie 转发到对象存储域名。每项功能必须有独立回退策略和资源上限。

### 4.6 发布

更新对应功能文档、`findings.md`、测试、CHANGELOG 和发布清单，运行完整测试后单独提交并推送。没有当前账号证据的功能只能停留在文档和候选实现阶段。

## 5. 统一安全要求

- 不扫描、猜测或访问其他用户、群组或租户的数据；
- 不绕过登录、SSO、验证码、风控或权限；
- 不在日志、错误响应、网页或 Git 历史中输出凭据和签名地址；
- 不把“接口返回候选空间”当成“空间可读”，必须执行根目录权限验证；
- 不因为追求简单而取消 HTTPS 警告、Basic Auth、请求大小限制或并发限制；
- 新接口失败时保留当前已验证的实现路径。

## 6. 参考来源

- OpenList 项目：https://github.com/OpenListTeam/OpenList
- WPS 驱动：https://github.com/OpenListTeam/OpenList/tree/main/drivers/wps
- WPS 驱动文档：https://doc.oplist.org/guide/drivers/wps

本项目只借鉴公开资料中的思路和请求形状，不复制外部项目代码，也不把外部项目的兼容性声明扩展到本项目。
