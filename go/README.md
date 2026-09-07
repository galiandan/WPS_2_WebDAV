# wps-adapter (Go)

本目录是 WPS 2 WebDAV 的长期运行服务，module path 为
`github.com/galiandan/WPS_2_WebDAV/go`。它已经替代 VPS 上的 Python 常驻
服务；Python 只保留为协议参照实现、开发工具和本地 `wps_login.py` 登录助手。

## 常用命令

```sh
cd go
go fmt ./...
go vet ./...
go test ./...
go test -race ./...
CGO_ENABLED=0 go build -trimpath -o /tmp/wps-adapter ./cmd/wps-adapter
```

发布目标是 Linux `amd64` 和 `arm64`。本地交叉构建：

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o /tmp/wps-adapter-linux-amd64 ./cmd/wps-adapter
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -o /tmp/wps-adapter-linux-arm64 ./cmd/wps-adapter
```

## 构建元数据

默认版本为 `0.9.8`。发布构建应注入提交号和 UTC 构建时间：

```sh
CGO_ENABLED=0 go build -trimpath \
  -ldflags "-s -w -X main.version=0.9.8 -X main.commit=$(git rev-parse HEAD) -X main.buildTime=$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  -o /tmp/wps-adapter ./cmd/wps-adapter
```

```sh
/tmp/wps-adapter --version
# 0.9.8 commit=<commit> build_time=<UTC时间>
```

## 命令

- `wps-adapter serve [--bind ADDRESS] [--port PORT]`：启动 WebDAV、REST 和网页服务。
- `wps-adapter check-config`：校验本地配置并组装服务，不访问 WPS。
- `wps-adapter --version`：输出版本、提交号和构建时间摘要。

## 目录约定

- `cmd/wps-adapter/`：CLI、应用组装、信号处理和退出码。
- `internal/config/`：环境变量、默认值和运行期校验。
- `internal/app/`：凭据、workspace、WPS 客户端、预算、存储和 HTTP 组装。
- `internal/wps/`：WPS 控制面、签名对象存储、上传、下载和 refresh。
- `internal/storage/`：路径、单/多空间、分页、缓存和 COPY 中继。
- `internal/httpserver/`：REST、WebDAV、Basic Auth、锁和静态资源路由。
- `web/`：嵌入二进制的 `index.html`、`style.css`、`app.js`，没有前端构建步骤或外部资源。

## 运行约束

服务使用纯 Go、`CGO_ENABLED=0` 单二进制构建，不需要 Python、Node.js 或运行时
依赖。Native 安装器会优先使用主机 Go `1.25+`，否则自动下载并校验固定版本的
Go 工具链；Docker 最终镜像为 `scratch`，只包含服务二进制和 CA 证书。

网页登录和 WebDAV 共用同一个 Basic Auth。WPS Cookie、CSRF、workspace 和
refresh 轮换文件由配置指定，服务不会把它们写入日志。所有上传、下载、目录
递归、并发和临时磁盘操作都经过预算限制。

## 前端

前端由 Go `embed` 提供。它支持多空间根目录、列表/网格视图、目录预取、拖放
上传、上传速度和进度、下载、重命名、移动、删除、新建文件夹、搜索、主题和
云盘显示名称设置。资源保持原生 HTML/CSS/JavaScript，无浏览器扩展和第三方
前端依赖。

## 迁移记录

实现任务和 Python/Go 对照证据见 [`MIGRATION-LOG.md`](MIGRATION-LOG.md)，总体
计划见 [`../docs/go-rewrite-plan/`](../docs/go-rewrite-plan/)。`src/wps_adapter/`
仍保留用于参照和回滚验证，不应被 Native 或 Docker 服务启动。
