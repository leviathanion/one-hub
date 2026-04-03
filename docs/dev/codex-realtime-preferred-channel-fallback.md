---
title: "Codex Realtime Preferred Channel 失败后的 Fresh Fallback 方案"
layout: doc
outline: deep
lastUpdated: true
---

# Codex Realtime Preferred Channel 失败后的 Fresh Fallback 方案

## 文档状态

- 状态：提案
- 范围：`Codex /v1/realtime` 命中 preferred channel 后，same-channel open 失败时的 fresh reroute 语义
- 当前决策：保留 best-effort affinity，只增加“单次请求内排除已失败 preferred channel”的轻量机制

## 问题定义

当前 realtime 路径里，命中 affinity 后会先走同 channel 打开 session：

1. `prepareRealtimeChannelAffinity(...)` 命中 preferred channel
2. `tryAffinityRealtimeSession(...)` 在该 channel 上调用 `openRealtimeSessionWithFreshFallback(...)`
3. 如果 same-channel open 仍失败，且当前不是 strict affinity，请求会继续走 `openFreshRealtimeSession(...)`
4. 但 fresh reroute 之前并没有把刚刚失败的 preferred channel 排除掉，因此负载均衡仍可能再次选回同一个 channel

这会带来几个问题：

1. 同一请求内，对同一失败 channel 做重复尝试。
2. “same-channel 恢复失败”和“fresh reroute”之间的边界被冲淡。
3. retry 预算和上游连接资源被重复消耗。
4. 日志与指标上看起来像 fresh reroute，实际上却可能还是同一失败目标。

注意，这个问题并不等价于“共享 affinity binding 一定 stale”。因为 `tryAffinityRealtimeSession(...)` 内已经包含 same-channel fresh fallback；单次请求失败只说明“这个 preferred channel 在本次尝试里没成功”，不足以推出“全局 binding 必须立刻清掉”。

## 外部与相邻实现参考

### 外部资料

- Cloud Run 明确把 session affinity 定义为 `best effort affinity`，实例终止、并发打满或 CPU 打满时，后续请求会被路由到别的实例。
- YARP 的 session affinity failure policy 默认是 `Redistribute`：当 affinitized destination 不可用时，跳过 affinity lookup，交给普通负载均衡重新选一个健康目标。
- AWS Builders Library 强调 distributed fallback 往往难测、也可能扩大故障面，因此更推荐小而明确、可持续被主路径覆盖到的失败处理策略。

这些资料支持一个共同原则：affinity 是 hint，不是 owner 锁；失败后应尽量缩小 fallback 逻辑，而不是叠加更多重型协调。

### 相邻仓库

- `../sub2api/backend/internal/service/openai_ws_forwarder.go`
  - `preferredConnID` 是 hint，不是强 owner。
  - 连接真正可继续使用时，才保留或更新 sticky/session 绑定。
  - lease 断开后会把当前 `preferredConnID` 清空，避免同一失败连接继续污染后续 acquire。
- `../CLIProxyAPI/sdk/api/handlers/openai/openai_responses_websocket.go`
  - 偏向更轻的 execution/session 生命周期，不引入额外全局协调。

## 设计目标

1. same-channel open 失败后，本次请求的 fresh reroute 不应再立即选回同一个 preferred channel。
2. 保持当前 realtime best-effort affinity 语义，不引入 owner lease、fencing token 或全局排它。
3. strict affinity 仍然 fail-close。
4. pin channel 的行为保持不变。
5. 只有 fresh reroute 成功落到其他 channel 时，才改写共享 affinity。

## 推荐方案

### 1. 引入“请求级 failed preferred exclusion”

当 `tryAffinityRealtimeSession(...)` 已经真实进入 preferred channel 的 open 流程，并最终返回错误时：

1. 不立刻清理共享 affinity binding。
2. 仅把该 `preferredChannelID` 记入“当前请求不可再选”的排除集。
3. 随后的 `openFreshRealtimeSession(...)` 读取该排除集，像普通 `skip_channel_ids` 一样跳过它。

最小实现上，直接复用当前已经存在的 `skip_channel_ids` 即可，不需要再引入新的全局状态结构。

### 2. 失败边界只在当前请求内生效

这个排除集只对当前请求有效，不应该：

1. 写入 Redis。
2. 变成 channel 级 cooldown。
3. 影响其他并发请求。
4. 推断共享 affinity 一定失效。

这样可以避免把一次局部失败放大成跨请求、跨节点的全局判断。

### 3. strict / pinned 语义保持不变

- strict affinity：
  - preferred channel 失败后直接中止，返回当前已有错误语义。
- explicit pin：
  - pinned channel 本身就不是“可重新分配”的 affinity 目标，因此不做 redistribute。

也就是说，请求级排除只作用于“非 strict、非 pinned”的 fresh reroute。

### 4. 只在 fresh 成功后改写 affinity

推荐保持如下写路径：

1. preferred channel same-channel open 成功：
   - 继续沿用当前 binding
2. preferred channel 失败，但其他 channel fresh 成功：
   - 记录成功 channel，并把 affinity 改写到新 channel
3. preferred channel 失败，且其他 channel 也都失败：
   - 不因为这一次请求失败就清理共享 affinity

这符合 best-effort affinity 的边界：共享 binding 应由成功结果推进，而不是由单次失败盲删。

## 为什么这是复杂度与收益的最佳点

这个方案的收益很直接：

1. 修复“fresh fallback 又回到同一失败 channel”的明显低效行为。
2. 与现有 `skip_channel_ids`、`openRealtimeSessionWithFreshFallback(...)` 机制天然兼容。
3. 不需要引入新的分布式状态或后台清理逻辑。

这个方案刻意不做的事情：

1. 不做跨请求 quarantine。
2. 不做 shared lease / fencing。
3. 不做“失败后立刻判定 affinity stale”。

原因是 `tryAffinityRealtimeSession(...)` 里已经执行过 same-channel fresh fallback。此时再让 fresh reroute 回到同一 channel，收益极低；而把失败升级为全局协调问题，复杂度上升却很快。

## 不建议的方案

### 1. 对 preferred channel 做全局 cooldown / quarantine

问题：

1. 一次请求失败不代表 channel 在全局上真的不可用。
2. 会把局部问题放大成跨请求影响。
3. 需要额外处理 TTL、并发更新和观测语义。

### 2. 引入 owner lease / fencing token

问题：

1. 这会把 realtime affinity 从“共享 resume hint”推向“分布式 owner 协调”。
2. 与当前 best-effort 设计方向不一致。
3. 复杂度远高于当前问题的真实收益。

### 3. 允许 fresh reroute 继续把同一 preferred channel 当普通候选再试一次

问题：

1. 当前 same-channel 路径已经包含 fresh fallback。
2. 再选回同一个 channel，本质是重复尝试，不是新的恢复策略。

## 实现落点

### `relay/realtime.go`

建议修改点：

1. `getProvider()`
   - 当 `tryAffinityRealtimeSession(...)` 返回失败且当前不是 strict affinity 时，在进入 `openFreshRealtimeSession(...)` 前把 `preferredChannelID` 加入 `skip_channel_ids`
2. 可选地增加一个更明确的 helper，例如：
   - `excludeRealtimePreferredChannelForCurrentRequest(channelID int)`

### `relay/common.go`

当前 `currentRealtimeChannelSelection(...)` 已经会读取 `skip_channel_ids`，因此不需要再扩展新的 load-balancing 协议，只需复用已有选择器语义。

### 观测建议

建议补充以下 meta 或日志字段：

- `channel_affinity_preferred_open_failed`
- `channel_affinity_preferred_open_failed_id`
- `channel_affinity_preferred_open_failed_excluded`
- `channel_affinity_preferred_open_failed_reason`

这样可以把“preferred 命中但 session open 失败”和“fresh reroute 真正选到了哪里”分开观测。

## 测试要求

### 必测 1：same request 不再重选失败的 preferred channel

场景：

1. affinity 命中 channel A
2. channel A 的 same-channel open 失败
3. fresh reroute 仍有其他候选 channel

断言：

- 随后的 fresh 选择不再落回 channel A

### 必测 2：strict affinity 语义不变

断言：

- preferred channel 失败后直接 abort
- 不进入 redistribute

### 必测 3：pinned channel 语义不变

断言：

- pinned channel 失败后直接按当前语义返回错误
- 不通过 `skip_channel_ids` 继续重选其他 channel

### 必测 4：替代 channel 成功后才改写 affinity

断言：

- reroute 到 channel B 成功时，affinity 改写到 B

### 必测 5：单次失败不会盲删共享 affinity

断言：

- preferred channel 失败但本次请求最终未成功时，不因为这一次失败就清掉共享 binding

## 参考资料

外部资料：

- OpenAI Conversation state
  https://developers.openai.com/api/docs/guides/conversation-state
- AWS Builders Library: Avoiding fallback in distributed systems
  https://aws.amazon.com/builders-library/avoiding-fallback-in-distributed-systems/
- YARP Session Affinity
  https://learn.microsoft.com/aspnet/core/fundamentals/servers/yarp/session-affinity
- Cloud Run Session Affinity
  https://cloud.google.com/run/docs/configuring/session-affinity

相关实现参考：

- `relay/realtime.go`
- `relay/common.go`
- `relay/realtime_test.go`
- `../sub2api/backend/internal/service/openai_ws_forwarder.go`
- `../CLIProxyAPI/sdk/api/handlers/openai/openai_responses_websocket.go`
