---
title: "Responses WebSocket 架构"
layout: doc
outline: deep
lastUpdated: true
---

# Responses WebSocket 架构

## 文档状态

- 状态：当前实现。
- 范围：`GET /v1/responses` WebSocket ingress、Native Responses WebSocket upstream、turn actor、Stored Response owner barrier 和结算边界。
- 非范围：普通 HTTP Responses、`/v1/realtime`、WS 到 HTTP/SSE 的协议转换。

Steering 的计费映射、回执观察与资源证明生命周期见[透明转发与计费设计](./responses-ws-steering-lifecycle-design.md)。该文档是本文的 steering 专项契约；本地回归不代替真实上游协议验证。

## 核心结论

Responses WebSocket 只连接显式具备 native 能力的渠道。系统不把 Responses WS 转成 HTTP/SSE，也不把 Realtime 事件转换成 Responses 事件。没有合格渠道时，在 provider work 前返回 `426 responses_ws_unsupported_for_channel`。

连接内只有一条 native upstream，渠道固定；每个 turn 独立使用客户端原始 `model`、`store`、continuation 和工具门禁，并在计费阶段读取当前完整价格发布。渠道不能承载该精确 model 时直接失败，不切换渠道、不做 model mapping。

## 协议事件模型

- Responses HTTP 非流式返回单个 Response JSON，不存在 server event lifecycle。
- Responses HTTP 流式通过 SSE 发送 Responses Streaming Event Model。
- Responses WebSocket 使用不同传输，但 server events 与 ordering 仍遵循同一个 Responses Streaming Event Model。
- Realtime 是独立的 session 协议，使用 `response.done` 及其嵌套 status，不是 Responses WebSocket 的兼容别名。
- Codex 等 provider 的私有 supplier event 只能由对应 adapter 解释；adapter 对公共 Responses WebSocket 输出负责，私有事件名不能进入通用 classifier。

因此 Realtime terminal classifier、Responses WebSocket terminal classifier 和 provider-private lifecycle mapping 分属三个边界，不能通过共享 alias switch 合并。官方 Responses WebSocket 语义依据 [OpenAI WebSocket Mode](https://developers.openai.com/api/docs/guides/websocket-mode)，Realtime 语义依据 [OpenAI Realtime server events](https://platform.openai.com/docs/api-reference/realtime-server-events/input_audio_buffer/committed?lang=node)。

代码位置也按职责而非连接类型划分：`common/realtime` 与 `common/responsesws` 分别解释两个公开 wire event model；`runtime/realtime` 只提供长连接 session、frame 和 transport contract，不解析 JSON lifecycle。Realtime 的通用 `error` 事件多数可恢复，不结束 response；只有 `response.done` 结束该 response，缺失或未知 status 记录为未知结果而不是猜测成功。仅当 `error.error.event_id` 明确关联尚未创建出 response 的当前 `response.create` 时，session 才把它作为 create rejection 释放本地 admission。

## 边界划分

| 边界 | 职责 | 不负责 |
| --- | --- | --- |
| ingress | 认证、token policy、容量、首帧和 client event 校验 | provider 协议转换 |
| capability / routing | native、exact-model、OpenAI Data Residency、Stored lifecycle 门禁；首 turn 选路 | 已写入 upstream 后换渠道 |
| provider adapter | native 握手、原始 frame 转发、usage/close evidence 提取 | turn 调度、owner、扣费 |
| I/O pump | 双向搬运 frame，把 provider evidence 串行投递给 actor | 构造 provider lifecycle event |
| session actor | FIFO、turn 状态、owner delivery barrier、terminal/sequence 校验、结算触发 | 修改 provider terminal |
| settlement core | 根据 transport 与 provider evidence 计算唯一 final quota | 决定协议输出 |

## 连接与 turn 流程

```text
认证与容量门禁
  → 读取首个 response.create
  → capability / owner / exact-model 校验
  → 选择渠道并建立 native upstream
  → 为 turn 做 RPM 与 quota admission
  → 原样发送 response.create
  → provider frame 先经 actor 校验，再交付客户端
  → 官方 terminal 先交付，随后结算和记录
  → inject ack barrier 完成后推进 FIFO 下一 turn
```

连接建立前的候选失败仍可使用既有候选预算；一旦任何 `response.create` 进入 upstream write，就不跨渠道重放。普通 HTTP create 的历史 retry 不受此限制。

## Client event 与排队

- 支持 `response.create` 和针对当前活动 Response 的原始 `response.steer`；自动后继复用事前准入的独立预扣。
- `multi_agent.enabled=true` 的 turn 支持 beta `response.inject`；frame 原样转发。
- `response.cancel` 是 Realtime 事件，在 Responses WS 上拒绝。客户端断开通过关闭 upstream transport 终止工作。
- opening、pending、active turn 或尚未绑定的自动续接预扣存在时，新的 `response.create` 进入有界 FIFO。队列同时限制 frame 数和 payload bytes；超限 fail closed。
- actor mailbox 对所有在途 client/provider frame 使用每连接 64 MiB 的共享 payload 预算，事件出队后立即释放。
- terminal 前后收到的 `inject.created` / `inject.failed` 都会减少 pending 计数。客户端已提交的 inject 继续按序发送；provider terminal 只阻止新的 inject，turn 要等 pending acknowledgement 清零后才释放。

## Provider lifecycle contract

只有以下事件是 Responses terminal：

- `response.completed`
- `response.failed`
- `response.incomplete`

每个已知 terminal 必须包含非空 `response.id` 和非负整数 `sequence_number`。同一 turn 的 provider `sequence_number` 必须严格递增。`response.done`、`response.cancelled`、`response.canceled` 属于 Realtime 语义，视为上游协议错误。

顶层 `error` 默认是 request-level error，不伪装成 response terminal；它结束并结算当前 attempt，然后在同一连接推进 FIFO。明确分类的 connection-level error 或 workflow stop 会关闭连接；自动续接尚未消解时，request-level error 也在交付后关闭，避免在歧义执行后推进 FIFO。provider close、EOF、malformed frame、timeout 和本地错误都不能合成 `response.completed`。未知未来事件可以透传，但不会被误判为 terminal。

## `store` 与 owner delivery barrier

`store` 省略或为 `true` 时，只选择同时支持 create 和完整 Stored Response lifecycle 的渠道。OpenAI Data Residency 区域端点在价格与能力契约完成前不进入候选；Azure、AWS 和 Google Cloud 的部署 region 不属于这个判定。首个携带 Response ID 的 provider frame 对客户端可见前，actor 必须把 `response_id → UserId/channel_id` 持久化；持久化失败则中止交付并停止新工作，但已归属的合法 provider evidence 先进入原 attempt，closure cut 内后续用量仍进入结算；关闭排空不重试失败的 owner 写入。

`store:false` 不写 durable owner。当前连接已经观察到的 response ID 由 actor 以最近历史或独立持有的 steering 父证明保存，对应续接不依赖数据库或 ephemeral cache；跨请求只接受用户域内、有限 TTL 的 ephemeral proof，它只是来源证明和 soft preference，不能代替 durable owner。

后续 Stored Response 操作严格固定 owner channel，不 fallback。普通用户无法区分 owner miss、跨用户、tombstone 与过期记录。

## 结算与交付顺序

每个 turn 独立预扣；预扣与结算分别读取当时的当前完整价格发布，允许观察不同版本。provider terminal 必须先交付客户端，再执行结算和非 wire 副作用；terminal 交付后的结算失败只能记录为内部失败，不能用本地 error 替换 terminal。

存在可归属、合法的 provider usage 时按实际 usage Confirm；没有 usage 时一律 Cancel 并释放预扣。transport 结果只决定是否禁止重放和何时关闭 turn，不构成收费证据。

## Liveness

- 首帧 timeout 只约束 Upgrade 后等待第一个 `response.create`。
- idle timeout 只清理没有 opening、pending、active turn 或未绑定自动续接的业务空闲连接。
- active-turn provider-inactivity 默认 2 分钟，max-lifetime 默认 1 小时，均可设为 0 禁用；触发后关闭代理 turn；若仍无 provider usage，则 Cancel 预扣，不产生 provider terminal。
- 同一个 active watchdog 随当前 Response、未绑定自动候选和实际 child 切换目标及 generation；仅保留父资源证明时按 idle 管理，迟到旧父回执不刷新 child 的期限。
- ping/pong、read limit、write deadline 由共享 `wsconn.ManagedConn` 边界执行。

## 配置和运维

用户可配置项、默认值、迁移方式与容量说明见 [Responses WebSocket 使用文档](../use/responses-ws.md)。长期决策见 [ADR-0013](../adr/0013-require-native-responses-websocket-upstreams.md)、[ADR-0014](../adr/0014-preserve-native-responses-websocket-turn-semantics.md) 和 [ADR-0016](../adr/0016-separate-native-ws-liveness-from-response-lifecycle.md)。
