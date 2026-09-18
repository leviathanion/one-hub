---
status: accepted
---

# 分离协议生命周期 owner

Realtime、Responses HTTP streaming 和 Responses WebSocket 拥有独立 wire 契约，即使其事件名和 Response 对象看起来相似。Realtime 生命周期分类由 Realtime context 拥有并识别 `response.done`；Responses WebSocket 分类由 Responses WebSocket context 拥有并遵循 Responses Streaming Event Model；非流式 Responses HTTP 没有事件生命周期。provider-private supplier dialect 只在该 provider adapter 内解释，它可以在自己的契约证明无损含义时发出窄规范化的公共事件。

共享生命周期 alias 被拒绝，因为它们让导入包看起来协议中立，却静默接受外部 wire 事件。相似字符串不是兼容证据：通用 Realtime 代码不接受 Responses terminal，通用 Responses 代码不接受 Realtime terminal，provider 兼容规则不离开 provider 边界。

Wire 生命周期 owner 位于平行包边界：`common/realtime` 拥有 Realtime 事件语义，`common/responsesws` 拥有 Responses WebSocket 事件语义。`runtime/realtime` 故意限制在长生命周期 session、frame 和 transport 契约；它不解析任一公共 wire 协议。

Realtime `error` 不是 response terminal，因为大多数此类事件可恢复。session 只有在 `error.error.event_id` 匹配该 create 且没有 response 启动时，才能释放本地已准入的 `response.create`；否则它等待标准 `response.done` 或 transport 关闭。由于公共 Realtime 允许客户端省略 `response.create.event_id`，same-dialect adapter 只在字段缺失时注入 turn-scoped correlation ID，并从下游 error 事件中移除该代理拥有的值。显式客户端值和不相关错误保持不变。
