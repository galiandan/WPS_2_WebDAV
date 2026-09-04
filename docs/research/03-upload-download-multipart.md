# 上传、下载与分片流程

> 文档类型：OpenList 借鉴方向之三
>
> 更新时间：2026-09-04

## 1. 目标

复用 WPS 网页端实际使用的控制接口和对象存储临时签名地址，实现：

- 小文件和普通上传；
- 同名覆盖；
- 大文件分片上传；
- 上传失败重试；
- 临时地址流式下载；
- 不把 WPS Cookie 转发给对象存储域名；
- 不在 VPS 长期保存文件。

## 2. 证据等级

| 内容 | 证据级别 | 说明 |
| --- | --- | --- |
| `PUT /3rd/drive/api/v5/files/upload/create_update` | observed + reproduced | 本人账号小文件上传抓包和适配器回放 |
| 对象存储 PUT 使用动态指令中的 URL、Header 和方法 | observed + reproduced | WPS 返回上传指令，当前实现解析并执行 |
| `POST /3rd/drive/api/v5/files/file` 登记文件 | observed + reproduced | 本人账号上传流程和当前实现 |
| `POST /3rd/drive/api/v5/files/upload/block` 初始化分片 | observed + reproduced | 本人账号 100 MiB 文件，10 个 10 MiB 分片 |
| `PUT /3rd/drive/api/v5/files/upload/block` 获取/执行分片指令 | observed + reproduced | 每个分片返回对象存储 PUT 指令 |
| `POST /3rd/drive/api/v5/files/upload/block/merge` 合并分片 | observed + reproduced | 返回 XML 合并指令并完成对象存储合并 |
| `GET /api/v3/office/file/<id>/download` | observed + reproduced | 返回临时下载 URL 的控制请求 |
| OpenList 先完整缓存文件再上传 | external-reference | 外部实现行为，不能直接照搬到低内存 VPS |
| OpenList 其他 v5 下载路径 | candidate | 外部参考，当前项目继续以本人账号下载抓包为准 |

## 3. 小文件上传

本人账号观察到的控制流程：

```text
1. 计算文件校验值和大小
2. PUT /3rd/drive/api/v5/files/upload/create_update
3. 解析 method、url、headers、期望状态码和 key/etag 取值规则
4. PUT 到 WPS 返回的对象存储签名地址
5. POST /3rd/drive/api/v5/files/file 登记文件
6. 用返回对象刷新目录
```

`create_update` 请求包含 `groupid`、`parentid`、`name`、`size`、`contenttype`、`sha1`/校验值、`with_rapid` 和 CSRF 等字段。字段是否全部必需取决于具体路径和账号版本，不能脱离抓包简化为固定官方协议。

WPS 可能先返回 rapid upload 指令；本人账号一次测试中 rapid upload 返回 403，随后普通对象上传成功。因此当前实现把 rapid upload 当作可选路径，失败后回到正常上传。

登记请求的 `key` 必须遵从当前上传指令和响应。不能因为某次测试中对象 key 恰好等于 SHA-1，就把 SHA-1 永久写死为对象 key。

## 4. 100 MiB 分片上传

本人账号已复现的流程如下：

```text
POST /3rd/drive/api/v5/files/upload/block
  -> key, store, upload_id, limit

对每个 part_number：
  PUT /3rd/drive/api/v5/files/upload/block
    -> 对象存储 PUT 指令
  PUT <signed multipart URL>
    Content-MD5: <base64 MD5>
    Content-Type: application/octet-stream
    -> ETag

POST /3rd/drive/api/v5/files/upload/block/merge
  -> application/xml 的 CompleteMultipartUpload 指令
POST <signed complete URL>
  -> 合并 ETag

POST /3rd/drive/api/v5/files/file
  -> 正式文件记录
```

本人测试使用 10 个 10 MiB 分片；观察到 WPS 返回的最小分片大小为 5 MiB。实际实现应使用响应中的 `limit`，不能把 10 MiB 当成永久协议常量。

每个分片的 ETag 必须来自对象存储响应，并按 WPS 返回的 merge XML 规则提交。合并成功后仍然必须执行文件登记，否则对象存储中的对象未必会出现在云盘目录中。

## 5. 下载和流式转发

本人账号观察到的主流程：

```text
GET /api/v3/office/file/<file-id>/download
    ?support_checksums=...
    &get_direct_external_download_url=true|省略
    &cid=<file-level-link-id>
    -> JSON download_url/url

GET <WPS signed object URL>
    -> 文件内容
```

适配器只把第二步的字节流转发给 REST/WebDAV 客户端，不把签名 URL 交给客户端。对象存储请求不携带 WPS Cookie。签名 URL 的主机必须经过允许列表校验，并且必须是 HTTPS。

当前实现支持单范围 Range，但只有在配置启用且上游返回 206、Content-Range 与请求一致时才接受。下载响应仍需要继续加强实际字节数与 WPS 元数据长度的校验。

## 6. 重试、资源和失败处理

- 控制请求和对象上传使用有限次数重试；
- 分片失败时只重试当前分片，不重新上传已经成功的分片；
- 合并失败不能假定对象已登记成功；
- 未完成的 multipart upload 清理和断点续传目前仍待验证；
- 上传输入使用有限内存 spool，`WPS_UPLOAD_SPOOL_MEMORY=0` 不能被解释成“自动全部落盘”；
- 通过并发限制和最大上传大小保护 1.6 GiB 内存 VPS；
- 日志只记录阶段、分片号、大小和错误类别，不记录签名 URL、Cookie 或原始请求体。

## 7. 验收标准

1. 小文件上传后目录可见，下载内容和本地源文件校验一致。
2. 同名覆盖保持既有文件 ID，并且下载得到新内容。
3. 100 MiB 文件按 WPS 返回的分片限制上传，合并和登记均成功。
4. 对象存储请求不带 WPS Cookie。
5. 下载端收到正确的 `Content-Length`、`Accept-Ranges` 和已验证的 Range 响应。
6. 中途失败不会无限重试、无限占用内存或把临时文件长期留在 VPS。

