# Contributing

感谢参与。这个项目仍处于实验阶段，重点是把经过本人账号验证的 WPS 行为整理成可审查、可回归测试的适配器。

## 开始之前

- 只提交与本人账号或公开文档有关的、经过授权的改动。
- 不要猜测或扩展 WPS 权限，不要测试其他用户、租户或分享链接。
- 不要提交 Cookie、CSRF、refresh token、Authorization、签名 URL、完整 HAR、文件内容或个人部署信息。

## 本地检查

服务和回归测试只使用 Go 标准工具链，不访问 WPS：

```bash
cd go
gofmt -d .
go test ./...
go vet ./...
CGO_ENABLED=0 go build -trimpath -o /tmp/wps-adapter ./cmd/wps-adapter
cd ..
bash -n scripts/install-native.sh scripts/install-docker.sh scripts/uninstall.sh
git diff --check
```

`wps_login.py` 是给最终用户使用的独立登录助手，不参与 VPS 服务构建；修改它时要
同时运行 `python3 -m py_compile wps_login.py`。

涉及真实 WPS 行为的改动，需要在 `docs/research/findings.md` 中记录实验编号、证据等级和脱敏后的请求形状。原始抓包只保存在本机 `captures/`，不要放入提交。

提交消息应简短说明行为变化，例如 `Add multipart upload retry`。提交前检查 `git status`，确认没有本地环境文件、临时文件或凭据。
