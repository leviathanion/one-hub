# 开发与贡献

## 文档收敛说明

`docs/dev` 里原先有多份渠道亲和、usage 与 async task 设计文档，主题重复，而且不少内容混杂了“当前实现”和“未来方案”两种口径。

现在收敛成若干份“当前架构说明”、目标/诊断文档、历史文档和独立工具文档。每篇架构/方案文档顶部都必须有 `## 文档状态`，避免读者把历史方案、目标方案或诊断材料误读为当前实现。

## 文档状态口径

| 状态 | 含义 | 维护规则 |
| --- | --- | --- |
| 当前实现 | 当前代码已按该 contract 落地，可作为读代码和改代码的主说明 | 代码行为改变时同步更新 |
| 当前实现 + 目标约束 | 主链路已落地，但文档中仍包含下一阶段结构收敛方向 | 明确区分“已经落地”和“后续方向” |
| 目标方案 | 目标架构或重构方案，不承诺当前代码已全部实现 | 实现落地后必须改状态或拆出迁移记录 |
| 当前诊断 | 对外部行为、源码画像或当前差异的诊断材料 | 不作为最终实现 contract，必要时指向目标方案 |
| 历史方案 | 已被替代、未纳入当前路线，或仅保留长期取向 | 不从这些文档直接派生新实现 |
| 工具文档 | 可执行脚本、压测或操作口径 | 跟随工具入口和参数更新 |

## 文档递进关系

下表按 Git 提交时间记录 `docs/dev` 的主要演进关系。这里的时间只代表已提交历史；当前工作区新增但未提交的文档没有 Git 时间。

| 时间 | 提交 | 递进关系 | 当前阅读入口 |
| --- | --- | --- | --- |
| 2026-03-21 | `6e197c2f` | 删除旧 `performance-optimization-backlog.md`，改为可执行的 [Relay 压测脚本](./relay-performance-benchmark.md)。这是“优化想法列表”到“可重复压测工具”的收敛。 | [Relay 压测脚本](./relay-performance-benchmark.md) |
| 2026-04-05 | `d57a21f8` | 删除多份 affinity / recovery / billing 草案，收敛成当前 routing、task、billing 主文档。旧草案不再作为实现入口。 | [Channel Affinity 架构设计方案](./channel-affinity-architecture.md)、[one-hub Async Task 架构设计](./task-coordination-architecture.md)、[Billing / Usage 结算架构](./billing-settlement-architecture.md) |
| 2026-05-20 | `0e59a1d1` | 新增 [ResponsesWS 架构说明](./responses-ws-architecture.md) 与 [WebSocket Transport 复用方案](./websocket-transport-architecture.md)。这是 ResponsesWS 初始 actor / safety primitives 阶段。 | [ResponsesWS 架构说明](./responses-ws-architecture.md) |
| 2026-05-25 | `186d8396` | 新增 [wsconn 唯一传输边界架构方案](./wsconn-architecture.md)，取代 primitives-only 的 [WebSocket Transport 复用方案](./websocket-transport-architecture.md)。这是“共享工具函数”到“强制传输边界”的升级。 | [wsconn 唯一传输边界架构方案](./wsconn-architecture.md) |
| 2026-06-21 | `21c925a2` | 在 ResponsesWS 主架构之后，拆出 [ResponsesWS Settlement Core / Actor v2](./responses-ws-settlement-core-actor-v2.md) 与 [ResponsesWS Transport 边界重构方案](./responses-ws-transport-boundary.md)。这是把账务决策和 provider transport/evidence 边界从 actor 叙述中独立出来。 | 先读 [ResponsesWS 架构说明](./responses-ws-architecture.md)，再读 settlement 与 transport 两篇专项 |
| 2026-06-25 | `526354b9` | 新增 [ResponsesWS Attempt Replay 架构设计方案](./responses-ws-attempt-replay-architecture.md)，把 HTTP / native WS / HTTP bridge 的 request-level rejection retry 收敛成 attempt replay protocol。 | [ResponsesWS Attempt Replay 架构设计方案](./responses-ws-attempt-replay-architecture.md) |
| 2026-06-28 | `42b03d6c` | 新增 [Codex / PI OAuth 请求 Header 画像对照](./codex-pi-header-parity.md)，作为 Codex upstream parity 的诊断输入。 | [Codex / PI OAuth 请求 Header 画像对照](./codex-pi-header-parity.md) |
| 未提交 | 当前工作区 | 新增 [Codex Official Upstream 架构设计](./codex-official-upstream-architecture.md)，吸收并修正 Codex / PI header parity 诊断，形成 Codex provider official upstream 目标方案。 | [Codex Official Upstream 架构设计](./codex-official-upstream-architecture.md) |

阅读规则：

- 替代关系中只把后继文档作为当前实现入口；前序文档只保留历史上下文。
- 细化关系中先读主文档，再读专项文档；专项文档的 contract 优先于主文档里较早的泛化描述。
- 诊断到目标方案的关系中，诊断文档只说明事实画像和差异；真正的实现 contract 以目标方案为准。

## 当前文档索引

| 文档 | 主题 | 说明 |
| --- | --- | --- |
| [Channel Affinity 架构设计方案](./channel-affinity-architecture.md) | 渠道路由、responses affinity、Codex realtime affinity | 当前 routing / affinity 架构说明 |
| [Billing / Usage 结算架构](./billing-settlement-architecture.md) | usage / settlement / finalize / ResponsesWS conservative billing | 当前统一结算架构说明；ResponsesWS 采用保守有界计费 |
| [ResponsesWS Settlement Core / Actor v2](./responses-ws-settlement-core-actor-v2.md) | ResponsesWS defensive settlement core、trace、actor 账务边界 | ResponsesWS 账务主规格；`ExpectedFinalQuota` 是真实扣费唯一金额来源 |
| [ResponsesWS 架构说明](./responses-ws-architecture.md) | `/v1/responses` WebSocket、actor、quota、upstream snapshot、conservative billing | 当前 ResponsesWS ingress 架构说明；计费口径是不少计费、允许有界小幅多计费、不追求事务级精确 |
| [ResponsesWS Attempt Replay 架构设计方案](./responses-ws-attempt-replay-architecture.md) | HTTP / ResponsesWS request-level rejection replay、attempt barrier、rollback-before-retry | 当前 attempt replay 协议说明；只在 pre-accept、pre-visibility、可 rollback 的边界内重放 |
| [ResponsesWS Transport 边界重构方案](./responses-ws-transport-boundary.md) | ResponsesWS native WS / HTTP bridge transport、provider adapter 边界 | 主体边界已进入当前实现；保留 v1 迁移顺序和复审修复记录 |
| [Codex / PI OAuth 请求 Header 画像对照](./codex-pi-header-parity.md) | Codex / PI 自身 OAuth header 画像、one-hub 中转差异 | 用于后续 Codex provider header parity 修复和回归测试设计 |
| [Codex Official Upstream 架构设计](./codex-official-upstream-architecture.md) | Codex provider raw envelope、official header/body planner、ResponsesWS native upstream | 目标架构；一次性干净重构，不设 legacy/typed/bridge 运行时兼容态 |
| [WebSocket Transport 复用方案](./websocket-transport-architecture.md) | `/v1/realtime` 与 ResponsesWS 的旧底层 I/O 复用 | 历史方案；已被 wsconn 唯一传输边界取代 |
| [wsconn 唯一传输边界架构方案](./wsconn-architecture.md) | `common/wsconn` 作为唯一 WebSocket 传输边界 | 当前实现；业务层不再持有 `*websocket.Conn` |
| [one-hub Async Task 架构设计](./task-coordination-architecture.md) | async task、identity、fetch、sweeper、finalize | 当前异步任务架构说明 |
| [Execution Session Revocation 架构设计方案](./execution-session-revocation-refactor.md) | `runtime/session` 锁边界、revocation、Sweep、容量回收 | 当前 session manager revocation 架构说明 |
| [Relay 压测脚本](./relay-performance-benchmark.md) | 热路径压测工具与口径 | 独立保留 |

## 当前现状

| 文档 | 当前状态 | 说明 |
| --- | --- | --- |
| [Channel Affinity 架构设计方案](./channel-affinity-architecture.md) | 当前实现 | 当前代码已按该方案收敛，用于解释现有 routing / affinity 行为 |
| [Billing / Usage 结算架构](./billing-settlement-architecture.md) | 当前实现 + 目标约束 | `Quota -> SettlementEnvelope -> ApplySettlement` 是统一结算主链路；ResponsesWS uncertain/no-terminal 使用 preconsume floor |
| [ResponsesWS Settlement Core / Actor v2](./responses-ws-settlement-core-actor-v2.md) | 当前实现 + 目标约束 | settlement input/decision/applied trace 是 ResponsesWS 账务审计主规格；actor 不根据 diagnostic reason 改钱 |
| [ResponsesWS 架构说明](./responses-ws-architecture.md) | 当前实现 + 目标约束 | `GET /v1/responses` WebSocket ingress、actor/attempt、upstream snapshot、专用 upstream capability、conservative billing 和 actor v2 数据结构 |
| [ResponsesWS Attempt Replay 架构设计方案](./responses-ws-attempt-replay-architecture.md) | 当前实现 | request-level rejection replay 已按 attempt barrier、rollback-before-retry 和 downstream visibility barrier 落地 |
| [ResponsesWS Transport 边界重构方案](./responses-ws-transport-boundary.md) | 当前实现 + 目标约束 | 把 native WS / HTTP bridge transport 从 Codex realtime 语义中拆出；主体边界已进入当前实现，文档保留复审修复记录 |
| [Codex / PI OAuth 请求 Header 画像对照](./codex-pi-header-parity.md) | 当前诊断 | 记录 Codex / PI 本体 HTTP/WS header 画像，以及 one-hub 当前中转后的缺失、额外和逻辑不一致字段 |
| [Codex Official Upstream 架构设计](./codex-official-upstream-architecture.md) | 目标方案 | Codex provider 以 raw envelope contract 和 official planner 作为唯一协议边界；删除 legacy header、typed upstream contract、WS bridge fallback 和 model_headers override |
| [WebSocket Transport 复用方案](./websocket-transport-architecture.md) | 历史方案 | 原 primitives-only safety primitives 路线，已被 `common/wsconn` 唯一传输边界取代 |
| [wsconn 唯一传输边界架构方案](./wsconn-architecture.md) | 当前实现 | `common/wsconn` 是唯一 WebSocket 传输边界；业务层不再 import gorilla，CloseInfo first-write-wins，PongMiss/Idle 语义拆分 |
| [one-hub Async Task 架构设计](./task-coordination-architecture.md) | 当前实现 | `tasks` 行、settlement snapshot、local fetch、sweeper、finalize 已形成稳定边界 |
| [Execution Session Revocation 架构设计方案](./execution-session-revocation-refactor.md) | 当前实现 | `runtime/session` revocation 锁外化、批量 sweep 检查与 Codex execution session timeout 配置已落地 |
| [Relay 压测脚本](./relay-performance-benchmark.md) | 工具文档 | 对应 `hack/bench/relay_bench.go`，用于热路径压测与指标对照 |

## 目录

- [文档收敛说明](#文档收敛说明)
- [文档状态口径](#文档状态口径)
- [文档递进关系](#文档递进关系)
- [当前文档索引](#当前文档索引)
- [当前现状](#当前现状)
- [本地构建](#本地构建)
  - [环境配置](#环境配置)
  - [编译流程](#编译流程)
  - [运行说明](#运行说明)
- [Docker 构建](#docker-构建)
  - [环境配置](#环境配置-1)
  - [编译流程](#编译流程-1)
  - [运行说明](#运行说明-1)

## 本地构建

### 环境配置

你需要一个 golang 与 yarn 开发环境

#### 直接安装

golang 官方安装指南：https://go.dev/doc/install \
yarn 官方安装指南：https://yarnpkg.com/getting-started/install

#### 通过 conda/mamba 安装 （没错它不只能管理 python）

如果你已有[conda](https://docs.conda.io/projects/conda/en/latest/user-guide/install/index.html)或者[mamba](https://github.com/conda-forge/miniforge)的经验，也可将其用于 golang 环境管理：

```bash
conda create -n goenv go yarn
# mamba create -n goenv go yarn # 如果你使用 mamba
```

### 编译流程

项目根目录已经提供了本地构建的 makefile

```bash
# cd one-hub
# 确保你已经启动了开发环境，比如conda activate goenv
make all
# 更多 make 命令，详见makefile
```

编译成功之后你应当能够在项目根目录找到 `dist` 与 `web/build` 两个文件夹。

### 运行说明

运行

```bash
$ ./dist/one-api -h
Usage of ./dist/one-api:
  -config string
        specify the config.yaml path (default "config.yaml")
  -export
        Exports prices to a JSON file.
  -help
        print help and exit
  -log-dir string
        specify the log directory
  -port int
        the listening port
  -version
        print version and exit
```

根据[使用方法](/use/index)进行具体的项目配置。

## Docker 构建

### 环境配置

你需要 docker 环境，列出下列文档作为安装参考，任选其一即可：

- MirrorZ Help，此为校园网 cernet 镜像站：https://help.mirrors.cernet.edu.cn/docker-ce/
- docker 官方安装文档：https://docs.docker.com/engine/install/

### 编译流程

项目根目录已经提供了 docker 构建的 dockerfile

```bash
# cd one-hub
docker build -t one-hub:dev .
```

编译成功后，运行

```bash
docker images | grep one-hub:dev
```

你应当能找到刚刚编译的镜像，注意与项目官方镜像区分名称。

当然你也可以选择修改 Dockerfile，使用 `docker compose build` 进行编译。

### 运行说明

项目根目录提供了一份 [`docker-compose.yaml`](https://github.com/MartialBE/one-hub/blob/main/docker-compose.yml) 文件。你应当根据上一步 `docker build` 时采用的镜像名称进行修改，比如将`martialbe/one-api:latest`替换`one-hub:dev`。当然你也可以直接利用 `docker compose` 进行 build：

```yaml
image: martialbe/one-api:latest
```

替换为

```yaml
build:
  dockerfile: Dockerfile
  context: .
```

然后进行 `docker compose build` 即可。
