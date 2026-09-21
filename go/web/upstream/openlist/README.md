# OpenList 文件浏览器组件移植

来源：<https://github.com/OpenListTeam/OpenList-Frontend>，MIT。
2026-09-21 下载的 main 归档，package.json 版本 4.2.6；归档和原始文件的 SHA-256 记录在 manifest.json。

本目录保留未经修改的上游组件，供审阅、归属声明和以后同步时比较。它们不是直接运行的 Solid 应用，也不在 Go 静态资源白名单内。

当前运行版本在 ../../index.html、../../style.css 和 ../../app.js。移植将 Hope UI 的 JSX 布局属性转换为原生 DOM/CSS，保留现有 Go embed 构建。没有接入 OpenList 的后端、管理后台或存储驱动。

| 上游组件 | 当前落点 |
| --- | --- |
| Container / Body | 980px 居中容器、2% 内边距、80vh 内容区 |
| header/Header / Nav | 顶部搜索与视图切换、路径面包屑 |
| Obj | 8px 内边距、12px 圆角、阴影文件容器 |
| folder/List / ListItem | 名称、大小、修改时间三列，50/17/33 比例；移动端 76/24；整行打开与右键操作 |
| folder/Grid / GridItem | 100px 图标区、120px 最小网格宽度、居中文件名 |
| toolbar/Right / Icon | 右下角可收起的工具栏、蓝色操作图标 |
| Footer | 页面底部设置图标入口；来源和许可证保留在源码中 |
| app/theme | 浅色背景、圆角、主色和控件表面 |

WPS 空间导航、会话登录、TOTP/Passkey、版本更新、文件读写和上传队列继续使用 app.js 内已有的 /api/v1/ 接口。操作弹窗沿用本项目实现；文件图标沿用已有 SVG，使用上游统一主色。悬浮工具栏在手机端默认收起，用户切换后记录本地偏好。触屏显示文件操作按钮，桌面支持悬停、键盘聚焦和右键菜单。

许可证完整内容见 LICENSE；运行页面的 HTML 同样保留 MIT 声明。
