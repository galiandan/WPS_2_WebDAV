# 离线浏览器回归检查

安装 Python 的 `playwright` 包及 Chromium 后，在仓库根目录运行：

```sh
python test/ui-offline/space_navigation.py
```

也可通过 `WPS_UI_BROWSER` 指定现有 Chrome/Chromium 可执行文件。

测试通过浏览器请求拦截提供本地静态资源和可控 API 响应，不启动 HTTP 服务、不连接 WPS。空间切换用延迟响应验证即时高亮、快速切换不被旧响应覆盖、导航按钮和焦点稳定，以及缓存有效期、强制刷新与失败后的加载反馈。

上传与路径回归：

```sh
python test/ui-offline/upload_and_paths.py
```

覆盖特殊字符目录刷新及历史导航、队列运行中禁止重试、部分成功后刷新列表、失败重试恢复，以及覆盖确认期间收到取消事件后不再上传。空间导航检查同时验证加载期间禁用上传／新建，结束后恢复。
