# DEV 环境联调模板

本文提供不含固定 endpoint 和凭证的 HTTP/gRPC 联调步骤。开始前从目标环境部署方取得：

- HTTP base URL（若环境开放 HTTP public-read）；
- gRPC endpoint（若环境开放 gRPC）；
- 有效的 HS256 service token，且其 `namespaces` claim 包含测试 namespace；
- TLS、私有 CA、SNI/authority、代理和 gRPC reflection 等环境特有要求。

仓库不保存远端 token，也不声明某个 DEV 地址当前在线。不要把导出的 token、命令历史或
验证响应提交到 Git。

## 1. 环境变量

```bash
export AB_CONFIG_HTTP_BASE='https://<http-host>'
export AB_CONFIG_GRPC_ADDR='<grpc-host>:443'
export AB_CONFIG_GRPC_TARGET='grpcs://<grpc-host>:443'
export AB_CONFIG_TOKEN='<service-jwt-from-deployer>'
export AB_CONFIG_NAMESPACE='<authorized-namespace>'
```

若 gRPC endpoint 是内网明文地址，target 可写 `host:port` 或 `grpc://host:port`；若部署方
提供 Headless Service，通常写
`dns:///<service>.<namespace>.svc.cluster.local:<port>`。TLS endpoint 使用 `grpcs://`。

## 2. HTTP 验证

健康检查通常不需要 service token，但是否暴露由部署方决定：

```bash
curl --fail-with-body --show-error \
  "$AB_CONFIG_HTTP_BASE/healthz"
```

SDK HTTP transport 实际依赖以下两个 protojson 路径，可用它们做最小联调：

```bash
curl --fail-with-body --show-error \
  -H "Authorization: Bearer $AB_CONFIG_TOKEN" \
  -H 'Content-Type: application/json' \
  "$AB_CONFIG_HTTP_BASE/api/v1/config/pull_all" \
  -d "{\"namespaces\":[\"$AB_CONFIG_NAMESPACE\"],\"traceId\":\"dev-pull-all\"}"

curl --fail-with-body --show-error \
  -H "Authorization: Bearer $AB_CONFIG_TOKEN" \
  -H 'Content-Type: application/json' \
  "$AB_CONFIG_HTTP_BASE/api/v1/abtest/experiment_result" \
  -d "{\"namespace\":\"$AB_CONFIG_NAMESPACE\",\"userId\":\"dev-user\",\"experimentType\":\"EXPERIMENT_TYPE_CONFIG_VERSION\",\"displayType\":\"RESULT_DISPLAY_TYPE_FLAT_KV\",\"traceId\":\"dev-experiment-result\"}"
```

服务端还可能开放 `/api/v1/config/static` 与 `/api/v1/config/dynamic` 供无本地缓存客户端
使用；它们不是 SDK HTTP transport 的 PullAll 实现。具体开放范围以目标部署为准。

## 3. gRPC 验证

以下命令需要 `grpcurl`，并假定 endpoint 使用系统信任的 TLS 证书且开启 reflection：

```bash
grpcurl \
  -H "authorization: Bearer $AB_CONFIG_TOKEN" \
  "$AB_CONFIG_GRPC_ADDR" list

grpcurl \
  -H "authorization: Bearer $AB_CONFIG_TOKEN" \
  -d "{\"namespaces\":[\"$AB_CONFIG_NAMESPACE\"],\"traceId\":\"dev-pull-all\"}" \
  "$AB_CONFIG_GRPC_ADDR" tipsy.config.v1.ConfigService/PullAll

grpcurl \
  -H "authorization: Bearer $AB_CONFIG_TOKEN" \
  -d "{\"namespace\":\"$AB_CONFIG_NAMESPACE\",\"userId\":\"dev-user\",\"experimentType\":\"EXPERIMENT_TYPE_CONFIG_VERSION\",\"displayType\":\"RESULT_DISPLAY_TYPE_FLAT_KV\",\"traceId\":\"dev-experiment-result\"}" \
  "$AB_CONFIG_GRPC_ADDR" tipsy.abtest.v1.AbtestService/GetExperimentResult
```

reflection 未开放时，使用本仓 [`api/proto`](../api/proto) 的 proto 文件或部署方提供的
descriptor。私有 CA、明文 h2c 或 authority override 应按部署方要求设置 grpcurl 参数；不要
用跳过 TLS 校验作为长期配置。

## 4. SDK 配置

三语言均把同一 endpoint 分别传给 ConfigService 和 AbtestService（部署拆分两者时使用各自
地址），并把 token 作为静态值或动态 provider 注入。

Go：

```go
client, err := tipsyabconfig.Init(ctx, tipsyabconfig.Config{
	Namespaces:        []string{os.Getenv("AB_CONFIG_NAMESPACE")},
	ConfigServiceAddr: os.Getenv("AB_CONFIG_GRPC_TARGET"),
	AbtestServiceAddr: os.Getenv("AB_CONFIG_GRPC_TARGET"),
	Token:             os.Getenv("AB_CONFIG_TOKEN"),
})
```

Python：

```python
client = await init(Config(
    namespaces=[os.environ["AB_CONFIG_NAMESPACE"]],
    config_service_addr=os.environ["AB_CONFIG_GRPC_TARGET"],
    abtest_service_addr=os.environ["AB_CONFIG_GRPC_TARGET"],
    token=os.environ["AB_CONFIG_TOKEN"],
))
```

Java：

```java
TipsyAbConfigClient client = TipsyAbConfigClient.create(Config.builder()
        .namespaces(System.getenv("AB_CONFIG_NAMESPACE"))
        .configServiceAddr(System.getenv("AB_CONFIG_GRPC_TARGET"))
        .abtestServiceAddr(System.getenv("AB_CONFIG_GRPC_TARGET"))
        .token(System.getenv("AB_CONFIG_TOKEN"))
        .build());
```

HTTP transport 时将两个 service address 改为 `AB_CONFIG_HTTP_BASE`，并显式选择 HTTP；
Python 还需安装 `http` extra。地址语法、Subscribe 差异和 TokenProvider 见
[集成手册](usage-and-integration.md)。

## 5. 失败定位

| 现象 | 核对 |
|---|---|
| 401/Unauthenticated | token 是否过期、是否误用 Console token、Authorization header 是否为 Bearer |
| PermissionDenied/403 | token 的 namespaces 是否包含测试 namespace |
| TLS/证书错误 | endpoint、系统/私有 CA、SNI/authority 是否与部署方配置一致 |
| gRPC `list` 失败 | reflection 是否开放；不要据此直接判断业务 RPC 不可用 |
| PullAll 返回空快照或缺 key | namespace 拼写、发布状态和 token 授权；再向平台方确认环境数据 |
| HTTP 可用但 SDK 不推送 | HTTP transport 本来不 Subscribe，等待 PullInterval 或改用 gRPC |

联调结束后从 shell 和 secret store 的临时作用域清除 token。若 token 曾出现在日志、聊天或
Git 变更中，应按平台流程立即轮换，而不是仅删除文本。
