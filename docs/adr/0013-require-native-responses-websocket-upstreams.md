---
status: accepted
---

# 要求原生 Responses WebSocket upstream

客户端 Responses WebSocket 请求只路由到显式声明具备 Native Responses WebSocket operation 能力的 channel。Native 指真实上游 WebSocket，而不是 HTTP/SSE bridge；它本身不承诺 exact-wire 应用事件。已注册 provider adapter 可以在其 native Responses operation 使用 provider-specific discriminator 时执行窄而显式的生命周期映射，但只针对有损公共表示的状态。不支持的 channel 组合和请求形状在 provider work 前失败；work 之后观察到无法表示的 provider 结果会 fail closed，不重放。既有 HTTP adapter 保留其既有行为，因为本决策特定于 WebSocket transport。这放弃了 HTTP-only provider 的 bridge 覆盖，同时把 provider dialect 差异留在 adapter 边界。
