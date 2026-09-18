---
status: accepted
---

# WebSocket 生命周期事件歧义时 fail closed

> ADR-0030 保留本文的 fail-closed 与 no-replay 边界，但取代“保留或收取 quota floor”的计费决定；ADR-0033 进一步规定按原子 Price Component 判断哪些 provider evidence 可收费。

`response.create` turn 在上游写入开始之前就对 receive 路径可见。一旦进入 `WriteMessage`，错误无法证明 provider 是否收到请求，因此代理不重放同一有副作用的 create，并 fail closed 关闭连接。关闭始终遵循 ADR-0030：有 provider usage 才收费，否则释放 reservation。

没有可用 event 或 response 关联的 Realtime 顶层 `error` 仍然有歧义，fail closed 关闭连接。Native Responses WebSocket 契约不同：协议只允许一个在途 response，因此普通 request-level `error` 属于该活动 attempt；actor 原样交付它、结算并释放该 turn，然后推进有界 FIFO 而不重放。只有显式分类的 connection-level error 才关闭 Responses WebSocket。Codex attachment takeover 把已接受的出站事件转移到替代 mailbox，而 reader 代际拆除只丢弃过期 transport 事件并释放其共享字节 credit。provider terminal/error 准入和 attachment handoff 由 execution-session 锁串行化，因此旧 WS reader 在 takeover 期间无法留下永久 `Inflight` turn。

OpenAI Realtime `response.created` 可能由 provider 通过 VAD 或 idle-timeout 策略发起，而没有客户端 `response.create`。one-hub 无法在该 provider work 开始前观察到 per-turn Work Action，因此这条路径是普通 TCC 边界之外唯一的 post-work observation 结算例外。首个关联的 `response.created` 建立一个以 provider response ID 为键、可单独观察的 turn，固定连接的 principal 和执行归属，并创建没有 Submission Claim 的零预留 observation owner。来自该 turn 的 owner-correlated Authoritative Provider Usage 被归约为原子 Price Component，并按 ADR-0033 使用 ADR-0034 要求的当前完整价格发布恰好结算一次；缺失或冲突的 component 保持免费。该 owner 只记录和结算已经发生的工作，绝不授权 provider work、重放、重试、渠道 fallback 或追溯 Try。

该例外只适用于不存在匹配客户端 `response.create` 的 provider 自发 OpenAI Realtime turn。客户端发起的 `response.create` 仍是普通 Work Action，必须在上游写入前完成 Try。HTTP、Responses WebSocket、Async Task、客户端创建的 Realtime turn 以及本地推断的工作都不能复用 observation 路径。如果部署要求每个可收费 turn 都在 provider work 前完成 Try，它必须禁用 provider 自发 VAD/idle-timeout response 或自行承担其上游成本；它不能把 post-work observation 重新标为 Try。非空未知 response ID 绝不会被推断为属于另一个活动 turn。

首个 native Responses WebSocket create 写入之前，handshake 失败可以在请求捕获的重试预算内尝试另一个健康渠道。显式 channel pin 和 strict affinity 仍是单 owner 约束，最终 canonical provider error 在关闭前交付。进入 `WriteMessage` 后不允许任何渠道重试。

严格 Realtime affinity 是持久 owner 契约。临时 owner 不可用会让请求失败，但不删除绑定；非严格 affinity 仍可在 fallback 前清除失败偏好并在之后重新绑定。

Session close 只对已运行的 typed send result 等待固定 100ms grace；该等待只用于冻结 transport 事实和禁止重放，不决定最终收费。

普通队列准入保持有界，并在 frame/count 预算满时 fail fast。翻译管线在每个阶段保持有界队列，但不共享一个跨阶段 credit ledger，因此其瞬时保留字节峰值可以是这些阶段上界之和；这与新增多 owner event journal 或分布式 credit 协议相比被接受。
