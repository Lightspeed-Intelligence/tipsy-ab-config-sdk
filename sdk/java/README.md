# Tipsy AB-config Java SDK

Tipsy 配置中心 / A/B 实验平台对外公开 SDK 的 **Java** 实现，与 Go / Python SDK
**全量对齐**。语言基线 **Java 21**，Maven groupId `io.tipsy`。

SDK 是纯 gRPC / HTTP 下游客户端：进程内维护配置缓存（启动 `PullAll` + 长连
`Subscribe` 推送 + 周期兜底轮询），并通过 `AbtestService.GetExperimentResult`
解析实验命中。所有流量用 JWT Bearer 鉴权。**SDK 从不直连数据库，也不在客户端做分桶**
（哈希 / 分桶是服务端 `AbtestService` 的职责，SDK 只读结果）。

## 模块结构

`sdk/java` 是一个 Maven 多模块 reactor：

| 模块 | artifact | 说明 |
|---|---|---|
| `tipsy-abconfig-proto` | `io.tipsy:tipsy-abconfig-proto` | 由 `protobuf-maven-plugin` 从 `api/proto` 生成的 protobuf message + gRPC stub（构建期生成，不入库）。 |
| `tipsy-auth` | `io.tipsy:tipsy-auth` | HS256 JWT 签名小工具，完全独立（仅依赖 jjwt），不依赖主 SDK / proto / gRPC。 |
| `tipsy-abconfig` | `io.tipsy:tipsy-abconfig` | 主 SDK：配置缓存、gRPC / HTTP 传输、abtest 解析、公共客户端句柄，含可选的 `io.tipsy.abconfig.web` web 集成子包。 |
| `example` | （不发布） | 基于 JDK 内置 `com.sun.net.httpserver` 的可运行示例。 |

包根：主 SDK `io.tipsy.abconfig`（web 子包 `io.tipsy.abconfig.web`）、签名 `io.tipsy.auth`。

## 安装

### 本地构建 / 安装到 `.m2`

```bash
cd sdk/java
mvn -q -DskipTests install
```

完成后即可在下游 Maven 项目中引用：

```xml
<dependency>
  <groupId>io.tipsy</groupId>
  <artifactId>tipsy-abconfig</artifactId>
  <version>0.1.0</version>
</dependency>
<!-- 可选：需要本地签发服务 token 时 -->
<dependency>
  <groupId>io.tipsy</groupId>
  <artifactId>tipsy-auth</artifactId>
  <version>0.1.0</version>
</dependency>
```

> `tipsy-abconfig` 已 compile-scope 依赖 `tipsy-auth` 与 `tipsy-abconfig-proto`，
> 引入主 SDK 即可传递获得；显式声明 `tipsy-auth` 仅在你单独使用签名工具时需要。

> **Maven Central**：本任务只产出可构建、可测试、可本地 `mvn install` 的工程；
> 正式发布到 Maven Central 的流程后续文档化（占位）。

## 快速开始

```java
import io.tipsy.abconfig.AbtestContext;
import io.tipsy.abconfig.Config;
import io.tipsy.abconfig.TipsyAbConfigClient;
import java.util.List;
import java.util.Map;

// create() 会跑启动 PullAll、起后台循环；try-with-resources 在退出时 close()。
try (TipsyAbConfigClient client = TipsyAbConfigClient.create(Config.builder()
        .namespaces(List.of("tipsy-chat"))
        .configServiceAddr("grpcs://config.example.com:443")
        .abtestServiceAddr("grpcs://abtest.example.com:443")
        .token(System.getenv("TIPSY_TOKEN"))
        .build())) {

    // 每个请求构造一个 AbtestContext（uid + 属性），并显式传给每次 getConfig。
    AbtestContext abctx = client.newAbtestContext("user-123", Map.of("country", "JP"));

    // 动态解析：abtest 命中（白名单 > 实验）> 全量发布 > 默认值。
    String threshold = client.getConfig(abctx, "tipsy-chat", "rerank.threshold", "0.5");

    // 纯缓存静态读：getConfigStatic 返回 Optional（空串是合法值）。
    String staticVal = client.getConfigStatic("tipsy-chat", "rerank.threshold").orElse("0.5");

    System.out.println(threshold + " / " + staticVal);
}
```

完整可运行示例参见 [`example/src/main/java/io/tipsy/abconfig/example/Main.java`](example/src/main/java/io/tipsy/abconfig/example/Main.java)：
基于 JDK 内置 HTTP server，演示 `/static`（`getConfigStatic`）与 `/user`
（web helper 构造上下文 + `getConfig`），以及用 `tipsy-auth` 本地签发 token。运行：

```bash
TIPSY_TOKEN=... CONFIG_ADDR=grpcs://config.example.com:443 \
  ABTEST_ADDR=grpcs://abtest.example.com:443 NAMESPACES=tipsy-chat \
  mvn -q -pl example exec:java
```

## 配置解析 API

| 方法 | 说明 |
|---|---|
| `getConfigStatic(ns, key) → Optional<String>` | 纯全量缓存读，**不**做 ns 解析、不抛 ns 异常；空串是合法命中值，未命中返回 `Optional.empty()`。 |
| `getConfig(abctx, ns, key, default)` | 按用户解析动态配置；优先级 abtest 命中 > 全量 > 默认；单 ns abtest 失败静默降级到全量。 |
| `getConfigDefault(abctx, key, default)` | `getConfig` 的 ns-可省形式（ns 取项目默认）。 |
| `getExperimentResult(ExperimentResultRequest)` | 直通 `AbtestService.GetExperimentResult`，返回原始 proto 响应（读 `config_flat_kv` / `custom_flat_kv` / `groups` / `gray_hits`）。 |

`AbtestContext` 工厂：`newAbtestContext(uid, attrs)` / `(…, traceId)` /
`newAbtestContextForNamespace(ns, uid, attrs[, traceId])` / `emptyAbtestContext()`
（无用户身份的路径，永不发 RPC）。读访问器 `userId()` / `userInfo()` / `traceId()`。

**命名空间解析**：显式 ns > 项目默认 ns（`Config.defaultNamespace` 覆盖环境变量
`PROJECT_DEFAULT_NAMESPACE`）> `NamespaceRequiredException`；解析出的 ns 未订阅 →
`NamespaceNotSubscribedException`。

## 两种传输

`Config.transport`（`Transport.GRPC` / `Transport.HTTP`）选择传输；为 `null` 时
依次回退到环境变量 `TIPSY_SDK_TRANSPORT`、再到 gRPC 默认。

- **gRPC（默认）**：`PullAll` / `Subscribe`（服务端流）/ `GetExperimentResult`；
  keepalive 30s/5s/permit-without-stream；per-RPC Bearer 凭证；512MB 收发上限
  （channel `maxInboundMessageSize` + per-stub `withMaxOutboundMessageSize`）。
- **HTTP**：protojson over POST（`/api/v1/config/pull_all`、
  `/api/v1/abtest/experiment_result`），**仅轮询、无 Subscribe**；配置变更传播
  延迟受 `pullInterval` 约束。

### 地址 scheme（方案 Y）

`ConfigServiceAddr` / `AbtestServiceAddr` 在 gRPC 模式按方案 Y 解析：

| 形式 | 行为 |
|---|---|
| `host:port`、`grpc://host:port` | 明文 h2c |
| `grpcs://host:port[?authority=&insecure=]` | TLS；可选自定义 authority；`insecure=true/1` 关闭证书校验（仅 Dev / 直连 IP，生产禁用，会 WARN） |
| `dns:///service.ns.svc.cluster.local:port` | DNS name resolver，**自动开启客户端 `round_robin`**（K8s Headless Service 场景） |
| `passthrough:///`、`unix:`、`xds:///` | 原生透传 |
| `http(s)://…` | 在 gRPC 模式下报参数错误（请改用 HTTP 传输） |

HTTP 模式下地址按 `http(s)://` base URL 解释（非 `http(s)` 报参数错误）。

### 常用 Config 旋钮与环境变量

| Config | 默认 | 说明 |
|---|---|---|
| `namespaces` | 必填 | 订阅的业务命名空间 |
| `configServiceAddr` | 必填 | ConfigService 地址（gRPC target 或 HTTP base URL） |
| `abtestServiceAddr` | 可空 | AbtestService 地址；空 → 降级（不发实验 RPC，全走全量） |
| `token` / `tokenProvider` | 至少一项 | 静态 Bearer token / 动态 token 供应（provider 优先，逐 RPC 取） |
| `pullInterval` / `pullTimeout` / `pullRetries` | 10s / 5s / 3 | 兜底轮询周期 / 单 ns 拉取超时 / 启动重试次数 |
| `abtestTimeout` | 1500ms | 单次 `GetExperimentResult` 超时 |
| `startupFailOpen` | false | 启动 PullAll 失败是否吞掉（空缓存继续）而非抛 `StartupPullFailedException` |
| `maxRecvMessageSize` / `maxSendMessageSize` | 512MB / 512MB | gRPC 收 / 发上限 |
| `defaultNamespace` | "" → env | 项目默认 ns；空则取 `PROJECT_DEFAULT_NAMESPACE` |
| `transport` | null → env → grpc | 传输选择；空则取 `TIPSY_SDK_TRANSPORT` |
| `channelConfigurator` | null | `UnaryOperator<ManagedChannelBuilder<?>>` 注入缝（替代 Go 的 `DialOptions`） |
| `httpClient` | null | 注入 HTTP 模式的 `java.net.http.HttpClient`（不传则 SDK 自建并负责关闭） |
| `onBackgroundError` | null | 后台错误回调，phase 为 `startup_pull` / `periodic_pull` / `subscribe`，同步、recover 包裹 |

可观测：`client.health()`（`Health` 快照）、`client.metrics()`（`Metrics` 计数器）。

## Web 集成（框架无关）

基于消费方调研（pine-java：非 Spring、无 servlet、无 gRPC server，运行于 JDK 内置
`com.sun.net.httpserver`，`user_id` 在 JSON body，单请求跨虚拟线程 fan-out），
Java SDK 的 web 集成是**框架无关的显式上下文对象**：

1. **首选、唯一保证正确**：每个请求构造一个 `AbtestContext`（从你自己的请求体 / header
   取 `uid` 与属性），并**显式作为参数**传给每次 `getConfig`。这是跨虚拟线程 fan-out
   安全的唯一方式。

2. **可选便捷件**（`io.tipsy.abconfig.web` 子包，纯 JDK、零额外依赖）：
   - `AbtestContextHolder`：`ThreadLocal` 持有器（`set` / `get` / `clear` /
     `runWith`）。**警示：不会跨 `newVirtualThreadPerTaskExecutor()` 等 fan-out 传播**，
     仅适用 thread-per-request 边缘；fan-out 场景必须显式传参。
   - `HttpServerSupport`：针对 JDK 内置 `com.sun.net.httpserver` 的薄 helper —
     `extractTraceId(HttpExchange)`（`X-Trace-Id` → `X-Request-Id` → 新 UUID）、
     `AbtestUserProvider` 函数式接口（返回 `UserInfo`，用 `UserInfo.of(uid, attrs)`
     构造）、`wrap(client, provider, next)` 适配器（在边缘构造上下文并经
     `AbtestContextHolder` 暴露给 `next`，异常 / 空 provider 降级为空上下文 + WARN）。
     `com.sun.*` 为 JDK 自带内部 API（pine 同源），不引入任何外部 web 依赖；同样带
     fan-out 警示。

> 推荐 fan-out-safe 写法：在 handler 顶部 `AbtestContextHolder.get()` 读出一次
> `AbtestContext`，随后**显式**传给所有下游调用，不再依赖 ThreadLocal。

**不提供**：servlet `Filter`、Spring 自动配置、gRPC `ServerInterceptor`。

## 鉴权工具 `tipsy-auth`

HS256 JWT 签名，claims `{roles, namespaces, sub, iat, exp}`，与服务端验签契约一致
（仅签名，不实现验证）：

```java
import io.tipsy.auth.IssueOptions;
import io.tipsy.auth.JwtSigner;
import java.time.Duration;
import java.util.List;

JwtSigner signer = JwtSigner.create(System.getenv("TIPSY_SERVICE_SECRET"));
String token = signer.issue(IssueOptions.builder()
        .subject("my-service")
        .roles(List.of("business_sdk"))
        .namespaces(List.of("*"))
        .ttl(Duration.ofHours(2))
        .build());
```

`iat` / `exp` 为 unix 秒，无 `nbf` / `iss` / `aud`；`roles` / `namespaces` 即使为空
也输出 JSON 空数组 `[]`。接受任意长度 HMAC 密钥（与 Go / golang-jwt 一致）。

## 与 Go / Python SDK 的有意对外差异

均为语言映射 / 取舍，核心能力等价保留（详见设计 01 差异表）：

1. **上下文携带**：Go 用 `context.Context` 隐式携带（`WithAbtestContext` /
   `AbtestContextFromContext`）。Java **不提供**等价 API，改为**显式传参 `AbtestContext`**
   （首选）+ 可选 `AbtestContextHolder`（带 fan-out 警示）。理由：消费方单请求跨虚拟线程
   fan-out，隐式携带不安全。
2. **`getConfigStatic` 签名**：Go `(ns,key,default)→(string,bool)` → Java
   `(ns,key)→Optional<String>`（有意 Optional 化，杜绝空串误判；用 `.orElse(default)`）。
3. **`waitForAbtest`**：Go 导出该低层入口；Java **不在公共 API 暴露**（其触发 lazy
   记忆化的价值已被 `getConfig` 覆盖）。
4. **整体 startup deadline**：Go `Init(ctx, cfg)` 经 ctx 提供整体墙钟上界；Java
   `create(Config)` 不引入 ctx，单 ns 单次仍受 `pullTimeout` / `pullRetries` 约束。
5. **Logger**：Go `Config.Logger (*slog.Logger)` → Java 用 SLF4J 门面，不进 `Config`
   （宿主自选后端）。
6. **`DialOptions`**：Go `[]grpc.DialOption` → Java
   `channelConfigurator(UnaryOperator<ManagedChannelBuilder<?>>)` 注入缝。
7. **web 集成**：**不提供** servlet filter / Spring 自动配置 / gRPC ServerInterceptor。

## 开发

```bash
cd sdk/java
mvn -q -DskipTests package   # proto 生成 + 编译 + 打包
mvn -q test                  # 全部单元 / 集成测试
mvn -q -DskipTests install   # 安装到本地 .m2
```

> proto 由 `protobuf-maven-plugin` 在构建期从 `api/proto` 生成（自动下载 protoc 与
> grpc-java 插件二进制），生成代码不入库。

## 许可证

[MIT](../../LICENSE)
