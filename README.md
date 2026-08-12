# Tipsy AB-config SDK

Tipsy 配置中心与 A/B 实验平台的公开 SDK monorepo。业务服务通过 SDK 拉取 namespace
快照，在进程内缓存配置，并按用户解析实验、灰度和全量发布结果；SDK 不直接访问平台数据库。

## 模块

| 模块 | 路径 | 发布标识 |
|---|---|---|
| Go SDK | [`sdk/go/tipsyabconfig`](sdk/go/tipsyabconfig) | `sdk/go/tipsyabconfig/vX.Y.Z` |
| Go token 签发工具 | [`sdk/go/tipsyauth`](sdk/go/tipsyauth) | `sdk/go/tipsyauth/vX.Y.Z` |
| Python SDK | [`sdk/python`](sdk/python) | `python-sdk/vX.Y.Z` |
| Java SDK | [`sdk/java`](sdk/java) | `java-sdk/vX.Y.Z` |
| Go protobuf 生成代码 | [`api/gen/go`](api/gen/go) | `api/gen/go/vX.Y.Z` |
| 共享 proto 源 | [`api/proto`](api/proto) | 随对应生成代码发布 |

各模块独立发布。不要根据仓库根目录提交号推断 SDK 版本；请查看相应模块的
`CHANGELOG.md`、release/tag 或包仓库元数据。当前仓库元数据记录的版本为 Go
`v0.13.1`、Python `v0.14.1`、Java `v0.10.0`、Go proto `v0.7.0`。

## 选择接入方式

Go、Python、Java SDK 当前具备同一组核心能力：

- gRPC 与 HTTP transport；gRPC 支持 `PullAll`、`Subscribe` 和周期性
  `PullAll`，HTTP 只做周期性 `PullAll`，不建立 Subscribe 流；
- 静态 bearer token 与 `TokenProvider` 接入点；各语言的轮换时机见集成手册；
- 多 namespace 进程内快照缓存；
- 按用户解析单个 key 或整个 namespace；
- `has_dynamic_resolution=false` 的纯全量快路径。字段缺失时安全回退到正常
  A/B 解析路径。

生产环境优先使用 gRPC。Kubernetes 中若需要 SDK 直接发现并轮询多个后端 pod，使用
Headless Service 的 `dns:///<service>.<namespace>.svc.cluster.local:<port>` target；
三语言 SDK 都会为 `dns:///` 自动选择 `round_robin`。使用 HTTP 时，配置变更的可见延迟
受 `PullInterval`（默认 10 秒）约束。

## 安装

Go：

```bash
go get github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk/sdk/go/tipsyabconfig@latest
```

Python 和 Java 的安装源、版本发现与依赖要求分别见：

- [Python SDK README](sdk/python/README.md)
- [Java SDK README](sdk/java/README.md)

## Go 快速开始

```go
ctx := context.Background()
client, err := tipsyabconfig.Init(ctx, tipsyabconfig.Config{
	Namespaces:        []string{"my-app"},
	ConfigServiceAddr: "dns:///ab-config-headless.platform.svc.cluster.local:50051",
	AbtestServiceAddr: "dns:///ab-config-headless.platform.svc.cluster.local:50051",
	Token:             os.Getenv("TIPSY_TOKEN"),
})
if err != nil {
	log.Fatal(err)
}
defer client.Close()

// 每个业务请求创建并复用一个 AbtestContext。
abctx := client.NewAbtestContext(ctx, "user-123", map[string]any{"country": "JP"})
value, err := client.GetConfig(ctx, abctx, "my-app", "feature_x", "off")
if err != nil {
	log.Fatal(err)
}
```

完整 Go 示例见 [`sdk/go/example/main.go`](sdk/go/example/main.go)。Python/Java 示例见各自
README 和 `example/` 目录。

## 文档

- [集成手册](docs/usage-and-integration.md)：鉴权、transport、API 语义、兼容边界和排障
- [gRPC、负载均衡与配置更新](docs/tech-notes/grpc-load-balancing-and-push.md)：当前连接和部署模型
- [DEV 联调模板](docs/dev-http-token.md)：无固定 endpoint、无提交凭证的联调步骤
- [DEV E2E](test/dev-e2e/README.md)：SDK 与服务端的可执行兼容验证

## 开发

```bash
make proto          # 重新生成 checked-in Go protobuf 代码
make test           # Go workspace 测试
make python-build
make python-test
```

Java 与各语言发版流程见模块 README、`RELEASING.md` 和 `CHANGELOG.md`。共享 proto 的源文件
位于 `api/proto/tipsy/**`：Go/Python 生成代码提交进仓库，Java 在 Maven 构建时生成。

## 许可证

[MIT](LICENSE)
