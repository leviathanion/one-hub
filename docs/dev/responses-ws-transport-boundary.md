---
title: "ResponsesWS Native Transport 边界"
layout: doc
outline: deep
lastUpdated: true
---

# ResponsesWS Native Transport 边界

## 文档状态

- 状态：当前实现；原始交付与最小观察已分离。
- 范围：relay actor 与 provider-native Responses WebSocket adapter 之间的 transport/evidence contract。
- 非范围：HTTP/SSE bridge、Realtime session、跨协议事件转换。

共享规则以[Responses 透明转发与计费边界设计](./responses-transparent-relay-design.md)为准；本文记录 I/O、发送结果和资源交接契约。

## 唯一 upstream contract

relay 只消费 `common/responsesws.Upstream`：

```go
type Upstream interface {
    SendClientWithResult(context.Context, SendRequest) ResponsesWSTransportSendResult
    Recv(context.Context) (UpstreamEvent, error)
    Abort(reason string)
}
```

`OpenRequest` 显式携带首个 raw create、选中 model、principal/header snapshot、channel 和诊断 hook。adapter 可以做 provider 握手、最小 envelope 校验、usage/close evidence 提取，但不能选路、预扣、持久化 owner 或 finalize turn。

## Frame 所有权

`responsesws.Frame` 在构造时复制 payload，读取时返回副本，避免 I/O goroutine 与 actor 共享可变字节。公共 Responses wire 的事实源始终是 raw frame；typed projection 只服务本地策略和 evidence 提取。

native transport 不改写 `response.create.model`，不删除未知字段，不生成 lifecycle event。provider 事件的交付经过来源、安全及资源检查，未知观察值只限制相应本地决定。inject/steer 控制回执在 adapter 修改当前执行状态前被隔离。

## Send result

Native WS send 结果只有三种：

| status | 语义 | actor 行为 |
| --- | --- | --- |
| `not_attempted` | 明确没有进入底层 write | 不重放；closure 时按 usage Confirm/Cancel |
| `attempted` | write 成功返回 | pending turn 进入 active |
| `ambiguous` | 已进入 write 且返回错误，无法证明 provider 是否收到 | create 已有可关联的 provider 事件时继续观察原执行，否则关闭；辅助发送歧义关闭。无 usage 时 Cancel |

未知或字段组合非法的 send result 是 transport contract violation，fail closed。provider request-level `error` 通过 `Recv` event path 到达 actor，不塞进 send result，也不转换成 terminal。

关闭 actor 会停止 mailbox 投递，按已接收的 provider usage 一次 Confirm/Cancel。关闭专用的 completion channel 和 100ms 等待已删除；正常 SendResult、辅助命令一次完成 Receipt 及 open Adopted 仍独立承担结果消费和资源清理。发送结果不授予重放权。

每条 inject/steering command 另有独立的一次消费标识；候选释放后，原发送仍负责自己的结果。关闭与各新工作阶段共用 `postMu` 短锁许可，锁外进行 SQL/I/O；关闭排空只接收事实，不重新准入客户端工作。晚到 open 结果先登记清理责任并确认 `Adopted`，再使用或清理资源；`done` 不把清理权转回 worker。

## Receive evidence

`UpstreamEvent` 分离：

- 可交付的 provider frame；
- provider usage；
- provider close；
- attempt/response correlation；
- typed detail origin/phase；
- adapter 或 transport error。

接收事件的 transport attempt ID 只在 create 进入发送时更新，以承接先于 SendResult 到达的事件。辅助发送使用自己的完成关联，不能覆盖该接收 ID；已绑定的上游 Response ID 优先用于业务证据归属。物理关闭由 channel/generation 确定，不依赖当前业务 attempt。

inject/steer 控制回执先原样交付，再观察仍未结束的本地决定。旧父回执不修改当前 child 的序号、Response ID 或用量。`A terminal → B create → inject(A) → B 无 ID delta` 已纳入真实 native transport 回归。

I/O pump 只负责把这些事实可靠投递给 actor。pending write 结果未确认前，provider event 写入有事件数和字节数上限的 journal；超限时保留已有 evidence 并 fail closed，不能把已观察到的 provider 活动退化成 zero-charge。

## Provider adapter 要求

- OpenAI/Azure 与 Codex 各自实现 native opener，不复用 `/v1/realtime` session actor。
- client frame 至少校验合法 object、唯一 `type` 和支持的 event 类型。
- provider frame 只校验本地 envelope；资源/执行关联由 relay 检查，私有方言由 adapter 映射，纯观察字段不构成原帧交付门禁。
- adapter panic 在 native session 边界恢复，记录安全诊断并关闭 transport；不能把 panic 原值回显给客户端。
- provider peer close 与本地 abort 分开建模，任何 close 都不能自动成为 `response.completed`。
- `Abort` 是连接级停止，不向 provider发送 Realtime `response.cancel`。

## 配置边界

Native 是唯一的 Responses WS transport。`responses_ws_transport` 和 `capabilities` 已不属于受支持的配置，更新时应从 `channel.other` 移除，不保留回滚兼容分支。`responses_ws_native` 与 `responses_ws_self_hosted` 仍由该 JSON 唯一拥有，通过后台 Other(JSON) 编辑，不维护镜像开关。需要 HTTP 的客户端直接调用普通 HTTP Responses。

底层 socket deadline、ping/pong、read limit 与 close first-write-wins 由 `common/wsconn.ManagedConn` 统一实现，详见 [wsconn 架构](./wsconn-architecture.md)。
