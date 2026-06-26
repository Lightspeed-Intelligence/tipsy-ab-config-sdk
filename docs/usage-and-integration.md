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

> Salt 留空时用 layer.id / 实验 id 作为分桶 hash 种子。这意味着**删除并重建一个同名 layer / 实验时，新对象是新的 UUID，同一用户会被重新分桶、可能跳到不同组**，灰度/实验的用户黏性随之断裂。需要分桶可复现、可迁移、或重建后保持稳定时，显式设一个稳定的 Salt，别依赖留空回落。

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

> Salt 留空→用实验 ID 作分桶种子的稳定性后果同 §3.4 的 Salt 说明（重建换 ID = 换分桶）。

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
| Java | `TipsyAbConfigClient.getConfig` | `TipsyAbConfigClient.getConfigStatic` | `TipsyAbConfigClient.getExperimentResult` |

获取实验结果 API 是 `AbtestService.GetExperimentResult` 的 SDK 包装，适合直接读取 `custom_params`、查看命中的 group，或按 `layer_ids` / `experiment_type` / `display_type` 控制结果形态。它和 `GetConfig` 不同：不会读本地 config cache，不会做配置 value 合成，也不会自动发曝光事件；它返回原始实验计算结果。

### 4.0.0 实验入参语义：UID 与 attrs（被分流对象的身份与属性）

所有实验相关 API（`GetConfig` / `GetExperimentResult` 以及裸 HTTP/gRPC）都需要一个 `uid` 和一组 `attrs`（在 SDK 里通过 `AbtestContext` 或 `UserInfo` 携带，裸接口里是 `user_id` / `user_attrs`）。它们的语义是**被分流对象**的 ID 和属性，而不局限于"自然人用户"：

- **`uid`（被分流对象 ID）**：实验分桶（hashing / bucketing）所基于的唯一标识。
- **`attrs`（被分流对象属性）**：用于实验/分组/domain/layer 的**准入条件**判断的键值属性。

"被分流对象"是谁，由业务接入方按实验设计决定：

| 被分流对象 | `uid` 传什么 | `attrs` 传什么 |
| --- | --- | --- |
| 用户（最常见） | 用户 ID | 用户属性（如 country、vip、age 等） |
| 角色 | 角色 ID | 角色属性 |
| 项目 | 项目 ID | 项目属性 |
| 其它（设备、租户、会话……） | 该对象的唯一 ID | 该对象的属性 |

> 字段名沿用 `uid` / `user_id` / `user_attrs` 只是历史命名；语义上它代表"当前实验所分流的对象"，多数实验分流用户因而是用户 ID / 用户属性，但当实验按角色 / 项目等维度分流时，应传入对应对象的 ID 与属性。同一个 namespace 下不同实验若按不同维度分流，接入方需为每个调用传入与该实验设计一致的对象 ID 和属性。

**关于 `attrs` 的职责边界**：`attrs` 仅用于**实验准入条件（admission / audience）**的计算判断。平台方只负责"按配置的准入条件对传入的属性做计算和命中判断"，**不规定也不校验**具体应该传哪些字段、字段的业务语义是什么——**传入哪些属性键、各自代表什么含义，由实验填写者（在 Console 配置准入条件时）与业务接入方（在调用时传入属性时）共同约定、各自负责并自行保证一致**。平台不对属性字段做语义层面的校验：键名拼错、漏传、或类型选错（见 §5 开头 "`user_attrs` 值的格式"）通常表现为**准入不命中**而非报错。

准入条件（admission）的配置位置（domain / layer / experiment / group 级）见 §3.3–§3.5、§3.7；其 JSON 形态、计算与命中语义、属性类型的比较规则参见 tipsy-ab-config 平台文档与本文 §5 开头的 "`user_attrs` 值的格式"。

#### 准入条件举例（按国家 / 语言 / 注册时间筛人）

准入条件在 Console 配置、由平台引擎计算；下面是一个常见组合范例——按国家、语言（多选其一）加注册时间区间筛人：

```json
{
  "op": "and",
  "children": [
    { "field": "country",       "op": "in", "value": ["US", "CA", "GB"] },
    { "field": "language",      "op": "in", "value": ["en", "zh"] },
    { "field": "register_time", "op": ">=", "value": 1704067200 },
    { "field": "register_time", "op": "<=", "value": 1735689599 }
  ]
}
```

- `in` = 命中列表中任一即可（国家 / 语言用列表表达"多选其一"）。`field` 是任意属性键，按键名逐字匹配，引擎不对 `country` / `language` 等字段名做任何特殊处理；Console 字段字典里常预置这些键只是填写便利，不是引擎层面的"保留字段"。
- **IN 的"列表"在准入条件 JSON 里，不在 attrs 里**：业务侧 `user_attrs` 仍传单个标量值（`country="US"`、`language="en"`、`register_time=<epoch 秒整数>`），SDK 自动按类型包装；裸 HTTP/gRPC 按 §5 开头 "`user_attrs` 值的格式" 传。
- `register_time` 用 epoch 秒整数（int64）；区间用两个叶子（`>=` 起、`<=` 止）以 `and` 组合（引擎无单独 `between`）；用字符串 / ISO 日期做不了数值区间比较、只会不命中。
- `{}` 表示全部用户；支持 `and` / `or` / `not`，嵌套深度上限 5。

操作符全集、字段约定、更多范例与计算 / 命中语义以 **tipsy-ab-config 平台文档**为权威来源（平台 cookbook 的「附录：准入条件 admission 怎么写」与 console 文档 §3.4 受众），本文不复制完整算子表。

### 4.0 SDK 版本查询（怎么知道最新版本是哪个）

本仓使用 monorepo multi-module 管理，每条 SDK 走独立版本号、独立 git tag。要查"现在最新版是多少"有 4 个权威入口，任选其一即可，**不要去问平台维护方**：

| 入口 | 用途 | 命令 / URL |
| --- | --- | --- |
| **GitHub Releases 页**（最直观）| 看所有 SDK 的发布历史、release note、wheel 资产 | <https://github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk/releases> |
| **Python SDK CHANGELOG** | Python 端逐版本的 Added/Changed/Removed | <https://github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk/blob/main/sdk/python/CHANGELOG.md> |
| **Go SDK CHANGELOG** | Go 端逐版本的 Added/Changed/Removed | <https://github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk/blob/main/sdk/go/tipsyabconfig/CHANGELOG.md> |
| **Java SDK CHANGELOG** | Java 端逐版本的 Added/Changed/Removed | <https://github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk/blob/main/sdk/java/tipsy-abconfig/CHANGELOG.md> |
| **Git tags / Go list**（命令行）| 程序化获取 | 见下方命令 |

**命令行查最新版本**：

```bash
# Go SDK 最新版（公共 Go module proxy）
go list -m -versions github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk/sdk/go/tipsyabconfig

# Python SDK 最新 tag（公共 repo,无需 token）
curl -s https://api.github.com/repos/Lightspeed-Intelligence/tipsy-ab-config-sdk/releases \
  | jq -r '[.[] | select(.tag_name | startswith("python-sdk/v")) | .tag_name] | first'
# 输出形如: python-sdk/v0.3.0

# 或者直接 git 查
git ls-remote --tags --refs git@github.com:Lightspeed-Intelligence/tipsy-ab-config-sdk.git \
  | awk '{print $2}' | grep -E '^refs/tags/(python-sdk|sdk/go/tipsyabconfig)/v' | sort -V | tail -5

# Java SDK 最新版（Maven Central，无需凭据）
curl -s 'https://repo1.maven.org/maven2/io/github/lightspeed-intelligence/tipsy-abconfig/maven-metadata.xml' \
  | grep -oE '<release>[^<]+' | sed 's/<release>//'
# 或查 git tag（前缀 java-sdk/v）
git ls-remote --tags --refs git@github.com:Lightspeed-Intelligence/tipsy-ab-config-sdk.git \
  | awk '{print $2}' | grep -E '^refs/tags/java-sdk/v' | sort -V | tail -3
```

> Go 子目录 module 的 git tag 命名规则是 `<相对路径>/vX.Y.Z` —— 例如 `sdk/go/tipsyabconfig/vX.Y.Z`、`sdk/go/tipsyauth/vX.Y.Z`、`api/gen/go/vX.Y.Z`、`python-sdk/vX.Y.Z`、`java-sdk/vX.Y.Z`。

下文 §4.1 / §4.2 的安装命令默认拉最新版本；要在 CI / 生产里 pin 到具体 tag 时，把命令里的 `@latest` 或 `${LATEST_TAG}` 换成上面查到的具体 tag 即可。

### 4.1 Go SDK 接入

#### 4.1.0 接入方需要做什么（快速清单）

按下面顺序做，每一步都是一次性的：

**① 拉取 SDK（无需任何凭据）**

本仓 `Lightspeed-Intelligence/tipsy-ab-config-sdk` 是 **GitHub public repo**，公共 `proxy.golang.org` 直接代理，本机与 CI 都无需 PAT / Deploy key / GOPRIVATE。

业务仓根目录跑（拉最新版；要 pin 具体版本见 §4.0）：

```bash
go get github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk/sdk/go/tipsyabconfig@latest
# 自签 service token 还需要：
go get github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk/sdk/go/tipsyauth@latest
```

生产 / CI 里建议 pin 到具体 tag，把 `@latest` 换成 `@vX.Y.Z`（用 §4.0 的命令查最新 tag）。

之后 `go mod tidy`，`go.mod` 里会出现这两条 require，`go.sum` 会落 hash。

**确认你的业务工程 `go.mod` 的 `go` 指令**：可以是 `go 1.25.0`（或更高）；SDK 是独立 module 声明 `go 1.25.0`，不会把你顶到主仓的 `go 1.25.7`。

**② 建议给构建环境加 `GOPROXY` 兜底**

公共 proxy `proxy.golang.org` 对新发布 tag 有 1–5 分钟的首次缓存延迟；CI / 容器构建建议显式设置 GOPROXY，让 proxy 未命中时回退直连：

```bash
export GOPROXY=https://proxy.golang.org,direct
```

> 写法解读：`,direct` 后缀表示 "proxy 404 时直接 git clone 公开仓库兜底"。**有 proxy 时走 proxy（快），proxy 没追上时走直连（不阻塞）**。这是消费公开 Go module 仓库的社区惯例，不针对本 SDK。
>
> Docker 镜像写法：
> ```dockerfile
> ENV GOPROXY=https://proxy.golang.org,direct
> ```
>
> GitHub Actions 写法：
> ```yaml
> jobs:
>   build:
>     env:
>       GOPROXY: https://proxy.golang.org,direct
> ```

**③ 业务代码 import + 初始化**

```go
import (
    "github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk/sdk/go/tipsyabconfig"
    // 仅当需要自签 service token 时：
    "github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk/sdk/go/tipsyauth"
)
```

初始化代码见下文 §4.1 的 `tipsyabconfig.Init(...)` 示例。**SDK 不读取任何固定环境变量**，所有地址 / token / 超时通过 `Config` 字段传入；地址、token 来源由业务方自己用 ENV / Vault / 配置中心管理。

**④ Service token 来源（二选一）**

- **从 ab-config 平台拿一个长期 token 注入业务 ENV**（最常见）：用 `Config.Token = os.Getenv("AB_CONFIG_TOKEN")` 静态注入。
- **业务侧自签**（共享 HMAC 密钥时）：用 `tipsyauth.NewSigner(secret).Issue(tipsyauth.IssueOptions{...})` 生成短 TTL token，配合 `Config.TokenProvider` 每次 RPC 取新 token。详见 §2 鉴权边界 与本节后面的"Signer 用法"。

**⑤ 烟测：跑通最小回路**

业务进程启动后 `Init` 不报错、`Health()` 显示 `LastPullOK=true`、`SubscribeConnected=true`，就说明全链路通。

**⑥ 升级 SDK 版本**

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

最佳实践：把这两个地址存入业务服务自己的 ENV（变量名由业务方自定，如 `CONFIG_SERVICE_ADDR` / `ABTEST_SERVICE_ADDR`；SDK 本身不读取任何固定环境变量，地址通过 Config 字段传入），由业务代码启动时读取并传入 SDK。不要把地址硬编码在业务代码里；不同环境、集群、灰度部署时只需要调整 ENV。

#### 部署形态 → 地址配置对照表

| 部署形态 | 业务客户端配置 | 备注 |
| --- | --- | --- |
| DEV（Coolify 单实例 + Cloudflare） | `grpcs://dev-ab-config-grpc.infra.fantacy.live:443` | 公网 TLS，pick_first 单连 |
| 生产 K8S 同集群同 ns（推荐） | `dns:///tipsy-ab-config-headless.<ns>.svc.cluster.local:50051` | Headless Service + round_robin，纯内网 |
| 生产 K8S 同集群跨 ns（推荐） | `dns:///tipsy-ab-config-headless.<server-ns>.svc.cluster.local:50051` | 同上——同集群 CoreDNS 跨 ns 可解析，FQDN 里 `<server-ns>` 填 ab-config 所在 ns（不是调用方自己的 ns），纯内网 + round_robin，不必出 Ingress |
| 同集群 internal Ingress（过渡） | `tipsy-ab-config-grpc.internal:80` | 模式 B，需要 split-horizon DNS |
| 公网 TLS（已有部署） | `grpcs://ab-config.example.com:443` | 同 DEV 形态 |
| 虚拟机（自部署） | `host:port` 或 `grpcs://host:443` | pick_first 单连，无 LB |
| **K8S 跨集群 / multi-cluster mesh / federation（不覆盖）** | — | 本任务不提供方案，请走模式 B（service mesh 或 internal Ingress） |

关于上表中"同集群本地裸 Service DNS"的最后兼容形态：历史接入文档曾推荐过裸 `tipsy-ab-config:50051`（K8S ClusterIP Service DNS）写法。这是 L4 pin 模式（模式 A），多业务客户端 + 多 ab-config 实例下负载分布严重不均，**生产新接入不推荐**，仅作为历史兼容形态保留。新接入请按上表选 Headless（同集群推荐，含跨 ns）、internal Ingress（过渡）或公网 TLS（DEV / 已有部署）。

关于同集群 internal Ingress（模式 B，过渡形态）的额外约束：

- **强制要求**：集群内 DNS 必须通过 split-horizon DNS 把 internal Ingress 域名直接解析到 Ingress Controller 的 ClusterIP，**不能解析到公网域名（如 Cloudflare 那一套 `dev-ab-config-grpc.infra.fantacy.live`），否则流量会绕公网 hairpin 出去再回来**，延迟从亚毫秒变成跨海 RTT。
- 流量路径：业务 pod → internal Ingress Controller → ab-config pod。Ingress 在中间做 RPC 级负载均衡，单 RPC 多一跳但在 1ms 量级内可忽略。
- 这是 Headless 落地前的过渡方案；新集群推荐直接走 Headless（见 §4.1.1）。

直连源站 IP（私有证书，调试 / 排查）：`grpcs://47.253.175.59:443?authority=ab-config-grpc.example.com&insecure=true`（Go）；Python 同地址再配 `tls_root_certificates=<Origin Cert PEM>`。该形态属于调试链路，不在上表覆盖的常规部署形态内。

#### 4.1.1 Headless Service 部署要求（模式 C）

**适用范围**：模式 C（Headless Service + 客户端 round_robin）**仅适用于「客户端与 ab-config Server 在同一 K8S 集群同一 VPC」**。其它部署形态（Coolify、虚拟机、跨集群、公网域名）请按上表选 internal Ingress（模式 B）或公网 TLS（DEV / 已有部署）。

**服务端 Service YAML**：必须把 ab-config 的 Service 改为 Headless（`clusterIP: None`），DNS 才会返回所有 pod 真实 IP 列表，而不是单个虚 IP。示例片段：

```yaml
apiVersion: v1
kind: Service
metadata:
  name: tipsy-ab-config-headless
  namespace: <ns>
spec:
  clusterIP: None            # 关键：标记为 Headless Service
  selector:
    app: tipsy-ab-config
  ports:
    - name: grpc
      port: 50051
      targetPort: 50051
      protocol: TCP
```

**服务端 pod readinessProbe**：必须配 `readinessProbe`（HTTP `/healthz` 或 gRPC health check），否则 DNS 会把"还没起来的 pod IP"也返回给客户端，SDK Init 时连接到未就绪 pod 会触发首次 RPC 失败。示例：

```yaml
readinessProbe:
  httpGet:
    path: /healthz
    port: 8080
  initialDelaySeconds: 5
  periodSeconds: 5
```

**客户端配置**：地址前缀写 `dns:///`，**SDK 自动启用 `round_robin` 策略，无需任何 Config 字段**。这是地址字符串自描述的 opt-in 模式：

- Go SDK：`v0.4.0+` 起识别 `dns:///` 前缀，自动注入 `grpc.WithDefaultServiceConfig({"loadBalancingConfig":[{"round_robin":{}}]})`。
- Python SDK：`python-sdk/v0.5.0+` 起同样识别 `dns:///` 前缀，自动注入 `("grpc.service_config", <round_robin JSON>)` channel option。
- 业务方代码零变更，只改地址 ENV 即可。

**行为说明**：

- grpc 默认每 30s 重新解析一次 DNS（`dnsResolverMinResolveRate = 30s`）。pod 启停时，客户端最长 30s 感知，会自动 reconnect 到新解析出的 pod 集合，业务无感知。
- 一旦解析出 N 个 pod，SDK 与每个 pod 各建一条 HTTP/2 长连接（每个 RPC 走 round_robin），没有中间 Proxy 一跳，延迟最低。
- 现有 `Keepalive` 字段对每条子连接（subchannel）仍然生效。
- 如果业务方想要 pick_first 行为（即只连一个 pod），应直接用裸 `host:port` 或 `passthrough:///host:port`，**不要在 Config 里加 opt-out 字段**（SDK 不提供此字段）。

**强调**：模式 C 仅适用于「客户端与 ab-config Server 在同一 K8S 集群同一 VPC」。跨集群 / multi-cluster mesh / federation 不在本方案覆盖范围内，请走模式 B（service mesh 或 internal Ingress）。**同集群跨 namespace 同样推荐走模式 C**——同集群 CoreDNS 能解析任意 ns 的 headless FQDN，把 FQDN 里的 ns 段写成 ab-config 所在 ns 即可，纯内网 + round_robin，不必出 Ingress（见上表对照行）。

详细的负载均衡模式对比与 push 模型分析见 [`docs/tech-notes/grpc-load-balancing-and-push.md`](tech-notes/grpc-load-balancing-and-push.md)。

注意（**gRPC 模式地址语法 · 方案 Y**）：默认 gRPC 模式下，`ConfigServiceAddr` / `AbtestServiceAddr` 是 gRPC target，不是 Console 的 HTTPS URL。地址字符串自描述传输方式，规则如下：

| 写法 | 行为 | 适用 |
|---|---|---|
| 裸 `host:port`（如 `tipsy-ab-config:50051`） | 明文 h2c | 内网默认，向后兼容 |
| `grpc://host:port` | 明文 h2c | 显式明文 |
| `grpcs://host:port` | TLS（SNI/证书名取 host） | 公网标准（未来形） |
| `grpcs://host:port?authority=<域名>&insecure=<bool>` | TLS + 覆盖 :authority/SNI + 可选跳校验 | 直连源站 IP 等特殊链路 |
| `dns:///host:port` | 通过 DNS 解析所有后端 IP，SDK 自动启用 `round_robin`（Go v0.4.0+ / Python v0.5.0+） | K8S Headless Service，详见 §4.1.1 |
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

AbtestContext 的构造、惰性拉取与复用（重要）：

- **构造 `AbtestContext` 不再发起任何 `GetExperimentResult` RPC**。`NewAbtestContext` / `NewAbtestContextWithTraceID` 现在是纯创建：只填充 uid / attrs / trace_id，不对任何 namespace（包括默认 namespace）做预请求。
- 实验结果改为**惰性拉取**：在本请求链路里第一次对某 namespace 调用 `GetConfig` 时，SDK 才同步拉取该 namespace 的 config_version 实验结果（`type=config_version, display=flat_kv`），并在该 `AbtestContext` 内缓存。同一 namespace 在本 ctx 内**最多拉取一次**（at-most-once）；之后的 `GetConfig` 直接复用缓存。默认 namespace 与非默认 namespace 现在走完全一致的惰性路径。
- 这个请求级缓存只覆盖 config_version 实验结果，用于动态配置取值。custom_params 结果不走该缓存，需要调用 `GetExperimentResult`。
- **一次业务服务调用创建一个 `AbtestContext`，并在该次调用内对所有 `GetConfig` 复用同一个 ctx**（不要每次 `GetConfig` 都新建 ctx——那样会丢失请求级 memoize，导致同一 namespace 被重复拉取）。

构造一次 + 多次 `GetConfig` 复用：

```go
// 一次业务服务调用内构造一个 ctx
abctx := sdk.NewAbtestContext(ctx, "user-123", map[string]any{
    "country": "US",
    "vip":     true,
})

// 在本次调用内对所有 GetConfig 复用同一个 abctx
threshold, err := sdk.GetConfig(ctx, abctx, "tipsy-chat", "rerank_threshold", "0.5")
if err != nil {
    return err
}
topK, err := sdk.GetConfig(ctx, abctx, "tipsy-chat", "rerank_top_k", "20")
if err != nil {
    return err
}
// 上面两次 GetConfig 共享 abctx，tipsy-chat 的实验结果只拉取一次
_ = threshold
_ = topK
```

显式预热（可选）：

- 如果业务在构造 ctx 之后、首次 `GetConfig` 之前还有一段可与 RPC 重叠的逻辑，可以**显式**触发预热：`abctx.PrefetchConfigVersionFlatKvForNamespace(ns)`。
- 该 API 非阻塞（只触发异步拉取，立即返回）、幂等、at-most-once：预热后随后的 `GetConfig` 会复用同一次拉取，对同一 namespace 预热 + `GetConfig` 合计仍只发一次 RPC。空 ctx / 未订阅的 namespace 不发任何 RPC（短路）。

```go
abctx := sdk.NewAbtestContext(ctx, "user-123", map[string]any{"country": "US"})

// 显式预热：触发 tipsy-chat 的实验结果异步拉取，不阻塞
abctx.PrefetchConfigVersionFlatKvForNamespace("tipsy-chat")

// ... 这里可以做别的与 RPC 重叠的工作 ...

// 首次 GetConfig 复用预热结果，不会再发一次 RPC
value, err := sdk.GetConfig(ctx, abctx, "tipsy-chat", "rerank_threshold", "0.5")
_ = value
_ = err
```

请求入口 middleware 与 URL 白名单预热：

- Go SDK 已提供 `Client.Middleware(userProvider, opts...)`，gin 场景可用 `Client.GinMiddleware(gc, userProvider, opts...)` 包一层 gin handler。
- middleware 会在每个业务 HTTP 请求入口调用 `userProvider` 提取 `uid` 和 `attrs`，创建请求级 `AbtestContext`，并挂到 request context。
- **middleware 默认不做任何预热**（构造 ctx 即返回）。要在流量入口层预热，必须传入 URL 路径白名单选项 `sdk.PrefetchPaths("/chat", "/recommend")`，**只有请求路径精确命中白名单时，middleware 才对默认 namespace 触发一次预热**（等价于调用 `PrefetchConfigVersionFlatKvForNamespace(defaultNs)`）。
- **警告**：若要在流量入口层做预热（如用网络中间件包裹 handler），**必须用 URL 白名单控制，只有命中白名单的 URL 才预热**；否则会对每个穿过中间件的请求都发起一次实验 RPC，造成大量无用空请求（很多 handler 根本不读 `GetConfig`）。不配白名单 = 不预热，是安全的默认。
- 路径匹配是**精确匹配** `r.URL.Path`（不做前缀 / 正则）。默认 namespace 为空时预热 no-op（无 ns 可热）。

net/http 接入示例（带 URL 白名单预热）：

```go
userProvider := func(ctx context.Context, r *http.Request) (string, map[string]any, error) {
    return r.Header.Get("X-User-Id"), map[string]any{
        "country": r.Header.Get("X-Country"),
    }, nil
}

// 只有 /chat、/recommend 命中白名单的请求才预热默认 namespace；
// 不传 PrefetchPaths 则任何路径都不预热。
withAbtest := sdk.Middleware(userProvider, sdk.PrefetchPaths("/chat", "/recommend"))

mux.Handle("/chat", withAbtest(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
    abctx := tipsyabconfig.AbtestContextFromContext(r.Context())
    value, err := sdk.GetConfig(r.Context(), abctx, "", "rerank_threshold", "0.5")
    _ = value
    _ = err
})))
```

gin 接入示例（带 URL 白名单预热）：

```go
r.Use(func(gc *gin.Context) {
    sdk.GinMiddleware(gc, userProvider, sdk.PrefetchPaths("/chat"))
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

// NewAbtestContext 等价于 NewAbtestContextWithTraceID(..., "")，由 SDK 自动生成 trace_id。
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

AbtestContext 的构造、惰性拉取与复用（重要）：

- **构造 `AbtestContext` 不再发起任何 `GetExperimentResult` RPC**。`new_abtest_context(...)` 现在是纯创建：只填充 user_id / user_attrs / trace_id，不对任何 namespace（包括默认 namespace）做预请求。
- 实验结果改为**惰性拉取**：在本请求链路里第一次对某 namespace 调用 `get_config` 时，SDK 才拉取该 namespace 的 config_version 实验结果（`type=config_version, display=flat_kv`）并在该 `AbtestContext` 内缓存。同一 namespace 在本 ctx 内**最多拉取一次**（at-most-once）；之后的 `get_config` 直接复用。默认 namespace 与非默认 namespace 现在走完全一致的惰性路径。
- 这个请求级缓存只覆盖 config_version 实验结果。custom_params 结果不走该缓存，需要调用 `sdk.get_experiment_result(...)`。
- **一次业务服务调用创建一个 `AbtestContext`，并在该次调用内对所有 `get_config` 复用同一个 ctx**（不要每次 `get_config` 都新建 ctx）。

构造一次 + 多次 `get_config` 复用：

```python
# 一次业务服务调用内构造一个 ctx
ctx = sdk.new_abtest_context(
    user_id="user-123",
    user_attrs={"country": "US", "vip": True},
)

# 在本次调用内对所有 get_config 复用同一个 ctx
threshold = await sdk.get_config(ctx, "tipsy-chat", "rerank_threshold", "0.5")
top_k = await sdk.get_config(ctx, "tipsy-chat", "rerank_top_k", "20")
# 上面两次 get_config 共享 ctx，tipsy-chat 的实验结果只拉取一次
```

显式预热（可选）：

- 若构造 ctx 后、首次 `get_config` 前还有可与 RPC 重叠的逻辑，可显式触发预热：`ctx.prefetch_config_version_flat_kv_for_namespace(ns)`。
- 该 API 非阻塞（同步签名，内部触发异步拉取后立即返回）、幂等、at-most-once：预热后随后的 `get_config` 复用同一次拉取，对同一 namespace 预热 + `get_config` 合计仍只发一次 RPC。空 ctx / 未订阅的 namespace 不发任何 RPC。

```python
ctx = sdk.new_abtest_context(user_id="user-123", user_attrs={"country": "US"})

# 显式预热：触发 tipsy-chat 实验结果异步拉取，不阻塞
ctx.prefetch_config_version_flat_kv_for_namespace("tipsy-chat")

# ... 这里可以做别的与 RPC 重叠的工作 ...

# 首次 get_config 复用预热结果，不会再发一次 RPC
value = await sdk.get_config(ctx, "tipsy-chat", "rerank_threshold", "0.5")
```

请求入口 middleware 与 URL 白名单预热：

- Python SDK 已提供 ASGI/FastAPI middleware：`AbtestMiddleware`。
- middleware 会在每个 HTTP 请求入口调用 `user_provider` 提取 `uid` 和 `attrs`，创建请求级 `AbtestContext`，并写入 `abtest_ctx_var`。
- **middleware 默认不做任何预热**。要在流量入口层预热，必须传入 URL 路径白名单 `prefetch_paths=["/chat"]`，**只有 `request.url.path` 精确命中白名单时，middleware 才对默认 namespace 触发一次预热**（等价于 `ctx.prefetch_config_version_flat_kv_for_namespace(default_ns)`）。
- **警告**：若要在流量入口层做预热（如用网络中间件包裹 handler），**必须用 URL 白名单控制，只有命中白名单的 URL 才预热**；否则会对每个穿过中间件的请求都发起一次实验 RPC，造成大量无用空请求。不配 `prefetch_paths` = 不预热，是安全的默认。
- 路径匹配是**精确匹配** `request.url.path`（不做前缀 / 正则）。默认 namespace 为空时预热 no-op。
- 业务代码通常不直接读取实验结果，而是在同一请求中调用 `sdk.get_config(ctx, namespace, key, default)` 或 `sdk.get_config_default(ctx, key, default)`；当 `ctx` 传 `None` 时，`get_config` 会从 `abtest_ctx_var` 读取 middleware 注入的上下文。

FastAPI 接入示例（带 URL 白名单预热）：

```python
from fastapi import FastAPI, Request
from tipsy_ab_config.fastapi_middleware import AbtestMiddleware

app = FastAPI()

async def user_provider(request: Request):
    uid = request.headers.get("X-User-Id", "anonymous")
    return uid, {"country": request.headers.get("X-Country", "")}

# 只有 /chat 命中白名单的请求才预热默认 namespace；
# 不传 prefetch_paths 则任何路径都不预热。
app.add_middleware(
    AbtestMiddleware,
    sdk=sdk,
    user_provider=user_provider,
    prefetch_paths=["/chat"],
)

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

### 4.3 Java SDK 接入

Java SDK 与 Go / Python **全量对齐**，语言基线 **Java 21**，Maven 坐标 groupId `io.github.lightspeed-intelligence`。完整文档见 [`sdk/java/README.md`](../sdk/java/README.md)。

#### 4.3.0 接入方需要做什么（快速清单）

**① 引依赖（无需任何凭据，从 Maven Central 拉取）**

发布版本在 **Maven Central**（与 `page.liam:pine` 同一发布通道），业务工程直接声明依赖：

```xml
<dependency>
  <groupId>io.github.lightspeed-intelligence</groupId>
  <artifactId>tipsy-abconfig</artifactId>
  <version>0.1.0</version>   <!-- 最新版见 §4.0 的查询命令 -->
</dependency>
<!-- 仅在需要本服务自签 service token 时再加： -->
<dependency>
  <groupId>io.github.lightspeed-intelligence</groupId>
  <artifactId>tipsy-auth</artifactId>
  <version>0.1.0</version>
</dependency>
```

> `tipsy-abconfig` 已传递依赖 `tipsy-abconfig-proto`（生成的 gRPC stub）与 `tipsy-auth`；
> gRPC 传输用 `grpc-netty-shaded`（重定位 netty，避免与宿主 netty 冲突）。

**② 构造 client（进程级单例，应用启动时建一次，关闭时 `close()`）**

```java
import io.github.lightspeedintelligence.abconfig.*;
import java.time.Duration;
import java.util.List;

TipsyAbConfigClient client = TipsyAbConfigClient.create(
    Config.builder()
        .namespaces(List.of("tipsy-chat"))
        // gRPC（推荐 K8S 内 Headless + dns:/// 自动 round_robin）：
        .configServiceAddr("grpcs://dev-ab-config-grpc.infra.fantacy.live:443")
        .abtestServiceAddr("grpcs://dev-ab-config-grpc.infra.fantacy.live:443")
        .token(System.getenv("AB_CONFIG_TOKEN"))   // HS256 service token
        .build());
// 应用退出时：client.close();  （实现了 AutoCloseable）
```

> **地址 scheme（方案 Y，与 Go/Python 同）**：`grpcs://host:port[?authority=&insecure=]` 走 TLS；
> 裸 `host:port` / `grpc://host:port` 走明文 h2c；`dns:///svc.ns.svc.cluster.local:port` 自动启用
> 客户端 `round_robin`；`http(s)://` 需配 `.transport(Transport.HTTP)`（HTTP 模式 protojson POST，仅轮询、无 Subscribe）。

**③ 每次业务服务调用构造一个 `AbtestContext` 并显式传给 `getConfig`（在该次调用内复用）**

```java
// 进站请求里拿到 uid + 属性后，构造一个 ctx：
AbtestContext ctx = client.newAbtestContext(
    userId, Map.of("country", "US"));   // 也可带 trace_id 重载
// 在本次调用内对所有 getConfig 复用同一个 ctx（不要每个 getConfig 都新建）：
String threshold = client.getConfig(ctx, "tipsy-chat", "rerank_threshold", "0.5");
String topK = client.getConfig(ctx, "tipsy-chat", "rerank_top_k", "20");
// 上面两次 getConfig 共享 ctx，tipsy-chat 的实验结果只拉取一次
// 无用户身份的服务级读取：client.getConfigStatic("tipsy-chat", key).orElse("0.5");
// 直接读实验结果（custom_params / 分组）：client.getExperimentResult(ExperimentResultRequest.builder()...build());
```

> **构造不发 RPC、惰性拉取**：`newAbtestContext(...)` 是纯创建，不对任何 namespace（含默认 namespace）做预请求。第一次对某 namespace 调用 `getConfig` 时才拉取该 ns 的 config_version 实验结果，并在该 ctx 内缓存（at-most-once）。
>
> **显式预热（可选）**：若构造 ctx 后、首次 `getConfig` 前有可与 RPC 重叠的逻辑，可显式调用 `ctx.prefetchConfigVersionFlatKvForNamespace("tipsy-chat")`（非阻塞、幂等、at-most-once；空 ctx / 未订阅 ns 不发 RPC）。
>
> **Java 无网络中间件**：SDK 不提供 servlet/Spring filter，也没有自动预热入口。若自建 thread-per-request 入口（如基于 `com.sun.net.httpserver` 的 `HttpServerSupport`）想在入口层预热，必须**自行用 URL 白名单 gate**——只对命中白名单的路径调用 `ctx.prefetchConfigVersionFlatKvForNamespace(defaultNs)`，否则会对每个请求发起一次无用的实验 RPC。

> **为什么显式传 `AbtestContext`（而非 ThreadLocal）**：经调研业务方 `tipsy-recsys/pine-java`——
> 单个请求会在 `newVirtualThreadPerTaskExecutor()` 上 fan-out 到多个虚拟线程，ThreadLocal **不会**
> 跨 fan-out 传播。因此 SDK 的契约是**显式传参**（与 Go 的显式 `abctx` 参数一致）。SDK 另提供可选的纯 JDK
> 便捷件 `io.github.lightspeedintelligence.abconfig.web.{AbtestContextHolder, HttpServerSupport}`（基于 `com.sun.net.httpserver`），
> 仅用于 thread-per-request 边缘，且 Javadoc 明确标注 fan-out 不传播警示。**不**提供 servlet/Spring filter。

#### 4.3.1 与 Go/Python 的对外差异（有意设计，非缩水）

- `getConfigStatic` 返回 `Optional<String>`（命中含空串也是 present；未命中 `empty()`）——对应 Go 的 `(value, bool)`；调用方用 `.orElse(default)`。
- 用显式 `AbtestContext` 参数 + 可选 `AbtestContextHolder` 替代 Go 的 context-value（不提供 `WithAbtestContext`/`AbtestContextFromContext` 等价 API）。
- gRPC 拨号定制用 `Config.channelConfigurator(UnaryOperator<ManagedChannelBuilder<?>>)`（替代 Go 的 `DialOptions`）。
- 日志走 SLF4J 门面（宿主选 backend），不在 `Config` 里传 logger。
- `tipsy-auth` 的 `JwtSigner` 签发 HS256 service token，claims `{roles,namespaces,sub,iat,exp}`，与服务端验证契约一致。

#### 4.3.2 自签 service token（可选）

```java
import io.github.lightspeedintelligence.auth.*;
import java.time.Duration;
import java.util.List;

String token = JwtSigner.create(System.getenv("TIPSY_SERVICE_SECRET"))
    .issue(IssueOptions.builder()
        .subject("my-service")
        .roles(List.of("business_sdk"))
        .namespaces(List.of("*"))
        .ttl(Duration.ofHours(8))
        .build());
```

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
- `config_flat_kv` / `custom_flat_kv` 是**该 namespace 下所有命中实验的扁平聚合**：把每个命中实验/组的参数拍平进同一个 map。**不同实验若用了相同的 key 名，会互相覆盖**（后写覆盖先写），响应里看不出某个 key 来自哪个实验。要按实验/组精确归属，请用 `display_type=RESULT_DISPLAY_TYPE_EACH_EXPERIMENT_GROUP` 看 `groups[].experiment_id` / `group_id`；或给自定义参数 key 名加实验前缀避免撞车。（注意这与 §3.7「结果里没有某 key → 回退 full release」是两回事：那是“缺 key”，这是“多实验同名 key 撞车”。）
- `groups[]` 只在 `display_type=RESULT_DISPLAY_TYPE_EACH_EXPERIMENT_GROUP` 时填充；用 `RESULT_DISPLAY_TYPE_FLAT_KV` 或省略 `display_type`（默认 `UNSPECIFIED`）时 `groups` 恒为空数组，但 `config_flat_kv` / `custom_flat_kv` 仍照常返回值。想读“命中了哪些组”务必显式传 `EACH_EXPERIMENT_GROUP`，否则会误判成“没命中”。
- `experiment_type` 是**类型隔离过滤**：只返回与该类型匹配的实验。用 `EXPERIMENT_TYPE_CONFIG_VERSION` 过滤一个 `custom_params` 实验（或反之）会得到**空结果且不报错**。不确定时用 `EXPERIMENT_TYPE_ALL`，或传与目标实验一致的精确类型。
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

14. 读 `experiment_result` 时按响应形态选对参数：要分组明细传 `display_type=EACH_EXPERIMENT_GROUP`（否则 `groups` 恒空）；`experiment_type` 是类型隔离过滤，类型传错会得到空结果而非报错；`config_flat_kv`/`custom_flat_kv` 是 namespace 级聚合，跨实验同名 key 会互相覆盖（自定义参数 key 名建议带实验前缀）。

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
3. 每次业务服务调用（= 一次入站 HTTP 请求，或一次异步任务执行）创建一个 `AbtestContext`；它是 **User 粒度**（每个 User 每次服务调用一个 ctx），并在该次调用内**复用同一个 ctx 给所有 `GetConfig`**（不要每个 `GetConfig` 都新建——会丢失请求级 memoize，导致同一 namespace 被重复拉取）。
4. 业务中调用 `GetConfig` / `get_config`。构造 ctx 不发任何实验 RPC；首次 `GetConfig` 才惰性拉取该 namespace 的实验结果（at-most-once，本次调用内缓存）。
5. 非用户场景调用 `GetConfigStatic` / `get_config_static`。
6. 若要在流量入口层（如网络中间件包裹 handler）预热，**必须用 URL 白名单控制**（Go `sdk.PrefetchPaths(...)` / Python `prefetch_paths=[...]`），只有命中白名单的 URL 才预热；否则会对每个穿过中间件的请求发起一次无用空实验请求。也可在业务代码里按需显式调用 `PrefetchConfigVersionFlatKvForNamespace` / `prefetch_config_version_flat_kv_for_namespace`。

离线脚本或低频服务：

1. 优先使用 `POST /api/v1/config/static` 或 `POST /api/v1/config/dynamic`。
2. 需要完整实验详情时调用 `POST /api/v1/abtest/experiment_result`。
3. 用短 TTL 服务 token，namespace 最小授权。

平台自动化：

1. 使用 Admin API 创建 namespace/config/domain/layer/experiment。
2. 使用 public-read HTTP 做验收。
3. 使用 audit logs 追踪变更来源。
