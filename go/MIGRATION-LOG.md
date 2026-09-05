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

## B001 建立 Linux 参照环境

日期：2026-09-05

本机即 Linux（zen kernel，x86_64），无需 WSL。工具链实测：

- Python `3.14.7`（满足 CI 矩阵 3.11-3.14 的上限）。
- Go `go1.27.0`（`-X:nodwarf5` 本地变体）。
- Git 可用，remote 为 `git@github.com:galiandan/WPS_2_WebDAV.git`。

运行命令与结果：

1. `PYTHONPATH=src python -m unittest discover -s tests`：
   首次运行 155 项，1 failure。
2. `PYTHONPATH=src python -m compileall -q src tests wps_login.py`：通过。
3. `bash -n scripts/install-native.sh scripts/install-docker.sh`：通过。
4. 登录脚本生成检查（`tools/build_login_script.py --check`，由测试套件覆盖）：通过。
5. release manifest 检查（`tools/build_release_manifest.py --check`，由测试套件覆盖）：首次失败。

失败归类（按 B001 要求）：

- `test_release_manifest_matches_its_builder`：**仓库缺陷**，非环境缺失、非平台假设。
  `docs/go-rewrite-plan/` 与 `go/MIGRATION-LOG.md` 在最近提交中加入后，
  生成产物 `release-manifest.txt` 未重新生成。修复方式是运行
  `python tools/build_release_manifest.py` 重新生成（纯生成产物，无行为变化），
  已包含在本任务提交中。未发现 Windows 平台特有的失败（与 00-README 第 8 节
  描述的 Windows 结果不同，Linux 上仅此 1 项失败）。
- 未发现需要单独开修复任务的真实功能缺陷；未顺手修改任何功能。

修复后复跑：`PYTHONPATH=src python -m unittest discover -s tests` →
**155 项全部通过（OK）**。

完成条件达成：已有可信的 Linux Python 参照基线（155 全绿 + compileall + shell
语法 + 两个生成物检查），后续任务以它为协议 oracle。

回滚：本任务仅重新生成 `release-manifest.txt` 并追加本记录；回滚即
`git revert` 本提交。
