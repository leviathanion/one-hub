---
title: "Responses WebSocket 架构"
layout: doc
outline: deep
lastUpdated: true
---

# Responses WebSocket 架构

## 文档状态

- 状态：当前实现；包含本次透明转发与计费边界调整。
- 范围：`GET /v1/responses` WebSocket ingress、Native Responses WebSocket upstream、turn actor、Stored Response owner barrier 和结算边界。
- 非范围：普通 HTTP Responses、`/v1/realtime`、WS 到 HTTP/SSE 的协议转换。

共享接收边界见[Responses 透明转发与计费边界设计](./responses-transparent-relay-design.md)，覆盖 HTTP SSE、inject、Stored owner 失败收尾及独立计费组件。

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
| session actor | FIFO、执行归属、owner delivery barrier、最小终结观察、结算触发 | 修改 provider terminal |
| settlement core | 根据合法 provider evidence 计算唯一 final quota | 决定协议输出 |

## 连接与 turn 流程

```text
认证与容量门禁
  → 读取首个 response.create
  → capability / owner / exact-model 校验
  → 选择渠道并建立 native upstream
  → 为 turn 做 RPM 与 quota admission
  → 原样发送 response.create
  → provider frame 经安全与资源屏障交付，独立提取合法证据
  → 官方 terminal 先交付，随后结算和记录
  → 父执行结束后推进 FIFO；未消解 steering 后继继续阻挡
```

连接建立前的候选失败仍可使用既有候选预算；一旦任何 `response.create` 进入 upstream write，就不跨渠道重放。HTTP create 的工作后重放同样被禁止，详见[Responses 请求重试边界](./responses-ws-attempt-replay-architecture.md)。

## Client event 与排队

- 支持 `response.create` 和针对当前活动 Response 的原始 `response.steer`；自动后继复用事前准入的独立预扣。
- `response.inject` 在目标资源授权及输入资源检查后按原始 frame 发送；上游校验 multi-agent 模式和工具 schema，并决定活动或已完成目标的注入结果。
- `response.cancel` 是 Realtime 事件，在 Responses WS 上拒绝。客户端断开通过关闭 upstream transport 终止工作。
- opening、pending、active turn 或尚未绑定的自动续接预扣存在时，新的 `response.create` 进入有界 FIFO。队列同时限制 frame 数和 payload bytes；超限 fail closed。
- actor mailbox 对所有在途 client/provider frame 使用每连接 64 MiB 的共享 payload 预算，事件出队后立即释放。
- inject 回执不拥有父账务或 create FIFO。已提交辅助命令的发送资源及一次完成独立于父 reset，父 terminal 后的回执仍可交付；首次真实辅助发送歧义停止连接。

## Provider lifecycle contract

只有以下事件是 Responses terminal：

- `response.completed`
- `response.failed`
- `response.incomplete`

Response ID 用于资源和账务关联；缺失、重复或非递增序号不构成交付门禁。未识别的事件名保留原帧，不猜测其终结意义；Codex 私有别名由 adapter 根据明确契约转换，公共事件不补造序号。

泛化 `error` 原样交付，不结束当前 Response 或提前释放 steering 预扣。明确 connection fatal / workflow stop 停止新工作并收尾；当前尚未绑定 Response 的 create 收到明确 `previous_response_not_found` 且本次确有续接目标时，按 create 拒绝结束准入并推进 FIFO。其他无关联错误等待原执行或连接事实。未知事件、安全诊断和迟到回执不需要本地完整生命周期接受才能交付。

单命令的发送完成与当前 create 接收关联分离，辅助发送不能覆盖新 create 的无 ID 输出归属。关闭时按 `postMu` 固定截止数量收尾；不再等待 100ms 发送结果，也不保留 inject 回执计数、恢复对象或完整终结响应历史。HTTP SSE 读至 EOF 的原始交付、异常 abort/reset 及上下游时限见[共享设计](./responses-transparent-relay-design.md)。

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
