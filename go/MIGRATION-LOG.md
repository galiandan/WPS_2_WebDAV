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

## B002 记录性能基线

日期：2026-09-05

工具：新增 `go/benchmarks/python_baseline.py`（迁移专用，不进发布产物）。
方法：真实 Python 适配器跑在子进程，WPS 传输层换成进程内 fake upstream
（使用 client 本身提供的测试注入点，未修改参照实现）；父进程经真实
loopback HTTP 驱动并测量。完整数据在
`go/benchmarks/results/python-baseline.json`（同仓库保存，全部为
bench-* 占位数据，无任何真实 ID/Cookie/URL）。

环境：Linux 7.2.2-zen1-1-zen x86_64，Python 3.14.7，16 CPU，15 GiB RAM。
配置：默认参数（list_count=20、cache_ttl=2s、stream_chunk=1MiB、
max_uploads=2、max_downloads=4），仅 WPS_UPLOAD_MIN_FREE_BYTES=0 与
关闭 Basic Auth（loopback 基准）。

主要结果（p50，另见 JSON）：

| 场景 | 数值 |
| --- | --- |
| /healthz keep-alive | 41.0 ms |
| /healthz 新建连接 | 0.15 ms |
| /api/v1/status 冷 | 0.74 ms（fake upstream） |
| /api/v1/status 缓存命中（keep-alive） | 41.0 ms |
| REST 列表 204 条 冷（10 页上游分页） | 3.2 ms |
| REST 列表 204 条 热缓存（keep-alive） | 41.0 ms |
| PROPFIND Depth 1（204 条） | 6.2 ms |
| 下载 8 MiB / 64 MiB | 79 / 98 MiB/s，SHA-256 校验一致 |
| 上传 1 MiB / 8 MiB（spool+三摘要+signed PUT+登记） | 167 / 359 MiB/s，SHA-256 校验一致 |
| RSS 空闲 / 峰值 | 31.4 MiB / 43.3 MiB |
| 上游请求数（全部场景累计） | control list 89 次、islogin 1、object GET 5、PUT 2 |
| 文件描述符（结束后） | 4（无泄漏） |

发现 1（重要，影响所有 keep-alive 小响应）：Python 服务端接受的套接字
未禁用 Nagle（`disable_nagle_algorithm=False`）且响应为多次小段写，
keep-alive 连接上每个小响应稳定多出约 40 ms（41ms vs 新建连接 0.15ms；
在服务端 handler 上设置 TCP_NODELAY 后同场景 0.056 ms，客户端侧设置
无效，证明停顿在服务端）。该行为是当前 Python 可观察行为的一部分；
Go 侧（net/http 默认 TCP_NODELAY）不会复现。**属于潜在行为差异，
是否要求 Go 复现该停顿交由负责人决定（默认建议：不复现，记录为
已批准变更，因为它不是协议语义而是 TCP 交互特征）。**

发现 2（客户端取消释放）：断开检测只在流循环的两个 chunk 之间进行。
- RST 断开（SO_LINGER(0)，写路径立即出错）：上游对象流 138 ms 内释放，
  下载槽立即可复用。
- 干净 FIN 断开（接收缓冲区空）：服务端阻塞在 `sendall`，直到
  `ADAPTER_REQUEST_TIMEOUT`（默认 60 s）写超时才释放上游流；实测
  60.4 s。这是当前真实行为，Go 需要决定是否复现（见 D 系列决定，
  未决定前以保持行为为准）。

未运行项：真实 WPS 专用目录的低频小文件与 100 MiB 文件测试。原因：
执行环境无负责人凭据，且红线禁止执行模型访问真实账号；此项归入
灰度阶段（M1002）由负责人执行。fake upstream 方法论已固定，Go 用
同一 harness 形状对比。

回滚：仅新增 `go/benchmarks/` 与本记录；`git revert` 本提交即可。
