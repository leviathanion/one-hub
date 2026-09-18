---
status: accepted
---

# 把 native WebSocket liveness 与 Response 生命周期分离

> ADR-0030 取代本文的 unresolved settlement 结果；liveness 与 provider lifecycle 分离的决定继续有效。

Native Responses WebSocket 连接建立和 transport 保留 handshake、ping/pong、读和写 deadline。为限制长期持有的凭据和队列内存，provider 不活动清理默认两分钟，连接生命周期默认一小时；运营商可以在上游工作负载需要更宽松限制时把任一值设为零。这些 watchdog 关闭 relay，但不伪造 provider terminal；关闭时由 ADR-0033 对已归属 evidence 的原子 Price Component 执行唯一 Settlement Decision。
