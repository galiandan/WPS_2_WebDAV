# wps-adapter (Go)

本目录是 Go 重写的 module 根（module path
`github.com/galiandan/WPS_2_WebDAV/go`）。迁移细纲见
`../docs/go-rewrite-plan/`，逐任务证据见 `MIGRATION-LOG.md`。

## 常用命令

```sh
cd go

# 格式化与静态检查（每个任务提交前都要全绿）
go fmt ./...
go vet ./...

# 单元测试与竞态测试
go test ./...
go test -race ./...

# 本机构建
go build -o /tmp/wps-adapter ./cmd/wps-adapter

# 交叉构建（B200 完成条件：Windows 开发二进制 + Linux amd64/arm64）
GOOS=windows GOARCH=amd64 go build -o /tmp/wps-adapter.exe ./cmd/wps-adapter
GOOS=linux GOARCH=amd64 go build -o /tmp/wps-adapter-linux-amd64 ./cmd/wps-adapter
GOOS=linux GOARCH=arm64 go build -o /tmp/wps-adapter-linux-arm64 ./cmd/wps-adapter
```

## 版本注入

默认版本与 Python 参照实现对齐（0.9.8）。发布构建注入提交信息：

```sh
go build -ldflags "-X main.version=0.9.8 -X main.commit=$(git rev-parse --short HEAD)" \
  -o /tmp/wps-adapter ./cmd/wps-adapter
```

## 命令形状（B200 骨架）

- `wps-adapter --version`：输出版本、提交号和构建时间的非敏感摘要（首个字段为裸版本号）。
- `wps-adapter check-config`：校验环境配置并输出摘要，不访问 WPS。
- `wps-adapter serve --bind 127.0.0.1 --port 54321`：启动 HTTP 服务；
  骨架阶段仅提供 `/healthz`，真实路由随后续任务接入。

## 目录约定

- `cmd/wps-adapter/`：CLI、组装、信号与退出码。
- `internal/config/`：环境变量读取、默认值、集中校验。
- `internal/app/`：应用组装与生命周期。
- `web/`：前端三文件（index.html、style.css、app.js），Python 桥与
  Go embed 共用同一份，禁止复制第二份。页面自带内联 SVG 图标集，
  无外部资源、无构建步骤、无内联脚本（CSP 限制）。

## Web 前端

- 设计语言：简洁但不简单——克制的靛蓝→紫渐变只用于品牌标、主按钮
  与统计数字；玻璃拟态页头、背景极光、分层阴影构成质感。
- 主题：跟随系统 / 浅色 / 深色三态循环，跟随系统由 CSS
  `prefers-color-scheme` 实现，切换无闪烁；偏好保存在 localStorage。
- 视图：列表（可按名称/大小/修改时间排序，列头可点）与卡片网格
  双视图；目录加载期间展示骨架屏，空目录/搜索无结果/连接异常各有
  专属插画空状态。
- 动效：入场级联、行/卡片交错浮入、连接点脉冲、拖放遮罩行军蚁
  边框、弹窗弹性缩放、toast 滑入、上传进度环；全部尊重
  `prefers-reduced-motion`。
- 功能扩展（相对 Python 基线的行为增强，均已与后端契约对齐）：
  hash 路由（`#/路径`，刷新/前进/后退可用）、当前目录搜索高亮、
  上传队列托盘（逐文件状态、进度环、速度、整队取消）、移动对话框
  的文件夹选择器（目录接口不可用时回退手写路径）、同名文件夹
  冲突显式跳过提示、`/` 聚焦搜索、`Alt+↑` 返回上级、30 秒轮询在
  页面隐藏时暂停。
