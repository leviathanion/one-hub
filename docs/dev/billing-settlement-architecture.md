---
title: "Billing/Usage 结算收口方案"
layout: doc
outline: deep
lastUpdated: true
---

# Billing/Usage 结算收口方案

## 文档状态

- 状态：提案
- 范围：同步 HTTP 请求、realtime turn finalize、task async finalize
- 当前决策：`V1` 不改数据库 schema，不引入 full ledger，不重写 reserve 模型，先统一最终结算入口

## 核心结论

这版方案不是把 `new-api`、`sub2api`、`CLIProxyAPI` 的优点全部拼起来，而是只拿复杂度/收益比最高的部分。

`V1` 的最终取舍如下：

1. 不重写当前 attempt 级 `PreQuotaConsumption / Undo` 语义。
2. 统一以 `types.Usage` 作为最终结算输入。
3. 引入最小化的 `SettlementCommand`，以及同步、单入口的 `ApplySettlement(cmd)`。
4. `settlement truth` 只包含 user/token quota 的最终 delta。
5. Redis/cache reconcile 是 `post-commit cleanup`，不是 settlement success 条件。
6. consume log、user/channel used quota、request count、报表统计全部降级为 projection。
7. `BatchUpdateEnabled` 不得参与关键结算正确性。
8. `Redis gate` 只用于会脱离原始 request 生命周期、可能重复 finalize 的路径。
9. 不改数据库 schema，只复用现有 `model.Log.Metadata` 与 `Task.Properties/Data`。

## 问题定义

当前 `one-hub` 的主要问题，不是价格公式太简单，而是缺少一个单一结算边界。

现状中至少存在四个结构性问题：

1. `Quota` 同时承担定价、预扣、最终落账、日志记录、统计更新等多种职责。
2. `Quota.Consume()` 目前通过 goroutine 异步落账，导致“什么时候算真的结算完成”并不清晰。
3. 同步 HTTP、realtime、task 各有各的 finalize 路径，但最终扣费没有统一入口。
4. `Usage`、`UsageEvent`、`ResponsesUsage`、provider 内部 accumulator 都在描述 usage，但只有一部分真正进入最终扣费语义。

结果是：

- retry、streaming、realtime、extra billing、task completion 的边界各自为政
- 日志、统计、真实扣费副作用混在一起
- 很难证明“这次请求最终到底扣了什么、为什么只扣了一次”

## 设计目标

`V1` 的目标是：

1. 给最终结算建立单一入口。
2. 不改变现有价格公式与 extra billing 语义。
3. 不强行统一所有生命周期模型。
4. 在尽量不改数据库的前提下，显著降低重复结算、异步丢结算、职责混乱的风险。

## 非目标

`V1` 明确不做下面这些事：

1. 不引入 full ledger / double-entry accounting。
2. 不拆独立 billing service。
3. 不做 `FundingSource` 之类的大而全支付来源抽象。
4. 不重写当前 attempt 级 reserve/pre-consume 语义。
5. 不消除 retry 期间 quota 的暂时占用与回滚抖动。
6. 不把 consume log 或统计报表升级为账务真相源。
7. 不承诺 process crash / Redis 故障下的 durable exactly-once。
8. 不做数据库 schema 变更。

## 为什么 `V1` 不重写 reserve 模型

第一轮 review 的结论非常明确：`V1` 不应该把 reserve 改成 request-scoped single reserve。

原因不是这个方向永远不对，而是它在当前代码下迁移风险过高：

1. retry 后 `channel_id`、`group_ratio`、`new_model`、model mapping 可能漂移。
2. prompt token 估算可能随着 provider 选择或请求重解析变化。
3. streaming / realtime / task 的生命周期粒度并不一致。
4. 当前实现里预扣与撤销已经深度耦合在 attempt 级控制流中。

所以 `V1` 的取舍是：

- 保留当前 `Quota.PreQuotaConsumption()` / `Quota.Undo()` 语义
- 不先优化“预扣过程是否优雅”
- 先统一“最终只怎么算一次、扣一次”

这是复杂度/收益比更高的点。

## 目标架构

### 1. `types.Usage` 作为唯一最终结算输入

`V1` 不再引入第四种公开 usage 类型。

统一规则：

- `types.Usage`：唯一最终结算输入
- `types.UsageEvent`：realtime 增量观察类型，只在 finalize 前使用
- `types.ResponsesUsage`：协议 DTO，只在 provider / transport 层使用

所有非 `types.Usage` 的 usage 表达，进入最终结算前都必须先转换成 `types.Usage`：

- `UsageEvent -> ToChatUsage()`
- `ResponsesUsage -> ToOpenAIUsage()`
- provider accumulator 输出最终也落到 `types.Usage`

这样 `V1` 的收口点只有一个：`ApplySettlement(cmd)` 只吃 `types.Usage` 语义。

### 2. `SettlementCommand` 作为唯一 canonical settlement object

`SettlementCommand` 必须保持最小化，避免再次长成一个新的 `Quota`。

`V1` 里它只保留最终 commit 真正必需的字段：

- `identity`
- `user_id`
- `token_id`
- `channel_id`
- `model_name`
- `pre_consumed_quota`
- `final_quota`
- `usage_summary`
- `unlimited_quota`
- `request_kind`
- `fingerprint`

其中：

- `identity` 是结算身份，不等于所有路径都用 `request_id`
- `usage_summary` 只保留结算所需的最小 usage 摘要
- 富日志、完整 quota breakdown、调试字段、报表字段都不进入 `SettlementCommand`

这些信息都属于 projection，不属于 truth path。

### 3. `ApplySettlement(cmd)` 作为唯一最终结算入口

`ApplySettlement(cmd)` 是 `V1` 的核心。

它负责的不是所有副作用，而是按优先级拆成三层：

#### 3.1 Settlement truth

只有下面两项属于 truth：

1. user quota 的最终 delta
2. token quota 的最终 delta

这个 delta 的语义仍保持与当前系统一致：

- `delta = final_quota - pre_consumed_quota`

`ApplySettlement(cmd)` 的 truth phase 约束如下：

1. 必须同步执行。
2. 必须绕开 `BatchUpdateEnabled`。
3. 必须优先使用一个小事务包装 direct DB write。
4. 不允许复用会被 `BatchUpdateEnabled` 影响的 helper 作为关键落账入口。

也就是说，`ApplySettlement(cmd)` 不能把当前 `PostConsumeTokenQuotaWithInfo(...)` 的现状原样搬过去当作最终答案；它需要明确进入一个不受 batch 配置影响的 direct write 路径。

#### 3.2 Post-commit cleanup

下面这些属于 cleanup，不属于 truth：

1. realtime quota cache reconcile
2. Redis/cache invalidation
3. 其他结算成功后的缓存修复动作

关键约束：

- cleanup 只能发生在 truth commit 之后
- cleanup 失败只告警、记录、重试
- cleanup 失败不能把整个 settlement 判定为失败
- cleanup 失败后也绝不能重试整笔 settlement

这条边界必须写死。

#### 3.3 Projection / audit

下面这些全部降级为 projection：

1. `RecordConsumeLog`
2. `channel.used_quota`
3. `user.used_quota`
4. `request_count`
5. dashboard / statistics / 报表

它们的关键约束：

1. 它们不参与 settlement success 判断。
2. 即使 projection 失败，也不允许回滚 truth。
3. projection 的计数基准必须是 `FinalQuota`，不是 `delta`。

最后这条非常重要。预扣大于实扣时，如果 projection 用 `delta` 记账，统计会直接失真。

### 4. `Quota` 在 `V1` 中保留，但职责收窄

`V1` 不会立即删除 `Quota`，但会把它的职责收窄。

保留：

- 现有 price math
- extra billing 计算
- attempt 上下文
- log metadata 生成辅助

调整：

- `Quota.Consume()` 不再自己完成全部副作用
- `Quota.Consume()` 必须同步构建 `SettlementCommand` 并调用 `ApplySettlement(cmd)`
- 不再允许像当前 `ConsumeUsage()` 那样启动 goroutine 异步落账

明确不做：

- `V1` 不修 `Undo()` 的旧语义
- `Undo()` 当前异步返还的问题继续保留

也就是说，`V1` 不试图同时解决“最终结算边界不清”和“预扣/撤销模型不优雅”这两个问题。

## 生命周期划分

`V1` 不统一所有 flow 的生命周期模型，只统一它们的 final settlement 入口。

### 1. Unary HTTP

范围：

- `/v1/chat/completions`
- `/v1/completions`
- `/v1/responses`

语义：

1. 保留 attempt 级 `PreQuotaConsumption / Undo`
2. 成功路径统一 `types.Usage -> SettlementCommand -> ApplySettlement(cmd)`
3. `request_id` 在 unary HTTP 里主要用于观测和排障
4. unary HTTP 默认不依赖 `Redis gate`

### 2. Realtime turn finalize

realtime 的问题和 unary HTTP 不同。

它的 finalize 可能：

- 脱离最初 request call stack
- 因为 turn 状态更新、终止事件重复、补发 finalize 被再次触发

因此 realtime 需要独立的 settlement identity。

`V1` 约束：

- identity 最少使用 `session_id + turn_seq + phase`
- 必要时附带 `last_response_id` 指纹
- `UsageEvent` 必须在 finalize 边界转换成 `types.Usage`
- Redis/cache reconcile 仍然只属于 cleanup，不属于 truth

如果需要防重复 finalize，`Redis gate` 只允许挂在这个 identity 上，不能复用 unary 的 `request_id`。

### 3. Task async finalize

task 的 finalize 已经脱离 submit request 生命周期，因此不能复用 submit 时的临时内存状态。

`V1` 约束：

- identity 优先使用本地 `Task.ID + phase`
- 如果某条 finalize 路径暂时只能拿到第三方任务标识，则最少使用 `user_id + platform + task_id + phase`
- 如需异步 finalize，最小 settlement context 只能写入现有 `Task.Properties` 或 `Task.Data`
- 不改表结构

如果 task finalize 仍需要在稍后重新计算 `final_quota`，则必须持久化足够的 resolved pricing snapshot，而不是依赖未来时刻的 live config 重新推导。

换句话说，异步 finalize 不能默认假设：

- 价格配置没变
- group ratio 没变
- 渠道上下文还在内存里

这部分上下文若未来仍是计算输入，就必须以现有 JSON 字段快照保存。

## `Redis gate` 的定位

`Redis gate` 在 `V1` 里不是通用能力，而是选择性硬化层。

只建议用于下面这些路径：

1. realtime turn finalize
2. task completion / failure finalize
3. 其他会脱离原始 request 堆栈、可能重复触发 finalize 的路径

不建议用于：

1. 普通 unary HTTP 成功路径
2. 仅为了让 `request_id` 看起来更像幂等键的场景

同时必须区分两种失败：

1. `condition_mismatch`
   - 表示已有 finalize winner
2. `backend_error`
   - 表示 Redis 不可判定
   - 不得当成普通 miss

如果 Redis 不可用：

- unary HTTP 继续工作
- async finalize 不提供强防重保证
- 文档和实现都必须明确降级，而不是伪装成 still exactly-once

## 与 `BatchUpdateEnabled` 的关系

这是 `V1` 里必须写死的约束。

`BatchUpdateEnabled` 可以继续存在，但只能控制 projection。

它绝不能参与：

1. user quota 真正扣减
2. token quota 真正扣减
3. settlement success/failure 判断

因此 `ApplySettlement(cmd)` 的 truth phase 必须使用 direct DB write。

如果现有 helper 会因为 `BatchUpdateEnabled` 而退化成异步批处理，就不能进入 critical path。

## 落地顺序

### Phase 1

先改同步 HTTP 成功路径：

- `chat/completions/responses`
- 统一 `Quota.Consume -> SettlementCommand -> ApplySettlement`
- 保持 `PreQuotaConsumption / Undo` 不变

### Phase 2

再接 realtime finalize：

- `UsageEvent -> types.Usage`
- 明确 turn 级 identity
- 如需要，再加 Redis gate

### Phase 3

最后接 task：

- 先定义 task finalize identity
- 再把最小 settlement context 写进现有 JSON 字段
- 不改表

这个顺序是刻意选择的：

- 先处理最常见、最容易验证的同步路径
- 再处理会脱离 request 生命周期的 finalize
- 最后处理最容易受异步状态与配置漂移影响的 task

## 测试要求

### 必测 1：truth path 同步执行

断言：

- `Quota.Consume` 不再通过 goroutine 异步落账
- `ApplySettlement(cmd)` 返回前，truth phase 已完成

### 必测 2：`BatchUpdateEnabled` 不影响 truth correctness

断言：

- 打开/关闭 `BatchUpdateEnabled`
- user/token quota 的最终结果完全一致

### 必测 3：projection 失败不重复结算

断言：

- consume log 写失败
- channel/user used quota 更新失败
- request_count 更新失败
- 都不会触发第二次 settlement

### 必测 4：projection 基于 `FinalQuota`

断言：

- 预扣大于实扣时
- consume log / used quota / request_count / channel used quota 的统计语义仍正确

### 必测 5：unary HTTP 保持旧 reserve 语义

断言：

- retry 时仍按当前 attempt 级 `PreQuotaConsumption / Undo`
- `V1` 不改变 reserve/rollback 语义

### 必测 6：realtime finalize identity 正确

断言：

- 同一连接多个 turn 不会互相误判为重复 finalize
- 重复 finalize 命中同一 identity 时不会双扣

### 必测 7：task finalize 不依赖易变 live config

断言：

- submit 与 finalize 之间若价格配置变化
- task finalize 仍然按 snapshot 语义结算

## 明确拒绝的方案

为了防止后续实现跑偏，这些“不做”必须写死：

1. `V1` 不做 request-scoped reserve。
2. `V1` 不让 `SettlementCommand` 重新长成一个新的 `Quota`。
3. `V1` 不把 consume log / used quota / statistics 当成账务真相。
4. `V1` 不要求 unary HTTP 全量接入 `Redis gate`。
5. `V1` 不承诺 crash-safe exactly-once。
6. `V1` 不做 full ledger。
7. `V1` 不做数据库 schema 变更。
8. `V1` 不拆 billing service。

## 最终判断

这版方案解决的是“最终结算边界不清”。

它刻意不解决：

- reserve 过程是否优雅
- retry 期间 quota 是否会短暂抖动
- 异步 projection 是否能像账本一样强一致

这个取舍是有意的。

对 `one-hub` 当前阶段来说，更高 ROI 的不是把系统一次性改成重型账务架构，而是先把：

- 结算输入
- 结算对象
- 结算入口
- truth / cleanup / projection 边界

这四件事彻底定清。
