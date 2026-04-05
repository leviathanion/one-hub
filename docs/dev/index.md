# 开发与贡献

## 文档收敛说明

`docs/dev` 里原先有多份渠道亲和与 usage 设计文档，主题重复，而且取舍分叉。

现在把它们收敛成四份主题文档与一份独立工具文档：

| 文档 | 主题 | 说明 |
| --- | --- | --- |
| [Channel Affinity 统一方案](./channel-affinity-architecture.md) | 渠道路由、responses affinity、Codex realtime affinity | 以当前代码实际采用的方案为准 |
| [Billing / Usage 结算统一方案](./billing-settlement-architecture.md) | usage / settlement / finalize | 在多份提案中选出的最终推荐方案 |
| [one-hub Async Task 极简协调方案](./task-coordination-architecture.md) | async task、identity、fetch、sweeper、finalize | 统一说明 one-hub 当前 async task 的能力边界、数据模型与落地取舍 |
| [Execution Session Revocation 并发修复方案](./execution-session-revocation-refactor.md) | `runtime/session` 锁边界、revocation、Sweep、容量回收 | 说明 session manager revocation 检查的最终重构方向 |
| [Relay 压测脚本](./relay-performance-benchmark.md) | 热路径压测工具与口径 | 独立保留 |

## 当前现状

| 文档 | 当前状态 | 说明 |
| --- | --- | --- |
| [Channel Affinity 统一方案](./channel-affinity-architecture.md) | 已选型 | 当前代码已按该方案收敛，用于解释现有 routing / affinity 行为 |
| [Billing / Usage 结算统一方案](./billing-settlement-architecture.md) | V1 主干部分实现 | 现状与后续收敛目标共用同一份文档，后续实现继续以本文边界为准 |
| [one-hub Async Task 极简协调方案](./task-coordination-architecture.md) | V0.9 收敛方案 | 当前目标仍是最小 task 协调，不是完整 task coordinator |
| [Execution Session Revocation 并发修复方案](./execution-session-revocation-refactor.md) | 已实现并接入主流程 | `runtime/session` revocation 锁外化、批量 sweep 检查与 Codex execution session timeout 配置已落地 |
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
