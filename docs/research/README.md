# Research Notes

这里保存 WPS 逆向适配实验的公开、脱敏记录。原始 HAR、Cookie、Token、签名 URL 和文件内容不属于仓库。

| File | Purpose |
| --- | --- |
| [`scope-and-safety.md`](scope-and-safety.md) | 授权范围和安全边界 |
| [`capture-plan.md`](capture-plan.md) | 网页端抓包与实验顺序 |
| [`findings.md`](findings.md) | 已观察、重放和推断的请求事实 |
| [`prototype.md`](prototype.md) | 当前原型状态与研究边界 |
| [`request-record-template.md`](request-record-template.md) | 脱敏实验记录模板 |
| [`openlist-reference.md`](openlist-reference.md) | OpenList 借鉴总览、证据规则和研究流程 |
| [`01-login-status-preflight.md`](01-login-status-preflight.md) | 登录状态预检 |
| [`02-enterprise-space-discovery.md`](02-enterprise-space-discovery.md) | 企业空间、群组和目录发现 |
| [`03-upload-download-multipart.md`](03-upload-download-multipart.md) | 上传、下载和分片传输 |
| [`04-webdav-adapter-design.md`](04-webdav-adapter-design.md) | WebDAV 协议适配和兼容性 |

每条 WPS 相关结论都应绑定实验编号和证据等级。没有本人账号实验支持的行为只能标记为 `unknown` 或 `inferred`，不能当成稳定 API。
