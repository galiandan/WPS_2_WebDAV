# Research Notes

这里保存 WPS 逆向适配实验的公开、脱敏记录。原始 HAR、Cookie、Token、签名 URL 和文件内容不属于仓库。

| File | Purpose |
| --- | --- |
| [`scope-and-safety.md`](scope-and-safety.md) | 授权范围和安全边界 |
| [`capture-plan.md`](capture-plan.md) | 网页端抓包与实验顺序 |
| [`findings.md`](findings.md) | 已观察、重放和推断的请求事实 |
| [`prototype.md`](prototype.md) | 当前原型状态与研究边界 |
| [`request-record-template.md`](request-record-template.md) | 脱敏实验记录模板 |
| [`openlist-reference.md`](openlist-reference.md) | OpenList 借鉴总览、优先级和统一研究流程 |
| [`01-native-copy.md`](01-native-copy.md) | P0：原生 COPY |
| [`02-large-directory-depth.md`](02-large-directory-depth.md) | P0：大目录分页和 `Depth: infinity` |
| [`03-resumable-multipart.md`](03-resumable-multipart.md) | P1：分片失败续传 |
| [`04-upload-resource-protection.md`](04-upload-resource-protection.md) | P1：上传并发、缓存和资源保护 |
| [`05-duplicate-file-policy.md`](05-duplicate-file-policy.md) | P1：重复文件处理策略 |

每条 WPS 相关结论都应绑定实验编号和证据等级。没有本人账号实验支持的行为只能标记为 `unknown` 或 `inferred`，不能当成稳定 API。
