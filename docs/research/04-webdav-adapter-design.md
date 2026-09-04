# WebDAV 适配设计

> 文档类型：OpenList 借鉴方向之四
>
> 更新时间：2026-09-04

## 1. 目标

把 WPS 的 ID、目录和临时对象地址转换成客户端可理解的 WebDAV 资源，同时保留 REST API 供调试和脚本使用。设计重点是协议兼容性、错误语义、流式传输和对 WPS 行为的不误判。

## 2. 证据边界

| 内容 | 证据级别 | 说明 |
| --- | --- | --- |
| `GET`、`HEAD`、`PUT`、`MKCOL`、`DELETE`、`MOVE`、`PROPFIND` | observed/reproduced in repository | 当前适配器已有对应路由和测试；WPS 底层行为按 findings 记录区分 |
| WPS v5 task MOVE/DELETE | observed + reproduced | 本人账号网页抓包和适配器回放成功，需轮询 task progress |
| WebDAV COPY 通过适配器中继 | current design | 下载再上传，稳定但不是 WPS 原生复制 |
| WPS v3 batch COPY | candidate | 来自 OpenList 外部参考，当前账号未验证 |
| LOCK/UNLOCK 的本地协议处理 | current design | 用于 WebDAV 客户端兼容；不等于 WPS 云端文件锁 |
| `Depth: infinity` 的完整大目录语义 | protocol requirement/design | 当前实现有深度和数量限制，不应声称无限递归无条件支持 |

## 3. 路径和对象模型

客户端路径由适配器解析为 WPS 父子 ID：

```text
/                         -> 配置的 root_id
/目录/                     -> root_id 下名称为“目录”的 folder ID
/目录/文件.txt              -> folder ID 下名称为“文件.txt”的 file ID
```

显示名称只用于当前目录中的查找；后续操作必须使用已经解析出的 ID 和父目录 ID。路径必须拒绝空组件、`.`、`..` 越权穿越和不一致的尾部斜杠语义。

当前缓存按父目录保存子项，TTL 到期会刷新；缓存还需要总量上限，否则长期遍历大量目录可能造成内存增长。群组 ID 变化时必须使旧群组缓存失效，不能仅依据 `root_id` 判断工作区是否相同。

## 4. 方法映射

| WebDAV 方法 | 适配器行为 | WPS 侧当前证据 |
| --- | --- | --- |
| `PROPFIND` | 返回当前资源或目录成员的 XML 属性 | 由适配器列目录；`Depth` 有 0、1、infinity 限制 |
| `GET` | 解析下载地址并流式转发 | 本人账号下载控制接口已观察并复现 |
| `HEAD` | 返回元数据，不拉取正文 | 由适配器处理 |
| `PUT` | 流式接收并上传；同名时按覆盖策略处理 | 普通和分片上传已观察并复现 |
| `MKCOL` | 创建文件夹 | WPS v5 folder 接口已观察并复现 |
| `DELETE` | 发起 WPS v5 删除任务并轮询完成 | 本人账号已观察并复现 |
| `MOVE` | 发起 WPS v5 移动任务；跨父目录需等待完成 | 本人账号已观察并复现 |
| `COPY` | 当前使用下载再上传的中继复制 | WPS 原生 COPY 仍为候选，未切换默认行为 |
| `LOCK` / `UNLOCK` | 本地短期锁令牌和超时管理 | 当前为适配器协议兼容层，非 WPS 锁定证明 |

## 5. COPY 策略

当前中继 COPY 的流程是：

```text
解析源 -> 打开 WPS 下载流 -> 上传到目标 -> 返回目标属性
```

优点是复用已经验证的下载和上传路径，缺点是会消耗 VPS 与 WPS 之间的双向带宽，且大文件复制时间较长。目标覆盖不能伪装成原子操作；当前实现应在不确定时拒绝覆盖，并返回明确的 WebDAV 错误。

OpenList 参考中的候选接口形状为：

```text
POST /3rd/drive/api/v3/groups/<source-group-id>/files/batch/copy
```

它的字段、重复名称策略、目录递归和返回语义均未在当前账号确认，因此不能直接替换中继路径。未来若抓包验证成功，应先增加可关闭的实验策略，并验证单文件、目录、重复名称、覆盖和失败回滚。

## 6. Depth、锁和范围请求

### 6.1 `Depth`

WebDAV 客户端可能发送 `Depth: 0`、`1` 或 `infinity`。适配器可以接受 `infinity`，但必须受最大递归深度、最大条目数和响应大小限制保护。超限应返回可解释的 4xx/507，而不是无限递归或耗尽 VPS 内存。

### 6.2 `LOCK` / `UNLOCK`

WPS 网盘文件权限和 WebDAV 锁不是同一概念。适配器本地锁至少需要：

- 生成不可猜的锁令牌；
- 绑定资源路径、所有者和过期时间；
- 修改、删除、移动和覆盖时检查锁；
- `UNLOCK` 必须校验令牌；
- 重启后锁是否保留必须在文档中明确，不能声称已同步到 WPS。

### 6.3 Range

下载请求的 Range 只有在 WPS 对象存储实际返回并验证 206 后才能向客户端承诺。应校验 `Content-Range`、实际读取字节数和元数据长度；不支持的多范围请求应明确拒绝，不应拼接未经验证的多个上游请求。

## 7. 错误和可观测性

适配器需要把错误分成三层：

```text
客户端协议错误 -> 400/404/409/412/423/507
WPS 权限或会话错误 -> 401/403，并提供脱敏状态提示
WPS 网络或服务故障 -> 502/503/504
```

网页状态和 WebDAV 错误都不应泄露 Cookie、CSRF、refresh token、企业内部用户信息或签名 URL。日志应记录方法、脱敏路径、阶段、上游状态和 request id（若安全可用）。

## 8. 验收矩阵

1. Windows、Linux、手机和 NAS 至少完成 PROPFIND、GET、PUT、MKCOL、DELETE、MOVE。
2. 小文件 COPY 与大文件 COPY 均能完成，失败时不留下错误的目标文件。
3. `Depth: 0/1/infinity` 在限制范围内返回正确 XML，超限有明确错误。
4. LOCK 后修改被拒绝，正确 token 的 UNLOCK 恢复操作；过期锁自动清理。
5. 单范围断点下载的状态码、长度和内容正确。
6. WPS 未连接时客户端得到稳定的 401/502/503 语义，网页明确显示“WPS 未连接”。

