---
title: "Billing / Usage 结算统一方案"
layout: doc
outline: deep
lastUpdated: true
---

# Billing / Usage 结算统一方案

## 文档状态

- 状态：V1 主干已部分实现，后续实现与重构以本文为准
- 目标：在不新增表、不引入独立 billing service 的前提下，统一 unary / realtime / async task 的最终扣费边界

## 设计目标边界

这份设计首先不是精确记账系统设计。

one-hub 当前这套 billing / usage 机制，主要目标是：

1. 给配额控制提供一个稳定的最终扣费边界
2. 给运营和产品分析提供尽量稳定的 usage 统计口径
3. 避免 retry、async finalize、projection 失败带来的大幅重复统计或大幅漏统

它不是为了追求逐请求、逐分、逐厘都完全精确的账务系统。

明确允许的现实约束是：

- 允许非常小范围的多记或少记
- 允许个别请求在极端异常下出现轻微偏差
- 允许 projection 滞后或偶发缺失

但不允许：

- 同一请求被系统性重复多扣、多记
- 某条路径长期稳定性偏高或偏低，形成系统性偏差
- 小误差持续累积成肉眼可见的统计失真
- 为了追求账本级精确，引入明显高于收益的表、状态机、服务拆分

换句话说，V1 追求的是：

- bounded error
- stable aggregation
- low-complexity correctness

而不是：

- financial-grade exact accounting
- crash-safe exactly-once ledger semantics

## 结论先行

如果只追求边际收益最大的方案，V1 就只做下面四件事：

1. 保留现有 reserve 模型，不重写 `PreQuotaConsumption()` / `Undo()`
2. 所有最终扣费统一走一个同步入口 `ApplySettlement`
3. 明确区分 truth、cleanup、projection，禁止混写
4. async 场景只复用现有 `Task` JSON 字段和 Redis，不新增任何持久化账务模型

同时明确一个前提：

- 这套方案追求“小偏差内稳定可用”，不是“绝对精确记账”

这版方案允许的偏差只有三类：

1. 一个进程内收口对象 `SettlementCommand`
2. detached finalize 场景下的 Redis 幂等 gate
3. async task 场景下复用 `Task.Properties` / `Task.Data` 保存最小 snapshot

除此之外，V1 明确不做：

1. 不做 billing ledger / settlement history 新表
2. 不做 request-scoped single reserve
3. 不做 full double-entry accounting
4. 不做独立 reconciliation service
5. 不承诺 crash-safe exactly-once

## 问题本质

当前 one-hub 的主要问题不是价格公式，而是最终扣费边界分散。

具体表现为：

- reserve 与 finalize 混在不同路径里
- retry 后 `channel_id`、`group_ratio`、model mapping 可能漂移
- realtime / async task 的 finalize 生命周期脱离原始请求
- `logs`、`used_quota`、`request_count` 这类 projection 很容易被误当成账务真相

从第一性原理看，系统先要保证的只有三件事：

1. 最终到底扣了多少
2. user / token truth 为什么只落一次
3. projection 失败为什么不会破坏 truth

只要这三件事站稳，后面 reserve 是否优雅、是否做 ledger、是否追求更强幂等，都可以延后。

进一步说，这里“站稳”指的是：

- 在绝大多数正常路径下，统计口径稳定
- 在异常路径下，误差被限制在很小范围内
- 不出现大规模、系统性、可持续放大的偏差

## 现有模型已经足够

V1 的关键判断是：现有表已经足够承载这一版能力，不需要新增表。

### truth

账务真相只落在现有列上：

- `users.quota`
- `tokens.remain_quota`
- `tokens.used_quota`

### projection

统计与审计继续落在现有列 / 表上：

- `users.used_quota`
- `users.request_count`
- `channels.used_quota`
- `logs`

### async snapshot

异步 finalize 所需上下文只复用现有 JSON 字段：

- `tasks.properties`
- `tasks.data`

因此 V1 不是“数据库不变但系统模型大改”，而只是把现有扣费流程收口。

这里的 “truth” 也不是财务语义上的绝对账本真相，而是 one-hub 在当前复杂度预算下用于：

- quota 控制
- 最终消费落点
- 统计基线

的一组最小可信字段。

## 核心设计

### 1. `types.Usage` 是最终结算输入

所有 token-based 流程在进入最终扣费前，都统一归一到 `types.Usage`。

边界如下：

- `types.Usage`
  - 最终结算输入
- `types.UsageEvent`
  - realtime 增量观测类型，只在 finalize 前存在
- provider / transport 层 DTO
  - 只停留在协议层，不进入 settlement truth

V1 不再引入新的 canonical usage model。

### 2. `SettlementCommand` 只是进程内收口对象

`SettlementCommand` 的目的很简单：把最终结算需要的最小 truth 信息收拢到一个参数对象里。

建议字段：

- `identity`
- `fingerprint`
- `request_kind`
- `user_id`
- `token_id`
- `channel_id`
- `model_name`
- `pre_consumed_quota`
- `final_quota`
- `usage_summary`
- `unlimited_quota`

它的边界必须写死：

- 它不是新表
- 它不是新账本
- 它不是长期存活的领域模型
- 富日志、调试信息、埋点统计不进入它

### 3. `ApplySettlement` 是唯一 truth 入口

所有最终扣费必须统一走：

- `ApplySettlement(cmd, opts)`

这个入口只负责三层事情。

#### 3.1 truth

truth 只做一件事：

- 同步落 user / token 最终 delta

公式固定为：

`delta = final_quota - pre_consumed_quota`

要求：

- 必须同步执行
- 必须直接写 DB
- 不允许受 `BatchUpdateEnabled` 影响

这里坚持同步 direct write，不是因为要做精确账本，而是因为这是当前复杂度下最便宜、最稳的“止血点”：

- 可以明显减少重复扣费和漏回补
- 可以让 quota 控制和统计基线至少落在同一条边界上
- 不需要为了一点点精度提升，再额外引入新表和新服务

#### 3.2 cleanup

cleanup 只处理派生缓存，例如：

- realtime quota cache reconcile
- user quota cache refresh

要求：

- 只能在 truth 成功后执行
- cleanup 失败只告警
- cleanup 失败不回滚 truth

#### 3.3 projection

projection 只处理统计和审计，例如：

- `RecordConsumeLog`
- `channels.used_quota`
- `users.used_quota`
- `users.request_count`

要求：

- projection 不参与 settlement success 判定
- projection 失败不回滚 truth
- projection 的统计口径使用 `final_quota`，不是 `delta`

最后一条非常重要。

truth 的职责是纠正 reserve 和最终费用之间的差值；
projection 的职责是记录本次请求最终花了多少。

如果 projection 偶发失败，V1 接受有限偏差；
但如果 projection 统计口径本身错了，例如拿 `delta` 记最终消费，那就会制造持续性系统偏差，这不接受。

### 4. 保留当前 reserve 模型

V1 不重写 reserve 生命周期。

仍然保留当前 attempt-scoped 语义：

- `PreQuotaConsumption()`
- `Undo()`

原因不是它完美，而是它的替代成本太高：

- retry 语义会一起被牵动
- realtime / task finalize 也会一起被牵动
- 迁移和回归测试成本远高于收益

所以 V1 只解决：

- 最终怎么算一次
- 最终怎么扣一次

不解决：

- reserve 是否优雅
- retry 期间 quota 是否抖动
- reserve 是否应该 request-scoped

这也是一个有意识的精度取舍：

- reserve 过程允许存在很小的暂态误差
- 但最终 settlement 边界必须尽量稳定
- 不为了消灭暂态误差，引入远超收益的复杂生命周期重构

### 5. Redis gate 只用于 detached finalize

Redis gate 不是通用能力，只用于脱离原始请求生命周期、存在重复 finalize 风险的路径：

- realtime turn finalize
- async task finalize

不用于：

- 普通 unary HTTP 成功路径

identity 建议：

- realtime：`session_id + turn_seq + phase`
- task：优先 `Task.ID + phase`
- 如果拿不到本地行 ID，最少退化到 `user_id + platform + task_id + phase`

identity 还必须满足一个硬约束：

- detached finalize 的 identity 不能为空串
- `identity = ""` 只能视为实现未完成或异常保护失效，不能作为 steady-state 语义
- 对 async task，如果第三方 `task_id` 可能为空，提交路径就必须先拿到本地稳定 identity，优先使用 `Task.ID`

这里必须明确：

- Redis gate 只是防重辅助
- 它不是账务真相
- 它不能演变成 Redis 版 billing ledger
- 不允许因为不同业务路径的风控强度不同，就分裂出第二条 settlement truth 入口

对于 gate 不可用时怎么降级，必须先明确系统到底是要分路径选 policy，还是统一复用共享入口的现有行为。

当前 one-hub 为了不影响现有结算逻辑，统一沿用共享 `ApplySettlement` 的 `best_effort` 行为：

- `best_effort`
  - 记录告警后继续执行 truth
  - 适用于业务上接受“弱防重但不中断”的 detached finalize 路径
  - 这是当前 async task / realtime detached finalize 的统一选择

如果 Redis 不可用：

- unary HTTP 继续工作，因为它本来不依赖 Redis gate
- detached finalize 继续执行 truth，但退化成弱防重
- 文档里必须明确这是降级，不伪装成 exactly-once

这意味着：

- Redis gate 的价值是抑制“大偏差”
- 不是把系统推到“零偏差”

### 6. Task snapshot 只复用现有 JSON 字段

async task 的问题不是“怎么算”，而是 finalize 发生时 live config 可能已经变了。

所以 submit 成功后，需要把 finalize 所需的最小上下文写进现有 JSON 字段，而不是等任务完成时再根据实时配置重算。

建议最少保存：

- `identity`
- `fingerprint`
- `reserve_quota`
- `model_name`
- `channel_id`
- `token_id`
- `user_id`
- `final_quota` 或等价的价格快照
- `settlement_status`

这里也必须明确边界：

- snapshot 只是复用 `Task.Properties` / `Task.Data`
- 它不是新的 task 子表
- `settlement_status` 只是 JSON 里的轻量标记
- 它不能升级成一套新的持久化状态机

### 6.1 Async submit 的 durable acceptance 边界

V1 虽然不做 ledger，但 async submit 仍然必须定义清楚本地 durable acceptance 边界。

如果系统要对外承诺“accepted 后可稳定找回”，那必须满足下面这些约束：

- provider 已 accepted 之后，在向客户端返回成功前，必须已经存在可持久读取的本地 `Task` 行、snapshot 与 settlement identity
- 不能出现“远端任务继续执行、本地无记录、reserve 未回补、客户端只看到普通 5xx 可重试”的 silent orphan task
- provider accepted 之后的首次本地持久化，不能只是“再尝试插一行”；如果这一步失败却没有 durable reconcile handle，这条路径就是不完整的

在“不新增表”的前提下，优先选择的最小正确形态是：

1. 先准备本地 `Task` 占位行，拿到稳定 `Task.ID`
2. provider submit
3. provider accepted 后，把最小 settlement snapshot 冻结到这条本地行
4. 后续 finalize 全部基于这条已存在的本地行进行

如果实现暂时做不到这条边界，就不能再假设系统具备“accepted 后仍可稳定找回”的能力。

在 one-hub 当前这版极简方案里，处理方式是更保守地承认边界：

- provider accepted 后的本地落库失败，仍可能表现为 5xx
- 后续按现有 fetch / continue，也可能直接查不到任务
- 这属于明确接受的尾损，不为它额外引入新的 reconcile contract、submit 第三态或公共查询句柄

这不是理想语义，但它比“为了极小概率尾损再扩状态机和接口”更符合当前复杂度预算。

### 6.2 存量任务兼容 / rollout guardrail

没有 settlement snapshot 的旧 task 是迁移债务，不是 steady-state 正确路径。

因此 rollout 必须遵守：

- 旧 task 的 fallback finalize 只能作为兼容兜底，不能长期成为主路径
- fallback rollback 必须保持 user truth 与 token truth 的对称性，不能只退 `users.quota`
- `IncreaseUserQuota(...)` 但不恢复 `tokens.remain_quota / used_quota` 的 user-only refund 是禁止的，因为这会把 truth 永久打歪
- 如果旧 task 上缺少精确回补所需信息，应进入显式 repair-needed / reconcile 路径，而不是静默接受部分回补

## 三类请求的生命周期

### Unary HTTP

流程：

1. `PreQuotaConsumption()`
2. provider 执行与 retry
3. 成功后归一 usage
4. 构造 `SettlementCommand`
5. `ApplySettlement`
6. 失败则 `Undo()`

这一条路径不需要 Redis gate。

### Realtime

流程：

1. 累积 `UsageEvent`
2. finalize 时归一到 `types.Usage`
3. 构造 `SettlementCommand`
4. 如有重复 finalize 风险，则先过 Redis gate
5. `ApplySettlement`

这条路径的重点是：增量观测和最终结算必须分开。

### Async Task

流程：

1. 预扣额度，并准备稳定本地 task identity，优先先拿到本地 `Task.ID`
2. provider submit
3. provider accepted 后，立即把最小 settlement snapshot durable 到本地 `Task.Properties` / `Task.Data`
4. completion / failure 时读取 snapshot
5. 如存在重复 finalize 风险，则仍走同一个 `ApplySettlement` 入口，并沿用共享 `best_effort` gate policy
6. success 用已保存的最终金额结算
7. failure 用 `final_quota = 0` 结算，完成 reserve 回补

这条路径的重点是：finalize 不能依赖 submit 之后可能漂移的 live config。

## 复杂度控制规则

为了把复杂度控制在收益拐点以内，后续演进必须遵守下面这些硬约束：

1. 如果现有列和 JSON 字段还能表达，就不新增表
2. 如果 `ApplySettlement` 还能兜住，就不新增第二条 truth 编排路径
3. 如果问题只发生在 detached finalize，就不要把 Redis gate 扩散到所有请求
4. 如果 projection 可以失败后补偿，就不要把 projection 拉进 truth 事务
5. 如果 async task 已保存 snapshot，就不要在 finalize 时依赖 live pricing / live config 重算
6. 如果一个方案只是把误差从“很小”继续压到“极小”，但会显著增加模型复杂度，就拒绝它

## 为什么这是边际收益最大的点

这版设计的收益主要来自四点：

1. correctness 明显提升，因为最终扣费边界统一了
2. schema 成本为零，因为不新增表
3. 迁移成本可控，因为 reserve 模型不动
4. realtime / async task 这两条最容易重复扣费的路径也被纳入同一套边界

而它刻意放弃的东西，恰好是高复杂度低短期收益的部分：

1. request-scoped single reserve
2. full ledger
3. crash-safe exactly-once
4. billing service 拆分

所以这是当前 one-hub 最合适的复杂度/收益平衡点。

更直接地说：

- 如果目标是做精确记账，这套方案一定不够
- 但如果目标是“控制小偏差、保证统计可用、压住复杂度”，这套方案正好落在收益最优点

## 参考实现位置

当前代码里与本文对应的核心位置如下：

- `internal/billing/settlement.go`
- `relay/relay_util/quota.go`
- `relay/task/base/settlement.go`
- `model/token.go`

## 测试要求

至少要覆盖下面这些行为：

1. truth path 不受 `BatchUpdateEnabled` 影响
2. projection 使用 `final_quota`，不是 `delta`
3. detached finalize 在 gate 正常可用时，相同 identity 下不会双扣；gate 不可用时退化成弱防重
4. task finalize 使用 submit 时保存的 snapshot，而不是漂移后的 live config
5. task failure 能把 reserve 正确回补
6. 异常路径即使出现偏差，也不会形成明显系统性放大
7. provider accepted 后的本地持久化失败，会被明确归类为两种之一：要么满足 durable acceptance 边界并可稳定找回，要么被文档和日志显式承认为 5xx / not found / 丢失尾损，而不是隐含成未说明语义
8. async task / realtime detached finalize 的 identity 不会为空
9. 兼容旧 task 的 fallback rollback 仍然保持 user/token truth 对称
