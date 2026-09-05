# 契约测试（语言无关，黑盒）

本目录是与实现语言无关的黑盒契约测试。测试只通过 HTTP/原始 TCP 与被测
服务对话，不 import 适配器内部模块；被测服务由 harness 以子进程启动。

## 被测服务与输入方式

- `harness.Service` 负责启动被测服务：
  - 当前被测对象：真实 Python 服务入口（`python_service.py` 调用
    `wps_adapter.__main__.main`），仅 WPS HTTP 传输层替换为进程内
    fake upstream（`fake_upstream.py`，使用 client 自带的测试注入点）。
  - 未来被测对象：Go 服务入口。场景与断言不变，harness 以相同的环境
    变量、secret 文件与 scenario JSON 启动 Go 二进制即可。
- 服务地址：子进程绑定 harness 预分配的 loopback 端口，固定输出
  `listening=` 行作为就绪信号。
- Basic Auth：`ADAPTER_USERNAME_FILE` / `ADAPTER_PASSWORD_FILE`（0600，
  位于 0700 临时目录）。
- fixture upstream：无独立网络端口；fake upstream 与服务同进程，路由与
  行为由 `scenario JSON` 描述（路由正则、状态码、JSON/文本响应、延迟、
  barrier 并发屏障、对象存储内容）。
- 临时 secret 目录：`tempfile.mkdtemp`（0700），cookie/csrf/workspace 文件
  全部 0600；内容均为 `bench-*` 占位值。

## 观测与证据

- 每个上游请求（method、path、host、是否携带 Cookie/Authorization 等）
  追加写入 `upstream-requests.jsonl`。
- 并发计数、对象 PUT 摘要写入 `upstream-stats.json`（原子替换）。
- 每个场景把观察到的真实行为写入 `results/<场景ID>.json`，作为
  Python 基线证据；Go 对照结果将写入 `results/go/`。

## 场景分组与 ID

- `DEC-D01-A` 起为 D-01..D-09 兼容性决策特征测试（`test_decisions.py`）。
- 后续按 `docs/go-rewrite-plan/06-testing-risk-gates.md` 分为五组：
  health/auth、REST、WebDAV、WPS fixture、resource/fault；
  场景 ID 形如 `HTTP-AUTH-001`、`REST-LIST-001`、`DAV-LOCK-002`。

## 运行

```sh
python -m unittest discover -s contract_tests -v
```

## 安全

- 默认全部场景不访问真实 WPS；fixture 全部为本机 fake。
- 不把 Cookie、CSRF、rtk、Basic Auth 密码、签名 URL 或真实 ID 写入
  仓库；`results/` 中只有状态码、计数与占位名。
