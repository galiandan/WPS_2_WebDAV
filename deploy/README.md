# Deployment Templates

这些文件用于不依赖 Docker 的 systemd 部署：

- `wps-adapter.service`：服务单元。
- `wps-adapter-hardening.conf`：systemd drop-in。
- `wps-adapter-hardening.env`：低内存 VPS 的非秘密资源限制。

Docker 部署文件：

- `Dockerfile`：只包含 Python 标准库应用代码的镜像。
- `docker-compose.yml`：手动使用 Docker Compose 时的示例，`ADAPTER_PORT` 可自定义。

推荐直接使用仓库中的 `scripts/install-native.sh` 或 `scripts/install-docker.sh`；两者都支持 `--port PORT`、`--run-user USER`，默认使用执行 `sudo` 的当前用户，并且不会覆盖 `/etc/wps-adapter/secrets/`。

手动使用 Compose 时，宿主机端口映射由 Compose 的环境变量决定，`env_file` 只负责容器内部变量。先导出同一个端口，再启动：

```bash
export ADAPTER_BIND=0.0.0.0
export ADAPTER_PORT=18080
export WPS_ADAPTER_UID="$(id -u)"
export WPS_ADAPTER_GID="$(id -g)"
docker compose -f deploy/docker-compose.yml up -d --build
```

模板不包含 WPS Cookie、CSRF、Basic Auth 密码或其他部署凭据。完整安装说明见 [`../docs/deployment.md`](../docs/deployment.md)。
