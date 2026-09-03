# Security Policy

## Scope

这是一个面向本人已授权 WPS 企业云盘账号的实验性适配器，不是 WPS 官方客户端或官方 API SDK。WPS 接口、登录态和对象存储签名可能随时变化。

## Sensitive data

以下内容永远不要提交到 GitHub、Issue、聊天或日志：

- WPS Cookie、`rtk`、CSRF、refresh token、Authorization 和签名 URL。
- 原始 HAR、网络响应中的文件内容和个人账号信息。
- VPS 私钥、Basic Auth 密码、真实部署地址和企业空间标识。

如果凭据意外泄露，应立即在 WPS 和 VPS 上撤销/轮换，而不是只删除 Git 历史中的文件。

## Reporting

不要在公开 Issue 中粘贴认证请求或原始抓包。请先移除所有敏感值，再通过 GitHub 私密渠道或维护者约定的安全联系方式报告问题。
