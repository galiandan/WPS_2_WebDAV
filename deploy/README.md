# Deployment Templates

这些文件用于不依赖 Docker 的 Native 部署：

- `wps-adapter.service`：服务单元。
- `wps-adapter-hardening.conf`：systemd drop-in。
- `wps-adapter-hardening.env`：低内存 VPS 的非秘密资源限制。

Native 安装器优先使用 systemd；没有 systemd 的主机会使用便携后台模式，并将 PID 和日志保存到部署目录的 `data/` 和 `logs/`。安装器会识别常见 Linux 包管理器：`apt`、`dnf`、`yum`、`apk`、`pacman`、`zypper` 和 `xbps-install`。

Docker 部署文件：

- `Dockerfile`：Go 多阶段构建文件；最终镜像只包含静态 Go 服务和 CA 证书，不包含 Python 或 Go 工具链。
- `docker-compose.yml`：手动使用 Docker Compose 时的示例，`ADAPTER_PORT` 可自定义。

Docker 安装器会在当前发行版提供时自动安装 Buildx，并优先使用 Buildx 构建；没有该插件时才使用 Docker 自带的兼容构建器。

推荐直接使用仓库中的 `scripts/install-native.sh` 或 `scripts/install-docker.sh`；两者都支持 `--port PORT`、`--run-user USER`，默认使用执行 `sudo` 的当前用户，并将所有应用配置集中到部署目录的 `config/`。执行命令时若当前目录是根目录或用户主目录，默认部署目录为 `/opt/wps-adapter`；在其他工作目录执行时使用当前目录。安装器先从国内加速的 GitHub Release 下载预编译二进制，失败后才下载固定提交源码并现场编译；不会执行归档、二进制或工具链哈希校验。登录助手只通过 SSH 私钥或 SSH 密码写入 `config/secrets/`。网页显示名称可在登录网页后点击右上角齿轮修改，并保存到 `config/secrets/web-settings.json`。可用 `WPS_ADAPTER_DIR` 覆盖部署目录，或用 `WPS_ADAPTER_BINARY_RELEASE_TAG` 和 `WPS_ADAPTER_BINARY_BASE_URL` 指向自己的预编译 Release 目录。

卸载时使用仓库中的 `scripts/uninstall.sh`。脚本会自动识别 Native 和 Docker，并删除本机配置、凭据、服务和应用文件；`--remove-image` 还会删除项目 Docker 镜像。没有 Docker 时会自动跳过 Docker 清理。卸载不会删除 WPS 云盘上的文件或 Docker 软件本身。

手动使用 Compose 时，宿主机端口映射由 Compose 的环境变量决定，`env_file` 只负责容器内部变量。先导出同一个端口，再启动：

```bash
export ADAPTER_BIND=0.0.0.0
export ADAPTER_PORT=18080
export WPS_ADAPTER_UID="$(id -u)"
export WPS_ADAPTER_GID="$(id -g)"
docker compose -f deploy/docker-compose.yml up -d --build
```

如果 Docker Hub 在当前网络不可用，可额外设置 `WPS_ADAPTER_GO_BUILDER_IMAGE`，例如 `docker.m.daocloud.io/library/golang:1.25.0`。

模板不包含 WPS Cookie、CSRF、Basic Auth 密码或其他部署凭据。完整安装说明见 [`../docs/deployment.md`](../docs/deployment.md)。

Docker Compose 示例将 secret 目录保持可写，以支持“同目录临时文件 + 原子替换”的会话轮换；同时把默认的 Basic Auth 文件覆盖为只读挂载。若环境文件使用自定义文件名，需要同步修改 Compose 的两个只读文件挂载。

Compose 的目录挂载必须保持可写：Cookie/CSRF 轮换需要在目录内创建临时文件后再原子替换目标文件。
