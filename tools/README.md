# Tools

`build_login_script.py` generates the standalone `wps_login.py` helper.
`build_release_manifest.py` generates `release-manifest.txt`, which the
one-command installers verify before installing source files. After changing
tracked project files, run both generators and keep the manifest digest in the
two installer scripts synchronized.

这里的工具均使用 Python 标准库：

- `har_inspect.py`：输出 HAR 的脱敏摘要，或生成脱敏副本。
- `wps_har_probe.py`：从本机 HAR 重放已经观察到的本人账号读请求。
- `wps_curl_probe.py`：从本机粘贴的 cURL 请求重放列表/下载实验，不打印 Cookie。
- `wps_probe.py`：使用隐藏式输入的 Cookie 做本人账号的最小列表/下载探针。
- `build_login_script.py`：从登录源码生成可单独下载的 `wps_login.py`。

普通用户获取 Cookie 只需要下载仓库根目录的 `wps_login.py`，不需要 clone 整个项目。维护源码后运行：

```bash
python3 tools/build_login_script.py
python3 tools/build_login_script.py --check
```

原始 HAR 只能保存在本机 `captures/`，不要提交或发送到聊天。工具不用于扫描接口、枚举 ID 或访问其他用户数据。
