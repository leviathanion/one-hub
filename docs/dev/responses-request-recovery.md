---
title: "Responses previous_response_id 请求级恢复方案"
layout: doc
outline: deep
lastUpdated: true
---

# Responses `previous_response_id` 请求级恢复方案

## 文档状态

- 状态：提案
- 范围：`/v1/responses` 与 `/v1/responses/compact` 在 `previous_response_not_found` 下的处理语义
- 当前决策：先修正职责边界与错误分类，不默认做无 replay 能力的自动恢复

## 问题定义

`previous_response_id` 对 `responses` 请求不是普通 affinity hint，而是 continuation anchor。

当上游返回 `previous_response_not_found` 时，说明：

- continuation 锚点已经失效
- 当前请求依赖的上游状态可能不可用
- 问题不只是“换一个 provider 再试一次”，而是“该请求是否还能被语义等价地继续”

当前实现把恢复逻辑放在 `relay/responses.go` 的 `send()` 里，会带来三个实际问题：

1. `send()` 内部隐藏 reroute，职责边界错误。
2. `usage`、quota、model/channel 上下文不会天然跟随 reroute 刷新。
3. `previous_response_not_found` 被混入普通 retry / cooldown / provider failure 语义，导致可观测性和归因失真。

## 最终方案

### 1. 所有 reroute / retry 统一放回外层 orchestration

`relay/responses.go` 只负责：

- 在当前 provider 上执行一次 attempt
- 识别 `previous_response_not_found`
- 提供一个 responses-specific 的 handling plan helper

`relay/main.go` 在首次 `RelayHandler(relay)` 失败后，统一决定：

- 是否属于 stale continuation miss
- 是否清理 stale affinity
- 是否允许自动恢复
- 是否继续进入普通 retry / cooldown

只要动作涉及以下任一行为，就必须留在外层：

- 重选 provider
- 重选 channel
- 重建 usage / quota
- 刷新 model / channel 上下文

### 2. 当前阶段默认不做无 replay 能力的自动恢复

当前 one-hub 不具备 canonical transcript / compaction replay 能力，因此默认不做下面这种行为：

1. 清空 `previous_response_id`
2. 原样保留当前最小输入
3. 再次发送请求

这类行为不是严格恢复，而是可能有语义漂移的 fallback。

因此当前阶段命中 `previous_response_not_found` 时，推荐行为是：

1. 清理 stale affinity
2. 记录恢复候选 meta
3. 跳过普通 health failure 处理
4. 返回显式错误给客户端

推荐错误语义：

- continuation 锚点已失效
- 网关当前无法在不丢失上下文的前提下自动恢复
- 需要客户端携带完整上下文重新发起请求

### 3. `previous_response_not_found` 归类为 continuation miss，而不是 health failure

命中该错误时：

- 不调用普通 `processChannelRelayError`
- 不触发 cooldown
- 不消耗普通 retry 次数
- 不写入“当前 channel 故障导致 retry”的语义日志

只有未来的恢复 attempt 自己失败后，后续错误才进入普通 provider error 处理路径。

### 4. 未来仅在 replay-capable 前提下开启严格恢复

只有当 one-hub 具备下面能力时，才允许自动恢复：

1. 能重建 canonical full input 或 compacted context
2. 能证明重建后的请求与原请求语义等价
3. 能识别 anchor-sensitive 输入并拒绝不安全恢复，例如：
   - `function_call_output`
   - reasoning / encrypted content
   - 依赖上一轮 output 的增量输入

届时严格恢复流程应为：

1. 清理 stale affinity
2. 清空 `previous_response_id`
3. 用完整等价输入重建请求
4. 外层重新 `setProvider(...)`
5. 外层重新进入完整 `RelayHandler(relay)`

## Trade-Off

本方案的收益：

- 直接修复当前实现里的 correctness 风险
- 不再让 `send()` 内部隐藏 reroute 分支
- `usage`、quota、retry、cooldown 的归因更一致
- 不需要立即引入 transcript store、数据库或 Redis schema 变更
- 为未来真正的自动恢复保留扩展点

本方案的代价：

- 当前阶段的用户体验更保守
- 某些理论上可恢复的请求，在 replay 基础设施落地前仍需要客户端自行用完整上下文重发
- 当前阶段不会提供“尽量成功”的 best-effort 自动降级

这个 trade-off 是刻意选择的：优先保证语义正确和系统边界清晰，再考虑自动化恢复体验。

## 实现约束

### `relay/responses.go`

应保留：

- `shouldRecoverStalePreviousResponse`
- `responsesPreviousResponseRecoveredContextKey`
- stale affinity 清理 helper

应移除：

- `send()` 内部重选 provider
- `send()` 内部再次调用完整发送链路
- `send()` 内部处理 usage / quota 迁移

### `relay/main.go`

应新增：

- responses stale continuation miss 的外层 handling helper
- 在首次 `RelayHandler` 失败后优先执行该 helper 的流程

应保持：

- 普通 retry loop 继续负责普通 provider 故障重试
- continuation miss 默认不进入普通 retry / cooldown

## 测试要求

### 必测 1：`send()` 不再内部 reroute

断言：

- `send()` 在当前 provider 上只执行一次 attempt
- 不在内部调用 `setProvider()`
- 不在内部再次调用完整发送链路

### 必测 2：continuation miss 不走普通 health failure 路径

断言：

- 不调用普通 `processChannelRelayError`
- 不触发 cooldown
- 不消耗普通 retry 次数

### 必测 3：当前阶段默认不做语义漂移的自动恢复

场景：

- 请求携带 `previous_response_id`
- 上游返回 `previous_response_not_found`
- one-hub 不具备 replay-capable 条件

断言：

- 清理 stale affinity
- 返回显式错误
- 不自动清空 `previous_response_id` 后再次成功发送

### 必测 4：恢复候选 meta 可观测

断言：

- continuation miss 被记录为一次 recovery candidate
- 相关 meta 可用于日志与排障

### 必测 5：未来 replay-capable 恢复必须重新走完整 `RelayHandler`

这是为下一阶段预留的测试槽位。断言：

- 新 provider 持有新的 `usage`
- quota / consume log 使用新的 channel / model 上下文
- 自动恢复路径语义上等价

## 参考资料

外部资料：

- OpenAI Conversation state
  https://platform.openai.com/docs/guides/conversation-state?api-mode=responses
- OpenAI Compaction guide
  https://developers.openai.com/api/docs/guides/compaction
- AWS Builders Library: Making retries safe with idempotent APIs
  https://aws.amazon.com/builders-library/making-retries-safe-with-idempotent-apis/
- AWS Builders Library: Timeouts, retries, and backoff with jitter
  https://aws.amazon.com/builders-library/timeouts-retries-and-backoff-with-jitter/
- AWS Builders Library: Avoiding fallback in distributed systems
  https://aws.amazon.com/builders-library/avoiding-fallback-in-distributed-systems/
- Kubernetes Session Affinity
  https://kubernetes.io/docs/reference/networking/virtual-ips/#session-affinity
- YARP Session Affinity
  https://learn.microsoft.com/aspnet/core/fundamentals/servers/yarp/session-affinity

相关实现参考：

- `../sub2api/backend/internal/service/openai_ws_forwarder.go`
- `../sub2api/backend/internal/service/openai_ws_protocol_forward_test.go`
- `../CLIProxyAPI/sdk/api/handlers/openai/openai_responses_websocket.go`
- `../CLIProxyAPI/sdk/api/handlers/openai/openai_responses_websocket_test.go`
