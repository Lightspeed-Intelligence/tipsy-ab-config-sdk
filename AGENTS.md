# AGENTS.md — tipsy-ab-config-sdk 工程约定

面向在本仓工作的 agent / 工程师。本仓是多语言 SDK monorepo（Go / Python / Java）+ 共享 proto（`api/proto`）。

## 发版必须更新 CHANGELOG（硬规则）

发布**任何**语言 SDK 的新版本时，**必须**在该模块的 `CHANGELOG.md` 里加上对应版本段，与代码/tag 一起提交。顺序：**先写 CHANGELOG，再打 tag**（tag 内应已包含该条目）。

- 格式：Keep a Changelog —— `## [X.Y.Z] - YYYY-MM-DD` + `### Added / Changed / Fixed / Removed`（BREAKING 用 `### Changed (BREAKING)` / `### Removed (BREAKING)`）；顶部保留 `## [Unreleased]`；遵循 SemVer。
- 各语言 CHANGELOG 位置：
  - Go：`sdk/go/tipsyabconfig/CHANGELOG.md`
  - Python：`sdk/python/CHANGELOG.md`
  - Java：`sdk/java/tipsy-abconfig/CHANGELOG.md`（`tipsy-auth` 仅在该模块有功能改动时才动 `sdk/java/tipsy-auth/CHANGELOG.md`）
- 发版完整流程：`sdk/python/RELEASING.md`、`sdk/java/RELEASING.md`（Go 无独立 RELEASING，直接打 `sdk/go/tipsyabconfig/vX.Y.Z` tag）。

> 历史教训：`sdk/go/tipsyabconfig` 的 **v0.9.0 / v0.10.0 曾打了 tag 却漏补 CHANGELOG**（Python/Java 都补了），后于 commit `d32c162` 事后补记。发版清单里务必带上"更新 CHANGELOG"这一步——漏了就是账实不符（drift）。
>
> 可选强化：加一个 CI 门禁——"改了某模块源码但其 CHANGELOG 的 `[Unreleased]` 段未动 → 报红"（类比 python-sdk 的 proto-drift 门禁）；或迁到 release-please / Conventional Commits 自动生成 CHANGELOG + 版本 + Release。

## 发版机制（简）

- Tag scheme：`sdk/go/tipsyabconfig/vX.Y.Z`、`python-sdk/vX.Y.Z`、`java-sdk/vX.Y.Z`、`api/gen/go/vX.Y.Z`。
- **push tag → GitHub Actions 自动发布**（凭据在 CI secrets，本地无需）：Python → PyPI/构建；Java → Maven Central（release job 会 gate 校验 `tag 版本 == parent POM version`，不符 fail-fast）；Go 直接用 VCS tag（`go get ...@tag`）。
- **Java 版本 lock-step**：`sdk/java/pom.xml` parent + 4 个 child pom 的 `<parent><version>` 必须同版本；README/docs 里的 `<version>` 是**冻结的 0.1.0 样例**（指向 Releases/CHANGELOG 取真值），按惯例不随版本 bump。
- 发布版本号不可覆盖（PyPI / Maven Central 不可变）；打 tag 前确认远端 tag 与包版本未撞车。

## 行为改动三语言对齐

SDK 的**行为/逻辑**改动（重连退避、事件处理、缓存语义等）默认要 **Go / Python / Java 三端一起改**，不要只改上报问题的那一端（同一 class 的 bug 通常三端并存）。

- 共享 proto 单一真源在 `api/proto/tipsy/**`。
  - **Go**：`api/gen/go` 是 checked-in gen，改 proto 要 `make proto` 重生成并发 `api/gen/go/vX` tag。
  - **Java**：无 vendored gen，`tipsy-abconfig-proto` 构建期用 protobuf-maven-plugin 从 `api/proto` 现生成（改 proto 后 rebuild 即得）。
  - **Python**：`sdk/python/tipsy_ab_config/_proto/*_pb2.py` 是 **checked-in vendored gen**——改 proto 会让它 drift，`python-sdk.yml` 的 `proto-drift` job 在 push main 时 `git diff --exit-code` 报红。必须 regen：`PYTHON=<venv> bash scripts/gen-proto-py.sh`，且 **grpcio-tools 必须 ==1.66.2**（输出由该版本内置 protoc 决定、与运行时 protobuf 版本无关）。
