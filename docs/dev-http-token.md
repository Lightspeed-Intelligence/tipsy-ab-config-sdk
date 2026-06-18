# DEV 临时接入信息（HTTP + gRPC）

本文记录当前 DEV 环境的临时 HTTP 与 gRPC 接入信息，供本地开发和联调使用。

> Go SDK 现为独立 Go module（`go 1.25.0`），与主仓 `go 1.25.7` 解耦。Module 路径、`go get` / tag 规则详见 [usage-and-integration.md](usage-and-integration.md#41-go-sdk-接入)。

> 最近一次验证：2026-06-16，HTTP 与 gRPCs 均已打通，下方命令使用当前 DEV 临时 token 实测通过。

## HTTP Base URL

```text
https://dev-ab-config.infra.fantacy.live
```

走 CloudFlare，证书可正常校验（无需 `-k`）。已验证：

- `GET /healthz` 返回 `200 {"status":"ok"}`。
- `POST /api/v1/config/static` 返回 `200`。
- `POST /api/v1/config/dynamic` 返回 `200`。
- `POST /api/v1/abtest/experiment_result` 返回 `200`。

## gRPC 接入

当前 DEV 已为 gRPC 配置独立 DNS 记录，走 Cloudflare 橙云普通代理，不再命中
`*.infra.fantacy.live` 的 Cloudflare Tunnel wildcard。SDK 和 grpcurl 应直接使用
gRPC 专用域名：

```text
dev-ab-config-grpc.infra.fantacy.live:443
```

注意事项：

- 该地址是 gRPC target，不带 `https://` 前缀。
- 当前链路为 `client -> Cloudflare Proxied -> origin 443 -> Traefik -> app:50051`。
- Cloudflare zone 必须保持 `Network -> gRPC = On`。
- Traefik gRPC upstream 必须保持 `server.scheme=h2c`，且 gRPC router 不要挂 gzip middleware。
- gRPC 反射已开启，可直接 `list` / `describe`。
- 鉴权 header 为 `authorization: Bearer <token>`，与 HTTP 同一个 token。

已验证：

- 反射 `list` 返回全部 service（ConfigService / AbtestService / AuditService 等）。
- `ConfigService/GetStaticConfig` 返回 `{}`。
- `AbtestService/GetExperimentResult` 正常返回 `computedAt`。

### SDK 怎么填这条 gRPC（方案 Y 地址语法）

当前 DEV 推荐使用标准 TLS 域名地址：

```text
grpcs://dev-ab-config-grpc.infra.fantacy.live:443
```

- **Go SDK**：填入 `Config.ConfigServiceAddr` / `AbtestServiceAddr`。
- **Python SDK**：填入 `config_service_addr` / `abtest_service_addr`。

如果 SDK 兼容裸 gRPC target，也可以使用：

```text
dev-ab-config-grpc.infra.fantacy.live:443
```

不要再使用早期临时方案：

```text
grpcs://47.253.175.59:443?authority=dev-ab-config-grpc.infra.fantacy.live&insecure=true
```

该 IP 直连方案只用于 Cloudflare Tunnel 排查期间；当前独立橙云 DNS 已验证可用。

## 临时 Service Token

```text
eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJyb2xlcyI6WyJidXNpbmVzc19zZGsiXSwibmFtZXNwYWNlcyI6WyIqIl0sInN1YiI6InlvdXItc2VydmljZS1uYW1lIiwiZXhwIjoxNzgxOTg5MjE3LCJpYXQiOjE3ODEyNTQ4MTd9.P0QEZGRh2msXsEnShXGH3nbNbC-R-fpDP7XkJUkL4fM
```

说明：

- 这是 DEV 环境临时 token。
- `roles=["business_sdk"]`，`namespaces=["*"]`，可访问所有 namespace。
- `sub="your-service-name"`，当前只作为服务标识，不参与 namespace 权限校验。
- `exp=1781989217`，即 **2026-06-21 05:00 CST 过期**（`iat=1781254817`）。
- HTTP 与 gRPC 共用这一个 token。

## 验证命令

```sh
export AB_CONFIG_HTTP_BASE='https://dev-ab-config.infra.fantacy.live'
export AB_CONFIG_GRPC_ADDR='dev-ab-config-grpc.infra.fantacy.live:443'
export AB_CONFIG_TOKEN='eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJyb2xlcyI6WyJidXNpbmVzc19zZGsiXSwibmFtZXNwYWNlcyI6WyIqIl0sInN1YiI6InlvdXItc2VydmljZS1uYW1lIiwiZXhwIjoxNzgxOTg5MjE3LCJpYXQiOjE3ODEyNTQ4MTd9.P0QEZGRh2msXsEnShXGH3nbNbC-R-fpDP7XkJUkL4fM'
```

Health check:

```sh
curl -sS -i "$AB_CONFIG_HTTP_BASE/healthz"
```

Static config:

```sh
curl -sS -i \
  -H "Authorization: Bearer $AB_CONFIG_TOKEN" \
  -H 'Content-Type: application/json' \
  "$AB_CONFIG_HTTP_BASE/api/v1/config/static" \
  -d '{"namespace":"tipsy-chat","keys":[]}'
```

Dynamic config:

```sh
curl -sS -i \
  -H "Authorization: Bearer $AB_CONFIG_TOKEN" \
  -H 'Content-Type: application/json' \
  "$AB_CONFIG_HTTP_BASE/api/v1/config/dynamic" \
  -d '{"namespace":"tipsy-chat","userId":"test-user","keys":[]}'
```

Experiment result:

```sh
curl -sS -i \
  -H "Authorization: Bearer $AB_CONFIG_TOKEN" \
  -H 'Content-Type: application/json' \
  "$AB_CONFIG_HTTP_BASE/api/v1/abtest/experiment_result" \
  -d '{"namespace":"tipsy-chat","userId":"test-user","experimentType":"EXPERIMENT_TYPE_CONFIG_VERSION","displayType":"RESULT_DISPLAY_TYPE_FLAT_KV"}'
```

### gRPC 验证命令

需要 `grpcurl`（`go install github.com/fullstorydev/grpcurl/cmd/grpcurl@latest`，
国内可加 `GOPROXY=https://goproxy.cn,direct`）。下列命令直接使用 gRPC 专用域名。

公共参数：

```sh
GRPC_FLAGS=(-H "authorization: Bearer $AB_CONFIG_TOKEN")
```

List services（反射）:

```sh
grpcurl "${GRPC_FLAGS[@]}" "$AB_CONFIG_GRPC_ADDR" list
```

ListNamespacesByKind:

```sh
grpcurl "${GRPC_FLAGS[@]}" \
  -d '{"kind":"NAMESPACE_KIND_BUSINESS"}' \
  "$AB_CONFIG_GRPC_ADDR" tipsy.config.v1.ConfigService/ListNamespacesByKind
```

GetStaticConfig / GetDynamicConfig / PullAll:

```sh
grpcurl "${GRPC_FLAGS[@]}" \
  -d '{"namespace":"tipsy-chat","keys":[],"trace_id":"demo-trace-id"}' \
  "$AB_CONFIG_GRPC_ADDR" tipsy.config.v1.ConfigService/GetStaticConfig

grpcurl "${GRPC_FLAGS[@]}" \
  -d '{"namespace":"tipsy-chat","userId":"test-user","keys":[],"trace_id":"demo-trace-id"}' \
  "$AB_CONFIG_GRPC_ADDR" tipsy.config.v1.ConfigService/GetDynamicConfig

grpcurl "${GRPC_FLAGS[@]}" \
  -d '{"namespaces":["tipsy-chat"],"trace_id":"demo-trace-id"}' \
  "$AB_CONFIG_GRPC_ADDR" tipsy.config.v1.ConfigService/PullAll
```

GetExperimentResult:

```sh
grpcurl "${GRPC_FLAGS[@]}" \
  -d '{"namespace":"tipsy-chat","userId":"test-user","experimentType":"EXPERIMENT_TYPE_CONFIG_VERSION","displayType":"RESULT_DISPLAY_TYPE_FLAT_KV","trace_id":"demo-trace-id"}' \
  "$AB_CONFIG_GRPC_ADDR" tipsy.abtest.v1.AbtestService/GetExperimentResult
```

> `trace_id` 字段为可选：留空或省略时由服务端自动填充 UUID v4；外部传入任意非空字符串原样保留（不做格式校验、不重写）；最大 128 字符，超出会被服务端截断并打 WARN。
