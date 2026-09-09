# wps-adapter (Go)

本目录是 WPS 2 WebDAV 的生产服务，module path 为
`github.com/galiandan/WPS_2_WebDAV/go`。它是 VPS 上长期运行的唯一服务实现；
Python 只保留为用户电脑上的独立登录助手。

## 常用命令

```sh
cd go
go fmt ./...
go vet ./...
go test ./...
go test -race ./...
CGO_ENABLED=0 go build -trimpath -o /tmp/wps-adapter ./cmd/wps-adapter
```

发布目标是 Linux `amd64`、`arm64`、`386`、`armv6`、`armv7`、`ppc64le`、`riscv64` 和 `s390x`。本地交叉构建示例：

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o /tmp/wps-adapter-linux-amd64 ./cmd/wps-adapter
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -o /tmp/wps-adapter-linux-arm64 ./cmd/wps-adapter
```

带 `v` 前缀的 Git 标签会触发 `.github/workflows/release-binaries.yml`，自动生成上述 Linux 静态二进制并上传到 GitHub Release。Native/Docker 安装器优先下载这些资产；下载失败时才回退到源码和 Go 工具链现场构建。安装器不执行二进制或源码归档哈希校验，但会检查二进制是否可执行并能输出版本信息。

## 构建元数据

默认版本为 `0.9.96`。发布构建应注入提交号和 UTC 构建时间：

```sh
CGO_ENABLED=0 go build -trimpath \
  -ldflags "-s -w -X main.version=0.9.96 -X main.commit=$(git rev-parse HEAD) -X main.buildTime=$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  -o /tmp/wps-adapter ./cmd/wps-adapter
```

```sh
/tmp/wps-adapter --version
# 0.9.96 commit=<commit> build_time=<UTC时间>
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
- `internal/storage/`：路径、当前空间映射、分页、缓存和 COPY 中继。
- `internal/httpserver/`：REST、WebDAV、网页会话认证、Basic Auth、锁和静态资源路由。
- `web/`：嵌入二进制的 `index.html`、`style.css`、`app.js`，没有前端构建步骤或外部资源。

## 运行约束

服务使用纯 Go、`CGO_ENABLED=0` 单二进制构建，不需要 Python、Node.js 或运行时
依赖。Native/Docker 安装器会优先下载对应架构的预编译二进制；只有预编译路径失败时，
Native 才使用主机 Go `1.25+` 或下载固定版本的 Go 工具链，Docker 才下载 Go 构建镜像。
Docker 最终镜像为 `scratch`，只包含服务二进制和 CA 证书。安装器不执行发布归档、二进制
或工具链哈希校验。

网页使用安装时设置的唯一适配器账号登录和 HttpOnly 会话 Cookie；WebDAV 继续使用
同一组 Basic Auth 凭据。账号不写入额外数据库，凭据仍由安装器创建的受限 secret
文件管理；服务不会把它们写入日志。所有上传、下载、目录递归、并发和临时磁盘
操作都经过预算限制。

## 前端

前端由 Go `embed` 提供。它支持多空间虚拟目录、独立的单 WebDAV 子树、列表/网格视图、目录预取、拖放
上传、上传速度和进度、下载、重命名、移动、删除、新建文件夹、搜索、主题和
云盘显示名称设置。资源保持原生 HTML/CSS/JavaScript，无浏览器扩展和第三方
前端依赖。

## 代码边界

- `go/` 是 Native 和 Docker 使用的生产代码。
- `wps_login.py` 负责在用户自己的电脑上完成官方 WPS 登录并同步凭据。
- `contract_tests/results/` 只保存脱敏后的 JSON 契约金标准，供 Go 回归测试读取。

重写过程文档已经在 Go 版本定稿后移除；当前维护以代码、契约金标准、
[`docs/api.md`](../docs/api.md) 和 [`docs/architecture.md`](../docs/architecture.md)
为准。
