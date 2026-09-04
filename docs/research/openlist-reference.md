# OpenList WPS 参考与四方向演进设计

> 文档类型：外部项目调研、设计方案和实验计划
>
> 更新时间：2026-09-04
>
> 适用版本：当前 main 分支

## 1. 文档目的

本文件整理 OpenList 的 WPS 驱动，并把其中值得借鉴的四个方向转化为本项目后续可以执行、验证和回滚的设计：

1. 使用 islogin 做 WPS 登录状态预检。
2. 自动发现企业群组，尽量不让用户手工填写群组 ID。
3. 验证并接入 WPS 原生文件操作，尤其是服务端 COPY。
4. 正确区分企业云盘根目录、群组和用户指定的目标文件夹。

本文件不是 WPS 官方 API 文档，也不是对其他用户或租户进行测试的授权。OpenList 的实现和文档只能作为公开外部参考。任何要进入适配器默认行为的 WPS 请求，都必须先在账号所有者自己的测试目录中重新确认。

## 2. 结论摘要

OpenList 的 WPS 驱动证明了以下事实具有较强的外部参考价值：

- WPS 云盘网页端可以通过 Cookie 会话访问一组文件管理接口。
- 个人版和商业/企业版使用的 API 路径不完全相同。
- 企业账号可以从账号状态中取得企业上下文，再通过企业接口列出用户可见群组。
- 创建文件夹和重命名接口与本项目当前抓包结果一致。
- WPS 存在服务端复制、移动和删除的候选接口。
- 上传控制接口返回动态上传指令，调用方不应把对象存储 URL、方法和字段全部写死。

但 OpenList 不能直接替代当前项目：

- 它仍然要求手工复制 Cookie，没有解决本项目的低门槛登录同步和 rtk 自动续期。
- 它的上传实现会先完整缓存文件；这不符合本项目尽量不在 VPS 长期保存文件的目标。
- 它使用的部分 v3 文件操作接口与当前账号已经观察到的 v5 异步任务接口不同。
- 它的 WPS 驱动属于 AGPL-3.0 项目，本项目是 GPL-3.0-or-later，不能直接复制其源码作为实现捷径。

因此，本项目采用以下总原则：

~~~text
吸收接口发现思路和行为模型
        -> 用本人账号重新验证
        -> 以兼容策略接入
        -> 保留当前已验证的 v5/流式实现作为回退
~~~

## 3. 外部参考范围

### 3.1 目标项目确认

用户所说的“onelist”应为 OpenList。公开资料中与 WPS 驱动对应的项目和文档是：

- 项目主页：https://github.com/OpenListTeam/OpenList
- WPS 文档：https://doc.oplist.org/guide/drivers/wps
- WPS 驱动入口：https://github.com/OpenListTeam/OpenList/blob/main/drivers/wps/driver.go
- WPS API 工具：https://github.com/OpenListTeam/OpenList/blob/main/drivers/wps/util.go
- WPS 上传实现：https://github.com/OpenListTeam/OpenList/blob/main/drivers/wps/put.go
- WPS 数据结构：https://github.com/OpenListTeam/OpenList/blob/main/drivers/wps/types.go
- 项目许可证：https://github.com/OpenListTeam/OpenList/blob/main/LICENSE

如果未来发现用户指的是另一个项目，应新建独立参考记录，不能把两个项目的结论合并。

### 3.2 证据等级

本文件使用下面的证据等级：

| 等级 | 含义 | 可否直接改变默认行为 |
| --- | --- | --- |
| external-reference | 只在 OpenList 公开源码或文档中看到 | 不可以 |
| candidate | 外部实现给出了明确的请求形状，可作为本人账号实验候选 | 不可以 |
| observed | 本人账号网页请求中观察到 | 可以进入研究记录，仍需考虑兼容性 |
| reproduced | 本人账号请求由本项目在测试目录中重放成功 | 可以进入默认实现 |
| inferred | 根据多个请求推导的行为，尚未直接证明 | 不可以 |

本文件中关于 OpenList 的内容默认标为 external-reference 或 candidate，不会覆盖 findings.md 中的本人账号实验记录。

### 3.3 外部项目自己的限制声明

OpenList 的 WPS 文档明确说明，这个驱动基于历史逆向接口，项目组不会主动维护，也不应把 WPS 驱动当作稳定官方契约。它还建议在新的浏览器环境或无痕窗口中登录，通过 Network 中的 islogin 请求取得 Cookie 和 User-Agent。

这与本项目的安全边界一致：

- 只处理账号所有者自己正常获得的会话。
- 不绕过权限、SSO、验证码、风控或租户隔离。
- 不把 Cookie、刷新票据、签名 URL 或原始 HAR 放进仓库。
- 不把外部项目的“能请求”误认为 WPS 长期承诺。

## 4. 当前项目基线

当前项目的主要数据流是：

~~~text
网页 / WebDAV / REST
          |
     Adapter Server
          |
      WpsStorage
          |
      WpsDriveClient
          |
  WPS 控制 API + 对象存储
~~~

当前实现已经具备：

- GET /3rd/drive/api/v5/groups/<group-id>/files 目录列表。
- POST /3rd/drive/api/v5/files/folder 创建文件夹。
- PUT /3rd/drive/api/v3/groups/<group-id>/files/<file-id> 重命名。
- v5 异步任务形式的移动和删除，以及任务进度轮询。
- 普通上传、覆盖上传和 v5 block/multipart 分片上传。
- 由 WPS 下载接口取得临时对象地址，再由适配器流式转发内容。
- 单范围 Range 下载、WebDAV PROPFIND 的多种深度和资源限制。
- 本地登录助手、Cookie/CSRF/rtk 持久化和有限自动续期。
- WorkspaceState 对群组 ID 和根目录 ID 的安全持久化。

当前与四个方向直接相关的缺口是：

| 方向 | 当前状态 | 本文设计目标 |
| --- | --- | --- |
| 登录预检 | /healthz 只代表进程健康；网页已有连接状态，但还需要更明确的 WPS 预检模型 | 添加受 Basic Auth 保护的 WPS 状态预检，不把进程在线误报为 WPS 在线 |
| 群组发现 | 登录助手主要从当前官方页面地址取得群组上下文 | 登录后用 WPS 企业接口自动列出可见群组，用户按名称选择 |
| 原生文件操作 | 移动、删除已使用本人账号观察到的 v5 task；COPY 仍为适配器中继 | 用中性测试文件验证 v3 native COPY，再按结果采用双策略 |
| 根目录选择 | 默认 root_id=0，显式 URL 可选择子文件夹 | 保持根目录默认行为，增加群组选择和工作区身份校验，绝不使用 WPS 恢复的旧页面目录 |

## 5. 外部接口对照

### 5.1 账号登录状态

OpenList 在初始化时请求：

~~~text
GET https://account.kdocs.cn/api/v3/islogin
Cookie: <当前 WPS 会话>
~~~

其代码读取的响应字段包括：

| 字段 | 外部代码用途 | 本项目拟用途 |
| --- | --- | --- |
| companyid | 企业 ID | 企业上下文诊断和企业群组查询 |
| current_companyid | 当前企业 ID | 检查当前账号上下文是否变化 |
| is_company_account | 判断账号类型 | 识别企业/商业账号 |
| userid | 当前用户 ID | 仅用于本地诊断，不对外显示 |
| loginmode | 登录模式 | 记录诊断信息，不据此绕过任何登录流程 |

这些字段在本项目中只应被转换为有限的状态信息。网页和 REST 响应不返回用户 ID、企业 ID、Cookie 或原始响应正文，日志只记录状态和错误类别。

重要边界：islogin 是“会话是否可以被 WPS 账号服务识别”的预检，不是文件夹权限证明，也不是 refresh token。它不能替代对选定群组根目录的最小读取验证。

### 5.2 企业群组列表

OpenList 的 Business 模式使用候选接口：

~~~text
GET https://365.kdocs.cn/3rd/plus/groups/v1/companies/<companyid>/users/self/groups/private
Cookie: <当前 WPS 会话>
~~~

响应中使用的对象字段形状为：

~~~json
{
  "groups": [
    {
      "company_id": "<company-id>",
      "group_id": "<group-id>",
      "name": "<group-name>",
      "type": "<group-type>"
    }
  ]
}
~~~

上面的字段名和路径仅是外部候选，真实类型、是否需要额外 query、是否包含全部可见群组，都必须用本人账号验证。

OpenList 的后续修复记录特别指出：企业账号可能也能调用个人版的群组接口，但结果不一定完整。因此不能用“个人版接口返回了一组数据”作为企业群组发现成功的判据。企业账号应优先尝试企业接口；企业接口失败时只能进入明确的回退或人工选择流程，不能静默切换为个人版并声称列表完整。

### 5.3 文件列表

OpenList 调用的候选形状是：

~~~text
GET <drive-host>/api/v5/groups/<group-id>/files
    ?parentid=<parent-id>
    &offset=<offset>
~~~

本项目已经在本人账号上观察到更完整的企业版请求：

~~~text
GET /3rd/drive/api/v5/groups/<group-id>/files
    ?parentid=<parent-id>
    &linkgroup=true
    &include=acl,pic_thumbnail
    &with_link=true
    &review_pic_thumbnail=true
    &with_sharefolder_type=true
    &offset=0
    &count=20
    &orderby=mtime
    &order=desc
~~~

两者共同支持当前项目的对象模型，但本项目继续以本人账号的抓包和 findings.md 为准。OpenList 只会影响后续对分页、防重复 cursor 和账号模式的设计。

### 5.4 创建文件夹和重命名

外部驱动与本人账号已验证的请求一致或高度相似：

~~~text
POST /3rd/drive/api/v5/files/folder
JSON: groupid, parentid, name
~~~

~~~text
PUT /3rd/drive/api/v3/groups/<group-id>/files/<file-id>
JSON: fname
~~~

本项目已额外验证 CSRF 字段和返回对象，并保留了更严格的名称和路径校验。

### 5.5 原生移动、复制和删除候选

OpenList 当前驱动使用的候选接口如下：

~~~text
POST /3rd/drive/api/v3/groups/<source-group-id>/files/batch/move
JSON:
  fileids
  target_groupid
  target_parentid
~~~

~~~text
POST /3rd/drive/api/v3/groups/<source-group-id>/files/batch/copy
JSON:
  fileids
  groupid
  target_groupid
  target_parentid
  duplicated_name_model
~~~

~~~text
POST /3rd/drive/api/v3/groups/<group-id>/files/batch/delete
JSON:
  fileids
~~~

而本项目在本人企业账号上已观察并重放成功的是：

~~~text
POST /3rd/drive/api/v5/files/batch/task/move
JSON: groupid, parentid, dst_groupid, dst_parentid, fileids, option, csrfmiddlewaretoken
GET  /3rd/drive/api/v5/files/batch/task/progress?taskuuid=<taskuuid>
~~~

~~~text
POST /3rd/drive/api/v5/files/batch/task/delete
JSON: fileids, groupid, csrfmiddlewaretoken
GET  /3rd/drive/api/v5/files/batch/task/progress?taskuuid=<taskuuid>
~~~

这不是简单的版本号替换关系。可能的差异来源包括账号类型、WPS 前端版本、企业租户配置、接口演进或操作场景。特别是：

- v3 接口可能同步返回 result=ok，也可能在后台创建任务。
- v5 task 接口明确返回 taskuuid，必须轮询并检查失败列表。
- v3 duplicated_name_model 的实际语义尚未在本人账号中确认。
- COPY 的目标名称、覆盖规则和递归目录语义尚未确认。

因此，原生文件操作只能通过本文件第 9 节的本人账号实验引入。

### 5.6 动态上传指令

OpenList 的上传实现使用 create_update 返回的动态指令：

~~~text
PUT /3rd/drive/api/v5/files/upload/create_update
~~~

然后根据响应中的 method、url、请求头、表单字段、期望状态码和 ETag/key 取值规则，完成对象存储上传和文件登记。

这对本项目的启发是：控制接口返回值应被当成“上传指令”，而不是固定为某一个对象存储厂商的单一格式。本项目已经支持普通上传和 block/multipart 两条路径，后续可以继续复用这个动态解析思路。

OpenList 目前会先完整缓存文件并计算 SHA-1/SHA-256，再发送上传请求。这个做法便于重试和处理未知大小，但会增加 VPS 临时存储和磁盘压力。本项目继续使用请求级 spool、有限内存和磁盘空间保护，不采用长期缓存。

### 5.7 下载临时地址

OpenList 的候选下载接口是：

~~~text
GET <drive-host>/api/v5/groups/<group-id>/files/<file-id>/download?support_checksums=sha1
~~~

它得到临时 URL，并在返回给上层时附带 User-Agent 和 Referer。

本项目本人账号记录的下载主流程是：

~~~text
GET /api/v3/office/file/<file-id>/download
    ?support_checksums=...
    &get_direct_external_download_url=true|<omitted>
    &cid=<file-level-link-id>
-> JSON 临时对象地址
-> 对象存储 GET
~~~

本项目不直接把签名 URL 返回给 WebDAV 客户端，而是由适配器打开并流式转发，并校验签名地址主机属于允许的 WPS 对象存储范围。这比直接暴露短期签名地址更适合本项目的安全边界，也便于统一处理 Range。

## 6. 目标架构

四个方向接入后的目标数据流如下：

~~~text
本地登录助手
    |
    | 读取官方 WPS 临时隔离浏览器会话
    v
islogin 预检 -> 企业群组发现 -> 用户选择群组 -> 根目录/子目录选择
    |                         |
    |                         v
    |                 wps-workspace.json
    v
POST /api/v1/session/import 或 SSH 同步 Cookie/CSRF/工作区
    |
    v
适配器启动
    |
    +--> /healthz：只检查进程
    |
    +--> /api/v1/status：Basic Auth + 缓存的 WPS 预检
    |
    +--> WpsStorage：将适配器 / 映射到保存的 root_id
                 |
                 +--> 列表、元数据、下载、上传
                 +--> native COPY/MOVE/DELETE 策略
                 +--> 失败时使用已验证的安全回退
~~~

### 6.1 组件职责

| 组件 | 责任 | 不应承担的责任 |
| --- | --- | --- |
| wps_login.py | 打开官方登录页、读取临时浏览器会话、预检和选择工作区 | 代填 WPS 密码、处理验证码、猜测租户权限 |
| WpsClientConfig | 保存基础地址、凭据来源、刷新设置和策略开关 | 保存 Cookie 到普通日志或响应 |
| WpsDriveClient | 调用 WPS 控制 API、解析响应、执行有限重试 | 把签名 URL 交给不必要的外部调用方 |
| WorkspaceState | 原子持久化群组和根目录选择 | 静默替换用户明确选择的目录 |
| WpsStorage | 将虚拟路径解析为 WPS ID、提供目录缓存和复制回退 | 绕过 WPS 权限 |
| AdapterRequestHandler | Basic Auth、WebDAV/REST 协议、状态码和网页 API | 直接拼接未经校验的 WPS URL |
| Web UI | 显示连接状态、提供低门槛操作 | 显示 Cookie、Token、企业 ID 或签名 URL |

### 6.2 两层健康状态

必须保持两个概念分离：

~~~text
进程健康 != WPS 会话有效
~~~

建议状态模型：

| 状态 | 含义 | 页面文字建议 |
| --- | --- | --- |
| checking | 尚未完成最近一次预检 | 正在检查 WPS |
| not_configured | 没有 Cookie 或工作区 | 尚未连接 WPS |
| connected | islogin 成功且工作区检查通过 | WPS 已连接 |
| session_expired | WPS 返回 401，刷新也失败 | WPS 登录已过期 |
| permission_denied | 会话有效但目标群组/目录无权访问 | 无权访问当前工作区 |
| upstream_unavailable | DNS、TLS、超时或上游 5xx | WPS 暂时不可用 |
| invalid_response | 上游响应不是预期 JSON/字段 | WPS 响应异常 |

healthz 继续只代表 Python 进程能够接受请求，不访问 WPS。新增的状态接口需要 Basic Auth，避免未认证用户通过状态接口探测部署细节。

## 7. 方向一：使用 islogin 做登录状态预检

### 7.1 目标

解决以下用户体验问题：

- 第一次安装还没有 Cookie 时，网页不能显示成“已连接”。
- WPS Cookie 失效后，页面应明确提示重新登录，而不是只显示 upstream WPS request failed。
- VPS 进程正常但 WPS 网络不可达时，用户应看到“上游不可用”，而不是误以为服务崩溃。
- 自动 refresh 失败时，状态应说明“登录已过期”，但不暴露刷新细节。

### 7.2 请求规则

预检使用账号服务的只读请求：

~~~text
GET https://account.kdocs.cn/api/v3/islogin
~~~

请求实现要求：

1. 每次读取当前 credential source，支持登录助手刚刚替换的 Cookie。
2. 只向 WPS 账号域名发送 WPS Cookie。
3. 不把 Cookie 放入 URL、异常文本、日志或 JSON 响应。
4. 复用当前的 Set-Cookie 原子持久化逻辑，支持 WPS 轮换会话 Cookie。
5. 预检失败时只保留 HTTP 状态和安全错误类别。
6. islogin 本身不能被当作文件权限验证，随后仍需要检查已选工作区。

### 7.3 预检与自动续期的关系

预检不等于刷新：

~~~text
islogin 返回 200
    -> 当前会话可识别

文件 API 返回 401
    -> 先读取管理员/登录助手是否替换了凭据
    -> 再按已观察的 grant_token 尝试刷新
    -> 持久化 Set-Cookie
    -> 原请求只重试一次
~~~

不要为了刷新而定时高频调用 grant_token。预检只做低频状态检查，真正的刷新仍以业务请求遇到 401 为主要触发点。

### 7.4 状态接口设计

本项目已增加一个受 Basic Auth 保护的本地接口：

~~~text
GET /api/v1/status
~~~

当前实现的脱敏响应形状：

~~~json
{
  "status": "connected",
  "wps": "connected",
  "workspace": "ready",
  "account_type": "business",
  "last_checked_at": "<timestamp>",
  "retry_after": 0
}
~~~

不建议返回：

- Cookie、CSRF、rtk、Authorization。
- 完整 WPS 响应。
- 签名对象存储 URL。
- 默认暴露企业 ID、群组 ID、用户 ID。
- 上游错误正文。

当前实现的状态缓存：

- 成功结果缓存约 30 秒。
- 失败结果使用短暂退避，避免网页刷新造成请求风暴。
- 同时只有一个状态探测请求，其他请求读取同一结果。
- 业务文件请求仍可直接发现实际错误，不被状态缓存永久阻塞。

具体 TTL 由 `WPS_STATUS_PROBE_TTL` 和 `WPS_STATUS_FAILURE_BACKOFF` 集中配置，默认分别为 30 秒和 5 秒。

实现边界：`islogin` 仍是外部项目提供的候选接口，不是 WPS 官方稳定契约。本项目把成功的 JSON 对象作为会话服务已接受的证据；如果响应提供 `islogin=false`，则按登录过期处理，并额外验证当前根目录的最小列表权限。没有 Cookie、群组或根目录时不会访问账号服务。真实账号验收仍需在用户自己的 WPS 测试目录中低频执行。

### 7.5 与网页的交互

当前网页加载顺序为：

~~~text
加载页面
  -> 显示 checking
  -> 请求 /api/v1/status
  -> 根据状态显示 connected / session_expired / unavailable
  -> 再加载目录
~~~

目录请求失败时，页面还应根据 REST 错误码更新状态：

- wps_session_expired -> 登录已过期。
- wps_unavailable 且上游状态是网络/5xx -> WPS 暂时不可用。
- permission_denied -> 当前工作区无权访问。
- 其他目录错误 -> 保留具体的目录操作错误，不误改连接状态。

### 7.6 验收标准

在本人账号测试目录中至少验证：

1. 没有 Cookie 时页面显示未连接，healthz 仍为进程健康。
2. 有效 Cookie 时状态接口显示已连接，随后根目录可以列出。
3. 手工撤销会话后，页面显示登录过期，不显示 Cookie 或上游正文。
4. 重新运行登录助手替换凭据后，不重启服务即可恢复。
5. WPS 域名暂时不可达时显示上游不可用，恢复后自动恢复。
6. 并发刷新页面不会产生大量重复 islogin 请求。

## 8. 方向二：自动发现企业群组

### 8.1 目标

把当前流程中的：

~~~text
用户登录 -> 手动寻找/填写群组 ID
~~~

简化为：

~~~text
用户登录 -> 程序列出本人可见的企业空间 -> 按名称选择 -> 自动保存 ID
~~~

用户不需要理解 group_id、companyid 和 root_id 的区别。

### 8.2 自动发现流程

推荐流程如下：

~~~text
1. 从临时浏览器读取匹配 WPS 域名的 Cookie
2. 调用 islogin
3. 判断是否为企业/商业账号
4. 使用 companyid 请求企业群组列表
5. 校验响应中的群组对象
6. 一个群组时自动选择
7. 多个群组时按名称和序号让用户选择
8. 用 parentid=0 验证所选群组根目录
9. 保存 group_id 和 root_id=0
~~~

如果用户显式指定 --workspace-url：

~~~text
1. 仍然先进行账号预检
2. 验证 URL 中的企业/群组上下文
3. 验证该群组属于当前登录会话可见范围
4. 验证指定文件夹可读
5. 保存该文件夹 ID，而不是当前页面可能恢复的旧目录
~~~

### 8.3 多群组交互

登录助手显示的内容应该是用户可理解的信息：

~~~text
发现 2 个可用企业空间：
[1] 学校云盘
[2] 个人团队
请选择 [1]：
~~~

安全和可维护性要求：

- 不要求用户手动输入长数字 ID。
- 同名群组必须显示序号，不能只按名称静默选第一个。
- 选择前可以显示名称和有限的类型信息，不能显示 Cookie 或签名值。
- 只有被接口返回的群组才能进入候选，不扫描其他群组。
- 如果已有保存的群组仍在返回列表中，默认保留原选择。
- 如果原群组不再返回，必须提示用户重新选择，不能静默换到另一个群组。

### 8.4 回退策略

自动接口可能因为账号类型、接口变化或网络环境失败。回退顺序建议为：

1. 企业群组 API 自动发现。
2. 当前官方页面 URL 中已有的群组上下文。
3. 用户显式提供的 --group-id 或配置文件。
4. 明确失败，并告诉用户“无法自动识别群组”，而不是保存不完整状态。

不能做的事情：

- 不通过递增 ID、字典或扫描方式猜群组。
- 不调用未经本人账号观察的管理接口。
- 不因企业接口返回空数组就切换个人版接口并隐藏差异。
- 不把页面里自动恢复的文件夹 ID 当成根目录。

### 8.5 工作区状态格式

当前 wps-workspace.json 已使用最小格式：

~~~json
{
  "group_id": "<group-id>",
  "root_id": "0"
}
~~~

建议继续保持这个格式作为兼容核心。可以增加非敏感的可选诊断字段，但不应依赖这些字段才能工作：

~~~json
{
  "group_id": "<group-id>",
  "root_id": "0",
  "account_type": "business",
  "group_name": "<display-name>",
  "updated_at": "<timestamp>"
}
~~~

其中 group_name 只能用于显示，不能作为对象定位依据；真正定位永远使用 ID。企业 ID 和用户 ID 没有必要写入工作区文件，减少个人信息保存范围。

写入要求：

- 目录权限 0700，文件权限 0600。
- 使用同目录临时文件、fsync 和原子替换。
- 先验证新群组根目录可读，再替换旧状态。
- 更新失败时保留旧的有效工作区。

### 8.6 验收标准

1. 一个企业群组时不询问用户 ID，自动保存并能列根目录。
2. 多个企业群组时只显示可见群组，用户按名称选择。
3. 选择具体子文件夹时只保存显式指定的文件夹。
4. 登录后 WPS 自动跳回旧的无权文件夹时，仍然使用根目录 0。
5. 已保存群组不可访问时，页面明确显示工作区无权访问，不静默更换群组。
6. 个人账号或缺少企业字段时，行为可解释，不误报为企业群组发现成功。

## 9. 方向三：验证并接入 WPS 原生文件操作

### 9.1 目标

当前 WebDAV COPY 通过适配器下载再上传，优点是已经复用稳定的流式能力，缺点是：

- 占用 VPS 到 WPS 的双向带宽。
- 需要经过临时 spool 和上传并发限制。
- 复制大文件耗时较长。
- 如果客户端发起服务端复制，WPS 自身可能有更高效的路径。

目标是：在确认 WPS 原生 COPY 后，优先使用服务端复制；不能确认或不满足条件时继续使用当前中继，并且不因为尝试 native 失败而丢失源文件或已有目标。

### 9.2 策略模型

建议将文件操作策略抽象为：

~~~text
auto
  -> 使用已验证的当前 v5 task 路径
  -> 使用已验证的 v3 native 路径
  -> 使用当前安全的适配器中继

native-v3
  -> 只使用本人账号已验证的 v3 路径

task-v5
  -> 只使用本人账号已验证的 v5 task 路径

relay
  -> 只使用适配器下载/上传中继
~~~

默认仍应是 auto，但 auto 不是无限重试。每次操作必须有明确的“接口未支持”和“操作结果不确定”分类。

### 9.3 COPY 实验步骤

所有步骤只使用本人账号专用测试目录：

#### 准备

1. 创建两个空测试文件夹，例如 copy-source 和 copy-target。
2. 上传一个小型无隐私文本文件到 copy-source。
3. 记录文件大小和本地校验值，不记录 Cookie、签名 URL 或真实账号信息。

#### 第一次 native COPY

1. 记录源文件 ID、源群组 ID、目标文件夹 ID。
2. 以 OpenList 代码中的 v3 候选请求形状发起一次 COPY。
3. 只等待正常返回，不对 403、超时或未知响应无限重试。
4. 重新列出 copy-target，确认是否出现目标文件。
5. 下载目标文件并比较大小和文件校验值。
6. 清理测试目标，确认删除任务成功。

#### 重复和边界

至少验证：

- 同一源文件复制两次时的重复名称语义。
- 目标目录已有同名文件时是否拒绝、重命名或覆盖。
- 文件夹 Depth: 0、1 和 infinity 对 WebDAV COPY 的映射。
- 小文件和一个本人生成的较大文件。
- 目标文件夹和源文件属于同一企业群组的情况。
- 网络中断后重新列目录，判断操作结果是否已经发生。

不要在第一次实验中测试跨租户、其他用户、分享链接或没有明确权限的目录。

### 9.4 native COPY 接入条件

只有满足以下条件，才可以把 native COPY 设为默认：

1. 至少三次同一账号的成功复制。
2. 文件内容和大小与源一致。
3. 目标已存在时不会静默删除已有数据。
4. WPS 返回错误时适配器能区分“不支持”和“权限拒绝”。
5. 请求超时后能通过重新列目录判断结果，或者明确向用户报告结果不确定。
6. 文件夹复制的深度和数量有适配器级保护。
7. 测试结果已写入新的本人账号实验编号。

### 9.5 错误和回退规则

建议规则：

| 情况 | 处理 |
| --- | --- |
| 404/405/501 且确认是接口不存在 | 标记 native 不可用，回退 relay |
| 401 | 走统一凭据刷新流程，最多重试一次 |
| 403 | 默认视为权限或业务拒绝，不自动用 relay 绕过 |
| 409/重复名称 | 返回冲突，不删除已有目标 |
| 5xx | 如果操作结果可能已提交，先重新列目录，再决定是否重试 |
| 网络超时 | 不盲目重复可能产生副本的请求，报告结果不确定或先验证目标 |
| 返回 JSON 形状未知 | 不当作成功，不执行危险的补偿删除 |

尤其不能采用下面这种不安全逻辑：

~~~text
native COPY 任何失败 -> 删除目标 -> relay COPY
~~~

这可能把 WPS 已经创建的目标和原有同名文件一起破坏。

### 9.6 MOVE 和 DELETE 的借鉴

OpenList 的 v3 MOVE/DELETE 候选值得作为兼容候选，但当前项目的 v5 task MOVE/DELETE 已有本人账号证据和进度轮询，因此默认顺序应是：

~~~text
当前已验证的 v5 task
    -> 只有本人账号另行验证成功，才允许 v3 作为候选
~~~

如果加入 v3 路径：

- 需要限制连续 fileTaskDuplicated 重试次数和总超时时间。
- 请求返回成功后应重新读取目标目录或元数据确认。
- 删除必须确认 WPS 是否为异步任务、回收站删除还是立即删除。
- 不能因 v3 返回 result=ok 就跳过结果验证。

### 9.7 WebDAV 语义映射

服务端 COPY 接入后，WebDAV 仍然负责协议语义：

- Destination 解析为目标完整路径。
- Overwrite: F 在目标存在时返回 412。
- 当前不支持安全覆盖时继续返回 501，不先删除目标。
- Depth 仅允许已实现和受保护的 0、1、infinity。
- WPS native COPY 失败不能被伪装为 WebDAV 成功。

WPS 原生接口是优化手段，不改变适配器已经对外承诺的无数据丢失边界。

### 9.8 验收标准

1. WebDAV COPY 小文件 native 成功，目标内容正确。
2. native 不可用时 relay 仍能成功，且响应状态一致。
3. 目标存在时不会被无提示覆盖或删除。
4. 权限错误不会被错误转化为 relay 成功。
5. 超时后不会无限重试或生成不可控的重复副本。
6. 递归复制仍受条目数、深度、并发和临时空间限制。

## 10. 方向四：根目录、群组和目标文件夹模型

### 10.1 三个容易混淆的 ID

| 名称 | 作用 |
| --- | --- |
| 企业/租户 ID | 账号属于哪个企业上下文 | 主要用于企业群组发现和诊断 |
| 群组 ID | 当前企业云盘或团队空间 | 文件列表 URL 的 group-id |
| 根目录/文件夹 ID | 群组内的目录 | parentid 或适配器映射根 |

适配器对外的 / 不应自动等于 WPS 网页最后一次打开的目录。默认规则必须是：

~~~text
适配器 /
  -> 已选择 group_id 下的 root_id
  -> 未指定时 root_id=0
~~~

### 10.2 默认根目录规则

没有显式选择子文件夹时：

1. 登录助手自动发现或确认群组。
2. 保存 root_id=0。
3. 适配器启动后第一次列目录使用 parentid=0。
4. 不读取 WPS 页面 URL 中自动恢复的旧文件夹作为根目录。

这条规则解决了“刚登陆显示无权访问旧文件夹，必须手动切换后才正常”的问题。

### 10.3 显式子文件夹规则

只有用户明确提供 --workspace-url 或未来在界面中明确选择目标目录时，才使用具体文件夹 ID：

~~~text
https://365.kdocs.cn/space/<tenant>/<group>/<folder>
~~~

验证顺序：

1. 地址主机必须是允许的 WPS 主机。
2. URL 路径的企业和群组上下文必须与当前会话一致。
3. 文件夹 ID 必须符合本地安全格式校验。
4. 使用元数据或列表请求验证该目录可读。
5. 验证成功后再原子更新工作区文件。

如果验证失败：

- 不清空旧的有效工作区。
- 不退回到当前页面显示的其他目录。
- 向用户说明是无权访问、地址无效还是 WPS 不可用。

### 10.4 群组选择和 WebDAV 根

为了让 WebDAV 客户端有稳定的根路径，本项目暂不把多个群组暴露为 /学校云盘、/个人团队 等动态顶层目录。推荐行为是：

~~~text
登录助手选择一个群组
        -> WebDAV /
        -> 这个群组的 root_id
~~~

未来如果要支持多群组挂载，应作为明确的新功能设计独立的命名和权限模型，不能因为 OpenList 在其内部把群组列为根目录就直接改变当前 WebDAV 路径。

### 10.5 工作区身份变化

重新登录可能对应另一个 WPS 账号或另一个企业上下文。适配器应：

1. 预检当前账号类型和企业上下文。
2. 检查保存的群组是否仍在当前账号可见列表中。
3. 若不一致，将状态标为 workspace_mismatch 或 permission_denied。
4. 等用户明确选择新群组后再更新工作区。

不能只因为 Cookie 被替换就自动沿用旧群组 ID，也不能静默把文件根切到第一个可见群组。

### 10.6 验收标准

1. 无显式子目录时始终从 root_id=0 开始。
2. WPS 页面恢复到旧目录不会影响适配器根目录。
3. 显式指定的子目录可读时，WebDAV / 映射到该目录。
4. 子目录无权访问时，服务不会降级到其他目录。
5. 群组切换后，目录缓存被清理，旧路径不会继续解析到新群组。
6. 服务重启、凭据轮换和登录助手同步后，工作区状态保持一致。

## 11. 上传、下载和资源模型的取舍

### 11.1 借鉴动态上传解析

OpenList 的 create_update 处理方式值得保留为设计原则：

- 从响应读取实际 HTTP 方法。
- 从响应读取实际请求头和表单字段。
- 尊重服务端声明的期望状态码。
- 从声明的位置读取 ETag 和对象 key。
- 解析 JSON/XML 响应时限制大小。

本项目已有相同方向的普通上传控制和分片控制实现，后续只补充缺少的响应形状，不把 OpenList 的 Go 代码直接移植到 Python。

### 11.2 不采用完整缓存作为默认

本项目继续遵守：

- 普通上传使用内存 spool，超过阈值后使用请求级临时文件。
- 大文件分片只读取当前片段，上传结束或失败后清理临时内容。
- 下载不把整文件读入内存。
- COPY relay 使用现有下载/上传流，而不是建立永久副本。
- 并发上传、下载、递归复制和磁盘空闲空间都有上限。

如果未来需要跨请求续传，应先设计独立的临时会话状态、过期清理、磁盘配额和恢复一致性，不能简单把 OpenList 的完整缓存照搬过来。

### 11.3 下载链接处理

OpenList 返回直链的思路说明 WPS 可能允许调用方直接访问对象存储；本项目保留更保守的中继策略：

~~~text
WPS 控制 API -> 临时签名 URL -> 适配器校验目标主机 -> 流式对象 GET -> 客户端
~~~

这样可以：

- 不把签名 URL 直接暴露给客户端或日志。
- 在一个地方实现 Range、Content-Length 和错误映射。
- 防止对象存储响应重定向到未经允许的主机。
- 只向 WPS 控制 API 发送 WPS Cookie，不向对象存储发送 Cookie。

## 12. 凭据、安全和隐私边界

### 12.1 凭据范围

可能出现的敏感数据包括：

- WPS Cookie。
- rtk 持久刷新 Cookie。
- CSRF 值。
- WPS SDK 或网页请求中的其他会话字段。
- 对象存储临时签名 URL、AccessKeyId、Policy、Signature。
- 适配器 Basic Auth 密码。

这些值不能进入：

- Git、GitHub Issue、Pull Request 或公开文档。
- 命令行参数和 shell 历史。
- systemd 日志、HTTP 访问日志或异常消息。
- 网页正文、浏览器开发者控制台或 REST 返回体。

### 12.2 本地登录助手

继续使用临时隔离浏览器配置：

- 不读取用户日常 Chrome 配置。
- 只选择允许的 WPS Cookie 域名。
- 不处理 WPS 密码、SSO、验证码或风控。
- 预检和群组发现只访问当前登录会话能够正常访问的接口。
- 传输到 VPS 前不打印 Cookie。
- HTTP 仅在用户明确确认且网络可信时允许；HTTPS 或 SSH 优先。

### 12.3 服务端接口

新增状态和群组选择能力时：

- 所有管理接口都需要适配器 Basic Auth。
- 不通过 GET query 接收 Cookie 或 Token。
- 群组选择接口只接受登录助手验证过的候选 ID，不能作为任意 ID 访问代理。
- 状态响应只返回枚举状态，不转发上游正文。
- 写操作继续校验同源 Origin/Referer，并使用本地 CSRF secret。

### 12.4 OpenList 许可证

OpenList 仓库当前标记为 AGPL-3.0。本项目当前是 GPL-3.0-or-later。后续实现遵守以下做法：

1. 不复制 OpenList 的 Go 源码、注释或逐行改写实现。
2. 只参考公开文档、接口字段形状和经过独立验证的行为。
3. 如果确实需要移植代码，先单独进行许可证兼容性审查，保留版权和许可证通知，不把它混入普通功能提交。
4. 外部项目链接和“参考来源”保留在本文件中。

接口协议和测试结论不能代替许可证审查。

## 13. 建议的配置项

下面是实现阶段可以采用的配置方向，不代表现在已经存在的环境变量。命名可以在编码时根据现有风格调整。

| 建议项 | 默认方向 | 作用 |
| --- | --- | --- |
| WPS_STATUS_PROBE_TTL | 约 30 秒 | /api/v1/status 的成功缓存时间 |
| WPS_STATUS_FAILURE_BACKOFF | 短暂退避 | 避免网页刷新打爆上游 |
| WPS_GROUP_DISCOVERY | auto | 登录助手是否自动发现企业群组 |
| WPS_WORKSPACE_STRICT | true | 群组或目录身份不一致时是否拒绝静默切换 |
| WPS_FILE_OPERATION_MODE | auto | task-v5、native-v3、relay 或自动策略 |
| WPS_NATIVE_COPY | false 直到验证完成 | 是否允许把已验证的 native COPY 作为默认候选 |
| WPS_NATIVE_OPERATION_TIMEOUT | 有限秒数 | native 操作总超时 |
| WPS_NATIVE_RETRY_LIMIT | 小整数 | 只对确定可重试的请求限制重试次数 |

配置项必须满足：

- 未设置时保留当前已验证行为。
- 升级时不清理凭据和工作区文件。
- 发生未知 WPS 响应时 fail closed，不把未知状态当成功。
- 参数错误在启动或 check-config 阶段清楚提示。

## 14. 分阶段实施计划

### 阶段 A：登录状态预检（已实现）

范围：

- 增加 WPS islogin 只读客户端方法。
- 增加缓存和单飞锁。
- 增加 /api/v1/status。
- 网页区分进程健康和 WPS 连接状态。
- 不改变现有文件 API 的主要路径。

代码和自动化测试已覆盖第 7.6 节中的本地行为，包括未配置凭据、登录过期、工作区无权、异常响应、缓存和并发单飞。真实 WPS 账号的撤销会话、网络恢复和重新同步仍待用户在自己的测试目录中验收。

提交建议：

~~~text
Add WPS login status preflight
~~~

### 阶段 B：自动群组发现

范围：

- 登录助手在成功登录后执行 islogin。
- 企业账号调用企业群组候选接口。
- 单群组自动选择，多群组按名称选择。
- 选择后验证 parentid=0 并写入工作区。
- 保留当前页面 URL 和手动 ID 作为明确回退。

验收：第 8.6 节全部通过。

提交建议：

~~~text
Auto-discover WPS enterprise groups
~~~

### 阶段 C：原生 COPY 实验和策略接入

范围：

- 先只增加实验工具或受控 debug 入口，不改变默认 COPY。
- 使用本人测试目录验证 v3 native COPY。
- 记录结果到新的本人账号实验编号。
- 成功后加入 auto/native-v3/relay 策略和严格回退规则。

验收：第 9.8 节全部通过。

提交建议：

~~~text
Add verified WPS native copy strategy
~~~

### 阶段 D：根目录和身份一致性收束

范围：

- 将自动选择的群组与现有 WorkspaceState 对齐。
- 增加工作区身份变化提示。
- 清理群组切换后的元数据缓存。
- 补充部署、登录、故障排查文档。

验收：第 10.6 节全部通过。

提交建议：

~~~text
Harden WPS workspace selection
~~~

### 每个阶段的最低检查

~~~text
PYTHONPATH=src python3 -m unittest discover -s tests -v
python3 -m compileall -q src tests
python3 tools/build_login_script.py --check
python3 tools/build_release_manifest.py --check
git diff --check
~~~

涉及真实 WPS 请求时，还需要：

- 使用本人测试目录。
- 保存脱敏后的请求形状和实验编号。
- 不提交原始 HAR、Cookie、Token、签名 URL 或文件内容。
- 在提交前检查 git status 和发布清单。

## 15. 回滚方案

任何新策略都必须能单独关闭：

### 登录预检回滚

- 状态接口失败不应影响已有列表、上传和下载接口。
- 网页无法读取状态时显示 unknown，不能显示“已连接”。
- /healthz 保持旧的进程级语义。

### 群组发现回滚

- 保留现有 --workspace-url、--group-id 和工作区文件格式。
- 自动发现失败时不覆盖旧的有效工作区。
- 用户可以显式指定工作区完成恢复。

### native COPY 回滚

- 关闭 native 策略后使用现有 relay COPY。
- 不删除或修改已有源文件和目标文件。
- 未知响应不自动补偿删除。

### 根目录回滚

- root_id=0 仍是默认值。
- 旧版工作区文件只读取 group_id 和 root_id 两个字段也能工作。
- 新增诊断字段缺失时按默认值处理。

## 16. 未解决的问题

以下问题不能通过 OpenList 资料直接得出答案：

- 当前企业租户的 islogin 响应字段是否始终完整。
- 企业群组接口是否返回当前学校账号的全部可用群组。
- v3 native COPY 在当前账号上是否可用。
- v3 COPY 是否异步，以及是否有 task progress 接口。
- 重复名称模型和覆盖行为。
- native COPY 对非空文件夹和跨群组操作的限制。
- WPS 不同区域、版本和账号类型是否使用不同 host/path。
- 当前 block/multipart 上传会话是否支持进程退出后的续传和取消清理。

这些问题必须以本人账号的低频、最小范围实验回答，不能用猜测填空。

## 17. 最终设计原则

本项目吸收 OpenList WPS 驱动后的最终原则是：

1. 先做只读预检，再报告连接状态。
2. 能自动发现群组时自动发现，但永远允许用户明确选择。
3. 默认根目录是 WPS 企业群组的 0，不是浏览器恢复的旧目录。
4. 已验证的 WPS 原生操作优先，未知或不兼容时安全回退。
5. 任何可能造成重复、覆盖或删除的操作都不做无限重试。
6. 上传和下载继续优先流式，不把外部项目的完整缓存模型照搬到 VPS。
7. WPS Cookie、刷新票据、CSRF 和签名 URL 永不进入公开输出。
8. OpenList 是外部参考，不是 WPS 官方保证，也不是本项目的源码依赖。
