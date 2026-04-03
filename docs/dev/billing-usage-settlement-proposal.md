---
title: "Billing / Usage 收口重构方案"
layout: doc
outline: deep
lastUpdated: true
---

# Billing / Usage 收口重构方案

## 文档状态

- 状态：提案，已按 correctness / complexity / migration 三轮 review 收敛
- 目标：以最小必要复杂度收口 `one-hub` 当前 billing / usage 主路径，优先修正一致性问题
- 约束：
  - 优先不改数据库 schema
  - 不引入独立 billing service
  - 不追求 full ledger
  - 先修正“算一次、扣一次、记一次”的边界，再追求更强审计

## 背景

当前 `one-hub` 的 billing / usage 问题，不主要来自单个定价公式，而主要来自“没有单一结算边界”：

- `relay/main.go` 中每个 retry attempt 都会重新执行 `NewQuota -> PreQuotaConsumption -> send -> Undo/Consume`
- `relay/relay_util/quota.go` 同时承担：
  - price math
  - 预扣
  - 最终消费
  - consume log 记录
  - realtime cache quota 更新
- `types.Usage`、`types.UsageEvent`、`types.ResponsesUsage`、provider 侧 usage accumulator 已经在逐步收敛，但最终扣费仍然分散
- realtime 走 `UsageEvent + RealtimeTurnObserver`
- task 仍然依赖 `NewQuota(..., 1000)` 这种占位式预扣和 provider-specific 补偿逻辑

结果是：

1. retry / realtime / task / responses 各自有一套半独立结算语义
2. usage 归一化和 quota 副作用没有在同一个对象边界内闭合
3. 日志、统计、扣费、副作用提交没有严格区分主路径与旁路

## 目标

本方案只追求以下目标：

1. 所有 token-based 流程最终都进入同一条 settlement 主路径
2. 单个逻辑请求不再因为 retry 而重复预扣 / 撤销
3. 结算副作用只允许通过一个同步入口执行
4. 在不改数据库 schema 的前提下，尽可能降低重复结算和结算漂移
5. 保留现有 price math 和 extra billing 公式，避免一开始就重写经济模型

## 非目标

本方案明确不做以下事情：

1. 不引入 full ledger / double-entry accounting
2. 不拆独立 billing service
3. 不先做 `FundingSource` 之类的支付源抽象
4. 不承诺“无数据库改动下的 durable exactly-once”
5. 不顺手重写现有 price formula、日志表或统计表

## 核心取舍

最佳平衡点不是把 `CLIProxyAPI`、`new-api`、`sub2api` 的优点全量拼接，而是只吸收三件事：

1. 像 `CLIProxyAPI` 一样，把 usage 的最终结算输入收口
2. 像 `new-api` 一样，把“预扣 / 成功提交 / 失败回滚”建成请求级生命周期
3. 像 `sub2api` 一样，给结算入口加上 request-id + fingerprint 的幂等闸门

同时主动放弃三件复杂度过高、当前阶段收益递减的事：

1. 放弃 full ledger
2. 放弃服务拆分
3. 放弃过度通用的资金来源抽象

## 最终决策

### 决策 1：`types.Usage` 作为 token-based 流程的唯一最终结算输入

- `types.UsageEvent` 只保留为 realtime 增量观察类型
- `types.ResponsesUsage` 只保留为协议 DTO
- provider / stream / realtime 的边界层负责把自己的 usage 转成 `types.Usage`

这意味着：

- chat / completions / responses / realtime turn 最终都以 `types.Usage` 进入结算
- 不新增一个新的公开 canonical usage type
- 复杂度控制在“收口边界”，而不是“再发明一套类型系统”

### 决策 2：跨所有流程的唯一 canonical object 是 `SettlementCommand`

`SettlementCommand` 才是最终结算对象。它至少应包含：

- `request_id`
- `fingerprint`
- `flow_kind`
- `user_id`
- `token_id`
- `channel_id`
- `model_name`
- `usage`，可选，类型为 `*types.Usage`
- `final_quota`
- `quota_breakdown`
- `log_meta`

约束：

- `request_id` 统一复用现有网关 request id，而不是在结算层再生成一套新标识
- token-based 流程优先通过 `usage -> final_quota`
- task 场景允许直接构造 `final_quota`，不强迫异步任务伪装成 token usage

### 决策 3：不引入接口层级，先用一个 concrete `RequestSettlement`

V1 不需要一套 interface hierarchy。

推荐仅引入一个 concrete 结构，例如 `RequestSettlement`，负责：

- `Reserve()`
- `CommitSuccessfulAttempt(...)`
- `Rollback()`

它不是 service，不是 plugin，不做 RPC 边界，只是 relay 内部的生命周期对象。

### 决策 4：保留现有 `Quota` 的 price math 和 log meta 逻辑，先抽 orchestration

`relay/relay_util/quota.go` 当前最大的价值，不是它的副作用 orchestration，而是：

- 定价公式
- extra billing 折算
- log metadata 组装
- timing / affinity meta 拼装

V1 不优先重写这些逻辑，而是把下面这些能力从 `Quota` 中抽离：

- retry 周期内的预扣 / 撤销编排
- 最终副作用提交
- 幂等保护

### 决策 5：`ApplySettlement` 必须是唯一同步主入口

所有核心经济副作用只允许通过 `ApplySettlement(command)` 执行。

核心要求：

1. 同步执行，不走异步 worker
2. 允许使用 detached context 避免 client cancel 直接打断结算
3. 仍在主调用链内等待完成，不把结算丢给后台 goroutine
4. consume log / analytics 是旁路，不是结算真相源

### 决策 6：V1 幂等依赖 Redis gate，但不虚构强保证

不改数据库 schema 的前提下，V1 幂等只能依赖：

- request 级 in-process once
- Redis gate 的 request-id + fingerprint 抑制

不允许在文档里把它写成 full exactly-once。

V1 的真实承诺应当是：

1. Redis 正常、进程连续时，可以抑制同一 logical request 的重复 commit
2. 同一 `request_id` 携带不同 fingerprint 时，必须视为 conflict 并打高优先级日志
3. Redis 不可用时，不伪装成强幂等，只保留弱保证

## 目标架构

### Phase 1：收口 HTTP unary 路径

适用范围：

- `/v1/chat/completions`
- `/v1/completions`
- `/v1/responses`
- 其他同类同步 HTTP relay 路径

目标流程：

1. 完成 `setRequest -> setProvider -> reparse`
2. 计算 prompt tokens
3. 基于第一次成功选定的 price context 创建 `RequestSettlement`
4. 只做一次 reserve / preconsume
5. retry 只负责切换 provider / channel 和重发，不再重复预扣 / 撤销
6. 成功时用成功 attempt 的 `usage + channel/model/context` 构造 `SettlementCommand`
7. 调用唯一的 `ApplySettlement`
8. 全部失败时只做一次 rollback

这里的关键变化是：

- retry 不再拥有自己的 quota 生命周期
- reserve 属于整个外部请求，而不是某次 attempt
- commit 只发生一次，并且只认最终成功 attempt

补充说明：

- reserve 仍然只是基于当前估算公式的预扣，不自动提供比现有系统更强的授信控制
- 如果最终成功 attempt 的实际成本高于 reserve，系统仍然只是在 commit 阶段做差额处理
- 因此 Phase 1 解决的是“一致性边界”，不是“更强的资金充足性证明”

### Phase 2：收口 realtime turn

realtime 不能直接套用“HTTP request 对应一个 settlement”的语义。

正确边界应当是：

- websocket / realtime session 是 transport 生命周期
- turn 才是结算生命周期

因此 Phase 2 的决策是：

1. 继续用 `types.UsageEvent` 作为 turn 内增量观察类型
2. `FinalizeTurn` 时把终态 usage 转成 `types.Usage`
3. 每个 turn 独立构造一次 `SettlementCommand`
4. 最终结算与 HTTP unary 共用同一个 `ApplySettlement`

补充约束：

- `UpdateUserRealtimeQuota` 在 Phase 2 之前仍可保留，但只作为 soft limit / UX 信号
- realtime cache quota 不能继续被描述成最终记账真相源

### Phase 3：收口 async task

task 的问题和 unary request 不同，它天然跨请求：

- submit 请求发生一次
- poll / callback / task update 发生在后续请求中

因此不能把它做成“内存里的同一个 `RequestSettlement` 对象”。

V1 的正确做法是：

1. submit 阶段只做 reserve，并把最小 settlement snapshot 持久化
2. completion / failure 阶段读取 snapshot，再 commit 或 rollback

为避免改表，snapshot 使用现有字段承载：

- `model.Task.Properties`
- `model.Task.Data`

建议 snapshot 至少包含：

- `request_id`
- `fingerprint`
- `reserve_quota`
- `model_name`
- `channel_id`
- `token_id`
- `user_id`
- `price_context`
- `settlement_status`

约束：

- 在 Phase 3 真正落地前，不要试图把现有 `NewQuota(..., 1000)` 硬包装成统一架构
- 异步任务必须显式区分 submit-time reserve 和 completion-time settle

## `ApplySettlement` 的职责边界

`ApplySettlement` 负责的只有三类事情：

1. 幂等判断
2. 核心经济副作用提交
3. 旁路审计记录

### 1. 幂等判断

推荐复用 `runtime/session` 里的显式状态风格，而不是返回布尔值。

例如：

```go
type SettlementGateStatus string

const (
    SettlementGateApplied           SettlementGateStatus = "applied"
    SettlementGateConditionMismatch SettlementGateStatus = "condition_mismatch"
    SettlementGateBackendError      SettlementGateStatus = "backend_error"
)
```

行为要求：

- 同一 `request_id + fingerprint` 再次进入时，视为幂等成功或 no-op
- 同一 `request_id` 但 fingerprint 不同，视为 conflict
- Redis backend error 只能返回“不确定”，不能伪装成 miss

补充约束：

- `backend_error` 不是自动放行信号
- 在 response 尚未开始对客户端可见的路径上，应优先 fail closed
- 在 response 已完成、stream 已写出或 async completion 已开始的路径上，不应再做盲目自动重试，而应记录 `settlement_unknown` 并交由运维排障

### 2. 核心经济副作用提交

这里必须明确一个 review 后追加的硬约束：

`ApplySettlement` 不能继续依赖 `BatchUpdateEnabled` 的通用批处理队列来完成核心配额写入。

原因：

- 当前 `IncreaseUserQuota` / `DecreaseUserQuota`
- `UpdateUserUsedQuotaAndRequestCount`
- `UpdateChannelUsedQuota`

在 `BatchUpdateEnabled` 下都会退化成异步队列写入。

如果继续沿用这层，哪怕 `ApplySettlement` 看起来是同步的，也只是“假同步”。

因此 V1 必须把下面这些核心写入改成直接、同步、可确认的执行路径：

- user quota
- token quota
- user used quota / request count
- channel used quota

推荐做法：

- 在一个明确的 DB transaction 内执行这些核心更新
- consume log 放在 transaction 之后 best-effort 写入
- 只有 transaction 成功后，才允许把 Redis gate 标记为 committed

### 3. 旁路审计记录

consume log 继续复用现有 `model.Log.Metadata`，但身份需要升级。

推荐至少追加：

- `request_id`
- `settlement_fingerprint`
- `settlement_flow_kind`
- `preconsumed_quota`
- `final_quota`
- `quota_breakdown`
- `extra_billing`
- `attempt_count`
- `selected_channel_id`
- `selected_model_name`

明确约束：

- log 不是账本
- log 写入失败不回滚已经成功的核心结算
- log metadata 用于排障、对账辅助、后续迁移观察

## V1 保证与非保证

### V1 保证

1. 一个逻辑请求只应存在一条 canonical settlement 主路径
2. retry 不再反复创建 / 撤销自己的 quota 生命周期
3. token-based 流程最终都收口到 `types.Usage`
4. Redis 正常时，可抑制常见的重复 commit
5. 结算和日志被明确分为主路径与旁路

### V1 不保证

1. Redis 故障或进程崩溃下的 durable exactly-once
2. full ledger / immutable journal
3. 比当前更强的资金充足性证明
4. 无需改表就完成严格财务级 reconciliation

尤其需要明确：

如果 DB transaction 已成功，但 Redis gate 的 committed 标记失败，系统仍然会落入“已成功结算但幂等标记状态不确定”的灰区。

在不引入 schema 级唯一约束或 durable ledger 的前提下，这个问题不能被彻底消除，只能：

- 明确记录
- 限制自动重试
- 依赖运维排障

这也是本方案刻意不承诺 strong exactly-once 的原因。

## 兼容性要求

以下旧行为需要在文档中明确保持兼容：

1. 现有 quota 公式和 extra billing 价格逻辑保持不变
2. 现有 consume log 基本字段保持不变
3. 现有 `extra_billing` metadata 继续保留
4. 现有 `first_response` / affinity / routing-group metadata 继续保留
5. realtime 在 Phase 2 之前的 soft-limit 体验不被突然移除
6. task 在 Phase 3 之前继续按现有路径运行，不做半收口

## 实现顺序

### 第一步：只收口 HTTP unary

涉及范围：

- `relay/main.go`
- `relay/relay_util/quota.go`
- `types/common.go`
- `types/responses.go`
- provider 侧的 usage 归一化边界

目标：

- 先把同步 HTTP 请求的结算边界收口
- 不同时改 realtime 和 task

### 第二步：接 realtime turn finalize

涉及范围：

- `relay/relay_util/realtime_turn_observer.go`
- `providers/openai/realtime_turn_state.go`
- 相关 realtime provider 终态 usage 生成逻辑

目标：

- 只替换“最终结算入口”
- 暂不重写 turn 内 usage 观察逻辑

### 第三步：接 async task

涉及范围：

- `relay/task/main.go`
- `model/task.go`
- 各 provider task adaptor / poller

目标：

- 用现有 `Task.Properties` / `Task.Data` 存 settlement snapshot
- 去掉 `NewQuota(..., 1000)` 这种占位式路径

## Rollout 要求

这个方案不应一次性切到全量。

推荐至少分为四层 rollout：

1. `off`
2. `http_unary_only`
3. `http_unary + realtime`
4. `all`

要求：

- 开关应优先作为内部配置，不先暴露给用户 UI
- 每一层 rollout 都必须有明确回退路径
- 生产环境 rollout 应默认要求 Redis 可用；无 Redis 的部署只能按“弱幂等保证”理解
- Phase 1 未稳定前，不进入 Phase 2
- Phase 2 未稳定前，不进入 Phase 3

## 测试要求

### 必测 1：retry 只 reserve 一次

断言：

- 同一外部请求在多次 attempt 中只做一次 reserve
- 中间失败 attempt 不再单独 undo
- 最终只有成功 attempt 触发 commit

### 必测 2：同一 request_id 不重复 commit

断言：

- 相同 `request_id + fingerprint` 重放时，不重复扣费
- 相同 `request_id` 但不同 fingerprint 时，返回 conflict 或至少打高优先级日志

### 必测 3：BatchUpdate 不再成为核心结算主路径

断言：

- user quota / token quota / used quota / channel used quota 的核心结算路径不依赖通用 batch queue
- 即使 log 仍走 batch，主结算也不受影响

### 必测 4：Redis gate backend_error 语义稳定

断言：

- `backend_error` 不会被当成普通 miss 继续结算
- pre-response 路径与 post-response 路径的处理策略明确且可测试
- `settlement_unknown` 有明确日志或指标可观测

### 必测 5：responses / extra billing 口径不变

断言：

- web search / code interpreter / file search / image generation 的 extra billing 口径保持一致
- 日志 metadata 中的 `extra_billing` 结构保持兼容

### 必测 6：realtime turn 最终只结算一次

断言：

- turn 内增量 usage 只用于观察，不直接形成最终重复扣费
- `FinalizeTurn` 后只进入一次 canonical settlement

### 必测 7：task snapshot 可在不改表前提下跨请求恢复

断言：

- submit 阶段写入 settlement snapshot
- completion / failure 能读取 snapshot 并 commit / rollback
- 不依赖新增表或新增列

## 三轮 review 后的修订记录

### Round 1：Correctness Review

这一轮把方案从“一个统一 session 解决所有问题”修正为“三种生命周期分治”：

1. unary request
2. realtime turn
3. async task

同时补充了两个硬约束：

- `ApplySettlement` 不得继续依赖 `BatchUpdateEnabled`
- V1 不得宣称 durable exactly-once

### Round 2：Complexity Review

这一轮主动删掉了会快速膨胀复杂度的设计：

1. 不新增新的公开 canonical usage type
2. 不引入 interface hierarchy
3. 不拆 billing service
4. 不先做 `FundingSource`
5. 不先重写 price math

最后保留的是最小必要内核：

- `types.Usage`
- concrete `RequestSettlement`
- `SettlementCommand`
- 单一 `ApplySettlement`
- Redis gate

### Round 3：Migration Review

这一轮把 rollout 和兼容边界补全：

1. task snapshot 明确落在现有 `Task.Properties` / `Task.Data`
2. consume log schema 不变，只扩 metadata
3. 分阶段 rollout，不允许一口气改 chat + realtime + task
4. 明确先 Phase 1，再 Phase 2，再 Phase 3

## 最终结论

从当前 `one-hub` 的代码现实出发，最佳平衡点不是“完整账务系统”，而是：

1. `types.Usage` 作为 token-based 最终结算输入
2. `SettlementCommand` 作为唯一 canonical settlement object
3. concrete `RequestSettlement` 管理 reserve / commit / rollback 生命周期
4. 同步的 `ApplySettlement` 作为唯一结算入口
5. Redis gate 提供 V1 幂等抑制
6. `model.Log.Metadata` 与 `Task.Properties/Data` 作为无 schema 变更下的旁路审计和异步快照承载

这已经足以显著降低当前的 billing / usage 架构漏洞，同时把复杂度控制在一个可落地、可回退、可渐进演进的范围内。
