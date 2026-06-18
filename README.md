# Tipsy AB-config SDK

Tipsy 配置中心 / A/B 实验平台的对外公开 SDK 套件。这是从私有仓库 `tipsy-ab-config` 拆分出来的对外面，方便下游服务通过公共 Go module proxy 直接拉取，无需任何凭据。

仓库内容：

- **Go SDK**（多 module + `go.work`）
  - `api/gen/go` — `config.proto` / `abtest.proto` 的 protobuf + gRPC 生成代码
  - `sdk/go/tipsyauth` — JWT 签名小工具
  - `sdk/go/tipsyabconfig` — 主 SDK，提供 PullAll / Subscribe / GetDynamicConfig / 命中实验 / 曝光上报等接口
  - `sdk/go/example` — Go 用法示例
- **Python SDK** — 见 [`sdk/python/README.md`](sdk/python/README.md)
- **Proto 源** — `api/proto/tipsy/{config,abtest}/v1/*.proto`

## 安装

### Go

```bash
go get github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk/sdk/go/tipsyabconfig
```

### Python

详见 [`sdk/python/README.md`](sdk/python/README.md)。

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
