# Contributing

感谢参与。这个项目仍处于实验阶段，重点是把经过本人账号验证的 WPS 行为整理成可审查、可回归测试的适配器。

## 开始之前

- 只提交与本人账号或公开文档有关的、经过授权的改动。
- 不要猜测或扩展 WPS 权限，不要测试其他用户、租户或分享链接。
- 不要提交 Cookie、CSRF、refresh token、Authorization、签名 URL、完整 HAR、文件内容或个人部署信息。

## 本地检查

项目只依赖 Python 标准库，测试不访问 WPS：

```bash
PYTHONPATH=src python3 -m unittest discover -s tests -v
python3 -m compileall -q src tests
python3 tools/build_login_script.py --check
python3 tools/build_release_manifest.py --check
git diff --check
```

涉及真实 WPS 行为的改动，需要在 `docs/research/findings.md` 中记录实验编号、证据等级和脱敏后的请求形状。原始抓包只保存在本机 `captures/`，不要放入提交。

提交消息应简短说明行为变化，例如 `Add multipart upload retry`。提交前检查 `git status`，确认没有本地环境文件、临时文件或凭据。
