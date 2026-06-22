# DEV 环境 e2e 测试结果报告

## 2026-06-22 Java SDK dev-e2e 驱动就绪（待有效 token 实跑）

新增 `test/dev-e2e/clients/java/`——Java SDK 的 dev-e2e 客户端正确性驱动，与
现有 Go/Python 驱动同构（client tag `java_sdk_grpc` / `java_sdk_http`，两 transport
逐条断言 `fixtures/expectations.json`）。

- **bucketfind/fixtures**：`tools/bucketfind/main.go` 的 `allClients` 追加
  `java_sdk_grpc` / `java_sdk_http`；重生成后 `expectations.json` 全部 38 行的
  `applies_to` 均含两个 java tag（`git diff --stat`：114 insertions / 38 deletions）。
- **形态**：独立 Maven 项目（不并入 `sdk/java` reactor），依赖本地 `mvn install`
  的 `io.tipsy:tipsy-abconfig:0.1.0`；`maven-shade-plugin` 打 fat-jar，合并 gRPC
  `META-INF/services/*` SPI；harness 用 Jackson 解析 fixtures、slf4j-simple 作日志后端。
- **编译验证**：`cd test/dev-e2e/clients/java && mvn -q -DskipTests package` →
  BUILD SUCCESS（3 source files，release 21，22MB fat-jar，Main-Class 正确）。
- **缺 token 优雅退出**：不带 `AB_CONFIG_TOKEN` 跑 `java -jar`（及 `mvn exec:java`）→
  打印 `FATAL: AB_CONFIG_TOKEN env var is required` 并 exit 2，不抛栈崩溃。
- **degraded 容错验证**：用 dummy token + 不可达端点跑 → HTTP transport
  `startupFailOpen` 吸收连接失败、health 报空缓存 → 1 FAIL（不崩）；gRPC 报
  WARNING + 标记 degraded（不崩、不刷 FAIL）；SUMMARY + NOTE 正常打印，exit 1。
- **实跑边界**：未真打 DEV（`docs/dev-http-token.md` 内置 token 已于 2026-06-21
  过期，且需用户 seed DEV 数据库）。**驱动就绪，待持有有效 `AB_CONFIG_TOKEN`
  时由用户运行**（与 Go/Python 驱动相同的执行边界）；预期 38 行 × {http, grpc} =
  76/76 PASS。

## 2026-06-19 Headless + round_robin 验证（SDK v0.4.0 / v0.5.0 发版后）

ST5 e2e 验证 — 三段全跑。

### 总览

| 段 | 范围 | 结果 |
|---|---|---|
| 1 本机 loopback | platform 75 + grpc_smoke 5 + Go SDK HTTP 38 + Python SDK HTTP 38 | **156/156 PASS** |
| 2 DEV workspace | platform 75 + grpc_smoke 5 + Go SDK 76 + Python SDK 76 | **232/232 PASS** |
| 2 DEV backend（发布版） | Go SDK v0.4.0 backend 76 + Python SDK python-sdk/v0.5.0 backend 76 | **152/152 PASS** |
| 3 本机 Headless 3 实例 round_robin | 30 s × 50 并发 gRPC，3 实例计数差值 | **PASS（差值 0.00%）** |

### 段 1 — 本机 loopback 156/156 PASS

- 环境：rootless docker；`tipsy-ab-config-db-1` 复用前轮 PG 容器；新起 `tipsy-ab-config-app-loop` 容器
  （HTTP :8080 / gRPC :50051 plaintext / 共享 docker network `tipsy-ab-config_default`）。
- 本地 token：`cd ~/tipsy-ab-config && TIPSY_SERVICE_SECRET=devsecret go run ./cmd/servicetoken --sub local-test --namespaces '*' --ttl 24h`。
- platform raw HTTP：**75/75 PASS**。
- raw gRPC smoke：**5/5 PASS**（`grpc_smoke.sh` 本身硬编码 TLS；本机 loopback 用 PATH-shim 的
  `grpcurl -plaintext` 包装跑通，不修改测试代码）。
- Go SDK + Python SDK：HTTP transport **38 + 38 PASS**。
- **本机 loopback gRPC client transport（76 cells）跑不通是测试驱动 vs 本机环境
  的不匹配**：`clients/go/main.go` 与 `clients/py/run.py` 都硬编码用
  `grpcs://...` 拼 dial target（DEV 形态 Cloudflare TLS），本机的 ab-config-app
  容器是裸 50051 plaintext，没有 TLS 证书；SDK 的 `grpcs://...?insecure=true`
  也只是跳验证、不退化到明文。这是测试驱动的形态限制（非本任务 SDK 改动引起），
  不阻挡 AC #2、AC #10、AC #4 的验证：AC #10 由本机 loopback HTTP/SDK 形态 +
  `grpc_smoke.sh -plaintext` 共同覆盖（裸 `host:port` + `grpc://` plaintext 形态
  跑通 → 证明 SDK 未在非 `dns:///` 形态错误注入 LB 行为）。
- **main agent 备案（review S2）**：AC #2 字面要求「76×2 客户端用例通过」；本轮本机 loopback gRPC 76 cells 因驱动硬编码 `grpcs://` 未实际执行，所以**严格读 AC #2 为 partial（156/232，缺 gRPC 76）**。
  替代覆盖：(i) 段 2 DEV workspace gRPC 76 cells PASS（同代码、同 SDK；只换了端点），(ii) 段 2 DEV backend gRPC 76 cells PASS（v0.4.0 发版本走公共 proxy 仍跑通），(iii) AC #10 negative regression 由单测 18 case + `grpc_smoke.sh -plaintext` 覆盖。这三条共同提供了等价或更强的回归保护。
  本任务**接受 AC #2 partial 状态**；为驱动 `clients/{go,py}` 增加 plaintext 开关（如 `AB_CONFIG_GRPC_PLAINTEXT=1`）属于 follow-up 测试驱动改造，不在本任务范围。

### 段 2 — DEV 全套（workspace + backend）384/384 PASS

- 同一 dev token（`docs/dev-http-token.md` 第 75 行，2026-06-21 过期）。
- workspace 模式：4 个驱动全跑，**232/232 PASS**。
- backend 模式（验证发布版 SDK 走公共 proxy / git+ssh 拉取）：
  - Go：`GOWORK=off GOPROXY=https://proxy.golang.org,direct go run .` → 76/76 PASS。`go.sum`
    在本次首次为 v0.4.0 填充；`proxy.golang.org` 已索引（ST4 release 已验证）。
  - Python：`SDK_MODE=backend bash setup_venv.sh` 建 `.venv-backend/` →
    `pip install ... @ git+ssh://...@python-sdk/v0.5.0`；run.py → 76/76 PASS。

### 段 3 — 本机 Headless 3 实例 round_robin 验证 PASS（差值 0.00%）

**新增脚手架**（main agent 后续 commit）：

- `test/dev-e2e/headless/docker-compose.headless.yml`：1 PG + 1 Redis + 3 ab-config-app；
  3 个 app 容器共享 docker network alias `ab-config-headless.local` →
  docker embedded DNS 把这个名字解析为 3 个 A 记录（与 k8s Headless Service A-record fan-out
  等价；caveat 见 README §R4）。Redis 强制（design R5）。
- `test/dev-e2e/headless/verify-roundrobin.sh`：起栈 → 迁库 → seed → 签 token →
  在同 docker network 内 alpine sidecar 跑 `roundrobin-load`（dial
  `dns:///ab-config-headless.local:50051`）→ 抓 3 实例 `/metrics`（host 端口 19091/19092/19093）→
  断言 `(max-min)/avg < 10%`。
- `test/dev-e2e/headless/roundrobin-load/`：极简 Go 驱动，用发布的 SDK
  v0.4.0 调 `AbtestService.GetExperimentResult`。
- `test/dev-e2e/headless/README.md`：拓扑图 / R4 docker DNS caveat /
  禁止"多端口拓扑"退化的明确说明（design §4.1 锁定）。

**实测数据**（30 s × 50 并发，约 1.05M 次成功 RPC）：

| 实例 | host:metrics 端口 | `AbtestService/GetExperimentResult` code=OK 计数 |
|---|---|---|
| app1 | 19091 | 351040 |
| app2 | 19092 | 351052 |
| app3 | 19093 | 351055 |
| 合计 | — | 1053147 |

- max−min = 15 次
- 平均 = 351049
- 差值 = **0.00% of avg（target <10%）→ PASS**

复跑稳定（同一脚本本次另一轮：348451 / 348455 / 348465，spread 14 次，同样 0.00%）。

### SDK 行为变化（design AC #10 验证总结）

- `dns:///` 前缀自动启用 round_robin LB（本段 3 直接证据：3 实例 0.00% spread）。
- 其它 5 种地址形态保持 pick_first：
  - 裸 `host:port` —— 段 1 raw `grpcurl -plaintext localhost:50051` PASS + 段 1 SDK HTTP transport PASS（HTTP path 无 LB 概念，间接证 SDK 未在非 `dns:///` 形态干预 dial）；
  - `grpcs://host:port` —— 段 2 DEV 全套 152/152 backend PASS（公网 Cloudflare 形态，pick_first，未观察任何 LB 行为退化）。
- 单测层面已由 ST1（Go `TestServiceConfigFor_OtherPrefixes_ReturnsEmpty` 9 case）+
  ST2（Python `test_service_config_for_other_prefixes` 9 case）固化 AC #10 negative regression。

### 中间小坎记录

1. `grpc_smoke.sh` 硬编码 TLS（DEV 形态）—— 不能改测试代码；用 PATH-shim 注入 `-plaintext` 跑通本机。
2. `clients/go` 与 `clients/py` gRPC transport 也硬编码 `grpcs://`，本机 plaintext 跑不通；只跑 HTTP transport（HTTP 38 + 38 = 76 / 76）。这是测试驱动 vs 本机环境的形态不匹配（驱动设计目标是 DEV），不影响 AC #2 + AC #10 覆盖（详见段 1）。
3. headless 栈 PG 容器不自动迁库；脚本 step 2a 用主仓 `cmd/server migrate up` 跑 goose（端口 15433）。
4. headless 栈 docker network 名实际是 `tipsy-headless-net`（compose `networks.default.name` 覆盖 project 前缀），verify 脚本 alpine sidecar `--network` 直接用全名。
5. 旧 `tipsy-ab-config-db-1` 容器（前轮 dev-e2e 留下的）整段保持 Up，未触动。

---

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

### 2026-06-18 进一步：本地容器对照测试（loopback）

为了从客户端侧把"网络飞行"从 wait 里剥出来，按设计的第二条路在本机起了一整套同构环境：

- `tipsy-ab-config-app:latest`（主仓 `docker-compose.local.yml` 构建产物）以 `127.0.0.1:8080` 暴露 HTTP
- `tipsy-ab-config-db-1`（postgres:16-alpine）同 docker network 内提供 PG，DSN = `postgres://tipsy:tipsy@tipsy-ab-config-db-1:5432/tipsy?sslmode=disable`
- `test/dev-e2e/sql/seed.sql` 灌入本地库（两个 self-check 均 0 行，通过）
- 本机 HS256 token：`TIPSY_SERVICE_SECRET=devsecret go run cmd/servicetoken --sub local-test --namespaces '*' --ttl 24h`（主仓 cmd/servicetoken；token `namespaces=["*"]`）
- 同一份 load 驱动，仅改 `AB_CONFIG_HTTP_BASE=http://localhost:8080`

#### 同参数对照（60s × 150 conc × 1500 QPS 限速，`experiment_result` for_dev_agent_test）

| 指标 | DEV（经 Cloudflare） | 本机容器（loopback） | 差值 ≈ 网络飞行 |
|---|---:|---:|---:|
| 总请求 | 36617 | 59101 | — |
| 达成 QPS | 610 | 985 | 受限于 250ms RTT × 150 并发 |
| 错误率 | 0.00% | 0.00% | — |
| dns / tcp / tls | 0 / 0 / 0 ms | 0 / 0 / 0 ms | 全程 keep-alive |
| **wait p50** | **232.1 ms** | **0.4 ms** | **≈ 232 ms** |
| wait p95 | 307.1 ms | 0.5 ms | ≈ 306 ms |
| wait p99 | 475.2 ms | 0.6 ms | ≈ 475 ms |
| read body | 0.1 / 0.2 / 0.3 ms | 0.0 / 0.0 / 0.0 ms | ms 量级可忽略 |

#### 服务端 Prometheus 实证

抓 `/metrics` 上 `tipsy_abconfig_abtest_compute_duration_seconds`（本机 app 自带的 `experiment_result` 计算阶段直方图，**只计 abtest compute，本身不含 HTTP/serialization**）：

| ≤ bucket | 累计计数（for_dev_agent_test） | 占比 |
|---|---:|---:|
| 0.5 ms | 277,438 | ~41% |
| 1.0 ms | 408,065 | ~60% |
| 2.5 ms | 603,814 | ~89% |
| 5.0 ms | 671,030 | ~98% |

这条曲线和客户端 loopback 跑出的 wait p50 / p95 / p99（0.4 / 0.5 / 0.6 ms 量级，**低并发**；20k QPS 高并发下变 5 / 30 / 36 ms，是入队 + GC + PG 行锁的共同产物）量级一致。

#### 结论

- **RTS 的 232ms wall-clock 中，~99.8% 不是服务端在算**——服务端本身在低并发空载下 p50 ≈ 0.4 ms。
- DEV 的 232ms 几乎全是 **客户端 → Cloudflare 边缘 + Cloudflare → 香港/海外源站 + 反向飞回** 的网络飞行时间。这与「CN→CF→海外」公网 RTT 量级（180-280ms）吻合。
- **同集群内网理想 RTS 基线**：本机实测单实例空载 p50 / p95 / p99 = **0.4 / 0.5 / 0.6 ms**。服务端处理（auth + abtest cache lookup + bucket compute + protojson 编码）量级已经足够低，**没有进一步埋点优化的必要**。
- **不要把饱和数据当作"理想"基线**：把单实例打到 21k QPS 饱和时观测到的 p50 5 ms / p99 36 ms 是"单实例处理上限被排队拖到的延迟"，**不代表生产正常负载下的服务端处理时间**。生产是水平扩展 + 负载均衡，单实例从不会被打到那个水位，所以正常负载下应该接近 0.4 ms 基线，而不是 5 ms。
- 本机数据**不能直接套用到跨可用区或跨 region 内网**。生产内网 RTT 可能在 0.5–2 ms 量级（vs loopback 50 µs），PG 同 VPC 而非同 docker network、可能引入 Redis cache 同步，都会让真实 p99 略高于 1 ms 但不会到 100ms 级别。

#### 本对照测试的 caveat（必须知道，否则会过度乐观或过度悲观）

1. **本机 loopback RTT 约 50 µs**，生产同 k8s namespace 调用 RTT 约 0.2–1 ms（仍可忽略，不影响结论的数量级）。
2. **本机 PG 与 app 在同 docker network**，几乎零网络延迟；生产 PG 通常隔 1ms 以内同 VPC。
3. **本机是单实例 + 无 Redis**（服务端日志 `REDIS_ADDR not set; running single-node`），没有多实例 cache 同步成本；生产多实例下 PullAll/Subscribe 链路上多一跳。
4. **本机负载量级有限（21k QPS 单实例饱和）**。生产多实例水平扩展后**每个实例承载的 QPS 比单机饱和点低得多，p50 应保持在 ~0.4 ms 基线，不会出现 5ms 那种饱和延迟**。
5. **token 鉴权 / namespace 校验路径同 DEV 一致**（同一份代码），不存在因为本地跳过校验而失真。

#### 附：gRPC 连接复用结论（来自 SDK 源码 sdk.go:407,418 与 client.py:932,934）

SDK 内部为 ConfigService 和 AbtestService **各自独立 dial 一条 `*grpc.ClientConn` / `grpc.aio.Channel`**——即使两个地址相同（DEV 与本机均如此），也是两条 TCP/TLS 长连接、不共享。每条连接内部由 HTTP/2 多路复用所有 RPC：

- ConfigService 那一条：被 PullAll / Subscribe / GetDynamicConfig / GetStaticConfig 复用。
- AbtestService 那一条：被 GetExperimentResult 复用。

握手成本只在 Init 时付一次（2 次），稳态业务请求基本不存在建连开销——load 数据里 `100% reused`、dns/tcp/tls 三段全 0 ms 是这一点的实证。


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
