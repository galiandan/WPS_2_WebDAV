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

登录初始化回归：

```sh
python test/ui-offline/auth_startup.py
```

延迟脚本与会话响应，检查首屏及检查期间不显示登录表单，覆盖已登录、未登录、未启用认证和网络失败。

纯文本预览回归：

```sh
python test/ui-offline/text_preview.py
```

覆盖中文编码与手动切换、空文件、多字节截断、安全显示、二进制提示、请求失败与取消、阅读控制及手机布局。截图保存到 `/tmp/wps-text-preview-desktop.png` 和 `/tmp/wps-text-preview-mobile.png`。

新建文本文件回归：运行 python test/ui-offline/text_create.py。

覆盖 UTF-8 编写保存及随后预览、空文件、特殊字符路径、文件名校验、同名保护、失败/关闭保留草稿及目标目录、加载和保存状态、手机布局；截图保存到 /tmp/wps-text-create-desktop.png 和 /tmp/wps-text-create-mobile.png。
