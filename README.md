# Tipsy AB-config SDK

Tipsy 配置中心 / A/B 实验平台的对外公开 SDK 套件。这是从私有仓库 `tipsy-ab-config` 拆分出来的对外面，方便下游服务通过公共 Go module proxy 直接拉取，无需任何凭据。

仓库内容：

- **Go SDK**（多 module + `go.work`）
  - `api/gen/go` — `config.proto` / `abtest.proto` 的 protobuf + gRPC 生成代码
  - `sdk/go/tipsyauth` — JWT 签名小工具
  - `sdk/go/tipsyabconfig` — 主 SDK，提供 PullAll / Subscribe / GetDynamicConfig / 命中实验 / 曝光上报等接口
  - `sdk/go/example` — Go 用法示例
- **Python SDK** — 见 [`sdk/python/README.md`](sdk/python/README.md)
- **Java SDK**（Maven 多模块，Java 21）— 见 [`sdk/java/README.md`](sdk/java/README.md)
  - `sdk/java/tipsy-abconfig-proto` — `config.proto` / `abtest.proto` 的 protobuf + gRPC 生成代码（构建期生成）
  - `sdk/java/tipsy-auth` — JWT 签名小工具（`io.github.lightspeed-intelligence:tipsy-auth`）
  - `sdk/java/tipsy-abconfig` — 主 SDK（`io.github.lightspeed-intelligence:tipsy-abconfig`），含可选的 `io.github.lightspeedintelligence.abconfig.web` web 集成子包
  - `sdk/java/example` — 基于 JDK 内置 `com.sun.net.httpserver` 的 Java 用法示例
- **Proto 源** — `api/proto/tipsy/{config,abtest}/v1/*.proto`

## 安装

### Go

```bash
go get github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk/sdk/go/tipsyabconfig
```

### Python

详见 [`sdk/python/README.md`](sdk/python/README.md)。

### Java

```bash
cd sdk/java
mvn -q -DskipTests install   # 安装到本地 .m2（io.github.lightspeed-intelligence:tipsy-abconfig:0.1.0）
```

详见 [`sdk/java/README.md`](sdk/java/README.md)。

## 快速开始（Go）

```go
package main

import (
	"context"
	"log"
	"time"

	"github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk/sdk/go/tipsyabconfig"
)

func main() {
	client, err := tipsyabconfig.New(tipsyabconfig.Config{
		Endpoint:  "config.example.com:443",
		Namespace: "my-app",
		ServiceID: "my-service",
		// 其它可选项见 Config 字段文档
	})
	if err != nil {
		log.Fatalf("new client: %v", err)
	}
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	values, err := client.GetDynamicConfig(ctx, "user-123", map[string]any{
		"country": "JP",
	}, []string{"feature_x", "feature_y"})
	if err != nil {
		log.Fatalf("get dynamic config: %v", err)
	}
	for k, v := range values {
		log.Printf("%s = %s", k, v)
	}
}
```

完整可运行示例参见 [`sdk/go/example/main.go`](sdk/go/example/main.go)。

## 快速开始（Java）

```java
try (TipsyAbConfigClient client = TipsyAbConfigClient.create(Config.builder()
        .namespaces(List.of("tipsy-chat"))
        .configServiceAddr("grpcs://config.example.com:443")
        .abtestServiceAddr("grpcs://abtest.example.com:443")
        .token(System.getenv("TIPSY_TOKEN"))
        .build())) {

    AbtestContext abctx = client.newAbtestContext("user-123", Map.of("country", "JP"));
    String value = client.getConfig(abctx, "tipsy-chat", "rerank.threshold", "0.5");
    System.out.println(value);
}
```

完整可运行示例参见 [`sdk/java/example/src/main/java/io/github/lightspeedintelligence/abconfig/example/Main.java`](sdk/java/example/src/main/java/io/github/lightspeedintelligence/abconfig/example/Main.java)，
更多用法（两种传输、地址 scheme、web 集成、`tipsy-auth`）见 [`sdk/java/README.md`](sdk/java/README.md)。

## 文档

- [HTTP token 获取与使用](docs/dev-http-token.md)
- [SDK 集成手册](docs/usage-and-integration.md)

## 开发

```bash
# 重新生成 Go protobuf 桩
make proto

# 在本地跑全部 Go 模块测试
make test

# Python SDK
make python-build
make python-test
```

## 许可证

[MIT](LICENSE)
