# Tools

这里仅保留发布清单工具。它不依赖 Python：

- `build-release-manifest.sh`：生成或校验 `release-manifest.txt`，安装器会在部署前
  用它对应的 SHA-256 清单验证源码归档。

修改仓库文件后运行：

```bash
bash tools/build-release-manifest.sh
bash tools/build-release-manifest.sh --check
```

普通用户获取 Cookie 只需要下载仓库根目录的 `wps_login.py`，不需要 clone 整个项目。
原始 HAR 只能保存在本机 `captures/`，不要提交或发送到聊天。
