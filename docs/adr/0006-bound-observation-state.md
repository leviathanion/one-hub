---
status: superseded by ADR-0011, ADR-0012, ADR-0014, ADR-0018
---

# 有界 observation 状态

> GPT-5.6 修复由 ADR-0011、ADR-0012、ADR-0014 和 ADR-0018 取代。新方案可以保留有界 wire 投影，但不保留本文的持久 observation identity、首模型锁定或 observation-led accounting 设计。

> ADR-0030 和 ADR-0034 取代本文的计费决定：只有 Authoritative Provider Usage 才能 Confirm 收费，每个计费阶段读取当前完整价格发布，而不是保留准入时的策略。

Relay observation 保持有界内存，绝不为了无界解析或持久记账而延迟 provider 载荷交付。后续普通 terminal 事件可以在超大非 terminal 事件之后恢复证据，但单个超大 terminal 可能保持未知并丢失本地 observation 计费。这个罕见失败被接受，以替代磁盘暂存、无界解析器或持久 outbox；交付失败仍通过 metrics 和日志可观察。

对 Native Responses WebSocket 透传，解析出的 provider terminal 即使向下游客户端交付失败也会入队。如果 provider 连接在携带稳定 response identity 的 terminal 到达前关闭，relay 不制造持久 turn 记录；连接诊断覆盖该罕见情况，因为持久 incomplete-turn 关联需要 attempt journal。Observation identity 是 channel ID 加 provider response/resource ID，因此不同 wire 投影可以去重，而极端 provider ID 复用会碰撞。既有 usage 派生的 identity 不回填，因此升级后首次重新观察旧 provider 资源可能创建一行 canonical row。

当 provider 返回不同模型名时，在结算时解析显式配置的 exact 或 wildcard 价格。缺失的 actual-model 价格可以使用请求模型当前的显式策略；不捏造全局兜底价格。并发目录更新可能被后续计费阶段观察到，正如 ADR-0034 所接受的那样。

本文原先允许无 token usage 的固定费用推断，该计费决定已由 ADR-0030 取代：缺少 Authoritative Provider Usage 时必须 Cancel。Native Responses WebSocket 的连接模型约束不受此替代影响。
