# WPS 请求与行为发现

这份文件只记录已经从本人账号实验中观察或复现的事实。没有抓包之前，不预填 WPS 的域名、路径、字段名或认证方案。

## 当前状态

- 阶段：核心读写请求采集完成，已实现 WebDAV/REST 外层的最小原型；大文件分片已完成一次 VPS 真实回放。
- WPS 请求：已采集目录列表、小文件上传、小文件下载和 100 MiB 分片上传成功后的请求序列。
- 已确认的 WPS API：`GET /3rd/drive/api/v5/groups/<group-id>/files` 可返回指定父目录的列表；上传流程中的 `pre_check` 成功响应为 `{"result":"ok"}`；已观察到浏览器通过 Cookie 会话访问这些接口。
- 原型网络调用：按需启用；导入模块和 `/healthz` 不访问 WPS。

## 发现表

| 编号 | 操作 | 观察到的事实 | 证据 | 原始材料位置 | 待验证 |
| --- | --- | --- | --- | --- | --- |
| L-01 | 打开本人测试文件夹 | `GET /3rd/drive/api/v5/groups/<group-id>/files` 返回 `{files: [...], next_filter, next_offset, result}`；列表项含 `groupid`、`parentid`、`fname`、`fsize`、`ftype`、`ctime`、`mtime`、`fver`、`fsha`、`deleted`、`id`、权限对象等字段；状态码为 `200` | observed | 用户提供的请求信息和响应；账号/对象标识已替换，原文不入库 | 认证位置、文件夹项形状、分页边界、最小必需 query 参数 |
| U-01 | 向本人测试文件夹上传小文件 | UI 显示上传完成；已确认 `PUT /3rd/drive/api/v5/files/upload/create_update` 为 JSON 控制接口；浏览器先尝试 rapid upload（本次 `403`），随后执行签名对象存储 PUT、`POST /3rd/drive/api/v5/files/file` 和目录刷新 | observed | 用户提供的安全 HAR 摘要和响应；文件名、账号/对象标识和签名值不入库 | `create_update` 响应字段、rapid upload 成功条件、是否分片 |
| D-01 | 从本人测试文件夹下载小文件 | `GET /api/v3/office/file/<file-id>/download`（200），查询参数名为 `support_checksums`、`get_direct_external_download_url`、`cid`；随后出现带 `response-content-disposition=attachment` 和签名参数的对象存储 `compatible` 请求（200） | observed | 用户提供的安全 HAR 摘要和 Network 截图；下载签名 URL 不入库 | `download` 响应字段、请求头、Range 行为、流式下载细节 |
| F-01 | 在本人测试文件夹创建空文件夹 | `POST /3rd/drive/api/v5/files/folder`，JSON 请求体含 `groupid`、`parentid`、`name`、`owner`、`parsed`、`csrfmiddlewaretoken`；响应为 `result=ok` 的文件夹对象 | observed | 用户提供的 Network 请求和响应；CSRF、姓名和对象标识不入库 | 重复名称、非法名称、父目录权限错误 |
| X-01 | 删除本人测试文件夹 | `POST /3rd/drive/api/v5/files/batch/task/delete` 提交 `fileids`、`groupid`、`csrfmiddlewaretoken`；响应返回 `taskid`、`taskuuid`；随后轮询 `GET /3rd/drive/api/v5/files/batch/task/progress?taskuuid=...`，响应 `finish=1`、`status=success`、`result=ok` | observed + reproduced | 用户提供的 Network 请求/响应；测试目标为本人此前创建的空文件夹；敏感值和对象标识不入库 | 非空目录行为、批量删除、回收站保留时间 |
| R-01 | 重命名本人测试文件夹 | `PUT /3rd/drive/api/v3/groups/<group-id>/files/<file-id>`，JSON 请求体含 `fname`、`csrfmiddlewaretoken`；状态码为 `200`，响应直接返回更新后的文件夹对象 | observed + reproduced | 用户提供的 Network 请求/响应，以及适配器 REST 重放结果；名称、对象标识和 CSRF 不入库 | 文件重命名、重复名称、非法名称、跨目录移动 |
| M-01 | 将本人测试文件夹移动到本人测试子目录 | `POST /3rd/drive/api/v5/files/batch/task/move` 提交 `groupid`、`parentid`、`dst_groupid`、`dst_parentid`、`fileids`、`option`、`csrfmiddlewaretoken`；响应返回 `taskid`、`taskuuid`；随后轮询进度接口并得到 `finish=1`、`status=success`、`result=ok` | observed + reproduced | 用户提供的 Network 请求/响应，以及适配器 REST 回放和目录列表结果；源/目标对象标识和 CSRF 不入库 | 批量移动、跨目录同时改名、重复名称、跨企业空间移动 |
| O-01 | 覆盖本人测试文件 | 同名上传选择“覆盖”后，`create_update` 与 `file` 均为 `200`；最终对象保持原 `id`，`fver` 从 `1` 变为 `2`，`ctime` 保持、`mtime` 更新；请求含 `md5`、`successactionstatus=201` 等字段 | observed + reproduced | 用户提供的 Network 请求/响应，以及适配器 REST 覆盖和下载校验结果；文件名、对象标识、校验值、CSRF 和签名 URL 不入库 | 版本历史、并发覆盖、超大文件覆盖、覆盖文件夹 |
| U-02 | 上传本人生成的 100 MiB 测试文件 | `POST .../files/upload/block` 初始化；10 个 10 MiB 分片分别经 `PUT .../files/upload/block` 获取指令并上传到签名对象存储；`POST .../files/upload/block/merge` 获取合并指令并提交 `CompleteMultipartUpload` XML；最后 `POST .../files/file` 登记正式文件 | observed + reproduced | 用户提供的 Network 请求/响应；VPS 适配器上传后再下载，大小和 SHA-256 与原始测试文件一致；测试文件名、账号/对象标识、上传 ID、签名 URL、校验值和 ETag 原值不入库 | 失败续传、取消/清理、超大文件和分片覆盖 |

### L-01 请求形状

```text
method: GET
path: /3rd/drive/api/v5/groups/<group-id>/files
status: 200
query:
  parentid: <own-folder-id>
  linkgroup: true
  include: acl,pic_thumbnail
  with_link: true
  review_pic_thumbnail: true
  with_sharefolder_type: true
  offset: 0
  count: 20
  orderby: mtime
  order: desc
```

本次观察到的主机为 `365.kdocs.cn`。请求中的企业空间 ID 和文件夹 ID 属于本人账号上下文，记录中不保留原值。当前不能断言所有 query 参数都是必需的；后续只在本人测试目录中逐项验证。

## L-01 目录列表响应

当前已观察到的响应结构（所有账号/对象标识均为占位符）：

```json
{
  "files": [
    {
      "groupid": "<own-group-id>",
      "parentid": "<own-folder-id>",
      "fname": "<test-file-name>",
      "fsize": 11890,
      "ftype": "file",
      "ctime": "<unix-timestamp>",
      "mtime": "<unix-timestamp>",
      "store": "<observed-number>",
      "storeid": "",
      "fver": 2,
      "fsha": "<40-hex-checksum>",
      "deleted": false,
      "id": "<own-file-id>",
      "creator": {
        "id": "<own-user-id>",
        "name": "<redacted>",
        "avatar": "<redacted-url>",
        "corpid": "<own-tenant-id>"
      },
      "modifier": "<same-shape-as-creator>",
      "file_acl": {"modify": "1"},
      "admin_file_perm": true,
      "file_perms_acl": {
        "read": 1,
        "download": 1,
        "upload": 1,
        "delete": 1,
        "rename": 1,
        "move": 1
      },
      "link_id": "<redacted-share-id>",
      "link_url": "<redacted-share-url>"
    }
  ],
  "next_filter": "file",
  "next_offset": -1,
  "result": "ok"
}
```

### 已确认字段含义

| 字段 | 当前结论 | 证据等级 |
| --- | --- | --- |
| `files` | 当前父目录下的对象数组 | observed |
| `groupid` | 当前企业空间/群组标识候选 | inferred |
| `parentid` | 当前对象的父目录标识 | observed |
| `fname` | 显示名称候选 | observed |
| `fsize` | 文件大小，响应中为字节数候选 | inferred |
| `ftype` | 文件类型；当前样本为 `file` | observed |
| `ctime` / `mtime` | 创建/修改时间的 Unix 时间戳候选 | inferred |
| `id` | 文件对象标识候选 | observed |
| `fsha` | 内容校验值候选，具体算法未确认 | inferred |
| `deleted` | 删除状态布尔值 | observed |
| `file_perms_acl` | 细粒度操作权限，当前样本中包含读、下载、上传、删除、重命名、移动等权限 | observed |
| `next_offset` | 当前为 `-1`，表示没有下一页的候选信号 | inferred |
| `result` | 成功响应为 `ok` | observed |

### 尚未确认

- `ftype` 的文件夹实际值。
- `groupid`、`corpid` 和 `id` 的边界及相互关系。
- `next_filter` 的含义，以及 `offset` / `count` 的分页规则。
- `fsha` 的算法和是否可用于上传校验。
- `link_id` / `link_url` 是否与普通下载流程有关；在适配器中暂不使用分享链接。

## 认证

| 项目 | 结论 | 证据 | 备注 |
| --- | --- | --- | --- |
| 会话位置 | L-01 请求通过 Cookie 携带会话上下文 | observed | 只记录字段名，不记录值；原始值已按安全要求作废 |
| Authorization 头 | L-01 中未观察到 | observed | 不能据此推断写请求也不需要其他认证头 |
| CSRF | 名为 `csrf` 的 Cookie 存在 | observed | 写请求是否需要对应 header/body 字段待验证 |
| 刷新方式 | WPS 账号 SDK 使用 `POST /passport/secure/api/grant_token`，JSON 体为 `{"grant_type":"refresh_token"}` | observed | 公开 SDK 脚本中的请求形状；需要 `rtk` Cookie；本人的成功响应值不记录 |
| 刷新票据 Cookie | 本机 Chrome Cookie 元数据显示 `rtk` 位于 `.kdocs.cn`、路径 `/passport/secure`，HttpOnly 且为持久 Cookie | observed | 只记录名称和属性，不记录值；普通云盘 API 请求可能不会携带此 Cookie |
| 会话轮换 | WPS SDK 续期后依赖浏览器 Cookie 更新；适配器已持久化 WPS 响应中的 `Set-Cookie` | inferred + local prototype | 现有业务 HAR 未保留 `Set-Cookie` 值；仍需本人账号一次真实续期验收 |
| 失败表现 | 未知 | - | - |

## 目录与对象模型

| 字段/行为 | 结论 | 证据 | 备注 |
| --- | --- | --- | --- |
| 文件 ID | 列表/上传完成对象中的 `id` | observed | 原值不入库 |
| 文件夹 ID | `metadata.fileinfo.fileid` / 列表对象 `id` 候选 | observed | 原值不入库 |
| 父目录字段 | `parentid` | observed | - |
| 下载上下文 ID | 列表/文件对象中的 `link_id` 与下载请求 query 的 `cid` 形状一致 | observed | 适配器按文件传递，不记录原值 |
| 分页 | `offset`、`count`，响应有 `next_offset` | observed | 完整分页边界待验证 |
| 文件/文件夹区分 | `ftype`；已观察到 `file` 和 `folder` | observed | 其他类型待验证 |

## 上传与下载

| 能力 | 结论 | 证据 | 备注 |
| --- | --- | --- | --- |
| 小文件上传 | 已成功；包含预检查、对象存储 PUT 和文件登记阶段 | observed | - |
| 快速上传 | 请求存在；本次 `POST /api/v7/drives/<drive-id>/files/<opaque-id>/rapid_upload` 返回 `403`，随后回退普通上传 | observed | 当前测试文件未走快速上传 | 成功条件和对象 ID 来源 |
| 分片上传 | 已观察到并由 VPS 适配器回放成功的 100 MiB block/multipart 流程 | observed + reproduced | U-02；默认按 50 MiB 阈值、10 MiB 分片选择；上传后下载校验一致 | 失败续传、取消/清理、分片覆盖 |
| 上传完成确认 | `POST /3rd/drive/api/v5/files/file` 返回正式文件对象 | observed | 请求体已记录字段名 |
| 下载临时 URL | `GET /api/v3/office/file/<file-id>/download` 返回短期下载地址 | observed | 不保留地址值 |
| Range/续传 | 未知 | - | - |

### U-01 上传请求序列（初步）

这次小文件上传成功，浏览器 Network 面板显示了以下候选阶段。顺序来自单次观察，不能把请求名称直接当作已确认 API 语义：

```text
pre_check       上传前检查，截图中出现 filename、group_id 参数名
metadata        元数据相关请求，出现 with_link=true
create_update   `PUT /3rd/drive/api/v5/files/upload/create_update`，JSON 控制请求
rapid strategy  `GET /api/v7/drives/<drive-id>/files/<opaque-id>/rapid_upload/strategy?size=...`
rapid upload    `POST /api/v7/drives/<drive-id>/files/<opaque-id>/rapid_upload`，本次响应 `403`
<signed-request>  URL 中出现 AccessKeyId、Signature 等签名参数名
file            文件相关请求
path            路径相关请求
<numeric-id>    名称为数字的请求，具体作用未知
files           上传完成后的目录刷新
```

`pre_check` 的响应已观察为 `{"result":"ok"}`，未返回文件 ID、上传地址或分片信息；它当前只确认上传可以继续的候选含义。

`metadata?with_link=true` 的响应已观察为 `{fileinfo, folderinfo, result, user_acl}`。其中 `fileinfo.ftype` 为 `folder`，`fileinfo.parentid` 为 `0`，`fileinfo.fsize` 为 `0`；`folderinfo.modify` 为字符串 `"1"`。本次样本的 `user_acl` 包含 `upload`、`download`、`delete`、`rename`、`move` 等权限字段，具体权限必须以每个对象的实际响应为准。姓名、对象 ID 和分享链接不入库。

`create_update` 的响应已观察为一个上传指令对象：`method` 为 `PUT`，请求头要求 `content-type: application/octet-stream`，成功状态期望为 `200`，结果需要从响应头 `ETag` 和 `x-obs-save-key` 取值；`store` 为 `obscn`；快速上传检查支持 `md5`、`sha1`、`sha256`。响应还包含短期对象存储预签名 URL，但 URL、AccessKeyId、Policy、Signature 和其中的对象键均为敏感数据，不入库。当前仍需确认普通 `file` / `path` 请求分别承担上传确认、元数据登记还是目录刷新。

`file` 请求已确认是 `POST /3rd/drive/api/v5/files/file`，状态码为 `200`。它的响应是一个正式文件对象（顶层直接是对象，不是 `files` 数组），并包含 `result: ok`。安全保留的字段结构为：`id`、`groupid`、`parentid`、`fname`、`fsize`、`ftype: file`、`fver`、`fsha`、`ctime`、`mtime`、`deleted`、`store`、`storeid`、`creator` / `modifier` 字段结构和 `link_id` / `link_url` 字段。原始名称、用户资料、分享链接和所有对象标识不入库。该响应支持“上传成功后返回正式文件元数据”的结论，但请求体仍需确认，暂不单独证明它是上传确认接口。

`POST /3rd/drive/api/v5/files/file` 的请求体字段名已观察为：`key`、`groupid`、`parentid`、`name`、`parent_path`、`sha1`、`size`、`store`、`etag`、`isUpNewVer`、`apiErrorInfo` 和 `csrfmiddlewaretoken`。本次观察中 `key` 与 `sha1` 的值相同，具体是否始终使用 SHA-1 作为对象键仍需用新的中性测试文件验证；`etag` 来自前一步对象存储响应的候选结论。`csrfmiddlewaretoken` 是敏感值，只记录字段名，不记录值。真实文件名、账号/对象标识、校验值和 ETag 原值不入库。

第二次 U-01 安全 HAR 还确认 `create_update` 请求体字段名为：`groupid`、`parentid`、`parent_path`、`size`、`name`、`req_by_internal`、`client_stores`、`contenttype`、`startswithfilename`、`successactionstatus`、`group_id`、`parent_id`、`file_id`、`with_rapid`、`tried_store`、`sha256` 和 `csrfmiddlewaretoken`。字段类型已观察到：目录/文件 ID 与大小为 number，路径和尝试存储列表为 array，快速上传和内部请求标志为 boolean，内容类型、客户端存储和文件名前缀为 string；具体值尚未作为项目事实固定。

该 HAR 还观察到 rapid upload strategy 请求返回 `200`，但 rapid upload 请求返回 `403`；随后 `create_update` 返回 `200`，对象存储 `ks3_compatible` 请求使用 `PUT` 和 `application/octet-stream` 并返回 `200`，最后执行 `POST /3rd/drive/api/v5/files/file`。这证明本次上传走了普通上传回退路径，不证明分片上传能力。

安全记录：签名请求只保留参数名，不保留 URL 值；上传内容、Cookie、Token 和签名不得进入仓库。小文件流程是“控制请求 + 对象存储 + 文件登记”的组合；U-02 进一步观察到分片初始化、逐片控制/直传、对象存储合并和文件登记的组合，但仍不能据此推断失败续传或其他账户的行为。

### U-02 大文件分片上传

本人重新生成了一个正好 100 MiB 的中性测试文件，并在本人企业测试目录中上传。Network 面板观察到以下顺序：

```text
POST /3rd/drive/api/v5/files/upload/block
  JSON: with_rapid, hash, size, group_id, name, parent_id, tried_store,
        csrfmiddlewaretoken
  response: key, store, upload_id, limit, rapid_upload_checksums, result

for part_number = 1..10:
  PUT /3rd/drive/api/v5/files/upload/block
    JSON: key, md5, part_number, part_size, req_by_internal, store,
          upload_id, csrfmiddlewaretoken
    response: method=PUT, request.body_type=file,
             request.headers.Content-MD5, request.headers.Content-Type,
             response.expect_code=[200], url=<signed-object-storage-url>
  PUT <signed-object-storage-url>
    Content-Type: application/octet-stream
    Content-MD5: <base64-part-md5>

POST /3rd/drive/api/v5/files/upload/block/merge
  JSON: key, req_by_internal, store, part_infos, upload_id,
        csrfmiddlewaretoken
  response: method=POST, request.body_type=data,
           request.headers.Content-Type=application/xml,
           request.body_data=<CompleteMultipartUpload>...,
           url=<signed-object-storage-url>
POST <signed-object-storage-complete-url>
  XML: CompleteMultipartUpload with one Part/ETag/PartNumber entry per part

POST /3rd/drive/api/v5/files/file
  JSON: key, groupid, parentid, name, parent_path, sha1, size, store,
        etag, isUpNewVer, apiErrorInfo, csrfmiddlewaretoken
```

本次观察中服务端返回的分片限制包含 `min_part_size=5242880`、`max_part_size=5368709120` 和 `max_parts=10000`；浏览器实际使用 10 MiB 分片，共 10 片。每片控制请求中的 `md5` 是十六进制值，对象存储请求使用对应的 Base64 `Content-MD5`；对象存储返回的 ETag 被放入合并 XML 和后续 `part_infos`。合并成功后，文件登记请求返回正式文件对象，大小为 100 MiB，`key` 与 `sha1` 在该样本中相同。

这条记录的证据等级为 `observed + reproduced`：浏览器端已成功完成该流程，VPS 适配器也已按同一请求形状上传并下载校验成功。一次早期回放曾在合并指令读取阶段收到不完整的上游指令并返回 `502`，随后重试回放成功；当前不把失败重试、续传或清理语义当作已确认能力。签名 URL、上传 ID、文件 ID、文件名、校验值、ETag、Cookie 和 CSRF 值均不记录。

### D-01 下载请求序列（初步）

本人测试文件下载成功后，Network 面板观察到：

```text
GET /api/v3/office/file/<file-id>/download?support_checksums=...&get_direct_external_download_url=...&cid=...  (XHR, 200)

Follow-up verification: on the deployed test account, omitting
`get_direct_external_download_url` produced `result=unSupport` (HTTP 403),
while the same confirmed file request with
`get_direct_external_download_url=true` and the captured `cid` returned HTTP
200 with a signed download URL. The adapter now sends this flag by default;
the signed URL itself is never logged or stored by the adapter.
compatible?response-content-disposition=attachment...&Signature=...  (document, 200)
```

此外还有 `metadata?from=preview...` 和若干 `configure` 请求，暂视为 UI 配置/元数据依赖，不能当作下载主流程。`download` 请求明确带有校验算法参数；其响应已观察到 `download_url`、`url`、`fize`、`fver`、`store` 和 `status` 字段，其中 `download_url` 与 `url` 都是对象存储兼容接口的短期预签名下载地址，`status` 为 `finished`。响应中 `fize` 的拼写按原样记录，是否为服务端固定字段仍待验证。完整请求头、请求体和 Range 行为仍待确认。签名下载 URL 只记录存在，不记录 URL、AccessKeyId、Expires、Signature 或对象键。

安全 HAR 结构摘要进一步确认：`download` 请求为无请求体的 `GET`，响应状态为 `200`、Content-Type 为 `application/json`；后续对象存储响应为 `application/octet-stream`。当前 HAR 没有显示 `Authorization` 请求头；Cookie 可能由 HAR 的独立 cookies 字段保存，因此不能仅凭 header_names 判断会话是否不存在。

后续验证发现，适配器此前使用固定企业 `cid` 时，`.txt` 和 `.docx` 请求会返回 `HTTP 403`、`result=UnSupportFileType`；改用列表对象中的文件级 `link_id` 后，浏览器捕获的 `.txt` 请求形状（省略 `get_direct_external_download_url`）返回 `200`，已有 `.docx` 也返回 `200`。另一些文件（本次 `.har` 和 `.json` 样本）在省略该参数时返回 `403`，加入已观察的 `get_direct_external_download_url=true` 后返回 `200`。适配器现在优先使用文件级 `link_id` 和无该参数的请求，遇到 `403` 时只重试一次已观察的 `true` 变体；固定全局 `cid` 仅作为没有 `link_id` 时的显式后备配置。

### X-01 删除请求序列

本人在网页端删除此前由适配器创建的空测试文件夹，观察到删除由异步任务完成：

```text
POST /3rd/drive/api/v5/files/batch/task/delete
  JSON: fileids, groupid, csrfmiddlewaretoken
  response: result, taskid, taskuuid
GET /3rd/drive/api/v5/files/batch/task/progress?taskuuid=<taskuuid>
  response: finish, status, result, failed_list, total, taskuuid 等
```

本次进度响应为 `finish=1`、`status=success`、`result=ok`、`failed_list=null`、`total=1`。适配器提交单个文件或文件夹 ID，并轮询进度；只有任务完成且失败列表为空或为 `[]` 时才报告成功。任务失败、失败列表非空或超时会报告上游错误，不会误报为删除成功。根路径删除在适配器本地直接拒绝。

这条记录证明了本人账号下单个测试对象的删除流程，不证明批量删除、非空文件夹级联删除、回收站语义或恢复接口。

### R-01 重命名请求序列

本人在网页端将测试文件夹改名，观察到一次同步更新请求：

```text
PUT /3rd/drive/api/v3/groups/<group-id>/files/<file-id>
  JSON: fname, csrfmiddlewaretoken
  response: 更新后的文件对象，包含 groupid、parentid、fname、ftype、id、mtime 等字段
```

本次响应状态为 `200`，响应直接是更新后的文件夹对象，没有额外的任务轮询。该事实只确认了单个对象重命名；没有确认跨目录移动的请求或语义。适配器的 REST `PATCH` 使用 `name` 作为外层字段，并将其映射为上游要求的 `fname`；WebDAV 同目录 `MOVE` 使用目标路径最后一个组件作为新名称。

随后通过已部署适配器调用 REST `PATCH /api/v1/entries?path=...` 重放同一操作，返回 `HTTP 200` 和更新后的对象；再用 WebDAV 同目录 `MOVE` 重放，返回 `HTTP 201`。两次名称都与 WPS 网页端操作一致，证明当前单对象重命名的 REST 和 WebDAV 适配链路已复现。

### O-01 覆盖更新请求序列

本人在网页端向已有同名测试文件上传新内容并选择“覆盖”，观察到：

```text
GET /3rd/drive/api/v5/files/upload/pre_check?...  (本次 403，流程继续)
PUT /3rd/drive/api/v5/files/upload/create_update  (200)
  JSON: md5, client_stores, startswithfilename, successactionstatus=201,
        file_id=0, tried_store, with_rapid, sha256 等
<signed-object-storage-request>  (URL 和签名不记录)
POST /3rd/drive/api/v5/files/file  (200)
  JSON: key, sha1, name, size, etag, isUpNewVer=false 等
```

最终响应是原文件对象：`id` 不变，`fver` 从 `1` 变为 `2`，创建时间保持而修改时间更新；内容校验值和大小随新内容变化。该观察证明 WPS 的同名覆盖会生成新版本，而不是创建新文件。随后通过 VPS 适配器的 REST `PUT`（`overwrite=true`）覆盖同一测试文件，返回 `HTTP 201`，再下载并与本地新内容比较一致，覆盖链路已复现。

### M-01 移动请求序列

本人在网页端将测试文件夹移动到另一个本人测试目录，观察到异步移动任务：

```text
POST /3rd/drive/api/v5/files/batch/task/move
  JSON: groupid, parentid, dst_groupid, dst_parentid, fileids, option, csrfmiddlewaretoken
  response: result, taskid, taskuuid
GET /3rd/drive/api/v5/files/batch/task/progress?taskuuid=<taskuuid>
  response: finish, status, result, failed_list, total, taskuuid 等
```

本次任务的 `finish=1`、`status=success`、`result=ok`、`failed_list=null`、`total=1`。适配器提交单个对象的源父目录、目标父目录和对象 ID，并等待任务成功；源名称会保留。适配器 REST `PATCH` 回放返回 `HTTP 200`，随后目标目录列表能看到被移动的测试文件。WebDAV `MOVE` 的目标路径最后一个组件必须与原名称一致，REST 可以使用 `parent_path` 或完整 `destination`。本次实验不证明批量移动、跨目录同时改名或跨企业空间移动。

## A-01 适配器兼容性与稳定性

以下是适配器自身的实现结论，不是对 WPS 私有 API 的新猜测：

| 能力 | 当前结论 | 证据等级 | 边界 |
| --- | --- | --- | --- |
| WebDAV `COPY` | 已实现；文件通过下载流再上传，文件夹按目录递归创建 | local prototype | 没有宣称 WPS 有服务端 COPY 接口；复制过程中失败可能留下已完成的部分目标树 |
| `LOCK` / `UNLOCK` | 已实现进程内独占写锁和 `If`/`Lock-Token` 校验 | local prototype | 服务重启、超时或多进程部署后锁不保留；未调用 WPS 锁接口 |
| `Depth: infinity` | 已实现递归 `PROPFIND` | local prototype | 默认最多 10000 个条目、64 层；超出返回 `507` |
| Range 下载 | 适配器会转发单范围请求，并要求对象存储实际返回 `206` | local prototype | WPS 侧 Range 行为仍需本人账号独立抓包确认；多范围不支持 |
| 上传失败恢复 | 普通上传和单个分片在当前请求内有限重试，并重新获取签名地址 | local prototype | 进程退出后的跨请求续传、分片取消/清理未确认 |
| 资源保护 | 默认 2 个上传、4 个下载；spool 受内存、文件大小和磁盘余量限制 | local prototype | 参数可通过环境变量调整；返回 `503`/`507` 时客户端需要重试或降低并发 |

## A-02 凭据续期边界

本轮从公开的 WPS 账号 SDK `https://ac.wpscdn.cn/account/libs/js/kso-acct-sdk.min.563070dd.js` 中观察到 `checkKsoSid` 在会话 Cookie 条件满足时调用 `POST /passport/secure/api/grant_token`，请求体为 `{"grant_type":"refresh_token"}`；SDK 的账号域名推导规则对 `365.kdocs.cn` 得到 `account.kdocs.cn`。本机 Chrome Cookie 元数据还显示存在路径为 `/passport/secure` 的 HttpOnly 持久 Cookie `rtk`。这些观察只记录字段、路径和属性，不记录任何认证值。

适配器 `0.4.0` 在上游 `401` 时先检查 secret 是否已被管理员替换；否则使用当前完整 Cookie 调用该刷新授权，并将响应 `Set-Cookie` 原子合并回 Cookie 文件，再重试原请求一次。WPS 正常响应中的 Set-Cookie 也会被持久化。没有 `rtk`、刷新票据已撤销或需要交互式登录时，服务返回 `503` 和上游状态 `401`；适配器不处理密码、SSO、验证码或风控。公开 SDK 代码证明了请求形状，但本人的真实续期成功响应仍需在 VPS 上做一次低频验收。

## A-03 本地交互式登录引导

普通适配器网页不能直接读取 `365.kdocs.cn` 的登录 Cookie：两个页面不同源，且关键的 `rtk` Cookie 为 HttpOnly。`0.5.0` 的本地 `wps_login.py` 助手使用 Chrome/Chromium 的本地 DevTools Protocol 启动临时隔离配置，用户在官方 WPS 页面完成登录后，助手自动检测会话中的 Cookie，只保留匹配云盘主机和 `kdocs.cn` 域名后缀的 Cookie，并要求同时存在 `rtk` 和 `csrf`。现在助手还读取当前官方页面地址：默认接受 `/space/<企业ID>/<群组ID>` 或带有 WPS 自动恢复文件夹的 `/space/<企业ID>/<群组ID>/<文件夹ID>`，但只保存企业云盘根目录 `root_id=0`；使用 `--workspace-url` 时才严格选择具体文件夹。优先通过受 Basic Auth 保护的 HTTPS 接口发送 Cookie 快照和工作区选择，SSH 标准输入仍可作为备用通道；Cookie、CSRF 和工作区文件都以临时文件加重命名方式更新。Cookie 值不进入命令参数、日志或仓库。此流程是本人账号的交互式引导，不代填密码，不绕过 SSO、验证码或风控。

## 记录规则

1. 每条结论绑定一个实验编号，例如 `L-01` 或 `U-01`。
2. `observed` 只表示在浏览器请求中看到；`reproduced` 才表示原型已经成功重放。
3. 请求路径可保留结构，但认证值、签名值和个人信息必须替换。
4. 如果同一字段在不同接口含义不同，分别记录，不用一个猜测覆盖全部接口。
