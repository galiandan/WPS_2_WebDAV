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

- `wps-adapter --version`：输出版本号。
- `wps-adapter check-config`：校验环境配置并输出摘要，不访问 WPS。
- `wps-adapter serve --bind 127.0.0.1 --port 54321`：启动 HTTP 服务；
  骨架阶段仅提供 `/healthz`，真实路由随后续任务接入。

## 目录约定

- `cmd/wps-adapter/`：CLI、组装、信号与退出码。
- `internal/config/`：环境变量读取、默认值、集中校验。
- `internal/app/`：应用组装与生命周期。
- `web/`：前端三文件（index.html、style.css、app.js），Python 桥与
  Go embed 共用同一份，禁止复制第二份。
