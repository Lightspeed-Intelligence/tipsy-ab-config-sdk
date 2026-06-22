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

典型部署：客户端配 ClusterIP Service DNS（如 `tipsy-ab-config.<ns>.svc.cluster.local:50051`），grpc-go 默认 DNS resolver pick-first 拿到 Service 的虚 IP，跟它建一条 TCP。kube-proxy iptables/IPVS 在 TCP 握手时挑一个 backend pod，**之后这条 TCP 一直 pin 到那个 pod**。

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
grpc.NewClient("dns:///tipsy-ab-config-headless.<ns>.svc.cluster.local:50051",
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
config_service_addr = "tipsy-ab-config-grpc.internal:80"   # 或 :443 走 TLS
abtest_service_addr = "tipsy-ab-config-grpc.internal:80"
```

集群内 DNS（CoreDNS）通过 split-horizon 把 `tipsy-ab-config-grpc.internal` 解析到 internal Ingress Controller 的 ClusterIP，**不出 VPC**。

落地工作：

- 在 ab-config 服务前部署一个 **internal-only Ingress Controller**（NGINX/Traefik 都可），与对外 Ingress 分开，`ingressClassName: internal`。
- 给 ab-config 配 Ingress 资源，指向 internal Ingress Controller。
- 在 CoreDNS 加 rewrite 把 `tipsy-ab-config-grpc.internal` 解析到 internal Ingress 的 ClusterIP。
- 业务方按 4.1 接入文档配 ENV。

代价：每个 RPC 多一跳 < 1ms 的内网飞行 + 一次 HTTP/2 帧 parse/serialize（µs 级）。**好处**：RPC 级公平 LB、客户端无需感知 pod 拓扑、与对外 Ingress 隔离。

### 4.2 终态推荐（TODO）：Headless Service + round_robin（模式 C）

业务客户端配：

```
config_service_addr = "dns:///tipsy-ab-config-headless.<ns>.svc.cluster.local:50051"
abtest_service_addr = "dns:///tipsy-ab-config-headless.<ns>.svc.cluster.local:50051"
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

**推荐**：internal Ingress 走明文 h2c（端口 80 或集群内自定义）。客户端配 `tipsy-ab-config-grpc.internal:80`，省 TLS 成本。

### 6.3 必须绕开 Cloudflare

公网域名走 Cloudflare（如 `dev-ab-config-grpc.infra.fantacy.live`）。集群内**绝不能解析到 Cloudflare**——那就回到原始 232ms 跨海路径。**split-horizon DNS 是强制要求**。

## 7. 决策汇总

| 决策点 | 当前现状 | 短期推荐 | 长期终态 |
|---|---|---|---|
| K8S 内同 ns 接入方式 | 已落地（Go SDK v0.4.0 / Python SDK v0.5.0）：Headless Service + 客户端 round_robin（模式 C） | 同左（终态已落地） | 同左 |
| SDK 客户端连接数 | 2 条（ConfigService + AbtestService 各一）；模式 C 下每条 channel 展开为 N 个 subchannel（每 pod 一条），由 grpc-go / grpcio 自动管理 | 已落地（v0.4.0 / v0.5.0） | 不变 |
| 多 pod 配置 push fan-out | 已有 Redis + NodeInternal.Notify star fan-out | 不变 | 不变 |
| 服务端 `MaxConcurrentStreams` | 默认 `MaxUint32`（不限） | 不变 | 不变 |
| 集群内 DNS | 取决于 CoreDNS 配置 | split-horizon 强制把 internal Ingress 域名解析到 ClusterIP（仅模式 B 过渡形态需要） | 模式 C 直接走 `dns:///<service>.<ns>.svc.cluster.local`，由 K8S 原生 DNS 解析 |
| 接入文档 | 已落地（v0.4.0/v0.5.0）：`docs/usage-and-integration.md` §4.1 部署形态对照表 + §4.1.1 Headless 部署要求 | 同左 | 同左 |

## 8. 待办（TODO）

- [x] SDK 客户端 LB 能力（已落地于 `sdk/go/tipsyabconfig/v0.4.0` 与 `python-sdk/v0.5.0`，tag 待 ST4 发版推出）：
  - SDK 自动识别 `dns:///` 前缀启用 `round_robin`（不新增 Config 字段，业务方零 API 变更）；
  - Go：`dial()` 内通过 `grpc.WithDefaultServiceConfig` 注入；Python：`_build_channel` 通过 `("grpc.service_config", json)` channel option 注入；
  - 单测覆盖 LB 策略切换与 AC #10 负向行为（非 `dns:///` 目标不注入 service config）。
- [x] 接入文档 §4.1 / §4.1.1 已重写为部署形态对照表 + Headless 部署要求（替代旧的 internal Ingress 推荐 + TODO 链接）。
- [ ] 平台侧：评估给 ab-config 加 **internal Ingress Controller** 部署，作为模式 A → 模式 B 过渡的服务端工作（仍保留作平台侧 TODO，不属于 SDK 工作）。
- [ ] 平台侧：评估把 Service 改成 **Headless**（`clusterIP: None`），作为模式 C 终态的服务端工作（仍保留作平台侧 TODO，不属于 SDK 工作）。
- [ ] 本文档迁出 `docs/tech-notes/` 后归档到内部 wiki，留索引页指向归档位置。

## 9. Headless 模式下 Subscribe push fan-out 是否能简化？

提问场景：模式 C 下客户端按 round_robin 跟每个 pod 各持一条 gRPC 长连接。pod-1 收到管理员写入时，是否可以"只 push pod-1 自己手上的客户端"，把对 sibling pod 的 fan-out 简化为"只通知它们刷 cache 就行,不要 push"？

### 9.1 结论

**不能简化**。所有 pod 都需要被通知，所有 pod 都需要 push 自己手上的 Subscribe 流。否则其他 pod 上的客户端会漏推、退化为 SDK 端 10s 兜底 PullAll 才感知，push 模型变 poll 模型。

### 9.2 推导

Subscribe 是 server-streaming RPC。grpc-go 的 round_robin LB 在调用 unary RPC 时按策略选 subchannel，但对 server-streaming RPC **只在某一条 subchannel 上开流**——选了哪条就 pin 在那条上直到流断开（reconnect 后新流可能选别的 subchannel）。

所以模式 C 下，M 个客户端的 Subscribe 流**分散在所有 N 个 pod 上**（不是每个客户端跟所有 pod 都有 Subscribe 流）：

```
客户端 c1 的 Subscribe 流 ──pin──► pod-X (X 由 LB 选定)
客户端 c2 的 Subscribe 流 ──pin──► pod-Y
...
客户端 cM 的 Subscribe 流 ──pin──► 分布在所有 N 个 pod 上
```

写入发生在 pod-1（无论是 admin API 写入还是其它 pod 路由进来的），它只看得到自己手上的 Subscribe 流——其它 pod 上的 c2/c5/c7... 它根本不知道。如果只让 pod-1 push 自己手上的客户端，pod-2..N 上的 Subscribe 流就**永远收不到这次变更的 push**——只能靠 SDK 端的 fallback PullAll（默认 `PullInterval=10s`）兜底，把"实时 push"语义降级为"最多 10s 延迟的 poll"。

所以 fan-out 不可省。

### 9.3 当前 ab-config 的方案恰好就是对的

`internal/api/grpc/configservice/notifier.go` 的 star 拓扑 + Redis sibling 发现：

```
pod-1 收到写入
  ├─► self: WakeLocal(ns) → 唤醒 pod-1 本地所有订阅该 ns 的 Subscribe 流并 push
  ├─► RPC: NodeInternal.Notify(pod-2) → pod-2 WakeLocal(ns) → push pod-2 本地订阅者
  ├─► RPC: NodeInternal.Notify(pod-3) → pod-3 WakeLocal(ns) → push pod-3 本地订阅者
  └─► ...
```

每个 pod 维护自己手上的活跃 Subscribe 列表（grpc.ServerStream 句柄）。写入扇出后，每个 pod 各自给自己手上的客户端 push。Headless 模式 / 模式 B（Ingress L7）/ 普通 ClusterIP（模式 A）下这套机制**完全相同**——LB 模式只影响 Subscribe 流落在哪个 pod 上，不影响 push 路径。

注意：`NodeInternal.Notify` 通知的内容是「namespace 变了，去拉新数据」（轻量唤醒信号），**不是把整份配置数据通过 RPC 传过去**。每个 pod 自己去 DB / cache 拉新版本数据，再 push 到自己手上的流。

### 9.4 可以做的 micro-optimization（与本任务无关）

`notifier.go` 注释里写 originator 路径 "star fan-out, self included"——pod-1 在 fan-out 时把自己也算进去，走的是和 sibling 完全相同的代码路径（避免维护两套）。理论上可以把 self 路径短路（写入完成后直接 WakeLocal，不再对 self 发 fan-out RPC），省一次 self RPC，但对延迟影响 < 1ms，**不属于本设计推荐的优化方向**。

## 10. Headless 不影响公网域名 / HTTP transport / ClusterIP 接入

提问场景：如果平台在 K8S 开启 Headless 模式，是否影响公网域名访问、HTTP 链路、以及继续走普通 ClusterIP 的业务方？

### 10.1 结论

**不影响**。Headless 与 ClusterIP **可以并存**，应作为生产 K8S 部署的标准形态。同一组 Pod 同时挂多个 Service（一个 ClusterIP、一个 Headless），互不干扰。

### 10.2 K8S Service 语义复习

K8S 里 Service 是 Pod 上的「选择器策略」。同一组 Pod 可以被多个不同 Service 同时选中——只要 selector 匹配，每个 Service 独立暴露同一组 Pod，行为彼此正交。

### 10.3 推荐的生产部署形态

```
                       ┌────────────────────────────────┐
                       │ Pod(app=tipsy-ab-config)       │
                       │   :8080 HTTP                   │
                       │   :50051 gRPC                  │
                       │   :9090 metrics                │
                       └───────────────┬────────────────┘
                                       │
   ┌───────────────────────────────────┼───────────────────────────────────┐
   │ 同一组 Pod,同时被多个 Service 选中(selector 都是 app: tipsy-ab-config) │
   ▼                                   ▼                                   ▼
┌───────────────────┐   ┌───────────────────────────┐   ┌───────────────────────────┐
│ Service           │   │ Service                   │   │ Ingress → Service         │
│ tipsy-ab-config   │   │ tipsy-ab-config-headless  │   │ tipsy-ab-config-public    │
│ (ClusterIP, 虚 IP)│   │ (Headless, clusterIP:None)│   │ (LoadBalancer)            │
└─────────┬─────────┘   └─────────────┬─────────────┘   └─────────────┬─────────────┘
          │ kube-proxy NAT            │ DNS 返回所有                  │ 公网 SLB
          │ 到 1 个 pod               │ ready Pod 真实 IP             │
          ▼                            ▼                              ▼
   集群内 HTTP transport       集群内 gRPC 业务方(推荐)        公网客户端、跨集群业务方
   集群内历史 gRPC 接入        同 ns 与跨 ns,SDK dns:///       (grpcs://...:443)
```

### 10.4 各 Service 的角色

| Service | 是否分配 ClusterIP | 谁会用 | 行为 |
|---|---|---|---|
| `tipsy-ab-config`（ClusterIP） | 有 | 公网 Ingress 后端、HTTP transport 业务方、历史 gRPC 接入（裸 `tipsy-ab-config:50051`）、跨集群/集群外业务方 | kube-proxy iptables NAT 到 1 个 pod；gRPC 长连接 L4 pin（模式 A） |
| `tipsy-ab-config-headless`（Headless） | 无（`clusterIP: None`） | 集群内 gRPC 业务方（推荐，同 ns 与跨 ns 都适用） | DNS 返回 N 个 pod IP；SDK v0.4.0+ 看到 `dns:///` 前缀自动启用 round_robin（模式 C） |
| Ingress 后端 Service | 有 | Cloudflare → SLB → Ingress → backend | Ingress 必须能摸到 backend 的虚 IP，**只能是 ClusterIP** |

### 10.5 为什么 Headless 不能替代 ClusterIP

1. **公网 Ingress 链路**：Cloudflare → SLB → Ingress Controller → backend Service，Ingress Controller 必须有一个虚 IP 作为转发目标。**Headless 不分配虚 IP，Ingress 摸不到**。所以公网路径必须有 ClusterIP Service。
2. **HTTP transport**：HTTP 是短请求或多连接池，L4 pin 在 HTTP 路径上影响很小（不会出现 gRPC 那种"一条长连接所有 RPC 都 pin 同一个 pod"的不均衡）。HTTP 业务方继续走 ClusterIP 是合理的，没必要也走 Headless。
3. **跨集群业务方**：跨集群通常根本解析不了 `*.svc.cluster.local`（除非有 multi-cluster mesh），只能走 Ingress。注意：**同集群跨 namespace 不在此列**——同集群 CoreDNS 能解析任意 ns 的 headless FQDN，跨 ns 同样推荐走 Headless（`dns:///tipsy-ab-config-headless.<server-ns>.svc.cluster.local:50051`，`<server-ns>` 填 ab-config 所在 ns），纯内网 + RPC 级 round_robin，不必出 Ingress。
4. **历史兼容**：旧业务方写裸 `tipsy-ab-config:50051` 进 ENV 已经在跑，行为是 L4 pin。这种接入在 pod 数较少 + 客户端数 ≫ pod 数的场景下概率上自然均衡，迁移不紧迫。强行去掉 ClusterIP 会让这部分业务方直接报错。

### 10.6 YAML 与运维落地

平台仓 `cmd/prod.manifests.yaml` 提供 ClusterIP Service `${name}`，`cmd/prod.headless-service.yaml` 提供 Headless Service `${name}-headless`。两个文件分别 `kubectl apply`，selector 一样、端口一样、共享同一组 Pod。详见主仓 `docs/aliyun-prod-deploy.md` §9.1。

业务方接入选哪条路径只看他们在 ENV 里填的地址，**不需要改 Pod、不需要改镜像、不需要重启服务**：

| 业务方场景 | ENV 配置 |
|---|---|
| 集群内同 ns gRPC（推荐） | `dns:///tipsy-ab-config-headless.<ns>.svc.cluster.local:50051` |
| 集群内跨 ns gRPC（推荐） | `dns:///tipsy-ab-config-headless.<server-ns>.svc.cluster.local:50051`（`<server-ns>` 填 ab-config 所在 ns，纯内网 + round_robin） |
| 集群内同 ns HTTP | `http://tipsy-ab-config.<ns>.svc.cluster.local:8080` + SDK `Transport: "http"` |
| 跨集群 / 公网 | `grpcs://prod-ab-config-grpc.<your-domain>:443` |
| 集群内 gRPC 历史 | 裸 `tipsy-ab-config.<ns>.svc.cluster.local:50051`（仍工作，不推荐新接入） |

## 11. 参考

- gRPC LB 官方博客：<https://grpc.io/blog/grpc-load-balancing/>
- SDK 客户端 LB 实现入口（符号定位，行号会随提交漂移）：
  - Go：`sdk/go/tipsyabconfig/sdk.go` 的 `serviceConfigFor`（`dns:///` → round_robin service config JSON）与 `(*Client).dial`（在 dial options 中注入 `WithDefaultServiceConfig`）。
  - Python：`sdk/python/tipsy_ab_config/client.py` 的 `_service_config_for` 与 `_build_channel`（在 channel options 中注入 `("grpc.service_config", json)`）。
- SDK 双 channel 构建位置（符号定位）：
  - Go：`sdk/go/tipsyabconfig/sdk.go` 内 `Init` 调用 `cli.dial(configTarget)` 与 `cli.dial(abtestTarget)`。
  - Python：`sdk/python/tipsy_ab_config/client.py` 内 `init` 调用 `_build_channel(cfg, cfg.config_service_addr, ...)` 与 `_build_channel(cfg, cfg.abtest_service_addr, ...)`。
- 服务端 push fan-out：`internal/api/grpc/configservice/notifier.go`、`cmd/server/main.go:879-934`（主仓 `tipsy-ab-config`）
- 平台 K8S 部署形态（含 ClusterIP + Headless 双 Service 共存）：主仓 `docs/aliyun-prod-deploy.md` §9.1、`cmd/prod.headless-service.yaml`、`cmd/prod.manifests.yaml`
- DEV 网络延迟对照实测：`test/dev-e2e/RESULTS.md` §2026-06-18
- SDK 接入文档：`docs/usage-and-integration.md` §4.1 / §4.1.1
