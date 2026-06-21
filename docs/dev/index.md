# 开发与贡献

## 文档收敛说明

`docs/dev` 里原先有多份渠道亲和、usage 与 async task 设计文档，主题重复，而且不少内容混杂了“当前实现”和“未来方案”两种口径。

现在收敛成若干份“当前架构说明”和一份独立工具文档：

| 文档 | 主题 | 说明 |
| --- | --- | --- |
| [Channel Affinity 架构设计方案](./channel-affinity-architecture.md) | 渠道路由、responses affinity、Codex realtime affinity | 当前 routing / affinity 架构说明 |
| [Billing / Usage 结算架构](./billing-settlement-architecture.md) | usage / settlement / finalize / ResponsesWS conservative billing | 当前统一结算架构说明；ResponsesWS 采用保守有界计费 |
| [ResponsesWS Settlement Core / Actor v2](./responses-ws-settlement-core-actor-v2.md) | ResponsesWS defensive settlement core、trace、actor 账务边界 | ResponsesWS 账务主规格；`ExpectedFinalQuota` 是真实扣费唯一金额来源 |
| [ResponsesWS 架构说明](./responses-ws-architecture.md) | `/v1/responses` WebSocket、actor、quota、upstream snapshot、conservative billing | 当前 ResponsesWS ingress 架构说明；计费口径是不少计费、允许有界小幅多计费、不追求事务级精确 |
| [ResponsesWS Transport 边界重构方案](./responses-ws-transport-boundary.md) | ResponsesWS native WS / HTTP bridge transport、provider adapter 边界 | 针对当前 provider 内部分叉的 v1 重构方案 |
| [ResponsesWS Provider Contract ADR](./responses-ws-provider-contract.md) | ResponsesWS provider-facing contract 决策 | 选择 `OpenResponsesWS` / `responsesws.Upstream` 作为长期 ResponsesWS provider contract |
| [WebSocket Transport 复用方案](./websocket-transport-architecture.md) | `/v1/realtime` 与 ResponsesWS 的旧底层 I/O 复用 | 旧方案说明；已被 wsconn 唯一传输边界取代 |
| [wsconn 唯一传输边界架构方案](./wsconn-architecture.md) | `common/wsconn` 作为唯一 WebSocket 传输边界 | 当前实现；业务层不再持有 `*websocket.Conn` |
| [one-hub Async Task 架构设计](./task-coordination-architecture.md) | async task、identity、fetch、sweeper、finalize | 当前异步任务架构说明 |
| [Execution Session Revocation 架构设计方案](./execution-session-revocation-refactor.md) | `runtime/session` 锁边界、revocation、Sweep、容量回收 | 当前 session manager revocation 架构说明 |
| [Relay 压测脚本](./relay-performance-benchmark.md) | 热路径压测工具与口径 | 独立保留 |

## 当前现状

| 文档 | 当前状态 | 说明 |
| --- | --- | --- |
| [Channel Affinity 架构设计方案](./channel-affinity-architecture.md) | 当前实现 | 当前代码已按该方案收敛，用于解释现有 routing / affinity 行为 |
| [Billing / Usage 结算架构](./billing-settlement-architecture.md) | 当前实现 + ResponsesWS v2 口径 | `Quota -> SettlementEnvelope -> ApplySettlement` 是统一结算主链路；ResponsesWS uncertain/no-terminal 使用 preconsume floor |
| [ResponsesWS Settlement Core / Actor v2](./responses-ws-settlement-core-actor-v2.md) | 当前实现 + 目标约束 | settlement input/decision/applied trace 是 ResponsesWS 账务审计主规格；actor 不根据 diagnostic reason 改钱 |
| [ResponsesWS 架构说明](./responses-ws-architecture.md) | 当前实现 + 待收敛设计 | `GET /v1/responses` WebSocket ingress、actor/attempt、upstream snapshot、专用 upstream capability、conservative billing 和 actor v2 数据结构 |
| [ResponsesWS Transport 边界重构方案](./responses-ws-transport-boundary.md) | 目标方案 | 把 native WS / HTTP bridge transport 从 Codex realtime 语义中拆出，供 OpenAI/Codex provider adapter 显式复用 |
| [ResponsesWS Provider Contract ADR](./responses-ws-provider-contract.md) | ADR | ResponsesWS 长期 provider contract 选择 `OpenResponsesWS(ctx, model, options)` 返回 `responsesws.Upstream` |
| [WebSocket Transport 复用方案](./websocket-transport-architecture.md) | 旧方案说明 | 原 primitives-only safety primitives 路线，已被 `common/wsconn` 唯一传输边界取代 |
| [wsconn 唯一传输边界架构方案](./wsconn-architecture.md) | 当前实现 | `common/wsconn` 是唯一 WebSocket 传输边界；业务层不再 import gorilla，CloseInfo first-write-wins，PongMiss/Idle 语义拆分 |
| [one-hub Async Task 架构设计](./task-coordination-architecture.md) | 当前实现 | `tasks` 行、settlement snapshot、local fetch、sweeper、finalize 已形成稳定边界 |
| [Execution Session Revocation 架构设计方案](./execution-session-revocation-refactor.md) | 当前实现 | `runtime/session` revocation 锁外化、批量 sweep 检查与 Codex execution session timeout 配置已落地 |
| [Relay 压测脚本](./relay-performance-benchmark.md) | 可直接使用 | 对应 `hack/bench/relay_bench.go`，用于热路径压测与指标对照 |

## 目录

- [文档收敛说明](#文档收敛说明)
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
