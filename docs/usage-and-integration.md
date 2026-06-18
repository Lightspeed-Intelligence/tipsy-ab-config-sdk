# Tipsy AB Config 使用与接入文档

本文同时面向人类工程师和 AI/自动化接入方，说明如何在 Console 创建配置与实验，如何使用 Go/Python SDK，如何直接调用裸 HTTP/gRPC 接口，以及接入前后应检查的事项。

## 1. 一句话模型

Tipsy AB Config 是一个“配置中心 + AB 实验”服务：

- 人类工程师在 `/console/` 创建 namespace、配置 key/version、domain/layer、experiment/group，并发布全量或实验版本。
- 业务服务通过 SDK 订阅配置快照，并按用户上下文调用动态配置 `GetConfig`。
- 没有 SDK 缓存能力的服务可以直接调用裸 HTTP/gRPC 读接口。

默认端口：

| 入口 | 默认地址 | 用途 |
| --- | --- | --- |
| Console / Admin HTTP | `http://localhost:8080/console/` | 人类配置、实验、审计、管理员白名单 |
| Admin API | `http://localhost:8080/api/v1/*` | Console 背后的管理接口 |
| Public-read HTTP | `http://localhost:8080/api/v1/abtest/experiment_result` 等 | 机器对机器读接口 |
| gRPC | `localhost:50051` | SDK 与平台内 RPC |

## 2. 鉴权边界

接入前先区分两类 token。

| 场景 | 鉴权 | token 来源 | 传递方式 |
| --- | --- | --- | --- |
| Console 页面、Admin API | tipsy-backend RS256 会话 JWT + `console_admin` 白名单 | tipsy-backend 登录态 | 浏览器 cookie `token`，或 header `token: <jwt>` |
| SDK、gRPC、Public-read HTTP | HS256 服务 JWT | 可信 token issuer 使用 `TIPSY_SERVICE_SECRET` 签发 | `Authorization: Bearer <jwt>` |

Console 首次部署必须先手工插入至少一个管理员：

```sh
psql "$DATABASE_URL" -c \
  "INSERT INTO console_admin(user_id, note) VALUES (<your-backend-uid>, 'bootstrap admin');"
```

本地开发里的 `go run -tags devtools ./cmd/devtoken --sub <uid>` 只用于生成 RS256 Console/Admin 会话 token，方便调试 `/console/*` 和 Admin API；它不是业务读 HTTP/gRPC 使用的 HS256 服务 token 签发入口。

服务 JWT 的 claim 形状是：

```json
{
  "sub": "your-service-name",
  "roles": ["internal_service"],
  "namespaces": ["tipsy-chat"],
  "iat": 1710000000,
  "exp": 1710003600
}
```

`namespaces` 可包含多个 namespace；内部服务 token 可用 `"*"`，业务服务建议只授予自己需要的 namespace。注意：当前实现只有一个 HS256 verifier secret，namespace 授权来自 token claim，而不是来自“每个 secret 绑定固定 namespace”。因此不要把 `TIPSY_SERVICE_SECRET` 下发给不完全可信的业务服务；否则拿到 secret 的服务可以自行签出任意 namespace 甚至 `"*"` 的 token。推荐由 ab-config 平台、tipsy-backend 或其他可信 token issuer 统一签发短 TTL 服务 token，业务服务只持有 token 或通过 token provider 获取 token。

本期接入现实：

- 当前还没有内建的在线 token 申请/签发接口。
- 接入方需要二选一：使用 signer 包 + `TIPSY_SERVICE_SECRET` 自行生成服务 token；或者向平台/运维人工请求一个已经预生成好的服务 token。
- 服务 token 有有效期，官方 signer 要求 `TTL > 0` 并会写入 `exp`。长期上线运行的业务服务如果暂时只能自签，较稳健的方式是在环境变量中配置 `TIPSY_SERVICE_SECRET`，进程内起定时任务，定期签发短 TTL token 并缓存在内存中，SDK 通过 `TokenProvider` 或等价机制读取当前有效 token。
- TODO：后续应设计更健壮、安全、可治理的 token 分发机制，例如由 tipsy-backend、网关或专门 token issuer 根据服务身份和授权表签发短 TTL token，业务方不直接接触 `TIPSY_SERVICE_SECRET`。

本仓库提供 `cmd/servicetoken` 用于签发 HS256 服务 token：

```sh
TIPSY_SERVICE_SECRET='<secret>' \
go run ./cmd/servicetoken \
  --sub 'your-service-name' \
  --namespaces 'tipsy-chat' \
  --ttl '24h'
```

多个 namespace 用逗号分隔：

```sh
TIPSY_SERVICE_SECRET='<secret>' \
go run ./cmd/servicetoken \
  --sub 'your-service-name' \
  --namespaces 'tipsy-chat,tipsy-backend' \
  --ttl '24h'
```

命令只向 stdout 输出 JWT 本体，不附加日志或换行，适合：

```sh
TIPSY_TOKEN=$(TIPSY_SERVICE_SECRET='<secret>' go run ./cmd/servicetoken --sub 'your-service-name' --namespaces 'tipsy-chat' --ttl '24h')
```

可信 token issuer 的 Go 签发示例：

```go
signer, err := tipsyauth.NewSigner(os.Getenv("TIPSY_SERVICE_SECRET"))
if err != nil {
    panic(err)
}
token, err := signer.Issue(tipsyauth.IssueOptions{
    Subject:    "my-service",
    Roles:      []string{"internal_service"},
    Namespaces: []string{"tipsy-chat"},
    TTL:        time.Hour,
})
if err != nil {
    panic(err)
}
```

## 3. Console 配置流程

推荐按这个顺序创建：

1. Namespace
2. Config key / draft / publish
3. Domain
4. Layer
5. Experiment / group
6. Start experiment
7. 业务服务接入 SDK 或裸接口验证

### 3.1 创建 Namespace

入口：`/console/namespaces`

填写：

| 字段 | 说明 |
| --- | --- |
| `namespace` | 业务命名空间。建议稳定、短横线或小写标识，例如 `tipsy-chat` |
| `description` | 用途说明 |

Namespace 是权限、配置、实验和 SDK 订阅的边界。SDK 只能消费初始化时订阅过的 namespace。

### 3.2 创建配置 Key 和版本

入口：`/console/configs`

创建配置时填写：

| 字段 | 说明 |
| --- | --- |
| Namespace | 配置所属 namespace |
| Key | snake_case，例如 `rerank_threshold` |
| Description / Category / Owner | 便于治理和搜索 |
| Initial draft value | 初始草稿值。文本中 `{{var}}` 会被 Console 识别为变量 |

创建后进入配置详情页：

- 保存 draft：编辑草稿值。
- Publish：把 draft 发布成一个 version。
- Full Release：把某个 version 设置为该 key 的全量发布版本。

动态配置的最终兜底值来自 full release；如果某个 key 没有全量发布，且用户没有命中实验/白名单版本，SDK 会返回调用方传入的 default。

### 3.3 创建 Domain

入口：`/console/domains`

Domain 是实验拓扑中的流量域。每个 namespace 会有 root domain；子 domain 可挂在 layer 下参与更细粒度分流。

创建子 Domain 时填写：

| 字段 | 说明 |
| --- | --- |
| Namespace | 所属 namespace |
| Name | namespace 内唯一 |
| Parent Layer | 该子 domain 挂在哪个 layer 下 |
| Custom ID | 可选；留空自动生成 UUID |
| Admission | JSON 准入条件，`{}` 表示所有用户 |

### 3.4 创建 Layer

入口：`/console/layers`

Layer 是一组互斥流量槽位，实验必须挂在某个 layer 下。

填写：

| 字段 | 说明 |
| --- | --- |
| Namespace | 所属 namespace |
| Domain | layer 所属 domain |
| Layer ID | Console 自动生成 UUID，可手改；创建后不可改 |
| Name | namespace 内唯一 |
| Salt | 分流 salt；留空使用 `layer.id` |
| Hash Fn | `xxhash` 或 `crc32`，默认 `xxhash` |
| Traffic Total | 默认 `10000`，表示万分桶 |
| Admission | JSON 准入条件，`{}` 表示所有用户 |

Layer 详情页用于管理：

- 当前 layer 下的 slot。
- 从该 layer 创建实验。
- 实验或子 domain 的流量区间。

### 3.5 创建实验

推荐从 Layer 详情页点击创建实验，Console 会带入 `layer_id`。

入口：`/console/experiments/new?layer_id=<layer_id>`

公共字段：

| 字段 | 说明 |
| --- | --- |
| Namespace | 实验所属 namespace |
| Layer | 实验所属 layer，创建后可通过接口转移 |
| Experiment Type | 创建后不可改 |
| Name | namespace 内唯一 |
| Description / Owner | 治理字段 |
| Salt | 留空时使用实验 ID |
| Hash Fn | `xxhash` 或 `crc32` |
| Traffic Total | 默认 `10000` |
| Admission | 实验准入条件 JSON，`{}` 表示所有用户 |
| Sticky enabled | 开启后用户保持首次命中的 group |

实验类型：

| 类型 | 用途 | group params |
| --- | --- | --- |
| `config_version` | 灰度某些配置 key 的某些版本 | 通过 key/version 选择器配置，提交形状为 `{"<key_id>": "<version_id>"}` |
| `custom_params` | 返回任意实验参数，不依赖配置中心 key/version | 任意 JSON，例如 `{"button_color":"red","ratio":0.3}` |

Group 字段：

| 字段 | 说明 |
| --- | --- |
| name | 分组名，例如 `A`、`B` |
| traffic_range_lo / traffic_range_hi | 分桶区间，左闭右开，范围在 `0..traffic_total` |
| admission | 分组级准入 JSON |
| params | config_version 用 key/version 选择器；custom_params 用 JSON |
| whitelist_uids | 每行一个 UID，命中白名单时优先进入该 group |

常见流量例子：

| 分组 | lo | hi | 含义 |
| --- | --- | --- | --- |
| A | `0` | `5000` | 50% |
| B | `5000` | `10000` | 50% |

创建实验后状态是 draft。进入详情页点击 Start 后才会参与计算。Pause/Resume/Complete/Archive 会改变实验生命周期。

### 3.6 灰度发布 Whitelist

入口：实验详情页的 `Gray Releases (whitelists)`，或 `/console/experiments/{id}/whitelists`

灰度发布和实验 group whitelist 是两个概念：

- 灰度发布把一批 UID 固定到某个 `(namespace, config key, config version)`，不依赖任何实验。
- 实验 group whitelist 是某个实验组内的白名单，只影响该实验的分组命中。

创建灰度发布时填写：

| 字段 | 说明 |
| --- | --- |
| Namespace | 所属 namespace |
| Key ID | config key 的 int64 ID，保持字符串/文本处理，避免 JS 精度丢失 |
| Version ID | config version 的 int64 ID，保持字符串/文本处理 |
| UIDs | 每行一个 UID；软上限 10000，硬上限 100000 |

灰度发布可后续追加 UID、删除 UID、offline。动态配置合成时，灰度/实验命中的版本优先于 full release。

### 3.7 配置和实验的计算优先级

动态配置 `GetConfig` 的合成顺序：

1. 用户命中白名单或实验 group，且 group 配了该 config key 的 version：返回该 version 的 value。
2. 未命中实验，或实验未覆盖该 key：返回 full release value。
3. 都不存在：返回调用方传入的 default。

这意味着“实验结果里没有某个 key”不是错误，而是正常回退到全量发布。

## 4. SDK 接入

SDK 里有直接获取实验结果的 API，不需要绕到裸 HTTP：

| 语言 | 动态配置 | 静态配置 | 获取实验结果 |
| --- | --- | --- | --- |
| Go | `Client.GetConfig` | `Client.GetConfigStatic` | `Client.GetExperimentResult` |
| Python | `Client.get_config` | `Client.get_config_static` | `Client.get_experiment_result` |

获取实验结果 API 是 `AbtestService.GetExperimentResult` 的 SDK 包装，适合直接读取 `custom_params`、查看命中的 group，或按 `layer_ids` / `experiment_type` / `display_type` 控制结果形态。它和 `GetConfig` 不同：不会读本地 config cache，不会做配置 value 合成，也不会自动发曝光事件；它返回原始实验计算结果。

### 4.0 SDK 版本查询（怎么知道最新版本是哪个）

本仓使用 monorepo multi-module 管理，每条 SDK 走独立版本号、独立 git tag。要查"现在最新版是多少"有 4 个权威入口，任选其一即可，**不要去问平台维护方**：

| 入口 | 用途 | 命令 / URL |
| --- | --- | --- |
| **GitHub Releases 页**（最直观）| 看所有 SDK 的发布历史、release note、wheel 资产 | <https://github.com/Lightspeed-Intelligence/tipsy-ab-config/releases> |
| **Python SDK CHANGELOG** | Python 端逐版本的 Added/Changed/Removed | <https://github.com/Lightspeed-Intelligence/tipsy-ab-config/blob/main/sdk/python/CHANGELOG.md> |
| **Go SDK CHANGELOG** | Go 端逐版本的 Added/Changed/Removed | <https://github.com/Lightspeed-Intelligence/tipsy-ab-config/blob/main/sdk/go/tipsyabconfig/CHANGELOG.md> |
| **Git tags / Go list**（命令行）| 程序化获取 | 见下方命令 |

**命令行查最新版本**：

```bash
# Go SDK 最新版（Go module proxy 解析）
GOPRIVATE='github.com/Lightspeed-Intelligence/*' \
  go list -m -versions github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk/sdk/go/tipsyabconfig

# Python SDK 最新 tag（直接读 GitHub Releases API）
curl -s -H "Authorization: token ${GH_PAT}" \
  https://api.github.com/repos/Lightspeed-Intelligence/tipsy-ab-config/releases \
  | jq -r '[.[] | select(.tag_name | startswith("python-sdk/v")) | .tag_name] | first'
# 输出形如: python-sdk/v0.2.0

# 或者直接 git 查（不需要 token）
git ls-remote --tags --refs git@github.com:Lightspeed-Intelligence/tipsy-ab-config.git \
  | awk '{print $2}' | grep -E '^refs/tags/(python-sdk|sdk/go/tipsyabconfig)/v' | sort -V | tail -5
```

> Go 子目录 module 的 git tag 命名规则是 `<相对路径>/vX.Y.Z` —— 例如 `sdk/go/tipsyabconfig/vX.Y.Z`、`sdk/go/tipsyauth/vX.Y.Z`、`api/gen/go/vX.Y.Z`、`python-sdk/vX.Y.Z`。

下文 §4.1 / §4.2 的安装命令默认拉最新版本；要在 CI / 生产里 pin 到具体 tag 时，把命令里的 `@latest` 或 `${LATEST_TAG}` 换成上面查到的具体 tag 即可。

### 4.1 Go SDK 接入

#### 4.1.0 接入方需要做什么（快速清单）

按下面顺序做，每一步都是一次性的：

**① 确认本机/CI 能访问私有仓库**

本仓 `Lightspeed-Intelligence/tipsy-ab-config` 是 **GitHub private repo**，公共 `proxy.golang.org` 永远返回 404 拉不到——这是 Go module + 私有 repo 的常规行为，必须配以下两项：

- **本机开发**：确保本机已有 GitHub SSH key 或 PAT，且能 `git clone git@github.com:Lightspeed-Intelligence/tipsy-ab-config.git` 成功。
- **业务服务的 CI**（GitHub Actions / GitLab CI / Jenkins 等都一样）：必须添加一个能读 `Lightspeed-Intelligence/tipsy-ab-config` 的凭据。两种典型做法：
  - **Deploy key**：在本仓 Settings → Deploy keys 添加业务仓的公钥（read-only 足够），业务仓 CI 用对应私钥跑 git。
  - **PAT / Fine-grained token**：业务仓 secrets 里放一个 read 权限的 token，CI 跑 `git config --global url."https://<user>:<token>@github.com/".insteadOf "https://github.com/"`。

**② 在业务仓 go.mod 旁边声明 `GOPRIVATE`**

最稳的做法是写进业务仓的 CI 工作流和本机 shell rc，告诉 Go 工具链"这个 path 走 direct VCS，不要试 proxy/sumdb"：

```bash
export GOPRIVATE=github.com/Lightspeed-Intelligence/*
# 可选：同时把 sumdb 也跳掉（GOPRIVATE 默认会带上，显式写一遍也无害）
export GONOSUMCHECK=github.com/Lightspeed-Intelligence/*
```

> CI 推荐做法是直接写进 workflow 的 `env:`，例如 GitHub Actions：
> ```yaml
> jobs:
>   build:
>     env:
>       GOPRIVATE: github.com/Lightspeed-Intelligence/*
> ```

**③ `go get` 拉取 SDK**

业务仓根目录跑（拉最新版；要 pin 具体版本见 §4.0）：

```bash
go get github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk/sdk/go/tipsyabconfig@latest
# 自签 service token 还需要：
go get github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk/sdk/go/tipsyauth@latest
```

生产 / CI 里建议 pin 到具体 tag，把 `@latest` 换成 `@vX.Y.Z`（用 §4.0 的命令查最新 tag）。

之后 `go mod tidy`，`go.mod` 里会出现这两条 require，`go.sum` 会落 hash。

**确认你的业务工程 `go.mod` 的 `go` 指令**：可以是 `go 1.25.0`（或更高）；SDK 是独立 module 声明 `go 1.25.0`，不会把你顶到主仓的 `go 1.25.7`。

**④ 业务代码 import + 初始化**

```go
import (
    "github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk/sdk/go/tipsyabconfig"
    // 仅当需要自签 service token 时：
    "github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk/sdk/go/tipsyauth"
)
```

初始化代码见下文 §4.1 的 `tipsyabconfig.Init(...)` 示例。**SDK 不读取任何固定环境变量**，所有地址 / token / 超时通过 `Config` 字段传入；地址、token 来源由业务方自己用 ENV / Vault / 配置中心管理。

**⑤ Service token 来源（二选一）**

- **从 ab-config 平台拿一个长期 token 注入业务 ENV**（最常见）：用 `Config.Token = os.Getenv("AB_CONFIG_TOKEN")` 静态注入。
- **业务侧自签**（共享 HMAC 密钥时）：用 `tipsyauth.NewSigner(secret).Issue(tipsyauth.IssueOptions{...})` 生成短 TTL token，配合 `Config.TokenProvider` 每次 RPC 取新 token。详见 §2 鉴权边界 与本节后面的"Signer 用法"。

**⑥ 烟测：跑通最小回路**

业务进程启动后 `Init` 不报错、`Health()` 显示 `LastPullOK=true`、`SubscribeConnected=true`，就说明全链路通。

**⑦ 升级 SDK 版本**

后续发版用：

```bash
go get github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk/sdk/go/tipsyabconfig@vX.Y.Z
go mod tidy
```

子目录 module 的 tag 命名硬性规则是 `<相对路径>/vX.Y.Z`（如 `sdk/go/tipsyabconfig/v0.2.0`），`go get` 时直接用 `@vX.Y.Z`，Go 工具链按 module path 自动找对应 tag。

---

**Module 化（拆分后）**：Go SDK 现为独立 Go module，`go.mod` 声明 `go 1.25.0`。声明 `go 1.25.0` 的工程（如 `tipsy-backend`）可直接 require，**不会被主仓的 `go 1.25.7`（后端 migration 依赖 `goose` v3.27.1 导致）顶高**。

Module 路径（同时也是 Go import 路径，前后一致）：

| Module path | 用途 | 消费方通常如何 require |
| --- | --- | --- |
| `github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk/sdk/go/tipsyabconfig` | 客户端 SDK（PullAll/Subscribe/GetConfig/GetExperimentResult/曝光） | 直接 require |
| `github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk/sdk/go/tipsyauth` | HS256 token 签名器（backend 自签 service token） | 按需 require |
| `github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk/api/gen/go` | 公共 proto 生成码 | 通常由 `tipsyabconfig` 间接拉入，backend 一般无需直接 require |

> 包路径与 module path 一致，下方所有 Go 示例的 `import` 语句无需任何改动。

**`go get` 与 tag 约定**：本仓使用同仓多 module（monorepo multi-module），子目录 module 的 git tag 必须带目录前缀。消费方 `go get` 时 tag 形如：

- `sdk/go/tipsyabconfig/vX.Y.Z`
- `sdk/go/tipsyauth/vX.Y.Z`
- `api/gen/go/vX.Y.Z`

（这是 Go 子目录 module 的硬性规则；用 §4.0 的命令查最新 tag。）

包路径：

```go
github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk/sdk/go/tipsyabconfig
github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk/sdk/go/tipsyauth
```

初始化：

```go
configAddr := os.Getenv("CONFIG_SERVICE_ADDR")
abtestAddr := os.Getenv("ABTEST_SERVICE_ADDR")
if configAddr == "" || abtestAddr == "" {
    return fmt.Errorf("CONFIG_SERVICE_ADDR and ABTEST_SERVICE_ADDR are required")
}

sdk, err := tipsyabconfig.Init(ctx, tipsyabconfig.Config{
    Namespaces:        []string{"tipsy-chat"},
    ConfigServiceAddr: configAddr,
    AbtestServiceAddr: abtestAddr,
    Token:             serviceToken,
})
if err != nil {
    return err
}
defer sdk.Close()
```

`ConfigServiceAddr` 和 `AbtestServiceAddr` 是 SDK 必须知道的两个 gRPC 地址：

- `ConfigServiceAddr`：用于 `PullAll` / `Subscribe` 拉取和订阅配置快照，也是 `GetConfigStatic` / 动态配置本地缓存的来源。
- `AbtestServiceAddr`：用于 `GetExperimentResult`，也是动态 `GetConfig` 在请求链路内获取实验命中结果的来源。

最佳实践：把这两个地址存入业务服务自己的 ENV（变量名由业务方自定，如 `CONFIG_SERVICE_ADDR` / `ABTEST_SERVICE_ADDR`；SDK 本身不读取任何固定环境变量，地址通过 Config 字段传入），由业务代码启动时读取并传入 SDK。不要把地址硬编码在业务代码里；不同环境、集群、灰度部署时只需要调整 ENV。典型三种环境形态：

- 内网直连（明文）：`ab-config-grpc:50051`（K8S Service DNS 等）。
- 公网标准（TLS + 域名）：`grpcs://ab-config-grpc.example.com:443`。当前 DEV 已验证：`grpcs://dev-ab-config-grpc.infra.fantacy.live:443`。
- 直连源站 IP（私有证书，调试 / 排查）：`grpcs://47.253.175.59:443?authority=ab-config-grpc.example.com&insecure=true`（Go）；Python 同地址再配 `tls_root_certificates=<Origin Cert PEM>`。

注意（**gRPC 模式地址语法 · 方案 Y**）：默认 gRPC 模式下，`ConfigServiceAddr` / `AbtestServiceAddr` 是 gRPC target，不是 Console 的 HTTPS URL。地址字符串自描述传输方式，规则如下：

| 写法 | 行为 | 适用 |
|---|---|---|
| 裸 `host:port`（如 `ab-config:50051`） | 明文 h2c | 内网默认，向后兼容 |
| `grpc://host:port` | 明文 h2c | 显式明文 |
| `grpcs://host:port` | TLS（SNI/证书名取 host） | 公网标准（未来形） |
| `grpcs://host:port?authority=<域名>&insecure=<bool>` | TLS + 覆盖 :authority/SNI + 可选跳校验 | 直连源站 IP 等特殊链路 |
| `http://` / `https://` | 在 gRPC 模式下报参数错误 | 应改用 HTTP 传输模式 |

- `authority=<域名>`：覆盖 HTTP/2 `:authority`，并作为 TLS SNI / 证书校验目标名。当反代（如 Traefik）按 Host/SNI 路由到 gRPC 后端时必填。
- `insecure=true`：跳过 TLS 证书校验，**仅用于直连源站 IP + 私有/自签证书（如 Cloudflare Origin Cert）的调试场景，生产严禁携带**。缺端口（`grpcs://host`）、非数字端口（`grpcs://host:abc`）都会在初始化时报参数错误。
- **不要在 gRPC 模式下把 `https://...` 原样填入地址字段**（那是 HTTP 读接口的 base URL，gRPC 模式会报参数错误）。若要走 HTTP，见下文"传输模式"小节。

**Go 与 Python 在 `insecure=true` 上的关键差异（务必注意）**：

- **Go**：`insecure=true` 真正跳过证书校验，`grpcs://<IP>:443?authority=<域名>&insecure=true` 可直连私有证书源站。
- **Python**：grpcio **没有**原生的"跳过校验"能力。`insecure=true` 在 Python 下只是"尽力而为 + 运行时 WARN"，对私有 CA 源站**大概率仍连不上**。Python 直连私有证书源站的正道是用 **`Config.tls_root_certificates`**（见下）注入该 CA 的 PEM，此时正常校验通过、且 SDK 自动附带服务令牌。
- 即：**同一个 `grpcs://...?insecure=true` 地址串，Go 能直连、Python 需配合 `tls_root_certificates`**。

**`Config.tls_root_certificates`（Python 专有，`Optional[bytes]`）**：PEM 字节，作为 `grpcs://` TLS 路径的 root_certificates，用于信任不在系统信任链中的私有 / 自签 CA（如 Cloudflare Origin CA）。它与 `channel_factory` 的关键区别是：SDK 仍构建自己的 secure channel 并**自动挂载鉴权拦截器**，服务令牌照常上链——而 `channel_factory` 会完全接管 channel 构建、不挂鉴权，需调用方自行处理 token。因此直连私有证书源站时优先用 `tls_root_certificates`，而非 `channel_factory`。

#### 传输模式：gRPC（默认）/ HTTP

SDK 默认走 gRPC，无需任何额外配置即为现状行为。在不便使用 gRPC 的环境（如仅放行 HTTP 的网关、不便引入 gRPC 运行时）可切换为 HTTP 传输模式：

- 通过 `Config.Transport`（Go）/ `Config.transport`（Python）字段，或环境变量 `TIPSY_SDK_TRANSPORT`，取值 `"grpc"` / `"http"`；解析优先级为 Config 字段 > 环境变量 > 默认 `grpc`。
- **HTTP 模式下地址字段填 base URL**（`http://host:port` 或 `https://host:port`），不是 gRPC target。SDK 会在 base URL 后追加固定路径 `/api/v1/config/pull_all`、`/api/v1/abtest/experiment_result`。地址不以 `http(s)://` 开头会在初始化时报参数错误。
- **HTTP 模式仅轮询、无推送**：不建立 gRPC 连接、不订阅 Subscribe 流，靠周期 `PullAll` 轮询感知配置变更。变更感知延迟上限 = `PullInterval`（默认 10s），建议 HTTP 模式按需调小（如 5s）。鉴权方式与 gRPC 一致（`Authorization: Bearer <服务令牌>`）。
- **Python HTTP 模式需安装可选依赖**：`pip install tipsy-ab-config[http]`（会装 httpx）；纯 gRPC 用户依赖不变。

Go HTTP 模式示例：

```go
sdk, err := tipsyabconfig.Init(ctx, tipsyabconfig.Config{
    Namespaces:        []string{"tipsy-chat"},
    Transport:         "http", // 或留空走默认 gRPC；也可由 TIPSY_SDK_TRANSPORT 指定
    ConfigServiceAddr: "http://ab-config:8080",
    AbtestServiceAddr: "http://ab-config:8080",
    Token:             serviceToken,
    PullInterval:      5 * time.Second, // 建议调小以缩短变更感知延迟
})
```

Python HTTP 模式示例：

```python
# 需先安装：pip install tipsy-ab-config[http]
sdk = await init(Config(
    namespaces=["tipsy-chat"],
    transport="http",  # 或留空走默认 gRPC；也可由 TIPSY_SDK_TRANSPORT 指定
    config_service_addr="http://ab-config:8080",
    abtest_service_addr="http://ab-config:8080",
    token=service_token,
    pull_interval=5.0,  # 建议调小以缩短变更感知延迟
))
```



动态配置：

```go
abctx := sdk.NewAbtestContext(ctx, "user-123", map[string]any{
    "country": "US",
    "vip":     true,
})

value, err := sdk.GetConfig(ctx, abctx, "tipsy-chat", "rerank_threshold", "0.5")
if err != nil {
    return err
}
```

静态配置：

```go
value, ok := sdk.GetConfigStatic("tipsy-chat", "rerank_threshold", "0.5")
```

默认 namespace：

- SDK 初始化时读取 `PROJECT_DEFAULT_NAMESPACE`，或使用 `Config.DefaultNamespace` 覆盖。
- `GetConfigDefault(ctx, abctx, key, default)` 会使用默认 namespace。
- 默认 namespace 必须在 `Config.Namespaces` 中，否则返回 `ErrNamespaceNotSubscribed`。

请求入口 middleware 与异步预请求：

- Go SDK 已提供 `Client.Middleware(userProvider)`，gin 场景可用 `Client.GinMiddleware(gc, userProvider)` 包一层 gin handler。
- middleware 会在每个业务 HTTP 请求入口调用 `userProvider` 提取 `uid` 和 `attrs`，创建请求级 `AbtestContext`，并挂到 request context。
- 创建 `AbtestContext` 时，SDK 会对默认 namespace 异步预请求一次 `GetExperimentResult(type=config_version, display=flat_kv)`；非默认 namespace 不会预请求，会在本请求链路第一次 `GetConfig` 该 namespace 时同步拉取并缓存。
- 业务代码不需要也不应该直接读取这个预请求 map。正确消费方式是在同一个请求里取出 `AbtestContext`，调用 `GetConfig` / `GetConfigDefault`。如果预请求已经完成，`GetConfig` 直接复用缓存；如果还没完成，`GetConfig` 会等待同一个请求级结果；如果预请求失败，则静默回退 full release。
- 这个请求级缓存只覆盖 config_version 实验结果，用于动态配置取值。custom_params 结果不走该缓存，需要调用 `GetExperimentResult`。

net/http 接入示例：

```go
userProvider := func(ctx context.Context, r *http.Request) (string, map[string]any, error) {
    return r.Header.Get("X-User-Id"), map[string]any{
        "country": r.Header.Get("X-Country"),
    }, nil
}

withAbtest := sdk.Middleware(userProvider)

mux.Handle("/chat", withAbtest(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
    abctx := tipsyabconfig.AbtestContextFromContext(r.Context())
    value, err := sdk.GetConfig(r.Context(), abctx, "", "rerank_threshold", "0.5")
    _ = value
    _ = err
})))
```

gin 接入示例：

```go
r.Use(func(gc *gin.Context) {
    sdk.GinMiddleware(gc, userProvider)
    gc.Next()
})

r.GET("/chat", func(gc *gin.Context) {
    abctx := tipsyabconfig.AbtestContextFromContext(gc.Request.Context())
    value, err := sdk.GetConfig(gc.Request.Context(), abctx, "", "rerank_threshold", "0.5")
    _ = value
    _ = err
})
```

直接取实验结果，适合 custom params：

```go
resp, err := sdk.GetExperimentResult(ctx, tipsyabconfig.ExperimentResultRequest{
    Namespace: "tipsy-chat",
    UserInfo: tipsyabconfig.UserInfo{
        UID: "user-123",
        Attrs: map[string]any{"country": "US"},
    },
    Type:        tipsyabconfig.ExperimentTypeCustomParams,
    DisplayType: tipsyabconfig.ResultDisplayFlatKv,
    TraceID:     "demo-trace-id", // 可选；留空时 SDK 自动生成 UUID v4
})
if err != nil {
    return err
}
custom := resp.GetCustomFlatKv()
```

#### AbtestContext 与 trace_id

`AbtestContext` 现在持有请求级的 `trace_id`：同一个 AbtestContext 内每次 `GetConfig` / 内部 `GetExperimentResult` 都复用同一个 trace_id，便于把"一条业务请求"在服务端日志、SDK 日志、未来的实验结果上报里串到一起。

```go
// 显式传 trace_id（推荐：业务侧已有上游 trace 时直接接管）
abctx := sdk.NewAbtestContextWithTraceID(ctx, "user-123", map[string]any{
    "country": "US",
}, "abc-trace-from-upstream")

// 留空 traceID ⇒ SDK 端 uuid.New().String() 自动生成
abctx = sdk.NewAbtestContextWithTraceID(ctx, "user-123", nil, "")

// 老接口 NewAbtestContext / NewAbtestContextForNamespace 保持不变；
// 内部会等价于 NewAbtestContextWithTraceID(..., "")，由 SDK 自动生成 trace_id。
```

`Middleware` / `GinMiddleware` 会优先从入站请求头复用 trace_id：先读 `X-Trace-Id`，再读 `X-Request-Id`，两者都缺时由 SDK 端生成 UUID v4。这样上游网关 / 调用方已有的 trace 链路天然贯通。

trace_id 为可选字符串。未传或空串时由 SDK / 服务端自动填充 UUID v4（36 字符带 `-`）；最大 128 字符，超出会被服务端截断并打 WARN。后台 PullAll 等定时调用会生成独立 trace_id，同一 SDK 实例的日志可能落到多个 trace 下，这是预期：trace_id 是"请求"维度而非"实例"维度。

> **关于 trace_id 的语义**：`trace_id` 仅作为"关联标识"使用——把同一次业务请求在 SDK 日志、服务端实验结果日志、以及未来的实验结果数据上报里串到一起。**业务方可以按自身需求自行选择传什么 ID**，平台不规定字面格式：例如搜推服务直接传服务侧的 `request_id`、已经接入 OpenTelemetry 的服务可以传 OTel trace id、有内部链路追踪系统的传它的 trace id；没有上游 ID 时不传即可，SDK / 服务端会自动补 UUID v4。

### 4.2 Python SDK 接入

包名：`tipsy_ab_config`

初始化：

```python
import os

from tipsy_ab_config import Config, init

config_addr = os.environ["CONFIG_SERVICE_ADDR"]
abtest_addr = os.environ["ABTEST_SERVICE_ADDR"]

sdk = await init(Config(
    namespaces=["tipsy-chat"],
    config_service_addr=config_addr,
    abtest_service_addr=abtest_addr,
    token=service_token,
))
```

`config_service_addr` 和 `abtest_service_addr` 与 Go SDK 含义一致，分别指向 ConfigService 和 AbtestService 的 gRPC 地址。最佳实践同样是把它们放在业务服务 ENV 中，由代码读取后传入 SDK。

动态配置：

```python
ctx = sdk.new_abtest_context(
    user_id="user-123",
    user_attrs={"country": "US", "vip": True},
    trace_id="demo-trace-id",  # 可选；None / 空串 ⇒ SDK 端 uuid.uuid4() 自动生成
)

value = await sdk.get_config(ctx, "tipsy-chat", "rerank_threshold", "0.5")
```

静态配置：

```python
value = sdk.get_config_static("tipsy-chat", "rerank_threshold", "0.5")
```

默认 namespace：

```python
value = await sdk.get_config_default(ctx, "rerank_threshold", "0.5")
```

请求入口 middleware 与异步预请求：

- Python SDK 已提供 ASGI/FastAPI middleware：`AbtestMiddleware`。
- middleware 会在每个 HTTP 请求入口调用 `user_provider` 提取 `uid` 和 `attrs`，创建请求级 `AbtestContext`，并写入 `abtest_ctx_var`。
- 创建 `AbtestContext` 时，SDK 会对默认 namespace 异步预请求一次 `GetExperimentResult(type=config_version, display=flat_kv)`；非默认 namespace 在本请求链路第一次 `get_config` 该 namespace 时拉取并缓存。
- 业务代码通常不直接读取预请求结果，而是在同一请求中调用 `sdk.get_config(ctx, namespace, key, default)` 或 `sdk.get_config_default(ctx, key, default)`；当 `ctx` 传 `None` 时，`get_config` 会从 `abtest_ctx_var` 读取 middleware 注入的上下文。
- custom_params 结果不走请求级 config_version 缓存，需要调用 `sdk.get_experiment_result(...)`。

FastAPI 接入示例：

```python
from fastapi import FastAPI, Request
from tipsy_ab_config.fastapi_middleware import AbtestMiddleware

app = FastAPI()

async def user_provider(request: Request):
    uid = request.headers.get("X-User-Id", "anonymous")
    return uid, {"country": request.headers.get("X-Country", "")}

app.add_middleware(AbtestMiddleware, sdk=sdk, user_provider=user_provider)

@app.get("/chat")
async def chat():
    value = await sdk.get_config_default(None, "rerank_threshold", "0.5")
    return {"rerank_threshold": value}
```

直接取实验结果：

```python
from tipsy_ab_config import UserInfo
from tipsy_ab_config._proto.tipsy.abtest.v1 import abtest_pb2

resp = await sdk.get_experiment_result(
    namespace="tipsy-chat",
    user_info=UserInfo(uid="user-123", attrs={"country": "US"}),
    experiment_type=abtest_pb2.EXPERIMENT_TYPE_CUSTOM_PARAMS,
    display_type=abtest_pb2.RESULT_DISPLAY_TYPE_FLAT_KV,
    trace_id="demo-trace-id",  # 可选；None / 空串 ⇒ SDK 自动生成 UUID v4
)
custom = resp.custom_flat_kv
```

FastAPI 可使用 `AbtestMiddleware` 或在请求中手动创建 `new_abtest_context` 并写入 contextvar。示例见 `sdk/python/example/main.py`。

`AbtestMiddleware` 在每次请求入口按顺序读取 `X-Trace-Id`、`X-Request-Id` 复用上游 trace；两者都缺时由 SDK 端生成 UUID v4。手动调用 `new_abtest_context` / `abtest_scope` 时也可以通过 `trace_id=` 显式传入。同一个 AbtestContext 内所有 `get_config` 与内部 `get_experiment_result` 调用都共享同一个 trace_id。

```python
async with sdk.abtest_scope(
    user_id="user-123",
    user_attrs={"country": "US"},
    trace_id="abc-trace-from-upstream",  # 可选；None / 空串 ⇒ SDK 自动生成
) as ctx:
    value = await sdk.get_config(ctx, "tipsy-chat", "rerank_threshold", "0.5")
```

trace_id 为可选字符串。未传或空串时由 SDK / 服务端自动填充 UUID v4（36 字符带 `-`）；最大 128 字符，超出会被服务端截断并打 WARN。后台 PullAll 等定时调用会生成独立 trace_id，同一 SDK 实例的日志可能落到多个 trace 下，这是预期。

> **关于 trace_id 的语义**：`trace_id` 是"关联标识"——把同一次业务请求在 SDK 日志、服务端实验结果日志、未来的实验结果数据上报里串到一起。业务方可以按自身需求选择传什么 ID（如搜推服务直接传 `request_id`、已接入 OpenTelemetry 的服务传 OTel trace id），平台不规定字面格式，详见 §4.1 同名说明。

## 5. 裸 HTTP 接口

裸 HTTP 读接口使用 HS256 服务 token：

```sh
AUTH="Authorization: Bearer $TIPSY_SERVICE_TOKEN"
BASE="http://localhost:8080"
```

**权限说明**：服务 token 的 `namespaces` 权限对这些 HTTP 端点生效。这些端点会把调用方 token 透传给后端做 namespace 权限校验，ns 受限的 token 访问未授权 namespace 会返回 `403`（之前的版本曾因内部代理而越权放行，现已收紧）。

**JSON 格式**：这些端点的响应统一用 protojson 输出 `UseProtoNames`（字段名为 snake_case，如 `user_id`、`config_flat_kv`、`business_snapshot_seq`）+ `EmitUnpopulated`（零值字段也输出，包括空 map `{}`、空数组 `[]`、`0`、空字符串、未填 timestamp）。请求方向服务端解码同时接受 snake_case 与 camelCase 字段名，但为与响应一致，本文示例统一用 snake_case。

**`user_attrs` 值的格式（仅适用于 5.1 `experiment_result` 与 5.2 `dynamic` 请求）**：`user_attrs` 在 proto 中是 `map<string, Value>`，其中 `Value` 是一个 `oneof`（tagged union），所以每个属性值必须用 `{标签: 值}` 显式带类型，不能直接写裸标量。标签如下：

| 标签 | 类型 | 示例 |
| --- | --- | --- |
| `s` | string | `{"country": {"s": "US"}}` |
| `i` | int64 | `{"age": {"i": 25}}`；大于 `2^53` 的整数必须写成字符串：`{"backend_uid": {"i": "1777013139821325267"}}`（见第 9 节） |
| `d` | double | `{"score": {"d": 0.87}}` |
| `b` | bool | `{"vip": {"b": true}}` |

为什么要显式带类型：受众条件按属性类型走不同比较语义（数值比较 vs 字符串等值），而裸 JSON 区分不出 `25` 是 int 还是 double，也区分不出 `"25"` 是字符串还是被误序列化的数字。类型选错会导致 audience 不命中而非报错，建议接入时单测覆盖。proto 定义见 `api/proto/tipsy/abtest/v1/abtest.proto` 的 `message Value`（`config.proto` 同名 mirror）。

**Go/Python SDK 用户不需要关心此格式**——SDK 接受裸 `map[string]any` / `dict[str, Any]`，内部按运行时类型自动包装为 `Value`（不支持的类型会被丢弃并打 warning）。本节规则只对裸 HTTP / 裸 gRPC（§6）调用方生效。

### 5.1 获取实验结果

`POST /api/v1/abtest/experiment_result`

请求：

```sh
curl -sS -H "$AUTH" -H 'Content-Type: application/json' \
  "$BASE/api/v1/abtest/experiment_result" \
  -d '{
    "namespace": "tipsy-chat",
    "user_id": "user-123",
    "user_attrs": {
      "country": {"s": "US"},
      "vip": {"b": true}
    },
    "layer_ids": [],
    "experiment_type": "EXPERIMENT_TYPE_CONFIG_VERSION",
    "display_type": "RESULT_DISPLAY_TYPE_FLAT_KV",
    "trace_id": "demo-trace-id"
  }'
```

典型响应（`UseProtoNames` + `EmitUnpopulated`，零值字段也会输出）：

```json
{
  "config_flat_kv": {
    "rerank_threshold": "12345"
  },
  "custom_flat_kv": {},
  "groups": [],
  "exposures": [
    {
      "key": "rerank_threshold",
      "version": "12345",
      "source": "experiment_group",
      "experiment_id": "exp-1",
      "group_id": "group-a",
      "experiment_status": "running",
      "release_id": "0"
    }
  ],
  "computed_at": "2026-06-12T00:00:00Z"
}
```

说明：

- `experiment_type` 可用 `EXPERIMENT_TYPE_CONFIG_VERSION`、`EXPERIMENT_TYPE_CUSTOM_PARAMS`、`EXPERIMENT_TYPE_ALL`。
- `display_type` 可用 `RESULT_DISPLAY_TYPE_FLAT_KV`、`RESULT_DISPLAY_TYPE_EACH_EXPERIMENT_GROUP`。
- `layer_ids` 为空表示计算所有 layer。
- `trace_id` 为可选字符串。未传或空串时由服务端自动填充 UUID v4（36 字符带 `-`）；最大 128 字符，超出会被服务端截断并打 WARN。该字段会出现在服务端实验结果计算路径的日志中，并作为后续"实验结果数据上报"的同一标识。**业务方可按自身需求选择传什么 ID**：搜推服务可以传服务侧的 `request_id`、接入了 OpenTelemetry 的服务可以传 OTel trace id、有自建链路追踪系统的传它的 trace id；平台不做格式校验，原样保留。
- Public-read HTTP 已设置 `UseProtoNames: true` + `EmitUnpopulated: true`：字段名为 snake_case，零值字段也会输出——空 map 序列化为 `{}`、空 repeated 为 `[]`、未填 timestamp 与数值/字符串零值同样出现。因此 `config_flat_kv`、`custom_flat_kv`、`groups`、`exposures`、`computed_at` 这些字段恒在，接入方可按字段恒存在解析（但仍需判断 map / 数组是否为空）。
- `config_flat_kv` 的 value、`exposures[].version`、`exposures[].release_id` 等 int64 字段在 JSON 中按 protojson 规则表现为字符串，接入方不要转成 JavaScript `Number`。
- `exposures[].version` 只对 config_version 实验/灰度发布有配置版本含义；custom_params 实验没有 config version 语义，该字段可能为 `0` 或无业务意义。

### 5.2 获取动态配置

`POST /api/v1/config/dynamic`

服务端会内部调用实验计算，并返回最终 value。

```sh
curl -sS -H "$AUTH" -H 'Content-Type: application/json' \
  "$BASE/api/v1/config/dynamic" \
  -d '{
    "namespace": "tipsy-chat",
    "user_id": "user-123",
    "user_attrs": {
      "country": {"s": "US"}
    },
    "keys": ["rerank_threshold"],
    "trace_id": "demo-trace-id"
  }'
```

响应：

```json
{
  "values": {
    "rerank_threshold": "0.7"
  }
}
```

`keys` 为空表示请求该 namespace 下全部 key 的最终值。

`trace_id` 同 §5.1：可选字符串，未传或空串由服务端自动填充 UUID v4，最大 128 字符；服务端会用同一个 trace_id 内部调用 `GetExperimentResult`，便于把动态配置合成的日志串到一起。

### 5.3 获取静态配置

`POST /api/v1/config/static`

只返回 full release 值，不依赖用户，不做实验计算。

```sh
curl -sS -H "$AUTH" -H 'Content-Type: application/json' \
  "$BASE/api/v1/config/static" \
  -d '{
    "namespace": "tipsy-chat",
    "keys": ["rerank_threshold"],
    "trace_id": "demo-trace-id"
  }'
```

`trace_id` 同 §5.1（可选字符串、空串服务端补 UUID v4、≤128 字符）。

### 5.4 拉取 namespace 全量快照

`POST /api/v1/config/pull_all`

返回每个请求 namespace 的全量配置快照（与 gRPC `ConfigService.PullAll` 同构）。该端点主要供 SDK 的 HTTP 传输模式装配本地缓存使用；裸调用方一般用不到——快照是"全量 version → value"原始数据，需自行实现本地缓存与实验/灰度计算才能取到最终 value（要直接取最终 value 请用 §5.2 / §5.3）。

```sh
curl -sS -H "$AUTH" -H 'Content-Type: application/json' \
  "$BASE/api/v1/config/pull_all" \
  -d '{
    "namespaces": ["tipsy-chat"],
    "trace_id": "demo-trace-id"
  }'
```

响应（代表性结构；字段同样为 snake_case，int64 编码为字符串）：

```json
{
  "snapshots": [
    {
      "namespace": "tipsy-chat",
      "business_snapshot_seq": "42",
      "experiment_snapshot_seq": "7",
      "keys": [
        {
          "key": "rerank_threshold",
          "full_release_version": "12340",
          "versions": {
            "12340": "0.5",
            "12345": "0.7"
          }
        }
      ]
    }
  ]
}
```

- `snapshots` 每项对应一个 namespace；`business_snapshot_seq` / `experiment_snapshot_seq` 是该 ns 的双 seq（int64 字符串）。
- `keys[].versions` 是 `version_id → value` 映射（key 为 int64 字符串），包含全量发布 version 与 abtest 报告的"可能命中"version；`keys[].full_release_version` 是当前 full release 的 version_id（无 active release 时为零值）。
- `experiment_snapshot_seq` 为 `0` 表示降级快照（abtest RPC 不可用，仅含全量发布 version）。

### 5.5 裸 HTTP 什么时候用

优先使用 SDK。裸 HTTP 适合：

- 脚本、低频后台任务、非 Go/Python 服务。
- 无法维护本地配置缓存的调用方。
- 运维验证与自动化验收。

不建议高 QPS 在线链路绕过 SDK 调 `/api/v1/config/dynamic`，因为 SDK 有本地缓存、订阅推送、请求级实验结果 memoize 和曝光处理。

## 6. 裸 gRPC 接口

服务定义在：

- `api/proto/tipsy/config/v1/config.proto`
- `api/proto/tipsy/abtest/v1/abtest.proto`

常用 RPC：

| Service | RPC | 说明 |
| --- | --- | --- |
| `tipsy.config.v1.ConfigService` | `PullAll` | SDK 拉取 namespace 快照 |
| `tipsy.config.v1.ConfigService` | `Subscribe` | SDK 订阅变更 |
| `tipsy.config.v1.ConfigService` | `GetDynamicConfig` | 动态配置裸 gRPC |
| `tipsy.config.v1.ConfigService` | `GetStaticConfig` | 静态配置裸 gRPC |
| `tipsy.abtest.v1.AbtestService` | `GetExperimentResult` | 实验结果裸 gRPC |

上述 5 个 request 消息（`PullAllRequest`、`SubscribeRequest`、`GetDynamicConfigRequest`、`GetStaticConfigRequest`、`GetExperimentResultRequest`）都包含一个可选 `string trace_id` 字段。未传或空串时由服务端自动填充 UUID v4（36 字符带 `-`）；最大 128 字符，超出会被服务端截断并打 WARN。`SubscribeRequest.trace_id` 仅与"建立这条订阅流"的入口动作绑定，用于日志中标记是哪一次订阅建立，后续每条 push 不重复使用同一 trace_id。

`trace_id` 是"关联标识"字段——把同一次业务请求在 SDK 日志、服务端实验结果日志、未来的实验结果数据上报里串到一起。业务方可按自身需求选择传什么 ID，平台不规定字面格式：搜推服务可以传 `request_id`、已接入 OpenTelemetry 的服务可以传 OTel trace id、有自建链路追踪系统的传它的 trace id；不传时由服务端补 UUID。

gRPC 鉴权统一使用 metadata：

```text
authorization: Bearer <HS256 service token>
```

若业务语言不是 Go/Python，建议直接从 proto 生成客户端，并复用本文的 namespace、user attrs（tagged-union `Value`，规则见 §5 开头 "`user_attrs` 值的格式"）、实验类型语义。

## 7. Admin API 快速参考

Admin API 面向 Console/自动化管理，使用 RS256 会话 token + admin 白名单，不使用服务 token。

| 操作 | Method + Path |
| --- | --- |
| 列 namespace | `GET /api/v1/namespaces` |
| 创建 namespace | `POST /api/v1/namespaces` |
| 列 config key | `GET /api/v1/configs/keys?namespace=<ns>` |
| 创建 config key | `POST /api/v1/configs/keys` |
| 保存 draft | `POST /api/v1/configs/drafts` 或 `PUT /api/v1/configs/drafts` |
| 发布 version | `POST /api/v1/configs/publish` |
| 列 version | `GET /api/v1/configs/versions?key_id=<id>` |
| 设置 full release | `POST /api/v1/configs/release/full` |
| 列 domain | `GET /api/v1/domains?namespace=<ns>` |
| 创建 domain | `POST /api/v1/domains` |
| 列 layer | `GET /api/v1/layers?namespace=<ns>` |
| 创建 layer | `POST /api/v1/layers` |
| 创建/调整 layer slot | `POST /api/v1/layers/{id}/slots` |
| 列 experiment | `GET /api/v1/experiments?namespace=<ns>` |
| 创建 experiment | `POST /api/v1/experiments` |
| 转移 experiment layer | `PATCH /api/v1/experiments/{id}/layer` |
| 启动 experiment | `POST /api/v1/experiments/{id}/start` |
| 暂停 experiment | `POST /api/v1/experiments/{id}/pause` |
| 恢复 experiment | `POST /api/v1/experiments/{id}/resume` |
| 完成 experiment | `POST /api/v1/experiments/{id}/complete` |
| 归档 experiment | `POST /api/v1/experiments/{id}/archive` |
| 列灰度发布 whitelist | `GET /api/v1/whitelists?namespace=<ns>` |
| 创建灰度发布 whitelist | `POST /api/v1/whitelists` |
| 查看灰度发布 whitelist | `GET /api/v1/whitelists/{id}` |
| 添加灰度 UID | `POST /api/v1/whitelists/{id}/uids` |
| 删除灰度 UID | `DELETE /api/v1/whitelists/{id}/uids/{uid}` |
| 下线灰度发布 whitelist | `POST /api/v1/whitelists/{id}/offline` |
| 审计日志 | `GET /api/v1/audit_logs?...` |

## 8. AI/自动化接入清单

AI agent 或脚本执行接入时，按以下不变式检查：

1. 已存在目标 namespace，且 SDK `Namespaces` 包含它。
2. 业务读接入已准备鉴权材料：要么有可用的 HS256 服务 token，要么有 `TIPSY_SERVICE_SECRET` 可自行签发 token；token 的 `namespaces` 必须覆盖目标 namespace。（本期推荐先在业务服务 ENV 中配置 `TIPSY_SERVICE_SECRET`，进程内起定时任务定期签发短 TTL token 并缓存在内存中，后续替换为更安全的 token 分发机制。）
3. 若使用默认 namespace，环境变量 `PROJECT_DEFAULT_NAMESPACE` 或 SDK config 已设置，并且该 namespace 已订阅。
4. 目标 config key 至少有一个已发布 version。
5. 需要兜底的 key 已设置 full release。
6. config_version 实验的 group params 使用 key_id 到 version_id 的映射，不使用 key name。
7. custom_params 实验的 params 是合法 JSON。
8. admission、custom params 中超过 `2^53` 的整数必须写成字符串。
9. 实验属于某个 active layer。
10. 实验 group 的分桶区间在 `0..traffic_total` 内，且无不期望的重叠。
11. 实验创建后已 Start，否则不会参与计算。
12. Console/Admin API 使用人类会话 token，且用户在 `console_admin` 白名单。
13. SDK / HTTP / gRPC 读接口的 `trace_id` 字段为可选字符串：未传或空串由服务端自动填充 UUID v4，外部传入任意非空字符串原样保留（不做格式校验、不重写），最大 128 字符，超出会被服务端截断并打 WARN。`trace_id` 是"关联标识"——把同一次业务请求在 SDK 日志、服务端实验结果日志、未来的实验结果数据上报里串到一起；业务方可按自身需求选择传什么 ID（搜推服务可以传 `request_id`、接入了 OpenTelemetry 的服务可以传 OTel trace id），平台不规定字面格式。SDK 高阶 API 把 trace_id 附在 `AbtestContext` 上（一个请求一个 ID）；Gin / FastAPI middleware 默认从入站头 `X-Trace-Id` → `X-Request-Id` 复用，否则生成。后台 PullAll / Subscribe 等定时调用会生成独立 trace_id（同一 SDK 实例的日志可能落到多个 trace 下，这是预期：trace_id 是请求维度而非实例维度）。

## 9. int64 与 JSON 精度

很多 ID 是 int64，HTTP JSON 响应会把这些字段序列化为字符串，例如：

```json
{
  "id": "1777013139821325267",
  "version": "1777013139821325268"
}
```

规则：

- 前端和自动化脚本不要把 ID 转成 JavaScript `Number`。
- admission / custom params 中大于 `2^53` 的整数必须加引号。
- config_version 实验结果里的 `config_flat_kv` 使用 key name 到 version_id 的映射；version_id 在 JSON 中也可能表现为字符串。
- custom_params 通过 protobuf `Struct` 传输，大整数只能安全地用字符串表达。

错误示例：

```json
{"backend_uid": 1777013139821325267}
```

正确示例：

```json
{"backend_uid": "1777013139821325267"}
```

## 10. 排障

| 现象 | 优先检查 |
| --- | --- |
| Console 401 | 浏览器是否有 tipsy-backend 会话 cookie；本地调试可用 `token` header |
| Console 403 `not in admin allowlist` | `console_admin` 是否已插入当前 `user_id` |
| SDK 初始化失败 | `Namespaces`、`ConfigServiceAddr`、`Token/TokenProvider` 是否为空；服务 token 是否有效 |
| `ErrNamespaceRequired` / `NamespaceRequired` | 未传 namespace，且未配置 `PROJECT_DEFAULT_NAMESPACE` |
| `ErrNamespaceNotSubscribed` / `NamespaceNotSubscribed` | namespace 不在 SDK 初始化订阅列表里 |
| 动态配置总是 default | key 是否有 full release；实验是否 started；group params 是否覆盖该 key；用户是否满足 admission |
| 命中了实验但值仍回退 full | SDK 本地缓存里没有该 version，检查 `PullAll/Subscribe` 和发布顺序 |
| 裸 HTTP 401 | 是否用 `Authorization: Bearer <service-token>`，不要用 Console 会话 token |
| Admin API 401/403 | Admin API 需要人类会话 token + admin 白名单，不接受服务 token |
| custom params 大整数异常 | 大整数是否写成字符串 |

## 11. 推荐接入方式

在线服务：

1. 使用 SDK。
2. 进程启动时 Init，订阅需要的 namespace。
3. 每个请求创建一个 AbtestContext。
4. 业务中调用 `GetConfig` / `get_config`。
5. 非用户场景调用 `GetConfigStatic` / `get_config_static`。

离线脚本或低频服务：

1. 优先使用 `POST /api/v1/config/static` 或 `POST /api/v1/config/dynamic`。
2. 需要完整实验详情时调用 `POST /api/v1/abtest/experiment_result`。
3. 用短 TTL 服务 token，namespace 最小授权。

平台自动化：

1. 使用 Admin API 创建 namespace/config/domain/layer/experiment。
2. 使用 public-read HTTP 做验收。
3. 使用 audit logs 追踪变更来源。
