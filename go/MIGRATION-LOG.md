# Go 重写迁移记录

本文件是 `docs/go-rewrite-plan/` 的执行工作记录，只作迁移记录，不进发布产物。
规则见 `docs/go-rewrite-plan/08-executor-checklist.md`：一次只做一个小任务，
测试不过不进入下一任务，每完成任务一个提交。

## B000 记录工作区状态

日期：2026-09-05

- 当前提交：`25c2784e6aebdef4997b84baaa7b61b769552935`（分支 `main`）。
- 工作区状态：干净（`git status --porcelain` 为空），无用户未提交改动。
- 项目版本：`0.9.8`（`pyproject.toml`），`CHANGELOG.md` 仍有 `[Unreleased]` 内容。
- 参照测试入口：`PYTHONPATH=src python -m unittest discover -s tests -v`，
  CI 配置在 `.github/workflows/test.yml`。
- `wps_login.py` 是生成物，来源为 `tools/build_login_script.py`。
- Python 参照实现规模（迁移期保留，不修改）：

| 文件 | 行数 |
| --- | --- |
| src/wps_adapter/client.py | 2598 |
| src/wps_adapter/server.py | 1854 |
| src/wps_adapter/web.py | 1064 |
| src/wps_adapter/storage.py | 857 |
| src/wps_adapter/login.py | 1547 |
| src/wps_adapter/workspace.py | 326 |
| src/wps_adapter/login_command.py | 319 |
| src/wps_adapter/har.py | 457 |
| src/wps_adapter/settings.py | 209 |
| src/wps_adapter/__main__.py | 160 |
| src/wps_adapter/provider.py | 104 |
| src/wps_adapter/__init__.py | 34 |

- 本机工具链：Linux (zen kernel)，`go1.27.0`，git 可用。
- Go 代码放在本仓库 `go/` 目录下，module path 为
  `github.com/galiandan/WPS_2_WebDAV/go`；包结构遵循
  `03-target-architecture.md` 第 4 节（cmd/wps-adapter + internal/*）。

检查：没有源码或配置被修改。
