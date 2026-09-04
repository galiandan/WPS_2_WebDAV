# 登录状态预检

> 文档类型：OpenList 借鉴方向之一
>
> 更新时间：2026-09-04
>
> 适用范围：只处理当前账号正常获得的 WPS 会话，不绕过登录、权限、验证码或租户隔离。

## 1. 目标

登录状态预检用于区分两个容易混淆的状态：

```text
适配器进程正常运行 != WPS 会话有效
```

它解决的用户体验问题是：

- 未配置 Cookie 时，网页明确显示“尚未连接 WPS”；
- Cookie 失效时，网页显示“登录已过期”，而不是笼统的上游错误；
- WPS 网络暂时不可达时，显示“WPS 暂时不可用”；
- WPS 会话有效但目标工作区无权访问时，显示权限错误；
- 状态检查不泄露 Cookie、CSRF、刷新票据、用户 ID 或签名下载地址。

## 2. 证据边界

| 内容 | 证据级别 | 说明 |
| --- | --- | --- |
| `GET /api/v1/status` 是本项目的本地状态接口 | observed in repository | 已在当前项目实现，受适配器 Basic Auth 保护 |
| 用 `GET https://account.kdocs.cn/api/v3/islogin` 判断账号会话 | candidate | 来自 OpenList 参考；当前账号 HAR 未确认该请求 |
| 文件 API 返回 401 可表示会话失效 | observed | 本项目错误处理和现有登录流程以该行为为依据 |
| 根目录最小列表可证明工作区可读 | inferred/design rule | 这是本项目的验证策略，不是 WPS 官方保证 |
| `islogin` 返回字段 `companyid`、`userid` 等 | external-reference | 只见于 OpenList 代码，不应直接当作当前账号契约 |

候选接口在没有当前账号新抓包和低风险重放之前，不得成为对外承诺，也不得据此实现权限绕过或自动选择其他空间。

## 3. 当前实现

相关实现位于 `src/wps_adapter/client.py` 和 `src/wps_adapter/web.py`。

登录助手还会在同步新 Cookie、CSRF 和工作区之前，使用这组新凭据请求一次已确认的企业文件列表接口。只有目标 `group_id + root_id` 返回合法目录列表后，才会执行 SSH、HTTP 或本地文件同步；验证失败不会覆盖已有凭据。

### 3.1 状态模型

当前状态被归纳为以下几类：

| 状态 | 含义 |
| --- | --- |
| `checking` | 正在执行最近一次检查 |
| `not_configured` | Cookie、CSRF 或工作区配置缺失 |
| `connected` | 会话预检成功且选定工作区可读 |
| `session_expired` | WPS 拒绝会话，刷新也没有恢复 |
| `permission_denied` | 会话有效，但目标群组或目录无权访问 |
| `upstream_unavailable` | DNS、TLS、超时或 WPS 5xx 等上游故障 |
| `invalid_response` | 上游响应不是预期 JSON 或字段无法解析 |

`/healthz` 只检查 Python 进程是否能接受请求，不访问 WPS；`/api/v1/status` 才表达 WPS 连接状态。

### 3.2 缓存和并发

状态检查具有有限缓存和失败退避：

- 成功结果默认缓存约 30 秒；
- 失败结果默认退避约 5 秒；
- 同一时间只允许一个状态探测请求；
- Cookie、CSRF、群组 ID 和根目录变化会使缓存失效；
- 普通文件请求仍然可以直接发现真实上游错误，不应被状态缓存永久阻塞。

对应配置为 `WPS_STATUS_PROBE_TTL` 和 `WPS_STATUS_FAILURE_BACKOFF`。

## 4. 设计流程

```text
请求 /api/v1/status
        |
        +-- 凭据缺失 ----------------> not_configured
        |
        +-- 可选：候选 islogin --------> 会话可识别？
        |                                  |
        |                                  +-- 否 -> session_expired
        |
        +-- 根目录最小列表 ------------> 工作区可读？
                                           |
                                           +-- 否 -> permission_denied / upstream_unavailable
                                           +-- 是 -> connected
```

预检不应主动高频刷新 refresh token。刷新只在真实文件请求遇到 401 后按现有凭据刷新策略触发，并且原请求最多重试一次。

## 5. 验证步骤

在本人测试目录执行，所有 HAR 和响应中的 Cookie、CSRF、签名 URL 及账号对象 ID 都不得进入仓库：

1. 空凭据启动适配器，确认 `/healthz` 正常、`/api/v1/status` 为 `not_configured`。
2. 导入有效凭据，确认状态为 `connected`，再确认目录列表成功。
3. 在 WPS 网页端撤销当前会话，确认状态变为 `session_expired`。
4. 重新运行登录助手替换凭据，确认无需重启适配器即可恢复。
5. 临时制造 DNS、TLS 或超时错误，确认显示 `upstream_unavailable`。
6. 使用无权目录测试 `permission_denied`，确认不会静默切换目录或群组。
7. 并发刷新多个页面，确认短时间内不会产生同数量的重复预检请求。

## 6. 不应做的事情

- 不把进程在线直接显示为 WPS 已连接；
- 不把 `islogin` 当成文件夹权限证明；
- 不在网页或日志显示完整上游响应；
- 不把候选接口写成 WPS 官方稳定 API；
- 不定时高频调用未知刷新接口；
- 不因预检暂时失败就删除或覆盖已有工作区配置。
