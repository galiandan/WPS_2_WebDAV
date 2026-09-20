# 离线浏览器回归检查

安装 Python 的 `playwright` 包及 Chromium 后，在仓库根目录运行：

```sh
python test/ui-offline/space_navigation.py
```

也可通过 `WPS_UI_BROWSER` 指定现有 Chrome/Chromium 可执行文件。

测试通过浏览器请求拦截提供本地静态资源和可控 API 响应，不启动 HTTP 服务、不连接 WPS。空间切换用延迟响应验证即时高亮、快速切换不被旧响应覆盖、导航按钮和焦点稳定，以及缓存有效期、强制刷新与失败后的加载反馈。
