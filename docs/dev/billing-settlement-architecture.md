---
title: "Billing / Usage 结算架构"
layout: doc
outline: deep
lastUpdated: true
---

# Billing / Usage 结算架构

## 文档状态

- 状态：当前实现
- 适用范围：unary request、Codex realtime turn、async task
- 目的：说明当前代码已经采用的结算架构、边界和 trade-off，而不是下一阶段的实现草案

## 这套架构真正解决什么

从第一性原理看，one-hub 的 billing 目标不是做一套财务级账本，而是把“最终扣多少、在哪一处落真相、重复 finalize 如何收敛”固定下来。

当前代码解决的是三件事：

1. 预扣和最终结算分离，但最终 truth 只走一条入口
2. unary、realtime、async task 三类请求共用同一套 settlement contract
3. detached finalize 场景在可接受复杂度内做幂等防重，而不是为此引入独立 ledger service

当前代码明确不解决：

1. crash-safe exactly-once ledger
2. 独立 billing service / reconciliation service
3. 财务语义的双分录账本
4. 所有 projection 绝对不丢

这不是缺陷，而是当前复杂度预算下的明确取舍。

## 当前实现的总链路

当前结算链路已经收敛为：

`Quota -> SettlementEnvelope -> ApplySettlement -> truth / cleanup / projection`

对应模块：

| 模块 | 职责 |
| --- | --- |
| `relay/relay_util/quota.go` | 计算价格、预扣额度、冻结结算快照 |
| `internal/billing/settlement.go` | 统一结算入口、Redis gate、防重、truth / cleanup / projection 分层 |
| `relay/relay_util/realtime_turn_observer.go` | realtime turn finalize 归一到 settlement |
| `relay/task/base/settlement.go` | async task snapshot 冻结与 finalize |
| `model/token.go` | `ApplyTokenUserQuotaDeltaDirect`，同步落 user/token truth |

## 核心分层

### 1. reserve 仍然是请求路径行为

当前代码保留了原有 reserve 模型：

- `Quota.PreQuotaConsumption()`
- `Quota.Undo()`

它仍然是 attempt-scoped，而不是 request-scoped global reserve。

当前保留这套模型的原因很直接：

1. 现有 relay 路径已经全面依赖它
2. async task 必须在 submit 时先冻结可回补的额度上下文
3. 全量重写 reserve 生命周期，牵动的范围远大于收益

async task 额外调用 `ForcePreConsume()`，强制在 submit 时持有预扣额度，因为 finalize 发生在脱离原请求的异步路径上。

### 2. `SettlementEnvelope` 是冻结后的最小结算快照

当前代码不再把最终结算所需信息散落在多条路径里，而是先由 `Quota.BuildSettlementEnvelope(...)` 冻结：

- `SettlementCommand`
- `SettlementOptions`

`SettlementCommand` 是当前系统里的最小 truth 输入，核心字段包括：

- `identity`
- `request_kind`
- `user_id`
- `token_id`
- `channel_id`
- `model_name`
- `pre_consumed_quota`
- `final_quota`
- `usage_summary`
- `unlimited_quota`

`SettlementOptions` 则承载非 truth 的附属动作：

- `Deduplicate`
- `Cleanup`
- `Projection`

这意味着当前系统已经把“扣费真相”和“日志/缓存/统计”在参数层拆开，而不是继续混写在调用方里。

### 3. `ApplySettlement` 是唯一 truth 入口

当前代码已经把最终扣费统一到：

```go
ApplySettlement(ctx, cmd, opts)
```

该入口的执行顺序固定为：

1. 归一化 `SettlementCommand`
2. 如需要，先尝试拿 Redis gate
3. 同步执行 truth 写入
4. truth 成功后执行 cleanup
5. truth 成功后执行 projection

其中 truth 公式已经固定：

`delta = final_quota - pre_consumed_quota`

这一步不会走 batch projection，而是直接调用 `model.ApplyTokenUserQuotaDeltaDirect(...)` 同步落库。

### 4. truth / cleanup / projection 已经分层

当前代码里三层边界已经明确。

#### truth

truth 只负责 quota 真相：

- `users.quota`
- `tokens.remain_quota`
- `tokens.used_quota`

特点：

- 同步执行
- 直接写 DB
- 不依赖 `BatchUpdateEnabled`
- 失败时直接让 settlement 失败

#### cleanup

cleanup 只处理派生缓存：

- realtime quota cache 回收
- user quota cache refresh

特点：

- 只在 truth 成功后执行
- 失败只记录日志
- 不回滚 truth

#### projection

projection 只处理统计和审计：

- `RecordConsumeLog`
- `channels.used_quota`
- `users.used_quota`
- `users.request_count`

特点：

- 只在 truth 成功后执行
- 失败只记录日志
- 不回滚 truth
- 统计口径使用 `final_quota`，不是 `delta`

这一点非常关键：`delta` 用来修正 reserve，`final_quota` 才代表这次请求最终消费了多少。

## 三类请求如何接入当前架构

### Unary Request

普通请求路径的现状是：

1. `Quota.PreQuotaConsumption()` 执行预扣
2. provider 返回 usage 后，`Quota.ConsumeUsage(...)`
3. `Quota` 内部构造 `SettlementEnvelope`
4. `ApplySettlement(...)` 同步完成最终结算

因此 unary 已经不再是“边算边扣”的散乱路径，而是先 reserve，再统一 settle。

### Realtime Turn

Codex realtime 当前采用 observer 方式接入：

1. turn 进行中，`RealtimeTurnObserver.ObserveTurnUsage(...)` 只更新 realtime quota cache
2. turn 结束时，`FinalizeTurn(...)` 冻结最终 usage 与 timing
3. 生成 identity：`caller=<ns>|channel=<id>|session=<sid>|turn=<seq>|finalize`
4. 调用 `Quota.ConsumeUsageWithIdentity(...)`
5. 最终仍落到 `ApplySettlement(...)`

当前 trade-off：

- turn 内实时配额控制依赖缓存，不是每个 usage event 都直接落 truth
- 最终 truth 只在 finalize 时统一落一次
- duplicate terminal event 依靠 identity + fingerprint 去重

### Async Task

async task 不是在 finalize 时重算 live config，而是在 submit 时冻结结算快照：

1. submit 前强制 `ForcePreConsume()`
2. `BuildTaskSettlementSnapshotProperties(...)` 构造 `SettlementEnvelope`
3. snapshot 持久化到 `tasks.properties`
4. 任务完成或失败时，`FinalizeTaskSettlement(...)` 从 snapshot 恢复 command/options
5. success 使用原始 `final_quota`
6. failure 把 `final_quota` 改成 `0`，完成 reserve 回补

这意味着 async task 的价格、预扣和最终结算上下文，已经与 submit 时刻绑定，而不是依赖 finalize 时的 live pricing。

## Redis gate 的实际职责

当前 gate 不是通用账本能力，只在 detached finalize 场景做防重：

- realtime turn finalize
- async task finalize

触发条件：

1. `opts.Deduplicate = true`
2. `identity` 非空
3. Redis 已启用且 client 可用

gate 行为：

- key 不存在：写入 fingerprint，允许本次 settlement 执行
- key 已存在且 fingerprint 相同：视为重复 finalize，直接 no-op
- key 已存在且 fingerprint 不同：视为 identity 冲突，拒绝复用
- gate backend 出错：记录告警，fail-open，继续执行 settlement

当前 TTL 固定为 `24h`。

### 当前 trade-off

- 获得什么：大多数 detached finalize 的重复触发不会双扣、双退或重复 projection
- 牺牲什么：Redis 抖动时系统会退化成弱防重，而不是中断主流程
- 为什么当前最合适：结算 truth 不能因为外部 gate 故障被整体阻断；one-hub 当前更看重 availability + bounded duplication，而不是强一致外部协调

## Async snapshot 的当前数据模型

当前 async task 复用 `tasks.properties` 保存：

- `SettlementEnvelope`
- settlement status：`reserved / committed / rolled_back`
- tracking：`provider_accepted`、`provider_task_id`

不新增账务表，也不新增 task ledger。

### 当前 trade-off

- 获得什么：submit 时的价格、预扣、token/user/channel/model 上下文被稳定冻结
- 牺牲什么：snapshot 和 tracking 共用一份 JSON，仍然需要控制写源数量
- 为什么当前最合适：当前 async task 只有 submit 和 sweeper/finalize 两类写源，没有必要引入第二套持久化模型

## 当前架构的关键不变量

1. 所有最终扣费都必须经过 `ApplySettlement(...)`
2. truth 必须先于 cleanup / projection
3. projection 统计口径必须使用 `final_quota`
4. detached finalize 的 identity 不能为空串
5. async task finalize 必须优先使用本地 `tasks.id` 构造 identity
6. gate backend 故障时允许 fail-open，但不能生成第二套 truth 路径
7. task snapshot 一旦从 `reserved` 进入终态，就不能再按另一份 finalize 结果覆盖 truth

## 兼容与回退路径

当前代码仍保留少量兼容分支，但这些都不是主路径：

1. legacy task 没有 snapshot 时，失败回补仍可能走旧的 refund 兼容逻辑
2. gate 不可用时，detached finalize 退化成弱防重
3. projection 失败时，truth 不回滚

这些兼容分支存在的原因不是它们理想，而是为了保证当前系统在存量行为下可演进收敛。

## 当前架构不做什么

1. 不新增 settlement ledger / history 表
2. 不把 `logs` 当成 truth
3. 不让 projection 成败决定 settlement 成败
4. 不把 Redis gate 扩散到所有 unary 请求
5. 不在 async finalize 时依赖 live pricing / live config 重算
6. 不承诺 crash-safe exactly-once

## 结论

当前代码里的 billing / usage 已经不是“实现草案”，而是一套已落地的统一结算架构：

- reserve 仍留在请求路径
- settlement truth 收敛到 `ApplySettlement`
- realtime 和 async finalize 通过 identity + Redis gate 做弱一致防重
- projection 与 truth 明确解耦

这套方案不是最强一致的，但在当前 one-hub 的复杂度预算里，它已经把最重要的边界固定住了：truth 只有一条路，重复 finalize 有收敛点，异步任务不再依赖漂移的 live config 结算。
