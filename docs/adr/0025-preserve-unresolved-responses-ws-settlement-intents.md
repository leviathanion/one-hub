---
status: superseded by ADR-0030
---

# 保留未决 ResponsesWS 结算 intent

ADR-0030 取代本决策并移除 unresolved intent：没有权威 provider usage 的 turn 现在以零最终费用 Cancel 其预扣。

没有 `response.completed`、`response.failed` 或 `response.incomplete` 的 Responses WebSocket provider close、EOF、malformed event、timeout 或下游取消不是最终 usage 证据。因此 relay 保留强制预扣为配额 truth，并存储一个以 turn attempt ID 为键的有界、会过期的 unresolved intent，包含 response ID、owner/channel、观察到的 usage、floor 和拟结算金额。在权威证据由未来 reconciliation 路径提供之前，它不进行最终结算。

这有意不新增通用计费账本或自动 reconciler。永久应用部分 usage 或 floor 被拒绝，因为它破坏了 transport loss 与 provider completion 的区分；退款被拒绝，因为 provider work 可能已经存在。记录 30 天后过期，并与既有生命周期清理任务共享，因此保留的证据只有一个 SQL owner 和有界生命周期。

Intent 插入在 turn attempt ID 上幂等，并在冲突时比较完整持久证据载荷。actor 只重试已分类的 transient MySQL、PostgreSQL、SQLite、bad-connection 和网络失败，在既有 detached lifecycle deadline 内使用两次短 backoff。最终持久化失败递增低基数 outcome metric，保持预扣不变，绝不发明 terminal settlement。这关闭了短暂的数据库失败窗口，而无需新增无界 worker 或改变保守计费策略。
