# Relay 压测脚本

用于验证 `/v1/chat/completions` 与 `/v1/responses` 热路径优化是否生效。

脚本位置：

- `hack/bench/relay_bench.go`

## 能力

- 并发压测 `chat` / `responses` 接口。
- 统计客户端观测到的首包、首 token、总耗时。
- 在压测前后抓取 `/api/metrics`，输出本次运行的：
  - `request_parse_ms`
  - `provider_select_ms`
  - `prompt_count_ms`
  - `upstream_header_ms`
  - `ttft_ms`
- 按 `path` 与 `transform_mode` 维度展示增量结果。

## 推荐口径

建议固定三种口径，避免每轮结果不可比：

- 冒烟验证：
  - `requests=200`
  - `concurrency=32`
  - `warmup=10`
- 标准基线：
  - `requests=4000`
  - `concurrency=1024`
  - `warmup=50`
- 高并发扫描：
  - `requests=20000`
  - `warmup=100`
  - `concurrency` 取 `1536 / 2048 / 4096 / 8192 / 16384 / 32768`

当前结果文档默认以“标准基线”作为主表，以“高并发扫描”作为附录。

## 快速开始

### 1. 零配置自举压测

默认会自动启动：

- 本地 mock upstream
- 本地 bench target
- 无认证 `/api/metrics`

直接执行即可：

```bash
go run ./hack/bench -requests 500 -concurrency 32
```

### 2. 压测 `chat/completions`

```bash
go run ./hack/bench \
  -self-hosted=false \
  -base-url http://127.0.0.1:3000 \
  -api-key sk-your-token \
  -metrics-user your-metrics-user \
  -metrics-password your-metrics-password \
  -scenario short-chat \
  -requests 500 \
  -concurrency 32
```

### 3. 压测 `responses`

```bash
go run ./hack/bench \
  -self-hosted=false \
  -base-url http://127.0.0.1:3000 \
  -api-key sk-your-token \
  -metrics-user your-metrics-user \
  -metrics-password your-metrics-password \
  -scenario responses-native \
  -requests 500 \
  -concurrency 32
```

### 4. 用自定义请求体压测

```bash
go run ./hack/bench \
  -self-hosted=false \
  -base-url http://127.0.0.1:3000 \
  -api-key sk-your-token \
  -metrics-user your-metrics-user \
  -metrics-password your-metrics-password \
  -path /v1/chat/completions \
  -request-file ./tmp/bench-chat.json \
  -requests 300 \
  -concurrency 16
```

## 常用参数

- `-scenario`
  - `short-chat`
  - `long-chat`
  - `tool-heavy`
  - `responses-native`
- `-stream`
  默认 `true`。流式请求时会统计客户端 TTFT。
- `-warmup`
  正式压测前的预热请求数，默认 `5`。
- `-qps`
  总体限速；`0` 表示不限速。
- `-timeout`
  单请求超时，默认 `2m`。
- `-request-file`
  直接读取 JSON 请求体，优先级高于 `scenario`。
- `-self-hosted`
  默认 `true`。开启后不需要 token，也不需要 metrics 认证。

## 建议场景

- `short-chat`：短上下文、短输出。
- `long-chat`：长上下文、短输出。
- `tool-heavy`：大 tool schema 请求。
- `responses-native`：`/v1/responses` 原生路径。

## 推荐执行顺序

1. 先跑 `short-chat` 与 `responses-native`，确认主链路没有退化。
2. 再跑 `long-chat` 与 `tool-heavy`，观察大请求体和大 schema 的本地开销。
3. 如果基线结果稳定，再补高并发扫描。
4. 将结果整理到单独结果文档，并注明日期与命令口径。

## 说明

- 默认模式会自动启动本地压测目标，可直接执行。
- 如果你要压真实服务，请显式加 `-self-hosted=false`。
- 不依赖 `wrk`、`k6`、`vegeta` 之类的外部压测工具。
