# SDK 拆离后业务侧接入 — DEV E2E 测试设计

- 日期：2026-06-18
- 目标：从下游业务方视角验证拆离后的 `tipsy-ab-config-sdk` 在 DEV 环境可被正常 `go get` / `pip install` 并完成主要业务调用路径；输出可重复运行的脚本与简短结果报告。
- 非目标：性能压测、负载稳定性测试、Console/Admin API、token 签发流程的端到端验收（这些由其他流程覆盖）。

## 1. 背景与边界

- 仓库 `Lightspeed-Intelligence/tipsy-ab-config-sdk` 已通过 `proxy.golang.org` 发布 `sdk/go/tipsyabconfig/v0.1.0`（已验证：`@v/v0.1.0.mod` 中 require 全部为 `v0.1.0`，无 `replace` 指令，外部消费者 `go get` 可直接解析）。
- Python SDK 暂未上 PyPI；外部消费者通过 `pip install "tipsy-ab-config @ git+https://github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk.git@python-sdk/v0.3.0#subdirectory=sdk/python"` 安装。
- DEV 端点（来自 `docs/dev-http-token.md`）：
  - HTTP base：`https://dev-ab-config.infra.fantacy.live`
  - gRPC：`dev-ab-config-grpc.infra.fantacy.live:443`（在 SDK 中以 `grpcs://...:443` 形式传入）
  - 临时服务 token：`namespaces=["*"]`，过期 2026-06-21 05:00 CST。
- 待验证的 namespace：`tipsy-chat`（文档默认示例 ns）。

## 2. 总体方案（A：独立消费者目录）

在 SDK 仓之外的独立工作区 `~/tipsy-sdk-e2e/` 建立两个最小消费者工程：

- `go-consumer/`：独立 Go module（`example.com/biz/go-consumer`），`go get` 拉取 `sdk/go/tipsyabconfig@v0.1.0`。
- `py-consumer/`：独立 venv + `pyproject.toml`，`pip install` 拉取 `python-sdk/v0.3.0`。

上层一个 `run-e2e.sh` 编排所有场景，输出 PASS/FAIL 矩阵。结果报告回写到 SDK 仓的 `docs/superpowers/specs/2026-06-18-sdk-dev-e2e-report.md`。

选择该方案的原因：唯一能同时验证 ① 公共 module proxy 真能拉到、② SDK 在业务侧完整运行（缓存 / 订阅 / 中间件 / trace_id）、③ DEV 服务端响应符合预期。仅靠 `curl` / 仅靠仓内 `example/` 都达不到「拆离后能不能用」的核心目的。

## 3. 工作区目录

```
~/tipsy-sdk-e2e/
├── README.md
├── .env.example              # AB_CONFIG_HTTP_BASE / _GRPC_ADDR / _TOKEN
├── run-e2e.sh                # 编排
├── go-consumer/
│   ├── go.mod                # require ...sdk/go/tipsyabconfig v0.1.0
│   └── main.go               # 单文件 CLI
└── py-consumer/
    ├── pyproject.toml        # tipsy-ab-config[http,fastapi] @ git+...@python-sdk/v0.3.0
    └── consumer.py           # 单文件 CLI（asyncio）
```

工作区位于 SDK 仓**外**，以确保 `go get` / `pip install` 路径与真实下游一致；harness 不修改 SDK 仓任何代码，仅在 SDK 仓内提交一份报告。

`run-e2e.sh` 首次运行时自动 bootstrap：

- 若 `go-consumer/go.sum` 不存在则 `cd go-consumer && go mod tidy`。
- 若 `py-consumer/.venv` 不存在则 `python -m venv .venv && .venv/bin/pip install -e py-consumer`。

## 4. 消费者 CLI 契约

两侧公用一套 flag，保证编排脚本对称：

| flag | 取值 | 说明 |
|---|---|---|
| `--mode` | `init` / `static` / `dynamic` / `experiment` / `server` | 场景 |
| `--transport` | `grpc`（默认） / `http` | 写入 `Config.Transport` |
| `--namespace` | 默认 `tipsy-chat` | 目标 namespace |
| `--key` | 默认 `rerank.threshold` | static/dynamic 的 key |
| `--user` | 默认 `e2e-user` | dynamic/experiment 的 uid |
| `--trace` | 默认空 | 显式 trace_id；空则 SDK 自动生成 |
| `--listen` | 默认 `:18080` | 仅 `server` 模式 |

每个非 server 模式跑完后向 stdout 打印**一行 JSON**结果，exit 0 表示成功，非 0 表示失败。`server` 模式监听并在 SIGTERM 上优雅退出。

### 4.1 模式语义

| mode | SDK 调用 | 通过判定 |
|---|---|---|
| `init` | Go: `Init(ctx, cfg)` → `Health()` → `Close()`；Python: `await init(cfg)` → `await aclose()` | Init/aclose 无异常；Go 侧额外断言 `Health().StartupCacheEmpty == false` 或 `LastPullOK == true`（任一为真即视作 PullAll 跑通了一次） |
| `static` | `GetConfigStatic(ns, key, "fallback")` | 无 error，返回值为非空字符串（可以是 fallback） |
| `dynamic` | `NewAbtestContext` → `GetConfig(ctx, abctx, ns, key, "fallback")` | 无 error，返回值为非空字符串 |
| `experiment` | `GetExperimentResult(EXPERIMENT_TYPE_CONFIG_VERSION, RESULT_DISPLAY_TYPE_FLAT_KV)` | 无 error，响应非 nil，`computed_at` 存在 |
| `server` | 安装 `Middleware` / `AbtestMiddleware`，`GET /probe` 返回 `{uid, trace_id, value}` | 外部 curl 携带 `X-Trace-Id: <known>`，响应 `trace_id == <known>` |

**通过/失败标准的设计原则**：只断言 SDK 返回无错误 + 响应 shape 合理；不断言具体业务值。DEV namespace 状态由运维随时调整，断言具体值会让 harness 变脆而无收益。

### 4.2 地址映射

```
transport=grpc → ConfigServiceAddr = AbtestServiceAddr = "grpcs://" + $AB_CONFIG_GRPC_ADDR
transport=http → ConfigServiceAddr = AbtestServiceAddr = $AB_CONFIG_HTTP_BASE
                 PullInterval = 5s（按 §4.1 文档建议）
Token         = $AB_CONFIG_TOKEN
```

## 5. 编排脚本

`run-e2e.sh` Bash 脚本，`set -euo pipefail`：

1. `source .env`（若存在），校验三个必需 env：`AB_CONFIG_HTTP_BASE` / `AB_CONFIG_GRPC_ADDR` / `AB_CONFIG_TOKEN`。缺失则报错退出。
2. 首次运行 bootstrap（§3）。
3. 主循环：

```
for lang in go py; do
  for transport in grpc http; do
    for scenario in init static dynamic experiment middleware; do
      run_step "$lang/$transport/$scenario"
    done
  done
done
```

4. 每个 `run_step` 以 30s 超时执行对应命令，捕获 stdout/stderr 与 exit code，记录 PASS/FAIL。
5. `middleware` 子步骤特别：后台启动 `--mode=server`，等待 `127.0.0.1:18080` 可连（最多 5s），`curl -H 'X-Trace-Id: e2e-XYZ' http://127.0.0.1:18080/probe`，断言响应 JSON `trace_id` 字段等于 `e2e-XYZ`，最后 `kill %1`。
6. 全部跑完后打印矩阵：

```
                  grpc          http
go    init        PASS          PASS
      static      PASS          PASS
      dynamic     PASS          PASS
      experiment  PASS          PASS
      middleware  PASS          PASS
py    init        PASS          PASS
      ...
overall: 20/20 PASS
```

7. 失败的 step 把 stderr 摘要打印在矩阵下方。脚本 exit 0 当且仅当所有 step PASS。

## 6. 结果报告

跑完后写 `docs/superpowers/specs/2026-06-18-sdk-dev-e2e-report.md`，包含：

- 矩阵快照
- 实际消费的版本：`sdk/go/tipsyabconfig/v0.1.0`、`python-sdk/v0.3.0`
- 使用的 DEV 端点 + token 过期时间
- 任何意外发现（如某 transport / scenario 失败）
- 复现命令：`cd ~/tipsy-sdk-e2e && ./run-e2e.sh`

报告随后 commit 到 SDK 仓。

## 7. 风险与对策

| 风险 | 影响 | 对策 |
|---|---|---|
| DEV token 2026-06-21 过期 | harness 此后所有 step 401 | 报告中标注过期时间；后续如需常驻 e2e 需配长期 token 或滚动签发流程 |
| Python SDK 不在 PyPI，install 依赖 GitHub 网络 | 首次 bootstrap 较慢 | 缓存 `.venv`；README 中标注 |
| DEV namespace `tipsy-chat` 配置变动 | static/dynamic 返回值可能变化 | 通过/失败只看 error + shape，不看具体 value |
| Python 3.14 与 SDK classifier 上限 3.13 不一致 | 可能依赖兼容性告警 | bootstrap 输出捕获；若 install 失败则记入报告 |
| HTTP 模式需要 `tipsy-ab-config[http]` extra | 漏装会运行时报错 | pyproject.toml 显式带上 `[http]` |

## 8. 后续

本设计完成实施后：

- 短期：harness 留在 `~/tipsy-sdk-e2e/`，作为开发者本地验证 SDK 拆离接入正确性的工具。
- 中期（如有需求）：迁入 CI（用户已表示当前不需要）。
- 长期：若上 PyPI，更新 `py-consumer/pyproject.toml` 改用纯 `tipsy-ab-config==X.Y.Z`。
