# 离线浏览器回归检查

使用 `python -m pip install -r test/ui-offline/requirements.txt` 安装 Playwright，并运行 `python -m playwright install chromium` 安装浏览器后，在仓库根目录运行：

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

目录树回归：运行 python test/ui-offline/directory_tree.py。

覆盖按需展开、缓存复用、精确高亮、键盘导航、新建同步、失败重试、删除后的迟到响应、重命名刷新、深层直达链接和手机抽屉。截图保存到 /tmp/wps-directory-tree-desktop.png 和 /tmp/wps-directory-tree-mobile.png。

图片和 PDF 预览回归：运行 `python test/ui-offline/media_preview.py`。

覆盖图片缩放、同目录切图、特殊字符路径、读取失败、关闭释放元素、文本切换、禁止主动文档格式与手机布局，并使用生产页面 CSP。PDF 截图可用于检查浏览器内置阅读器；截图保存到 `/tmp/wps-media-preview-desktop.png`、`/tmp/wps-media-preview-mobile.png` 和 `/tmp/wps-pdf-preview-desktop.png`。

批量与文件夹上传回归：运行 `python test/ui-offline/batch_and_folders.py`。覆盖勾选/Shift、多选筛选、复制移动删除逐项反馈、ZIP 表单下载、目录结构、空目录、重名冲突、取消与重试。

全局搜索回归：运行 `python test/ui-offline/global_search.py`。覆盖索引范围、类型/路径筛选、分页、扫描取消、不完整提示、过期响应、结果预览/定位及手机布局。截图保存到 `/tmp/wps-global-search-desktop.png` 和 `/tmp/wps-global-search-mobile.png`。

GitHub CI 会运行此目录的全部离线浏览器脚本，并保留截图供检查。PDF 阅读器测试使用完整 Chromium；本地也可将 `WPS_UI_BROWSER` 指向 Chrome 或 Chromium。
