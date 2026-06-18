# gRPC 长连接、负载均衡与 Subscribe Push 技术讨论

- 文档性质：内部技术讨论稿，整理对话中的关键结论。
- 适用范围：tipsy-ab-config 平台与 SDK 在 K8S 集群内 / 跨集群 / 公网部署时的网络拓扑选型。
- 后续：本文档先暂存于 SDK 工程下，待后续整理时归档到内部 wiki。

## 1. 背景与关键事实

### 1.1 SDK 维护的 gRPC 连接数

SDK 内部为 `ConfigService` 与 `AbtestService` **各自独立 dial 一条 `*grpc.ClientConn` / `grpc.aio.Channel`**，即使二者地址相同。证据：

- Go：`sdk/go/tipsyabconfig/sdk.go:407,418`（`cli.configConn = cfgConn`、`cli.abtestConn = abConn`）
- Python：`sdk/python/tipsy_ab_config/client.py:932,934`（`config_channel = _build_channel(...)`、`_build_channel(... abtest_service_addr ...)`）

**所以 SDK 是「2 条 HTTP/2 长连接」**，不是 1 条，也不是 5 条；每条内部通过 HTTP/2 stream 多路复用。

每条连接的复用情况：

| 连接 | 复用的 RPC |
|---|---|
| ConfigService 连接 | `PullAll` / `Subscribe` / `GetDynamicConfig` / `GetStaticConfig` |
| AbtestService 连接 | `GetExperimentResult` |

握手成本只在 `Init` 时付清（2 次 TCP + 2 次 TLS），之后所有 RPC 走多路复用，**稳态业务请求基本无建连开销**。这一点在 dev-e2e load 数据里得到实证：60s × 150 并发跑 36617 个请求，dns/tcp/tls 三段全 0 ms，reuse 率 100%（见 `test/dev-e2e/RESULTS.md` §2026-06-18）。

### 1.2 一条 HTTP/2 连接是否会成为性能瓶颈

理论上可能的瓶颈来自四方面：

1. **`SETTINGS_MAX_CONCURRENT_STREAMS`**：grpc-go 默认 `100`，超过后客户端排队。当前 ab-config 服务端 (`cmd/server/main.go:666` 的 `grpc.NewServer(...)` 没有显式调高该参数，**所以走 grpc-go 默认值 `math.MaxUint32`，相当于不限流**。
2. **单条 TCP 拥塞窗口 / BDP**：跨海高 RTT 链路上才显著；同集群内网 RTT < 1ms 完全碰不到。
3. **HTTP/2 over TCP head-of-line blocking**：一次丢包卡掉同 TCP 所有 stream，直到 TCP 重传。同集群极少丢包，影响可忽略；公网高丢包链路才显著。
4. **grpc-go 一条连接对应 1 个 writer goroutine**：极高 QPS（30k+/连接）时可能 CPU 受限。

**对当前业务场景，四个瓶颈点都远未到达**，单条多路复用 + 2 条独立 channel 已经够用。

### 1.3 是否要把 1 条改成 5 条客户端连接？

**不建议**，对当前场景**真实收益约等于 0**：

| 维度 | 多连接的真实价值 |
|---|---|
| 服务端 stream 限流 | 不是瓶颈（默认未限流） |
| HOL 丢包阻塞 | 同集群可忽略；跨海 RTT 也救不了 |
| 单 writer goroutine 排队 | QPS 远未到瓶颈量级 |
| **单请求延迟** | **零收益**（多连接不减小 RTT） |
| 吞吐上限 | 理论 ×N，但当前还远未触顶 |

代价：握手成本 ×N、服务端连接数 ×N、客户端 RAM / goroutine ×N、Subscribe push 流如何分配等架构问题。

**业界普遍做法**：先调高服务端 `MaxConcurrentStreams` 上限，再水平扩展实例。客户端多连接池主要在 **L4 LB pin 后端**这种特殊场景下作为反 pin hack 使用。

## 2. gRPC 负载均衡的三种主流模式

| 模式 | 拓扑示意 | 负载均衡单位 | 客户端连接数 |
|---|---|---|---|
| **A. L4 LB（连接级 pin）** | Client → L4 LB → 选 1 个 backend → 长连保持 | 按 TCP 连接 | 1 |
| **B. L7 Proxy（请求级）** | Client → Proxy → backend pool；Proxy 对每个 RPC 选 backend | 按 RPC | 1 到 Proxy |
| **C. 客户端 LB（直连）** | Client → 直连所有 backend pod；客户端自己做 round-robin | 按 RPC | N（每 pod 一条） |

### 2.1 模式 A：L4 LB（最容易踩坑的模式）

典型部署：客户端配 ClusterIP Service DNS（如 `ab-config-grpc.<ns>.svc.cluster.local:50051`），grpc-go 默认 DNS resolver pick-first 拿到 Service 的虚 IP，跟它建一条 TCP。kube-proxy iptables/IPVS 在 TCP 握手时挑一个 backend pod，**之后这条 TCP 一直 pin 到那个 pod**。

**问题**：N 个业务客户端起来，每个客户端 pin 1 个 pod。当客户端数 ≪ pod 数时极度不均；甚至会出现部分 pod 无流量。

gRPC 官方专门讨论过这个反 pattern：<https://grpc.io/blog/grpc-load-balancing/>。

**SDK 当前文档（v0.1.0）默认推荐写法 `ab-config-grpc:50051` 落入这种模式**，是后续要改的核心问题。

### 2.2 模式 B：L7 Proxy

典型组件：Envoy、Traefik、Nginx Ingress（开 HTTP/2 后端通信）、Cloudflare gRPC 模式、Istio sidecar。

行为：Proxy 维护一张 stream 映射表 `(客户端 conn, stream X) ↔ (backend conn, stream Y)`，每个 RPC 独立选 backend。负载均衡公平、客户端无需感知 pod 拓扑变化。

代价：每多一层 Proxy 多一次 HTTP/2 帧 parse + serialize + 一段内网 RTT。同集群同地域亚毫秒级。

### 2.3 模式 C：客户端 LB

客户端 SDK 通过 DNS / xDS / EDS 拿到所有 pod 真实 IP（k8s 用 **Headless Service**，`clusterIP: None`），跟每个 pod 各建一条连接，每个 RPC 用 `round_robin` / `pick_first` / `weighted` 策略选连接发。

典型配置：
```go
grpc.NewClient("dns:///ab-config-grpc.<ns>.svc.cluster.local:50051",
    grpc.WithDefaultServiceConfig(`{"loadBalancingConfig":[{"round_robin":{}}]}`),
)
```

优点：最低延迟（少一跳 Proxy），公平分布。缺点：客户端要会感知 backend 变更（pod 启停时 reconnect）；当前 SDK v0.1.0 还没有原生暴露这套配置。

## 3. 当前 dev 拓扑分析

```
SDK Client (CN)
   │  1 条 HTTP/2 over TLS 长连接
   │  ──────────────────────────────►
   ▼
Cloudflare 边缘（gRPC=On）
   │  Cloudflare 内部 RPC 级回源调度
   ▼
Traefik (Coolify 容器主机)
   │  scheme=h2c，loadbalancer.server.port=50051
   │  按 RPC 选 backend
   ▼
app 容器 :50051（当前只有 1 实例）
```

dev 是 **模式 B 串联（Cloudflare + Traefik）**，但 backend 只有 1 个 pod，所以「均衡」无实际体现。

跨海公网 RTT 主导了 wait p50 = 232ms（dev-e2e 实测）。同集群内网测试 wait p50 = 0.4ms，**232ms 中 ~99.8% 是网络飞行而非服务端计算**。

## 4. K8S 集群内同 namespace 接入推荐路径

**强制约束**：客户端必须用集群内 DNS 解析；如果业务客户端解析的是公网域名（Cloudflare 那一套）并且公网域名走出 VPC 再 hairpin 回集群，性能从亚毫秒变跨海 RTT。

### 4.1 当前过渡推荐：internal Ingress（模式 B）

业务客户端配：

```
config_service_addr = "ab-config-grpc.internal:80"   # 或 :443 走 TLS
abtest_service_addr = "ab-config-grpc.internal:80"
```

集群内 DNS（CoreDNS）通过 split-horizon 把 `ab-config-grpc.internal` 解析到 internal Ingress Controller 的 ClusterIP，**不出 VPC**。

落地工作：

- 在 ab-config 服务前部署一个 **internal-only Ingress Controller**（NGINX/Traefik 都可），与对外 Ingress 分开，`ingressClassName: internal`。
- 给 ab-config 配 Ingress 资源，指向 internal Ingress Controller。
- 在 CoreDNS 加 rewrite 把 `ab-config-grpc.internal` 解析到 internal Ingress 的 ClusterIP。
- 业务方按 4.1 接入文档配 ENV。

代价：每个 RPC 多一跳 < 1ms 的内网飞行 + 一次 HTTP/2 帧 parse/serialize（µs 级）。**好处**：RPC 级公平 LB、客户端无需感知 pod 拓扑、与对外 Ingress 隔离。

### 4.2 终态推荐（TODO）：Headless Service + round_robin（模式 C）

业务客户端配：

```
config_service_addr = "dns:///ab-config-grpc.<ns>.svc.cluster.local:50051"
abtest_service_addr = "dns:///ab-config-grpc.<ns>.svc.cluster.local:50051"
```

落地条件：

- ab-config 的 Service 改成 **Headless**（`clusterIP: None`），DNS 返回所有 pod 的 IP 列表。
- SDK 内部默认带 `grpc.WithDefaultServiceConfig({"loadBalancingConfig":[{"round_robin":{}}]})`，并暴露开关让业务方覆盖。
- 兼容性：grpc-go 与 grpcio 均原生支持 `dns:///` resolver 与 `round_robin` 策略。

优点：

- **延迟最低**：客户端直连 backend pod，没有 Proxy 中间一跳；
- 不依赖额外组件（不需要 Ingress、不需要 mesh）；
- gRPC 官方推荐方案。

需要 SDK 配合改动（v0.2 / v0.3 计划）：

- 暴露 `Config.LoadBalancingPolicy` 字段或在地址解析里识别 `dns:///` 前缀；
- 默认开 `round_robin` 但允许业务方关闭；
- 文档更新接入方式。

### 4.3 不推荐：裸 ClusterIP Service DNS（模式 A）

```
config_service_addr = "ab-config-grpc:50051"        # 当前文档默认写法（v0.1.0 历史）
```

这是 **L4 pin 模式**，对应 §2.1 的反 pattern。后续 SDK 与文档应明确弃用，仅作历史兼容。

## 5. 模式 B 下的 Subscribe Push 模型

### 5.1 Subscribe 流的物理形态

Subscribe 是 **server-streaming gRPC**：客户端发一次 `SubscribeRequest`，服务端建一条流，之后所有 push 沿这条流 server → client 发。

模式 B 下的 Subscribe 流物理路径：

```
业务客户端 A      Subscribe stream X       
   │ ────────────────────────────────►
                                          │
                                          Ingress
                                          │   维护映射表：
                                          │   (客户端 conn, stream X) ↔ (backend conn, stream Y)
                                          │
                                          │ ────────────────────────────────► app pod P
```

**两条独立的 HTTP/2 stream**，Ingress 在中间转发字节。stream 的生命周期内 pin 在同一对 backend pod 上。

### 5.2 push 能否精确打到客户端？

**能**。逻辑链：

1. SDK 发起 Subscribe 流；
2. Ingress 把这条流 pin 到 pod-3（在流的生命周期内不变）；
3. pod-3 内存里有「活跃订阅表」，记录这条 `grpc.ServerStream` 订阅了 `ns=tipsy-chat`；
4. 配置变化时通过 Redis fan-out 通知所有 sibling pod；
5. pod-3 收到通知，看到自己的活跃订阅表里有这条流，写一条 message 到该 stream；
6. 字节沿 pod-3 ↔ Ingress 的 backend stream → Ingress ↔ 客户端的 stream 流回客户端 A。

pod-3 不需要知道"客户端 A 是谁"，它只在它手上的 `grpc.ServerStream` 上 `Send()`，Ingress 自动把字节转发回正确客户端。

### 5.3 多实例配置变更如何让所有 pod 都收到通知？

ab-config 已有方案，证据：

- `internal/api/grpc/configservice/notifier.go:8-32`：
  ```
  Notifier is the in-process broadcaster connecting local writes and
  abtest NotifyBusinessNamespaceChange RPCs to active Subscribe streams.

  originator path: a local write is broadcast cluster-wide (star fan-out, self included).
  receiver path:   NodeInternal.Notify. Receiver-only (star topology), wakes local subscribers.
  ```
- `cmd/server/main.go:879-934`：Redis 维护 sibling 列表 + `NodeInternal.Notify` RPC 在 pod 之间扇出。

完整链路（多实例）：

```
[admin 写一个配置 version]
         │
         ▼
pod-1 收到写入   ──► Redis 拿到所有 sibling pod 列表
         │
         ├─► self: WakeLocal(ns) → 唤醒 pod-1 本地所有订阅该 ns 的 Subscribe 流 → push
         │
         ├─► RPC: NodeInternal.Notify(pod-2) → pod-2 WakeLocal(ns) → push 本地订阅者
         │
         ├─► RPC: NodeInternal.Notify(pod-3) → pod-3 WakeLocal(ns) → push 本地订阅者
         │
         └─► ... (star fan-out 给所有 sibling)
```

每个 pod 维护自己上面的活跃 Subscribe 流列表，发生配置变化时所有 pod 都收到通知，各自给自己上面的订阅者发 push。**所以模式 B 不破坏 push 模型**。

### 5.4 pod 重启时的行为

- pod-3 死掉 → pod-3 ↔ Ingress 的 backend stream 断 → Ingress 把状态映射回客户端这一头：客户端收到 stream RST/closed；
- SDK 的 `runSubscribe()`（`sdk/go/tipsyabconfig/subscribe.go:16`）是无限重连循环，断了就 backoff 重连；
- 新的 Subscribe 流通过 Ingress 重新 pin 到（可能是新的）pod-x；
- 期间 SDK 靠周期性 fallback PullAll 兜底（`Config.PullInterval`，默认 10s），保证最终一致。

### 5.5 模式 B 下 push 的唯一隐患

**Ingress 自身重启 / 滚动升级**：所有 Subscribe 流瞬时全断，全部 reconnect，可能产生瞬时风暴打到 Ingress。这是 Ingress 滚动升级的通用风险，不是 push 模型本身的问题。缓解办法：Ingress 多实例 + 平滑滚动。

## 6. 同集群走对外 Ingress 域名的代价

**结论**：本身亚毫秒级，但必须避开 hairpin 陷阱。

### 6.1 hairpin 陷阱（最常见坑）

如果对外 Ingress 通过云厂商的「公网 LoadBalancer Service」暴露，集群内 pod 解析对外域名拿到的是**公网 IP**。pod → 公网 LB → hairpin 回到集群 → Ingress → 后端 pod。这条路径出了 VPC 又回来，可能多 5–50ms（取决于云厂商 hairpin / LB 实现），且部分云上 hairpin NAT 被默认禁用。

**唯一规避办法：split-horizon DNS**，集群内 CoreDNS 把对外域名 rewrite 到 Ingress Controller 的 ClusterIP，**不出 VPC**。外部访问者解析到公网 IP，互不冲突。

### 6.2 TLS 开销

集群外 Ingress 通常用 TLS（`grpcs://...:443`）。集群内 → Ingress 仍走 TLS 会让吞吐降 30–50%（Envoy/Nginx 普遍数据）。

**推荐**：internal Ingress 走明文 h2c（端口 80 或集群内自定义）。客户端配 `ab-config-grpc.internal:80`，省 TLS 成本。

### 6.3 必须绕开 Cloudflare

公网域名走 Cloudflare（如 `dev-ab-config-grpc.infra.fantacy.live`）。集群内**绝不能解析到 Cloudflare**——那就回到原始 232ms 跨海路径。**split-horizon DNS 是强制要求**。

## 7. 决策汇总

| 决策点 | 当前现状 | 短期推荐 | 长期终态 |
|---|---|---|---|
| K8S 内同 ns 接入方式 | 文档默认 `ab-config-grpc:50051`（模式 A） | internal Ingress 域名（模式 B） | Headless Service + 客户端 round_robin（模式 C） |
| SDK 客户端连接数 | 2 条（ConfigService + AbtestService 各一） | 不变 | 不变（模式 C 后变成 2 × N pod 条） |
| 多 pod 配置 push fan-out | 已有 Redis + NodeInternal.Notify star fan-out | 不变 | 不变 |
| 服务端 `MaxConcurrentStreams` | 默认 `MaxUint32`（不限） | 不变 | 不变 |
| 集群内 DNS | 取决于 CoreDNS 配置 | split-horizon 强制把 internal Ingress 域名解析到 ClusterIP | 同 |
| 接入文档 | 旧文案推荐裸 Service DNS | 加 internal Ingress 推荐 + 模式 A 弃用警告 + 模式 C TODO | SDK v0.2/v0.3 推出 `dns:///` + round_robin 配置后改为模式 C |

## 8. 待办（TODO）

- [ ] SDK v0.2 / v0.3 规划：
  - 暴露 `Config.LoadBalancingPolicy` 字段（或识别 `dns:///` 前缀自动启用 round_robin）；
  - Go：通过 `grpc.WithDefaultServiceConfig` 注入；Python：通过 `grpc.aio.insecure_channel(..., options=[("grpc.service_config", ...)])` 注入；
  - 单测覆盖 LB 策略切换。
- [ ] 接入文档 §4.1 已经加了 internal Ingress 推荐 + 模式 A 弃用警告 + TODO 链接（本次同步落地）。
- [ ] 平台侧：评估给 ab-config 加 **internal Ingress Controller** 部署，作为模式 A → 模式 B 过渡的服务端工作。
- [ ] 平台侧：评估把 Service 改成 **Headless**（`clusterIP: None`），作为模式 C 终态的服务端工作。
- [ ] 本文档迁出 `docs/tech-notes/` 后归档到内部 wiki，留索引页指向归档位置。

## 9. 参考

- gRPC LB 官方博客：<https://grpc.io/blog/grpc-load-balancing/>
- SDK 源码连接定义：`sdk/go/tipsyabconfig/sdk.go:407,418`、`sdk/python/tipsy_ab_config/client.py:932,934`
- 服务端 push fan-out：`internal/api/grpc/configservice/notifier.go`、`cmd/server/main.go:879-934`（主仓 `tipsy-ab-config`）
- DEV 网络延迟对照实测：`test/dev-e2e/RESULTS.md` §2026-06-18
- SDK 接入文档：`docs/usage-and-integration.md` §4.1
