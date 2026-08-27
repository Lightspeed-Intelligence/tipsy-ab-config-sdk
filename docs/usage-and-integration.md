# Tipsy AB Config 集成手册

本文是业务服务接入 Go、Python、Java SDK 的当前说明。平台管理面的配置流程与 Admin API
不属于 SDK 契约，应以部署中的 Tipsy AB Config 服务文档为准。

## 1. 运行模型

SDK 初始化时向 `ConfigService.PullAll` 拉取所订阅 namespace 的完整快照，然后在进程内
解析配置：

1. 实验或灰度命中的 `config_key.key → config_version.id`；
2. 未命中时使用 full release；
3. 单 key 仍无值时返回调用方提供的 default；get-all 则省略该 key。

同一个业务请求应只创建一个 `AbtestContext`，并传给本请求中的所有动态配置读取。
上下文按 namespace 惰性调用 `AbtestService.GetExperimentResult`，同一个上下文中每个
namespace 至多拉取一次。创建上下文本身不发 RPC；可显式 prefetch 以提前启动拉取。

`user_id` 是被分流对象的稳定标识，不限定为自然人。`user_attrs` 是 admission 条件使用的
标量属性，支持 string、integer、double 和 boolean。空字符串、`"0"`（Python 还包括
`None`）表示无真实用户身份：三语言均跳过实验与白名单 RPC，只按 full release 解析。

### 当前缓存与更新语义

- gRPC：启动 `PullAll`，维持 server-streaming `Subscribe`，并保留周期性 `PullAll`
  安全网；空闲 heartbeat 只维持流，不更新缓存。
- HTTP：启动和周期性调用 `PullAll`，不建立 Subscribe；变更可见延迟受
  `PullInterval`（默认 10 秒）约束。
- `has_dynamic_resolution` 明确为 `false` 时，SDK 知道 key 是纯 full release，跳过
  必然无结果的 A/B 调用。字段为 `true` 或缺失时走完整 A/B 路径；因此连接旧服务端时
  功能安全，只是没有快路径。
- `GetConfigStatic` 是纯本地缓存读，不做实验计算。
- AbtestService 不可用时，动态读取降级到 full release/default。ConfigService 是缓存
  数据源；其启动拉取失败默认使初始化失败，只有显式开启 `StartupFailOpen` 才允许空缓存启动。

## 2. 版本与安装

本仓是多模块 monorepo，各语言独立发布。使用 Releases、模块 CHANGELOG 或包仓库元数据
确定版本；生产依赖应 pin 到明确版本。

| 模块 | 发布标识/坐标 | 当前仓库元数据 |
|---|---|---|
| Go | module path + `sdk/go/tipsyabconfig/vX.Y.Z` tag | `v0.13.1` |
| Python | `python-sdk/vX.Y.Z` / `tipsy-ab-config` | `v0.14.1` |
| Java | `io.github.lightspeed-intelligence:tipsy-abconfig` | `v0.10.0` |
| Go proto | `api/gen/go/vX.Y.Z` | `v0.7.0` |

版本入口：

- [GitHub Releases](https://github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk/releases)
- [Go CHANGELOG](../sdk/go/tipsyabconfig/CHANGELOG.md)
- [Python CHANGELOG](../sdk/python/CHANGELOG.md)
- [Java CHANGELOG](../sdk/java/tipsy-abconfig/CHANGELOG.md)

Go：

```bash
go get github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk/sdk/go/tipsyabconfig@v0.13.1
```

Python 和 Java 的安装源与完整依赖要求分别见
[Python README](../sdk/python/README.md) 和 [Java README](../sdk/java/README.md)。Python
当前要求 Python 3.10+、`grpcio>=1.66.2,<2`、`protobuf>=5.29.1,<7`；HTTP transport
还需安装 `http` extra。Java 当前要求 Java 21。

## 3. 鉴权

SDK 支持两种凭据，gRPC 与 public-read HTTP 两条 transport 行为一致：

| 模式 | 配置 | 传输形态 | 权限 |
|---|---|---|---|
| secretKey | Go `SecretKey` / Python `secret_key` / Java `Builder.secretKey(...)` | `Authorization: SecretKey <secret>` | 全量（任意 namespace） |
| service JWT | `Token` 或 `TokenProvider` | `Authorization: Bearer <token>` | 按 token claims 细粒度授权 |

两者均不同于 Console/Admin 使用的人类会话 token。初始化要求
secretKey / Token / TokenProvider 至少配置其一。

### 3.1 secretKey 模式（免签发 token）

secretKey 就是平台的 `TIPSY_SERVICE_SECRET` 本身，由部署方直接分发给业务服务，
业务方无需再经可信 issuer 签发 JWT。信任模型如实描述如下，部署方自行评估是否可接受：

- **持有 secretKey 即拥有全量访问权限**：校验通过等效于 `internal_service` role +
  `"*"` namespace，可请求任何 namespace 的配置与实验；role/namespace 细粒度授权在
  该模式下不生效。持有者也能用同一 secret 自行签发任意权限的 JWT——这是该模式
  有意的取舍，不是缺陷。
- **secret 随每个请求明文上线**（gRPC metadata / HTTP header）。建议仅在可信内网
  使用，或走 TLS（`grpcs://` / `https://`）入口。
- 需要按服务、按 namespace 收敛权限或做短 TTL 轮换时，仍应使用 JWT 模式。

行为约定：

- **优先级：`SecretKey > TokenProvider > Token`**（三端一致，逐请求求值）。同时配置
  时发送的是 `SecretKey <secret>`，token 形态的凭据不参与该请求。
- 平台侧校验 fail-closed：secretKey 无效时请求直接失败（Unauthenticated / 401），
  **不回退**到 Bearer token 校验。
- SDK 不从环境变量读取 secretKey，由业务方显式传入 Config。
- **部署顺序**：平台（verifier）先升级，SDK 侧后启用 secretKey。顺序颠倒时
  平台不认识 `SecretKey` scheme，请求会以 401 失败。

### 3.2 service JWT 模式

service JWT 为 HS256 签名，授权信息为：

```json
{
  "sub": "my-service",
  "roles": ["business_sdk"],
  "namespaces": ["my-app"],
  "iat": 1710000000,
  "exp": 1710003600
}
```

- 普通业务 SDK 使用 `business_sdk` role，并只授予业务需要的 namespace。平台进程间调用
  使用的 `internal_service` role 与 `"*"` namespace 属于内部服务身份，不应用于普通业务 SDK。
- 需要细粒度授权（限定 namespace/role）时使用本模式：token 的权限范围在签发时收敛，
  业务服务只持有 token 而非 secret。
- SDK 接受静态 token，也接受 `TokenProvider`。Go/Java 在每次请求取当前 token，适合由
  部署方的可信 issuer 做短 TTL 轮换。Python 的当前限制见下方“待人工核实”。
- 本 SDK 仓库不提供在线 token 申请服务。endpoint、token 和签发方式均由目标环境的
  部署方提供；不要把真实 token 或 secret 提交进仓库。

可信 issuer 可使用 Go `tipsyauth` 或 Java `tipsy-auth`。Go 示例：

```go
signer, err := tipsyauth.NewSigner(os.Getenv("TIPSY_SERVICE_SECRET"))
if err != nil {
	return err
}
token, err := signer.Issue(tipsyauth.IssueOptions{
	Subject:    "my-service",
	Roles:      []string{"business_sdk"},
	Namespaces: []string{"my-app"},
	TTL:        time.Hour,
})
```

## 4. Transport 与部署

三语言的显式配置优先于 `TIPSY_SDK_TRANSPORT` 环境变量；均未设置时默认为 gRPC。

| 模式 | 地址 | 配置更新 | 适用场景 |
|---|---|---|---|
| gRPC plaintext | `host:port`、`grpc://host:port` | Subscribe + PullAll | 可信内网 |
| gRPC resolver target | `dns:///host:port` 等原生 target | Subscribe + PullAll | Headless Service、xDS 等 |
| gRPC TLS | `grpcs://host:port` | Subscribe + PullAll | 跨网络或需 TLS 的入口 |
| HTTP | `http://` / `https://` base URL | 仅周期 PullAll | 环境无法稳定支持 gRPC streaming |

### Kubernetes 与负载均衡

`dns:///` 是显式 gRPC DNS resolver target。Go、Python、Java 都会对此类 target 注入
`round_robin`；bare address、`grpc://`、`grpcs://` 及其他 resolver target 保持 gRPC
默认的 `pick_first`，除非调用方通过语言特有的 channel 配置覆盖。

若要让 SDK 直接轮询多个后端 pod，部署方必须提供 Headless Service；普通 ClusterIP DNS
通常只返回一个虚拟 IP，客户端 `round_robin` 无法看到 pod 列表。经 L7 gRPC proxy 接入
时应使用 proxy 地址，由 proxy 做后端分配。完整模型见
[gRPC、负载均衡与配置更新](tech-notes/grpc-load-balancing-and-push.md)。

### gRPC 地址语法

- bare `host:port` 与 `grpc://host:port`：明文 h2c；
- gRPC 原生 resolver target（如 `dns:///`、`unix:`、`xds:///`）：原样交给语言运行时，
  当前 SDK 将它们作为 plaintext target；
- `grpcs://host:port[?authority=name&insecure=true]`：TLS。`authority` 覆盖 HTTP/2
  `:authority` 和 SNI/证书名；`insecure=true` 只适合开发诊断，生产不得跳过证书校验；
- HTTP URL 不能用于 gRPC transport，反之亦然。

Python `grpcio` 没有等价的 `InsecureSkipVerify`：`insecure=true` 只产生警告，不会关闭证书
校验；私有 CA 应通过 `tls_root_certificates` 注入。各语言的 channel 注入能力见其 README。

### HTTP wire contract

HTTP transport 使用 protojson POST，并追加以下固定路径：

- `POST {config-base}/api/v1/config/pull_all`
- `POST {abtest-base}/api/v1/abtest/experiment_result`

请求包含 `Content-Type: application/json` 和 `Authorization` 凭据（§3 的两种模式
均适用）。HTTP 模式不调用 Subscribe；
它不是对任意 gRPC 方法的通用转码层。

## 5. Go 接入

```go
client, err := tipsyabconfig.Init(context.Background(), tipsyabconfig.Config{
	Namespaces:        []string{"my-app"},
	ConfigServiceAddr: os.Getenv("CONFIG_SERVICE_ADDR"),
	AbtestServiceAddr: os.Getenv("ABTEST_SERVICE_ADDR"),
	Token:             os.Getenv("TIPSY_TOKEN"),
	// Transport: tipsyabconfig.TransportHTTP, // HTTP 时显式选择
})
if err != nil {
	return err
}
defer client.Close()

abctx := client.NewAbtestContext(ctx, "user-123", map[string]any{
	"country": "JP",
	"vip":     true,
})
value, err := client.GetConfig(ctx, abctx, "my-app", "feature_x", "off")
all, err := client.GetAllConfigs(ctx, abctx, "my-app")
staticValue, found := client.GetConfigStatic("my-app", "feature_x", "off")
```

动态 token：

```go
TokenProvider: func(ctx context.Context) (string, error) {
	return tokenStore.Current(ctx)
},
```

`PROJECT_DEFAULT_NAMESPACE` 或 `Config.DefaultNamespace` 为
`GetConfigDefault` / `GetAllConfigsDefault` 提供默认 namespace。Go 还提供 net/http
`Middleware` 与无 Gin 编译依赖的 `GinMiddleware`；默认不预热，只有显式配置
`PrefetchPaths(...)` 的精确路径才预热默认 namespace。完整 API 见包注释与
[`sdk/go/example`](../sdk/go/example)。

## 6. Python 接入

```python
import os
from tipsy_ab_config import Config, init

client = await init(Config(
    namespaces=["my-app"],
    config_service_addr=os.environ["CONFIG_SERVICE_ADDR"],
    abtest_service_addr=os.environ["ABTEST_SERVICE_ADDR"],
    token=os.environ["TIPSY_TOKEN"],
    # transport="http",
))

ctx = client.new_abtest_context(
    user_id="user-123",
    user_attrs={"country": "JP", "vip": True},
)
value = await client.get_config(ctx, "my-app", "feature_x", "off")
all_values = await client.get_all_configs(ctx, "my-app")
static_value = client.get_config_static("my-app", "feature_x", "off")
```

Python 的 `token_provider` 是 async callable，当前在初始化时调用并缓存结果。HTTP 模式需要
`httpx`：按当前安装方式加入 `http` extra。`AbtestMiddleware` 支持 ASGI/FastAPI，用
`contextvars` 保存请求级上下文；
默认不预热，`prefetch_paths` 是精确路径白名单。完整安装、middleware 与排障说明见
[Python README](../sdk/python/README.md)。

## 7. Java 接入

```java
try (TipsyAbConfigClient client = TipsyAbConfigClient.create(Config.builder()
        .namespaces("my-app")
        .configServiceAddr(System.getenv("CONFIG_SERVICE_ADDR"))
        .abtestServiceAddr(System.getenv("ABTEST_SERVICE_ADDR"))
        .token(System.getenv("TIPSY_TOKEN"))
        // .transport(Transport.HTTP)
        .build())) {

    AbtestContext abctx = client.newAbtestContext(
            "user-123", Map.of("country", "JP", "vip", true));
    String value = client.getConfig(abctx, "my-app", "feature_x", "off");
    Map<String, String> all = client.getAllConfigs(abctx, "my-app");
    String staticValue = client.getConfigStatic("my-app", "feature_x").orElse("off");
}
```

Java 使用 SLF4J，动态 token 类型为 `TokenProvider`。它不提供 servlet/Spring 自动集成；
请求跨线程 fan-out 时应显式传递 `AbtestContext`。纯 JDK web helper 只适用于其文档标明的
thread-per-request 范围。完整 Maven、地址和 API 说明见
[Java README](../sdk/java/README.md)。

## 8. API 语义

| 能力 | Go | Python | Java |
|---|---|---|---|
| 单 key 动态解析 | `GetConfig` | `get_config` | `getConfig` |
| 整个 namespace 动态解析 | `GetAllConfigs` | `get_all_configs` | `getAllConfigs` |
| 纯缓存 full-release 读 | `GetConfigStatic` | `get_config_static` | `getConfigStatic` |
| 原始实验结果 | `GetExperimentResult` | `get_experiment_result` | `getExperimentResult` |
| 默认 namespace 变体 | `*Default` | `*_default` | `*Default` |

### 动态配置优先级

`GetConfig` 系列的顺序为实验/灰度命中、full release、调用方 default。get-all 使用相同
解析，但没有逐 key default；无命中且无 full release 的 key 不返回，空字符串仍是合法值。
三语言返回新 map，调用方可以修改。

解析出的 namespace 必须属于初始化订阅集合。未提供显式或默认 namespace 时返回
`NamespaceRequired` 类错误；解析到未订阅 namespace 时返回 `NamespaceNotSubscribed`
类错误。已订阅但尚无快照时 get-all 返回空 map。

### AbtestContext、prefetch 与 trace_id

每个入站请求或异步任务创建一个上下文，并在整个调用链复用。并发首次读取会合并为同一
namespace 的一次实验拉取。显式 prefetch 非阻塞且幂等，适合与其他 I/O 重叠；不要在
所有网络请求入口无条件预热。

`trace_id` 是调用方可选的关联标识。未提供时 SDK 生成 UUID；同一个上下文的实验请求共享
该值。中间件优先复用 `X-Trace-Id`，其次 `X-Request-Id`。它只用于关联日志/请求，不改变
分桶或配置解析。

### 原始实验结果

`GetExperimentResult` 系列直接返回 proto 响应，不读取本地配置缓存，不合成配置 value，
也不自动上报曝光。重要字段：

- `config_flat_kv`: config key 名到 `config_version.id`；
- `custom_flat_kv`: arbitrary JSON 参数；
- `groups[].params_versions`: 每个命中实验组的 key 到 version id；
- `gray_hits[].key_versions`: 按 gray release 分组的 key 到 version id；
- `exposures`: 仅为 wire compatibility 保留，新服务端不填充。

所有 wire 上的 int64 version 都是全局唯一的 `config_version.id`，不是每个 key 内的语义
`version_no`。

### int64 与 JSON

JavaScript/JSON 消费者不能安全地把任意 int64 当作 Number。管理接口、自动化脚本或其他
非 protojson 客户端处理 ID/version 时应使用字符串或 bigint-safe parser，避免超过
`2^53-1` 后精度丢失。

## 9. 当前实现中待人工核实的偏差

### Python TokenProvider 不能持续轮换

- **设计预期**：三语言的 TokenProvider 都能为短 TTL token 提供动态轮换能力。
- **代码事实**：Go 的 provider 在每个 RPC 获取 token，Java 的 provider 在每个 gRPC/HTTP
  请求获取 token；Python 仅在初始化时调用 async provider 一次，之后 interceptor 与 HTTP
  transport 读取私有 `_TokenCache` 的缓存值，client 没有公开 refresh API 或后台刷新任务。
- **影响**：Python 长生命周期进程若只配置短 TTL `token_provider`，初始化时取得的 token
  过期后不能通过 provider 自动更新；当前应由宿主在 token 有效期内重建 client，或使用能
  覆盖进程寿命的部署方 token。
- **证据**：[`client.py` 的 `_TokenCache` 与初始化路径](../sdk/python/tipsy_ab_config/client.py)、
  [`Go tokenSource`](../sdk/go/tipsyabconfig/sdk.go)、
  [`Java TokenSource`](../sdk/java/tipsy-abconfig/src/main/java/io/github/lightspeedintelligence/abconfig/TokenSource.java)。

该差异没有明确的语言能力或兼容原因，需要人工确认应补 Python 轮换实现，还是把 Python
`token_provider` 的产品契约收窄为“异步初始化取 token”。本次仅记录事实，不修改实现。

## 10. 运行与可观测性

- client 是进程级对象，可并发复用；进程退出时调用 `Close` / `aclose` / `close`。
- Go/Java 的 `Health`/`health` 暴露最近 PullAll、Subscribe 和启动空缓存状态。HTTP 模式的
  SubscribeConnected 始终为 false，LastSubscribeErr 为空，这是 transport 语义，不是故障。
  Python 当前以日志与 `Metrics` 暴露后台状态，没有同名 health snapshot。
- Go/Java 的 background error callback 在 SDK 后台执行路径上同步调用，应保持轻量、非阻塞；
  Python 当前没有对应 callback 配置项。
- `StartupFailOpen=false` 是默认值。只有宿主明确能承受空缓存时才开启 fail-open，并对
  `StartupCacheEmpty` 和后台错误告警。

## 11. 排障

| 现象 | 检查项 |
|---|---|
| 初始化参数错误 | namespaces、ConfigService 地址、secretKey/Token/TokenProvider、transport 与 URL scheme |
| 启动 PullAll 失败 | service token 签名/有效期/namespace 权限、DNS/TLS、服务端可达性 |
| NamespaceRequired | 未传 ns，且未配置 `PROJECT_DEFAULT_NAMESPACE` 或语言对应 override |
| NamespaceNotSubscribed | ns 不在初始化订阅列表 |
| 动态配置总是 default | 快照是否含 key；full release/实验状态；user id/attrs 与 admission；版本是否已进入快照 |
| gRPC Subscribe 不连接 | token、DNS、TLS/SNI、L7 proxy 是否支持长流；同时查看周期 PullAll 是否健康 |
| HTTP 变更不立即生效 | HTTP 不 Subscribe；检查 PullInterval 和周期 PullAll 健康状态 |
| Python import 报 grpc 版本错误 | 安装 `grpcio>=1.66.2,<2` 并更新旧 lockfile |
| 401/403 | 是否误用了 Console 会话 token；JWT namespace 是否包含目标 ns；token 是否过期；secretKey 模式：值是否与平台 `TIPSY_SERVICE_SECRET` 一致、平台是否已升级支持 `SecretKey` scheme（部署顺序，§3.1） |
| 值对应到错误版本 | wire 值是 `config_version.id`，不要当作 `version_no` |

DEV 环境联调见[无凭证联调模板](dev-http-token.md)。远端 endpoint 和 token 均由该环境部署方
提供；本文不声明任何远端环境当前在线。
