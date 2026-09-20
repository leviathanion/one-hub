---
status: accepted
---

# 把 native WebSocket liveness 与 Response 生命周期分离

> ADR-0030 取代本文的 unresolved settlement 结果；liveness 与 provider lifecycle 分离的决定继续有效。

Native Responses WebSocket 连接建立和 transport 保留 handshake、ping/pong、读写 deadline、实际 I/O 空闲和连接总寿命限制。2026-09-20 删除单 active turn 的两分钟业务观察 watchdog：并行 work 不共享“当前响应进度”，候选归属不完整也不成为关闭连接的理由。连接 max lifetime 默认一小时，运营商可设为零；真实 I/O 限制仍按配置执行。关闭 relay 不伪造 provider terminal，由 ADR-0033 对已归属 evidence 的原子 Price Component 执行唯一 Settlement Decision。
