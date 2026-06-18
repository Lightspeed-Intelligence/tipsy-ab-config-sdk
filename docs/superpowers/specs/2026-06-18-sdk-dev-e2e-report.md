# SDK 拆离后业务侧接入 — DEV E2E 测试报告

- 日期：2026-06-18
- 设计：[`2026-06-18-sdk-dev-e2e-design.md`](./2026-06-18-sdk-dev-e2e-design.md)
- 执行者：本地手工触发 `~/tipsy-sdk-e2e/run-e2e.sh`
- 总体结论：**20/20 PASS**。Go + Python 两侧 SDK 在 DEV 环境的 `grpc` 与 `http` 双 transport 下均能正常初始化、读取静态/动态配置、获取实验结果、并通过 middleware 透传 `trace_id`。

## 1. 矩阵

```
              grpc         http
go init       PASS         PASS
go static     PASS         PASS
go dynamic    PASS         PASS
go experiment PASS         PASS
go middleware PASS         PASS
py init       PASS         PASS
py static     PASS         PASS
py dynamic    PASS         PASS
py experiment PASS         PASS
py middleware PASS         PASS
-------------------------------------------------------------------------
overall: 20/20 PASS
```

每个 cell 的含义见设计文档 §4.1。

## 2. 实际消费版本

| 包 | 来源 | 版本 |
|---|---|---|
| `sdk/go/tipsyabconfig` | `proxy.golang.org`（公共 Go module proxy） | `v0.1.0` |
| `sdk/go/tipsyauth`（间接） | 同上 | `v0.1.0` |
| `api/gen/go`（间接） | 同上 | `v0.1.0` |
| `tipsy-ab-config`（Python） | `git+https://github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk.git@python-sdk/v0.3.0#subdirectory=sdk/python` | `0.3.0` |

下游 `go get` / `pip install` 均无需 PAT 或 SSH key，公共仓直拉成功（这是「拆离为公开仓」的核心收益）。

工具版本：

- Go `1.26.2`（模块声明的 `go 1.25.0` 未被顶高）
- Python `3.14.5`，`grpcio 1.81.1`，`fastapi 0.137.2`，`httpx 0.28.1`，`protobuf 6.33.6`
- `tipsy-ab-config` wheel 在 Python 3.14 下安装无警告，运行正常（pyproject 中 classifier 只标到 3.13，但实际兼容更新版本）

## 3. DEV 端点 & 鉴权

- HTTP base：`https://dev-ab-config.infra.fantacy.live`
- gRPC：`dev-ab-config-grpc.infra.fantacy.live:443`（SDK 中以 `grpcs://...:443` 形式传入，方案 Y 地址语法）
- Service token：`namespaces=["*"]`，到期 **2026-06-21 05:00 CST**（`exp=1781989217`）
- 同一 token 共用于 HTTP 与 gRPC

## 4. 复现命令

```bash
cd ~/tipsy-sdk-e2e
cp .env.example .env   # 首次需粘贴当前 DEV token
./run-e2e.sh
```

首次运行自动 bootstrap：

- `go mod tidy` + `go build` 在 `go-consumer/`
- `python -m venv .venv` + `pip install -e .` 在 `py-consumer/`

整套跑完约 60–90 秒（视网络），全部 PASS 时 exit 0。

## 5. 过程中暴露的问题与修复

| 现象 | 根因 | 修复 |
|---|---|---|
| 首轮 `go middleware (grpc/http)` 双双 FAIL，`/probe` 返回 `404 page not found` | 默认监听端口 `:18080` 已被本机另一进程 `tipsy-v2-server` 占用，consumer 启动后 `ListenAndServe` 失败，但 readiness 探针误把 hold 在该端口上的别人响应当成"已就绪" | 把 Go consumer server 模式默认端口改成 `:18180`，Python 改成 `18181`，避开 18080/18081 这种公共试验端口 |
| Python middleware step 屏幕上喷 6 行 `curl: (7) connection refused` | uvicorn 启动包含 SDK PullAll 圆 trip，前 200ms × 几次 readiness 探针打到还没起来的端口 | 在 readiness 探针的 `curl` 上加 `2>/dev/null`，不影响实际探活逻辑，只是去掉噪声 |

修复后再跑一次即 20/20 PASS。

## 6. 验证了什么 / 没验证什么

✅ 已验证

- 公共 Go module proxy 解析 `sdk/go/tipsyabconfig@v0.1.0`（含间接依赖 `api/gen/go@v0.1.0` 与 `sdk/go/tipsyauth@v0.1.0`），无 `replace` 残留
- 公共 GitHub 仓 `pip install ... @ git+...#subdirectory=sdk/python` 在无凭据条件下成功
- gRPC 模式：`grpcs://dev-ab-config-grpc.infra.fantacy.live:443` 通过 Cloudflare 代理直达 origin，Subscribe 流稳定建立
- HTTP 模式：`PullInterval=5s` 轮询，`PullAll` / `GetExperimentResult` 走 `/api/v1/config/pull_all` 与 `/api/v1/abtest/experiment_result`，鉴权正常
- `GetConfigStatic` / `GetConfig`（动态）/ `GetExperimentResult`（config_version + flat_kv）全部 RPC 无 error
- Go `Client.Middleware` + Python `AbtestMiddleware` 在请求入口正确创建 `AbtestContext`、解析 `X-Trace-Id`、并把 trace_id 串到 `GetConfig` 调用链上
- Go SDK 端 `Health()` 报告 `SubscribeConnected=true`，`StartupCacheEmpty=false`

❌ 未验证（设计中即声明为非目标）

- 性能 / 压测
- Token 过期 / TokenProvider 切换路径
- 异常路径（namespace 未授权、token 失效、网络中断）
- Custom params 实验类型（DEV 当前未配置）
- 实验命中后的实际 group / version 结果（DEV namespace 状态不稳定，断言 shape 即可）

## 7. 后续

- DEV token 2026-06-21 过期，届时复跑前需要刷新 `.env` 中的 `AB_CONFIG_TOKEN`。
- 若 Python SDK 后续上 PyPI，可把 `py-consumer/pyproject.toml` 改为 `tipsy-ab-config[http,fastapi]==X.Y.Z` 纯版本声明，移除 git URL 依赖。
- 当前 harness 留在本机 `~/tipsy-sdk-e2e/`；按用户当前要求不入 CI。
