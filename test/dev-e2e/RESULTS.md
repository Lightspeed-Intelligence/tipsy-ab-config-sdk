# DEV 环境 e2e 测试结果报告

## 2026-06-18 复测：迁入公共 SDK 仓后首跑

- 测试日期：2026-06-18
- 上下文：套件从主仓 `tipsy-ab-config/test/dev-e2e/` 迁入到公共 SDK 仓
  `tipsy-ab-config-sdk/test/dev-e2e/`。迁入修复点：
  - 4 个 Go 驱动（`platform/`、`clients/go/`、`load/`、`tools/bucketfind/`）拆为各自独立 module，加入根 `go.work`；不再依赖原主仓父 module。
  - Go 模块 path 由 `Lightspeed-Intelligence/tipsy-ab-config/...` 改为 `Lightspeed-Intelligence/tipsy-ab-config-sdk/...`。
  - `resolveFixturesPath` 由「向上找 go.mod」改为「向上找 `test/dev-e2e/fixtures/expectations.json`」，兼容「在驱动模块目录内 `go run .`」的运行姿势。
  - `setup_venv.sh` editable 模式由「向上找 go.mod」改为「向上找 `sdk/python/pyproject.toml`」（公共 SDK 仓无顶层 go.mod）。
  - `setup_venv.sh` backend 模式 git URL 由旧 `tipsy-ab-config.git@python-sdk/v0.2.0` 升级到 `tipsy-ab-config-sdk.git@python-sdk/v0.3.0`（公共仓 + 最新 tag）。
- 目标环境（共享 DEV）：
  - HTTP：`https://dev-ab-config.infra.fantacy.live`
  - gRPC：`dev-ab-config-grpc.infra.fantacy.live:443`（Cloudflare 代理 + 标准 TLS）
  - 鉴权：HS256 service token（`namespaces=["*"]`，2026-06-21 过期）
- 数据：dev 数据库内 demo-test / for_dev_agent_test 两个 namespace 的种子数据继承自历史轮次，本轮未重新 seed。
- 重要约束：本轮**仅做迁入适配**，未改任何 SDK 工程源码、未改工程语言版本。

### 总览

| 测试类别 | 客户端/路径 | 结果 |
|---|---|---|
| 平台正确性 | 裸 HTTP（config/static、config/dynamic、abtest/experiment_result） | **75/75 PASS** |
| 平台正确性 | 裸 gRPC（grpcurl，反射 + 4 RPC） | **5/5 PASS** |
| 客户端正确性 | Go SDK — workspace 模式（gRPC + HTTP transport） | **76/76 PASS** |
| 客户端正确性 | Go SDK — backend 模式（公共 proxy + `sdk/go/tipsyabconfig@v0.1.0`，`GOWORK=off`） | **76/76 PASS** |
| 客户端正确性 | Python SDK — editable 模式（gRPC + HTTP） | **76/76 PASS** |
| 客户端正确性 | Python SDK — backend 模式（`git+ssh://...@python-sdk/v0.3.0`） | **76/76 PASS** |
| 中等负载 | HTTP `abtest/experiment_result`（for_dev_agent_test） | **PASS（56354 req, 0 错误）** |

### 关键路径证据

- **Go backend 模式不再需要 GOPRIVATE / SSH**：公共仓 + 公共 module proxy + `v0.1.0` 三件套已 work，`GOPROXY=https://proxy.golang.org,direct GOWORK=off go run .` 直接成功。这是「SDK 拆离为公开仓」最值钱的下游证据。
- **Python backend 模式使用 git+ssh + `python-sdk/v0.3.0`**：tag 命名规则匹配 monorepo multi-module 约定，安装时 SDK 自报版本 `0.3.0`，与 wheel 一致。
- **平台数据 7 类能力全覆盖**：分桶分流 / config_version / custom_params / layer-gap / experiment-gap / gray release / admission / group-whitelist / sticky / static-vs-dynamic 全部命中黄金期望。

### 压测细节（2026-06-18，90s，target QPS 2000）

| 指标 | 值 |
|---|---|
| 端点 | `abtest/experiment_result` |
| namespace | for_dev_agent_test |
| 并发 | 150 |
| 时长 | 90s |
| 总请求 | 56354 |
| 达成 QPS | 626.2 |
| 错误率 | 0.00%（0 错误） |
| 状态码 | 200 × 56354 |
| p50 延迟 | 229.1 ms |
| p95 延迟 | 304.7 ms |
| p99 延迟 | 443.1 ms |
| max 延迟 | 1057.7 ms |

延迟分布与 06-16 复测基本一致；本轮总请求数较少是因为单次跑时长缩短（90s 而非 150s），不是性能回退。明细 JSON 在 `test/dev-e2e/load/last-run.json`。

### 2026-06-18 追加：RTS 时延分解（httptrace 客户端分段）

给 load 驱动加了 `net/http/httptrace.ClientTrace`，把每个请求按 DNS / TCP 握手 / TLS 握手 / wait（请求写完→响应首字节）/ read body 五段单独打点。60s × 150 并发 × 1500 QPS 限速跑：

| phase | p50 | p95 | p99 |
|---|---:|---:|---:|
| dns | 0.0 ms | 0.0 ms | 0.0 ms |
| tcp | 0.0 ms | 0.0 ms | 0.0 ms |
| tls | 0.0 ms | 0.0 ms | 0.0 ms |
| **wait (server)** | **232.1 ms** | **307.1 ms** | **475.2 ms** |
| read body | 0.1 ms | 0.2 ms | 0.3 ms |
| **total** | **232.2 ms** | **307.4 ms** | **475.3 ms** |

样本量 36617 请求，其中 36612 走 keep-alive 复用连接（**100.0% reused**），仅 5 个首次建连。

**结论**：客户端这一侧几乎零成本——握手仅由首次建连那一发付清，后续全部走 keep-alive；JSON 响应体读取在 0.1–0.3 ms 量级，可忽略。**几乎 100% 的 wall-clock 都在 wait（"请求最后字节写完 → 收到响应第一个字节"）**。这一段不可再用 httptrace 细分；它包含且只包含：

1. 客户端 → Cloudflare 边缘的网络飞行
2. Cloudflare → 源站的回源飞行
3. Traefik 反代到 app
4. **app 内部处理**（鉴权、abtest 计算、DB / cache 读、protojson 编码）
5. 响应路径反向飞回客户端

**辅证**：06-16 把并发从 150 提到 450 时 p50/p95/p99 几乎不动（225/255/390ms），意味着 (3)+(4) 在当前负载下**不是排队瓶颈**——服务端处理是流水线常数，不是队头堵塞。这反向支持「绝大多数 232ms 是网络飞行 + Cloudflare 处理，而不是 app 计算」，但**仅靠客户端数据无法证明，也无法量化各段比例**。

**还需要服务端侧的信号才能继续拆分**。两条路任选其一即可（不属于本仓改动，记在下一步建议）：

- 让 app handler 入口/出口加日志或 `Server-Timing` header（`abtest_calc;dur=...`、`db_read;dur=...`、`json_encode;dur=...`），客户端读 header 即可得到 (4) 的拆分。
- 在同一根 token、同一个 body 下，从「业务内网（同地域低延迟链路）」直接命中源站对照本机经 CF 的 RTT，差值即 (1)+(2)+(5) 总和；剩余即 (3)+(4)。

### 结论

迁入后**所有历史用例（252+ 项）全部通过**，且公开仓 `go get` / `pip install` 全链路替代了原私仓 GOPRIVATE 路径——这正是 SDK 拆离的核心目的。

---

## 历史轮次

- 测试日期：2026-06-15 首轮 / 2026-06-16 复测（SDK module 化 + v0.1.0 发布后）
- 目标环境（共享 DEV）：
  - HTTP：`https://dev-ab-config.infra.fantacy.live`（CloudFlare）
  - gRPC：源站直连 `47.253.175.59:443`，`-authority dev-ab-config-grpc.infra.fantacy.live`，跳过 Origin Cert 校验
  - 鉴权：HS256 service token（`namespaces=["*"]`，2026-06-21 过期）
- 数据：用户在 dev 执行 `test/dev-e2e/sql/seed.sql`（demo-test、for_dev_agent_test 两个 namespace，完整场景）
- 重要约束：本轮**仅修改测试代码**，未改任何工程源码、未改工程语言版本；平台行为均按现状验证。
- 接入方式（复测版）：Go 客户端用 **module-化 SDK** 的发布版 `sdk/go/tipsyabconfig@v0.1.0` 直接 require（与 `tipsy-backend` 完全一致的接入路径），不依赖父仓 `go.work`、不使用 `replace`，能 `GOWORK=off` 独立构建。SDK 默认超时 `AbtestTimeout=1.5s`/`PullTimeout=5s`（工程版本已上调），测试客户端不再覆盖。

---

## 一、总览

| 测试类别 | 客户端/路径 | 结果 |
|---|---|---|
| 平台正确性 | 裸 HTTP（config/static、config/dynamic、abtest/experiment_result） | **75/75 PASS** |
| 平台正确性 | 裸 gRPC（grpcurl，反射 + 4 RPC） | **5/5 PASS** |
| 客户端正确性 | Go SDK — HTTP transport | **PASS（76/76，含 custom）** |
| 客户端正确性 | Go SDK — gRPC transport（源站直连） | **PASS（76/76，3 次稳定）** |
| 客户端正确性 | Python SDK — HTTP transport | **PASS（38/38，2 次稳定）** |
| 客户端正确性 | Python SDK — gRPC transport（源站直连 + Origin CA） | **PASS（38/38）** |
| 压力测试 | 中等负载（HTTP，experiment_result + dynamic） | **PASS（~22.2万请求，0 错误）** |

五条客户端路径（裸 HTTP / 裸 gRPC / Go SDK / Python SDK ×2 transport）对同一 (namespace, user, key) 返回一致且等于黄金期望（`test/dev-e2e/fixtures/expectations.json`，由 `bucketfind` 按平台同款 xxhash 反查生成）。

---

## 二、平台正确性（覆盖的能力点）

全部基于确定性分桶（`bucket = xxhash64(uid+"-"+salt) % traffic_total`）反查出的具体 UID 精确断言：

1. **分流计算**：
   - config_version 实验按绝对 `[lo,hi]` 闭区间命中对应组（demo E_cfg A/B；fda E_cfg2 G1/G2/G3）。
   - 同一 UID 在 layer 与 experiment 两级独立 salt 下分别分桶。
2. **gap（无覆盖→不产出→回退全量）**：
   - **layer 级 gap**：demo `layer:gap` 的 slot 仅覆盖 `[0,4999]`，落 `[5000,9999]` 的 UID 不命中 → `gap_key` 回退全量 `gap-FULL`。
   - **experiment 级 gap**：fda E_cfg2 三组覆盖 `[0,9998]`，桶 9999 无组覆盖 → `color` 回退全量 `c-FULL`（用反查到的确定性桶-9999 UID `u15903`/`u53053` 断言）。
3. **两种实验类型**：
   - `config_version`：落 `config_flat_kv`（key→version_id）与 dynamic 解析值。
   - `custom_params`：落 `custom_flat_kv`（任意 KV，类型/值正确，含 bool、int、float）。
4. **灰度（gray release，UID 白名单）**：
   - `gray-user-1/2` 的 `welcome_text` = `welcome-GRAY`（灰度值），且**灰度对同一 key 优先于实验**；同一用户 `banner_color` 仍随实验组（red，组 A）。
   - fda `gray-fda-1` 的 `greeting` = `hi-GRAY`。
5. **admission 定向**：demo E_admit 实验级 admission `country==US` —— `country=US` 得 `admit-EXP`，否则回退 `admit-FULL`。
6. **组白名单覆盖分桶**：`wl-force-B` 即使分桶本应落 A，因组白名单强制落组 B（`welcome-B`/`blue`）。
7. **sticky 粘性**：demo E_cfg `sticky_enabled=true`，同一新 UID 连续 4 次 `experiment_result` 返回稳定组；本地已验证 `experiment_sticky_assignment` 实际落行（dev 上以稳定性断言覆盖）。
8. **static vs dynamic**：`config/static` 恒返回全量发布值（`welcome-FULL`/`green`/`gap-FULL`/`admit-FULL`/`c-FULL`/`hi-FULL`），与实验/灰度叠加无关；`config/dynamic` 叠加实验/灰度。

---

## 三、客户端正确性

- **裸 HTTP**：平台驱动 `test/dev-e2e/platform`，75/75 PASS。
- **裸 gRPC（grpcurl）**：`grpc_smoke.sh`，5/5 PASS —— 反射列出 ConfigService/AbtestService、ListNamespacesByKind、GetStaticConfig、GetExperimentResult（config_flat_kv 版本正确）、GetDynamicConfig（解析值正确）。
- **Go SDK**：`test/dev-e2e/clients/go`，HTTP 与 gRPC（`grpcs://...?authority=...&insecure=true`）两种 transport 各 76 项全 PASS，连跑 3 次稳定。
- **Python SDK**：`test/dev-e2e/clients/py`（python3.12 venv + `[http]` extra），HTTP 38/38；gRPC 用 CloudFlare Origin CA PEM（`AB_CONFIG_GRPC_CA_PEM`）38/38。

### 测试代码调整（不涉及工程源码）

跑 dev 时发现 SDK 两种语言均偶发个别用例回退到全量值（结果在多次运行间漂移）。定位为**跨公网延迟导致 SDK 的 `AbtestTimeout` 超时**（SDK 默认 0.5s 为同机房调优值；dev 经 CloudFlare/源站直连单次 RTT 约 230ms，首个连接握手更慢）。超时后 `GetConfig` 正确回退全量发布值——**这是 SDK 的设计行为，平台数据/路由均正确**（同用例经裸 HTTP、裸 gRPC、直接 grpcurl 均返回正确实验值）。

修复仅改测试驱动：
- 将 SDK 客户端的 `AbtestTimeout`/`abtest_timeout` 提到 5s、`PullTimeout`/`pull_timeout` 提到 15s（仅测试客户端，生产保持紧默认值）。
- 在断言循环前加一次 warmup 调用预热连接。

修复后 Go ×3、Python ×2 连续运行 0 失败。

---

## 四、压力测试（中等负载）

- 工具：`test/dev-e2e/load`（Go，worker pool + 令牌桶限速）。
- 路径：HTTP `abtest/experiment_result`（主）+ `config/dynamic`，namespace `for_dev_agent_test`。
- 关键观察：dev 经公网访问，单请求 p50 ≈ 230ms，QPS 受并发数约束（QPS ≈ 并发 / 延迟）。

<!-- LOAD_RESULTS_PLACEHOLDER -->

两轮中等负载（限速 2000 QPS）：

| 指标 | 运行 A | 运行 B |
|---|---|---|
| 端点 | `abtest/experiment_result` | `config/dynamic` |
| namespace | for_dev_agent_test | for_dev_agent_test |
| 并发 | 150 | 450 |
| 时长 | 150s | 90s |
| 总请求 | 97,308 | 124,742 |
| 达成 QPS | 648.7 | 1,386.0 |
| **错误率** | **0.00%（0 错误）** | **0.00%（0 错误）** |
| 状态码 | 全部 200 | 全部 200 |
| p50 延迟 | 223.7 ms | 227.7 ms |
| p95 延迟 | 254.1 ms | 260.2 ms |
| p99 延迟 | 392.2 ms | 389.2 ms |
| max 延迟 | 1533.5 ms | 1389.6 ms |

合计约 22.2 万请求，**0 错误**。结果 JSON：`test/dev-e2e/load/run-experiment_result.json`、`run-dynamic.json`。

**关键解读**：
- 达成 QPS 受**公网往返延迟**约束，而非 dev 服务端容量。dev 经 CloudFlare/源站访问，单请求 p50 ≈ 225ms，故 `QPS ≈ 并发 / 延迟`（150 并发 → ~650 QPS；450 并发 → ~1.4k QPS）。2000 QPS 限速未触发。
- 把并发从 150 提到 450（QPS 翻倍），**p50/p95/p99 延迟基本持平**（225/255/390ms 量级）→ dev 在该负载下**未饱和**，瓶颈在网络 RTT 不在服务端。
- 全程 **0 错误、全 200**，无超时/限流/5xx。中等负载下 dev 稳定。
- 注：max 延迟有个别 ~1.4–1.5s 长尾（公网抖动/CloudFlare），占比极小，不影响错误率。

---

## 五、结论与风险

- 平台正确性、客户端正确性（两语言 × 两 transport + 裸 HTTP + 裸 gRPC）全部通过，五路结果一致。
- 压测：两轮中等负载共约 22.2 万请求、**0 错误**、p50≈225ms / p99≈390ms；QPS 受公网 RTT 约束而非服务端容量，dev 在该负载下未饱和、稳定。
- **已知风险/说明**：
  - dev gRPC 直连需 `:authority` + Origin Cert 处理：Go SDK 用 `insecure=true` 跳过校验；Python SDK（grpcio 无 skip-verify）需 Origin CA PEM。两者均已跑通。
  - 临时 token 2026-06-21 过期。
  - 测试代码位于 `test/dev-e2e/`，默认不进 `make test`（需联网打 dev）。

---

## 六、复测（2026-06-16）：按 backend 接入方式验证 SDK module 化 + v0.1.0 发布

### 上游变更
- SDK 拆为独立 Go module、统一 `go 1.25.0`；module 路径前缀改为 `Lightspeed-Intelligence/...`；首版 `sdk/go/tipsyabconfig/v0.1.0` 已发布。
- Go SDK `AbtestTimeout` 默认 500ms → **1.5s**；Python SDK `abtest_timeout` 默认 0.5s → **1.5s**。

### 测试代码同步（按 backend 方式接入，不改工程源码 / 不改工程 go 版本）
- `test/dev-e2e/clients/go/go.mod`：保留 `go 1.25.0`；将 `require .../tipsyabconfig` 从 `v0.0.0 + 本地 replace` 改为**直接 require `v0.1.0`、移除 replace**。`go.sum` 全部来自 GOPRIVATE+SSH 拉取的发布版（不依赖父仓 `go.work`）。
- 客户端运行用 `GOPRIVATE='github.com/Lightspeed-Intelligence/*'` + `git config url."ssh://git@github.com/".insteadOf "https://github.com/"` —— 与 `docs/usage-and-integration.md §4.1.0` 描述的 backend 接入清单完全一致。
- 移除测试客户端对 `AbtestTimeout`/`abtest_timeout` 的 5s 手工覆盖，使用 SDK 工程默认值 1.5s（首轮覆盖在 SDK 默认还是 500ms 时是必需的，现在已不再需要）。
- 验证：`GOWORK=off go list -m .../tipsyabconfig` → `v0.1.0`，`GOWORK=off go build .` 通过 —— 证明确实走的是发布版而非本地 sibling。

### 复测结果（针对 dev 重跑，2026-06-16）

| 测试类别 | 客户端/路径 | 结果 |
|---|---|---|
| 平台正确性 | 裸 HTTP | **75/75 PASS** |
| 平台正确性 | 裸 gRPC（grpcurl） | **5/5 PASS** |
| 客户端正确性 | Go SDK（v0.1.0 + 默认 1.5s，HTTP + gRPC，×3 稳定） | **76/76 × 3 PASS** |
| 客户端正确性 | Python SDK HTTP（默认 1.5s） | **38/38 PASS** |
| 客户端正确性 | Python SDK gRPC（默认 1.5s + Origin CA） | **38/38 PASS** |
| 压测 | 中等负载（HTTP experiment_result，150 并发 / 90s / 限速 2000 QPS） | **60,156 请求 / 668 QPS / 0 错误 / p50 220ms / p95 236ms / p99 336ms / max 757ms** |

- 与首轮相比：默认 1.5s 超时对当前 dev 跨公网 RTT 已足够（p99 ≈ 336ms），不再需要测试客户端额外加宽超时。
- 五路客户端结果仍然完全一致（同一固件、同一断言集）。
- 复测 JSON：`test/dev-e2e/load/run-experiment_result-v2.json`。
- 复测的 Go 驱动用 `GOWORK=off` 直接构建 `test/dev-e2e/clients/go`，确认实际拉取的是发布的 `tipsyabconfig v0.1.0` 而非工作区内的本地 sibling，这是和 backend 一模一样的接入路径。

## 六、Python SDK 也按 backend 方式接入（2026-06-16 补测，sdk-python v0.1.0 发布后）

上游 `release(sdk-python): prepare v0.1.0 for GitHub Releases distribution` 发布后，Python SDK 也按 backend 真实接入方式重新跑了一遍。

### 接入方式
- 不再用 `pip install -e sdk/python[http]`（in-repo editable），改用 **git+ssh 装发布版**，与 `sdk/python/README.md §Consumer onboarding` 对外接入指引一致：
  ```bash
  pip install 'tipsy-ab-config[http] @ git+ssh://git@github.com/Lightspeed-Intelligence/tipsy-ab-config.git@python-sdk/v0.1.0#subdirectory=sdk/python'
  ```
- 业务工程实际接入会用 `git+https://${GH_PAT}@...`（PAT 走 CI secrets）；本地我们用同名仓的 SSH key，等价路径。
- 装出来 `tipsy_ab_config 0.1.0`（而非本地 editable 的 0.8.0 dev），独立 venv `.venv-backend/`。
- 移除测试驱动里 `abtest_timeout=5.0`/`pull_timeout=15.0` 手工覆盖，使用 SDK 工程默认值（1.5s/5s）。
- `setup_venv.sh` 支持 `SDK_MODE=editable|backend` 两种模式（默认 `editable`）。

### 复测结果（首轮，IP-direct + Origin CA；后改为公网域名见 §七）
| 测试 | 结果 |
|---|---|
| Python SDK HTTP（v0.1.0 git+ssh + 默认 1.5s） | **38/38 PASS** |
| Python SDK gRPC（v0.1.0 git+ssh + 默认 1.5s + Origin CA） | **38/38 PASS** |

总计 **76/76**，与 backend pattern 的 Go SDK 完全对等。

## 七、改用 dev 公网 gRPC 域名（2026-06-16 第二轮复测）

发现首轮和此前的复测**还在用早期临时方案**——直连源站 IP `47.253.175.59:443` + `:authority` 覆盖 + 跳过证书校验（Go 用 `insecure=true`、Python 还塞了 Origin CA PEM）。`docs/dev-http-token.md` 已更新：dev 现有独立 CloudFlare 橙云 DNS（`dev-ab-config-grpc.infra.fantacy.live:443`），可走标准 TLS，明确写了**"不要再使用早期临时方案"**。

### 改动（仅测试代码）
- 三处驱动的 gRPC 默认地址改为公网域名：
  - `AB_CONFIG_GRPC_ADDR` 默认 `dev-ab-config-grpc.infra.fantacy.live:443`（原 `47.253.175.59:443`）
  - 不再传 `:authority` 覆盖、不再 `insecure=true`、Python 不再需要 Origin CA PEM
  - SDK addr 形如 `grpcs://dev-ab-config-grpc.infra.fantacy.live:443`（标准 TLS）
- `AB_CONFIG_GRPC_AUTHORITY` 保留为**遗留兼容开关**：当显式设置时回退到 IP 直连形态（运维排查用），默认空 → 走域名。
- 文档化的 IP 直连方案保留代码兜底，但**默认不走**。

### 复测结果（新域名 + 标准 TLS，2026-06-16）
| 测试 | 配置 | 结果 |
|---|---|---|
| 裸 gRPC（grpcurl） | 域名 + 标准 TLS，无 `-insecure`/`-authority` | **5/5 PASS** |
| Go SDK gRPC（v0.1.0 backend 模式） | `grpcs://...grpc.infra.fantacy.live:443` | **76/76 PASS**（含 HTTP）|
| Python SDK gRPC（v0.1.0 backend venv） | 同上，且**完全不需要 Origin CA PEM** | **76/76 PASS**（含 HTTP）|
| 平台裸 HTTP | 不变 | 75/75 PASS |

### Python SDK 实际版本核验（再次确认是发布版而非本地 editable）
```
$ test/dev-e2e/clients/py/.venv-backend/bin/pip show tipsy-ab-config
Name: tipsy-ab-config
Version: 0.1.0
Location: .../py/.venv-backend/lib/python3.12/site-packages
```
不是 `/home/.../sdk/python/...` 的 editable 安装，是 git+ssh 从 tag `python-sdk/v0.1.0` 装出来的发布版。

### 结论
经过此次清理：
- **Go SDK** ⇒ go get `sdk/go/tipsyabconfig@v0.1.0`（GOPRIVATE + SSH）
- **Python SDK** ⇒ pip install git+ssh `@python-sdk/v0.1.0#subdirectory=sdk/python`
- **gRPC 链路** ⇒ 标准公网域名 + 标准 TLS（无 hack）
- **超时** ⇒ SDK 工程默认值（1.5s）

四个客户端路径（Go HTTP/gRPC、Python HTTP/gRPC）+ 裸 HTTP + 裸 gRPC，**五路全绿、零 workaround**，与 `tipsy-backend` 真实接入路径完全一致。

## 八、关于 gRPC 反射

上游 `feat(server): gate gRPC reflection behind GRPC_REFLECTION env, default off` 把反射改成 env 控制、生产默认关闭。dev 环境已显式开启（`docs/dev-http-token.md` 仍记录"反射已开启"，本地 `grpcurl list` 实测可用），所以 `grpc_smoke.sh` 在 dev 上仍是 5/5 PASS。生产环境下 grpcurl 反射会被拒，是平台主动加固，不算回归。

## 九、复现实方式

```bash
# 1. 数据（用户在 dev 执行）
psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f test/dev-e2e/sql/seed.sql   # 等 ≥5s

# 2. 环境变量（gRPC 默认走公网域名 + 标准 TLS，无需 IP/authority/CA PEM）
export AB_CONFIG_HTTP_BASE='https://dev-ab-config.infra.fantacy.live'
export AB_CONFIG_GRPC_ADDR='dev-ab-config-grpc.infra.fantacy.live:443'  # 默认值，可不显式 export
export AB_CONFIG_TOKEN='<dev service token>'
# 仅 IP 直连排查时才设置以下两个（默认不要设）：
# export AB_CONFIG_GRPC_AUTHORITY='dev-ab-config-grpc.infra.fantacy.live'
# export AB_CONFIG_GRPC_CA_PEM='/path/to/origin.pem'

# 3. 平台正确性
go run ./test/dev-e2e/platform
PATH="$HOME/go/bin:$PATH" bash test/dev-e2e/platform/grpc_smoke.sh

# 4. 客户端正确性
# Go SDK：可用 workspace 模式（默认）或 backend 标准模式（GOWORK=off + 已发布的 v0.1.0）。
go run ./test/dev-e2e/clients/go                                  # workspace 模式
( cd test/dev-e2e/clients/go && \
  GOPRIVATE='github.com/Lightspeed-Intelligence/*' \
  GOWORK=off go run . -fixtures ../../fixtures/expectations.json ) # backend 模式（v0.1.0）

bash test/dev-e2e/clients/py/setup_venv.sh                    # 一次性：editable 模式（.venv/）
SDK_MODE=backend bash test/dev-e2e/clients/py/setup_venv.sh   # 或：backend 模式（.venv-backend/，git+ssh 装 v0.1.0）
# 走公网域名 + 标准 TLS，无需 Origin CA PEM
test/dev-e2e/clients/py/.venv/bin/python test/dev-e2e/clients/py/run.py --transport both             # editable
test/dev-e2e/clients/py/.venv-backend/bin/python test/dev-e2e/clients/py/run.py --transport both     # backend

# 5. 压测
go run ./test/dev-e2e/load -concurrency 150 -duration 150s -target-qps 2000

# 6. 清理（用户在 dev 执行）
psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f test/dev-e2e/sql/teardown.sql
```
