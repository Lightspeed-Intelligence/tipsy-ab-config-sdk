# gRPC、负载均衡与配置更新

本文描述 SDK 当前的连接、负载均衡和缓存更新模型。部署参数由目标环境提供；文中的域名与
Service 名均为示例，不代表某个远端环境的在线状态。

## 1. 每个 SDK client 的连接

Go、Python、Java client 都是进程级、可并发复用的对象。gRPC 模式通常建立：

- 一条 ConfigService channel，承载启动/周期 `PullAll` 与长期 `Subscribe`；
- 一条 AbtestService channel，承载按请求调用的 `GetExperimentResult`；如果
  `AbtestServiceAddr` 为空则不建立，动态配置按 full release/default 降级。

一条 gRPC channel 可以在同一 HTTP/2 连接上复用大量并发 stream。无需为并发请求手工创建
多个 SDK client；那会重复缓存、后台循环和 Subscribe 流。真正的连接数可能由 resolver、
负载均衡策略和底层 channel 实现决定，而不是由 SDK 固定为一个 socket。

HTTP transport 不创建 gRPC channel，也不建立 Subscribe。

## 2. target 解析与负载均衡

三语言采用相同的选择规则：

| gRPC target | TLS | SDK 默认负载均衡 |
|---|---|---|
| `host:port` | 否 | `pick_first` |
| `grpc://host:port` | 否 | `pick_first` |
| `grpcs://host:port` | 是 | `pick_first` |
| `dns:///host:port` | 否 | `round_robin` |
| 其他原生 resolver target | 按当前地址契约为 plaintext | gRPC 默认策略 |

调用方提供的语言特有 channel 配置可能覆盖上述默认值。

`round_robin` 只能在 resolver 返回多个 backend address 时分配流量。普通 Kubernetes
ClusterIP Service 的 DNS 通常返回单个 virtual IP，因此即使选择 round_robin，客户端也
看不到 pod 列表。需要 client-side LB 时使用 Headless Service：

```text
dns:///ab-config-headless.platform.svc.cluster.local:50051
```

Headless Service 的 DNS A/AAAA 记录列出 pod IP，gRPC resolver 监测列表变化，SDK channel
再以 round_robin 选择 subchannel。它不是 HTTP URL，不能写成 `http://...`。

## 3. 三种部署路径

### 客户端负载均衡：Headless Service

```text
business pod
  └─ SDK dns resolver + round_robin
       ├─ ab-config pod A
       ├─ ab-config pod B
       └─ ab-config pod C
```

适用于同一 Kubernetes 网络内的 SDK。部署方需要保证：

- Service 为 `clusterIP: None`；
- 端口映射到后端 gRPC listener；
- DNS 能返回 ready endpoints；
- NetworkPolicy 允许业务 pod 访问这些 pod IP。

### 代理负载均衡：L7 gRPC proxy

```text
business pod ── one SDK channel ── gRPC-aware proxy ── backend pods
```

SDK 连接 proxy，后端选择由 proxy 完成。proxy 必须支持 HTTP/2 gRPC 与长时间
server-streaming，不得把空闲 Subscribe 当作普通短请求提前关闭。若跨网络使用 TLS，证书
名称、SNI 和 `:authority` 必须与部署配置一致。

### Kubernetes ClusterIP

```text
business pod ── one long-lived channel ── ClusterIP ── selected backend pod
```

ClusterIP 可用于简单内网接入，但现有长连接通常持续落在最初选中的 backend；kube-proxy
不会按每个 gRPC RPC 重新分配已建立连接。它不适合依赖 client-side round_robin 的场景。

## 4. Subscribe 与 PullAll

gRPC 模式启动后同时维护两条更新机制：

1. `Subscribe` 是近实时 server-streaming 通道。请求携带 namespace 列表和客户端已知的
   `(business_snapshot_seq, experiment_snapshot_seq)`；服务端仅在任一 seq 更新时下发完整
   namespace snapshot。
2. 周期 `PullAll` 是安全网，默认每 10 秒检查一次。Subscribe 暂时中断时仍能最终恢复配置。

服务端可发送无配置载荷的 heartbeat。SDK 忽略其业务内容：不改变缓存、不推进 seq；它只让
空闲 HTTP/2 流保持活动。

Subscribe 断开后，SDK 重连并携带当前 seq。新连接若发现客户端快照落后，会收到新 snapshot；
如果已经最新则无需重复推送。后台失败通过各语言当前提供的 health、metrics、日志或 error
callback 暴露；运行时拉取或 Subscribe 失败不删除现有缓存。

HTTP transport 只执行启动和周期 `PullAll`。因此：

- 没有 Subscribe stream 或 heartbeat；
- `SubscribeConnected` 恒为 false、`LastSubscribeErr` 为空；
- 更新延迟上限由 `PullInterval` 和一次 PullAll 成功所需时间决定。

## 5. 多实例事件传播

Subscribe stream 只存在于某个 SDK 与它所连接的 config 实例之间。平台写入可能发生在另一个
实例，因此“写入成功”本身不能只更新本机 stream。服务端架构通过内部通知/fan-out 使各
config 实例重新组装快照并通知自己的本地 subscribers；SDK 的周期 PullAll 仍是消息丢失或
实例切换时的恢复路径。

Headless Service + round_robin 并不消除服务端 fan-out：不同 SDK 会连接不同实例，且每个
SDK 的 Subscribe stream 只由持有该 stream 的实例写入。fan-out 保证每个实例获知变化，
PullAll 保证最终校正。

## 6. 服务角色

同一套后端可按入口需求暴露多个 Service：

| Service | 作用 | 是否可替代其他角色 |
|---|---|---|
| Headless gRPC Service | SDK resolver 直接发现 pod，客户端 LB | 不能作为稳定 HTTP/Ingress upstream |
| ClusterIP Service | 稳定 virtual IP，供 HTTP/Ingress 或简单内网 gRPC | 不提供 pod 地址列表 |
| ExternalName（如有） | 提供 DNS 别名，不承载或负载均衡流量 | 不替代 Headless/ClusterIP |

不要让同集群业务流量为了复用公网域名而绕出集群再 hairpin 回来，除非网络、安全和成本策略
明确要求如此。公网 CDN/WAF 也必须明确支持 gRPC streaming；否则应提供集群内 Service 或
独立的 gRPC L7 入口。

## 7. TLS 与开发诊断

`grpcs://host:port?authority=name` 允许连接地址与证书名不同：SDK 拨号 `host:port`，同时把
`authority` 用于 HTTP/2 authority 与 TLS SNI。生产证书应由系统信任链或显式私有 CA 校验。

Go/Java 的 `insecure=true` 可以关闭证书校验；这是开发诊断开关，生产不得使用。Python
grpcio 无同等开关，当前 SDK 会警告但仍验证证书；应通过
`Config.tls_root_certificates` 提供私有 CA。

## 8. 运维核对清单

- SDK client 是进程级单例，而不是按请求创建。
- target 与 transport 匹配：gRPC 使用 gRPC 地址，HTTP 使用 `http(s)://` base URL。
- `dns:///` 指向真正返回 pod 地址的 Headless Service。
- L7 proxy 保持 HTTP/2 和长期 server-streaming。
- token 允许所订阅 namespace；若使用 provider，按[集成手册](../usage-and-integration.md)
  核对对应语言的实际刷新时机。
- 同时监控 PullAll 与 Subscribe；HTTP 模式不把 SubscribeConnected=false 当作告警。
- 配置更新演练既验证推送延迟，也验证断流后周期 PullAll 的恢复能力。

SDK 配置示例与地址语法见[集成手册](../usage-and-integration.md)。
