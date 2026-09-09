# Separate native WebSocket liveness from Response lifecycle

> ADR-0030 取代本文的 unresolved settlement 结果；liveness 与 provider lifecycle 分离的决定继续有效。

Native Responses WebSocket connection setup and transport retain handshake, ping/pong, read, and write deadlines. To bound long-held credentials and queue memory, provider-inactivity cleanup defaults to two minutes and connection lifetime defaults to one hour; operators may set either value to zero when the upstream workload requires a looser limit. These watchdogs close the relay，但不伪造 provider terminal；closure 时由 ADR-0033 对已归属 evidence 的原子 Price Components 执行唯一 Settlement Decision。
