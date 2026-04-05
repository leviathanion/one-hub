---
title: "one-hub Async Task 极简协调方案"
layout: doc
outline: deep
lastUpdated: true
---

# one-hub Async Task 极简协调方案

## 文档状态

- 状态：V0.9 去 `202` 收敛版
- 当前现状：当前代码目标仍是最小 task 协调，不承诺完整 task coordinator 语义；本文主要用于固定能力边界与接受的尾损
- 目标：在不新增接口、不新增 task 专用表的前提下，让 async task 方案尽量贴近当前代码，并把实现复杂度压到最低
- 前提：明确接受少量任务误杀、误退款、重复提交、上游其实成功但本地最终仍可能无法自动找回，甚至最终丢失；也接受极端情况下 provider 已 accepted 但 submit 返回 5xx、后续按现有 fetch 仍可能查不到任务；对 detached finalize，则接受 Redis gate 不可用时退化成弱防重 fail-open，但不为此改动现有共享结算逻辑

## 这版真正要解决什么

从第一性原理看，当前 one-hub 并不需要一个“功能完整的 task coordinator”。

这版只解决三件事：

1. provider submit 之前，先有本地占位 task 和冻结后的结算快照
2. async finalize 使用稳定的本地 identity，尽量避免双扣 / 双退
3. fetch / continue 保持现有接口与现有语义，fetch 只读本地状态，状态推进只保留一个内建来源：现有 sweeper

除此之外，下面这些能力这版都不追求：

- 不追求强恢复
- 不追求强 submit 幂等
- 不追求 accepted-but-untracked 一定可找回
- 不追求所有模糊 submit 都能被准确分类
- 不追求 fetch 读时刷新
- 不追求 callback / sweeper / fetch 三路并发协调

这不是缺陷，而是有意识的复杂度控制。

## 明确接受的损失

这版明确接受下面这些小概率损失：

1. provider 已接单，但本地 accepted 持久化异常时，submit 仍可能返回 5xx，后续按现有接口也可能查不到任务
2. transport 超时 / 解析失败后，本地删掉占位任务并重试，导致重复向 provider 提交
3. accepted 但没有稳定 handle 的任务，可能长时间停留 pending，最终仍被误杀或丢失
4. fetch 返回的状态在一个 sweeper 周期内不新鲜
5. Redis 或其他基础设施抖动时，detached finalize 会退化成弱防重 fail-open；极少数并发重入场景下，可能出现重复 finalize、重复记账或重复日志

这版的选择不是“绝不出错”，而是：

- 用更少的机制覆盖常见路径
- 把损失留在小比例任务级错误
- 不为了降低极端异常损失，引入第二套表、第二套接口或第二套状态机
- 但也不为了“更省代码”把计费 truth 边界做成系统性风险

## 明确不做什么

这版方案刻意不做下面这些事情：

1. 不新增跨平台统一 `/tasks` 接口
2. 不新增平台无关的 continue / resume API
3. 不把对外 `task_id` 迁移成 one-hub public handle
4. 不新增 task 专用表
5. 不做 `recovery_key` 抽象
6. 不做通用 reconcile service，也不做 submit 专用降级状态机
7. 不做通用 `properties v1` / merge framework
8. 不做完整 submit 三态的统一对外协议
9. 不做 crash-safe submit ledger
10. 不承诺 accepted 但没有 handle 的任务一定能自动找回
11. 不做 fetch 触发的远端刷新
12. 不做 callback 驱动的统一推进协议

## 为什么要继续收敛

前一版的问题不在于“考虑不周”，而在于承诺过多：

1. 一边不想加接口，一边又想引入新的 task contract
2. 一边不想加表，一边又想把 tracking / reconcile / routing 都塞进 `tasks.properties`
3. 一边想保持 provider native response，一边又想给模糊 submit 返回统一降级 envelope
4. 一边想低成本，一边又想获得接近 workflow engine 的恢复语义
5. 一边说接受状态滞后，一边又想让 fetch 读时刷新

这些目标叠在一起，只会把复杂度扩散到 submit、fetch、continue、sweeper、callback、finalize 的所有路径。

如果已经接受少量误杀、误退款、误记失败，那么更合理的做法不是继续补抽象，而是直接收敛到当前代码最容易站稳的边界。

## 能力边界与适用层级

为避免再把“通用能力上界”和“one-hub 当前落地”写成两份并行文档，这里直接把边界写清楚。

异步任务系统大致只有三种层级：

1. 纯透传代理
   - 服务端不保存本地 task state
   - 服务端不提供统一 fetch
   - submit 后客户端自己拿 provider handle 去查
2. 轻量 task coordinator
   - 服务端对外承诺稳定 task resource
   - 本地有稳定 query handle
   - query / refresh / sweeper / finalize 围绕这份本地资源推进
3. 重型 workflow engine
   - 多阶段状态机
   - queue / stream / reconcile service
   - 更强恢复和更强编排

one-hub 当前选择的不是第 2 层完整形态，也不是第 1 层纯透传。

更准确地说，它是：

- 保留本地 `tasks` 行和 settlement snapshot，解决 async finalize / refund / billing truth 边界
- 对外继续沿用 provider native `task_id` 与现有平台接口
- 不承诺稳定 public handle
- 不承诺 accepted 后一定可恢复查询
- 不承诺读时刷新、回调协调或 submit 强幂等

这也是为什么下面这些能力在“完整 task coordinator”里通常成套出现，但这版故意不接：

- `public_handle`
- `recovery_key`
- query 驱动 refresh
- reconcile service
- callback / sweeper / foreground 三路协调

原因不是这些能力没有价值，而是它们一旦出现，就不是一两个字段的小补丁，而是一整包语义承诺：

- accepted 后必须稳定找回
- 查询句柄必须独立于 provider handle
- handle 晚到时必须可恢复
- 多推进源并发时必须有明确合并规则

当前 one-hub 明确不做这包承诺，只保留最小本地跟踪与结算能力。

## 极简版核心结论

V0.9 收敛到下面几条硬约束：

1. 保留现有平台接口
2. `tasks.id` 继续作为内部 canonical identity，不升级成新的 public handle
3. `tasks.task_id` 继续表示 provider task id
4. `tasks.properties` 只保存 settlement snapshot 和最少量 tracking 信息
5. submit 成功路径必须先有本地占位 task，再调用 provider
6. 只要本地代码已经拿到正证据证明 provider 已 accepted，就不能走 placeholder 删除 + `Undo()`
7. 对外 submit 不新增 `202`、`accepted_but_degraded` 或其他专用降级状态
8. fetch / continue 只读本地任务，不承担远端刷新职责；查不到就报查不到
9. sweeper 是唯一内建状态推进源；是否以后补 callback，单独决策，不混进这版
10. detached finalize 继续绑定 `tasks.id`；async task 继续复用共享结算入口的 fail-open gate 行为，不单独改写现有计费逻辑

## 数据模型

### `tasks.id`

- 本地唯一 identity
- async finalize 的 canonical identity
- 只服务于内部幂等、日志与人工排障
- 不作为普通客户端的主查询句柄

这里要写清楚：

- 这版不把 `tasks.id` 升级成新的 public handle
- 不承诺客户端拿它直接走现有 fetch / continue
- 如果后续真的需要人工排障入口，再单独设计最小接口；不在这版里偷塞到 submit contract

### `tasks.task_id`

- 继续保持当前语义：provider task id
- 对外 fetch / continue / 管理查询仍然按现有语义使用它
- 不在这版里切换成 one-hub 自己的句柄

这么做的收益很直接：

- 不需要全链路迁移 `task_id` 语义
- 不需要改已有 fetch / continue 接口
- 不需要引入兼容层

代价也明确：

- 对外 task id 继续耦合 provider
- accepted 但拿不到 handle，或 handle 没有稳定持久化下来时，客户端暂时甚至永久无法按现有接口查询
- 这类任务如果按现有 fetch / continue 查不到，就直接按查不到处理，不做额外兼容

当前阶段，这个代价是可接受的。

### `tasks.properties`

`tasks.properties` 在这版里只承担一个主职责：

- 保存 async finalize 所需的最小 settlement snapshot

允许继续保留的 tracking 字段只有极少量、且必须贴近当前代码：

- `provider_accepted`
- `provider_task_id`

这版明确不建议再往里加：

- `recovery_key`
- `reconcile_reason`
- `lossy_recovery`
- `degraded_reason`
- 任意 routing / coordination 命名空间

原因很简单：

- 当前代码对 `properties` 的读写是整块反序列化和整块覆盖
- 如果继续往里堆更多协调字段，不引入 merge protocol 的前提下，很容易互相覆盖
- 与其做半套状态总线，不如明确只保留 settlement snapshot + 最小 tracking

诊断信息的落点也要明确：

- submit 当下的降级原因优先写系统日志
- 最终显式失败时，再写入 `fail_reason`
- 不为“排障更方便”把 `properties` 再扩成协调总线

同时必须把写时序不变量写清楚：

1. submit 路径先写 settlement snapshot；在 provider 返回前，任务还没有可用 tracking handle，sweeper 不应把它当成可同步任务
2. submit 路径在 provider accepted 后，最多再写一次最小 tracking；这一步结束前，不引入 callback、fetch 刷新或其他第二写源并发改同一行
3. sweeper / finalize 只在 submit 路径结束后的任务上推进终态；它们可以重写 `properties`，前提是 submit 已经结束
4. 这版不引入 callback、读时刷新或其他第二写源；以后如果新增任何写源，必须先证明不打破这几个时序约束，否则就需要 merge / version 机制

## submit 方案

### 主流程

submit 主流程保持简单：

1. 先预扣额度
2. 先写本地占位 task，拿到稳定 `tasks.id`
3. 把 settlement snapshot 冻结到 `tasks.properties`
4. 再调用 provider submit
5. 如果 provider 返回明确成功且拿到了稳定 handle，则回写 `tasks.task_id`
6. 正常成功路径返回现有 provider / 平台响应，不新增 submit 专用降级状态

这条顺序真正的价值只有两点：

1. finalize 依赖的计费上下文在 submit 时已经冻结
2. 正常成功路径下，本地先有 task 行，再有 provider handle

### submit 结果判定

为保持代码简单，这版仍然不要求所有 adaptor 实现一套完整的：

- `not_accepted`
- `accepted`
- `uncertain`

统一状态机。

但有一条不能再退的边界：

- 只要本地代码已经拿到正证据证明 provider 已 accepted，就不能再走普通失败清理路径

因此这版对外只保留下面两种最小分类：

1. `accepted`
   - provider 返回了明确成功响应
   - 响应体成功解析
   - 本地拿到了稳定 handle
   - accepted 相关信息已经稳定持久化在本地 task 行
2. `failed_or_ambiguous_before_acceptance`
   - 只能用于“无法证明 provider 已 accepted”的情况

另外还存在一类内部异常，但这版不把它升级成第三种对外状态：

- provider 已 accepted，但本地 accepted 持久化仍然异常
- 这类情况不新增 `202`、`accepted_but_degraded`、`local_task_id` 等公共 contract
- 内部应优先记日志，并避免错误执行 placeholder 删除 + `Undo()`
- 对调用方来说，这次 submit 可能表现为 5xx，后续按现有 fetch / continue 也可能查不到任务
- 这属于明确接受的极端尾损，不为它扩接口、扩状态机或引入新的查询句柄

也就是说，下面这些情况才允许被按失败处理：

- 超时
- 连接中断
- 响应解析失败
- 模糊 5xx
- 其他无法证明 provider 已 accepted 的 submit 失败

而下面这条边界要写得更保守：

- `provider success body 里没有可用 handle`，不自动等同于 accepted
- 只有 adaptor 有明确正证据证明 provider 已 accepted 时，内部才允许保留 placeholder 和 settlement snapshot 等待后续处理
- 但这仍然不是新的对外状态；现有接口查不到，就按查不到处理

不能再走普通失败清理路径的只有：

- provider 已 accepted，但本地 accepted 回写失败
- provider 已 accepted，且 adaptor 能低成本明确证明只是 handle 缺失

这条收口的 trade-off 很明确：

- 获得什么：删除 `202` / `accepted_but_degraded` 这条公共分支，避免客户端新 contract 和 submit 第三态
- 牺牲什么：极少数 provider 已 accepted 但本地持久化异常的任务，会表现成 5xx、not found 或最终丢失
- 为什么当前选择最合适：这是基础设施级小概率尾损，不值得为它引入新的公共状态和接口语义

### 失败路径

对 `failed_or_ambiguous_before_acceptance`，允许沿用当前最简单的处理方式：

1. 删除本地占位 task，或在删除失败时把它标成失败
2. 执行 `Undo()` 回补预扣额度
3. 继续沿用当前 provider 级 retry 策略

这意味着：

- 只有“无法证明 provider 已 accepted”的失败，才允许删除 placeholder + `Undo()`
- provider 已 accepted 但本地 accepted 持久化异常时，不新增专用 HTTP 状态，只把它视为内部异常尾损
- 这类尾损优先通过日志暴露，不通过新的对外 submit contract 暴露

### accepted 但没有 handle

这版仍然不为“accepted 但没有 handle”设计通用自动恢复协议，也不为它定义新的对外状态。

明确规则如下：

1. 不新增 `recovery_key`
2. 不引入新的通用 public query handle
3. 不要求每个 provider 都支持这条分支
4. 只有某个 adaptor 能低成本、明确证明 accepted 时，内部才允许保留本地 placeholder task 和 settlement snapshot
5. 现有 fetch / continue 不为这类任务补特殊查询语义；查不到就按查不到处理
6. 如内部实现选择保留 grace period，也只是 provider-specific 实现细节，不升级成平台级 contract
7. grace period 到期后，如果仍无 tracking handle，允许显式判失败并退款
8. 如果 adaptor 根本无法明确证明 accepted，就不要硬保留这条分支，直接归入 `failed_or_ambiguous_before_acceptance`

也就是说，这类任务在这版里的语义就是：

- 不是平台级承诺，只是 provider-specific 的低成本特判
- 无自动恢复承诺
- 不对调用方暴露新的 degraded 状态
- 可误杀
- 可丢失
- 可表现为 submit 5xx 或后续 not found

这么做牺牲的是少量 accepted task 的最终准确性，换来的是：

- 不需要新表
- 不需要 recovery join key 抽象
- 不需要 reconcile service
- 不需要新的统一客户端 contract

但要补上的底线也要写清楚：

- 后续如果最终失败退款，最终 `fail_reason` 或系统日志里必须能看出这是 handle 缺失尾损，不需要为此扩展 `properties`
- 不为了“少量 accepted task 查不到”引入新的公共查询句柄或新的 submit 状态

## finalize 与结算

### canonical identity

async task finalize 的 identity 继续优先使用：

- `task:<tasks.id>:finalize`

不要依赖 provider task id，因为 provider task id 可能：

- 为空
- 晚到
- accepted 后回写失败

### 结算规则

finalize 继续只做两件事：

1. 成功任务按 submit 时冻结的 `final_quota` 完成结算
2. 失败任务按 `final_quota = 0` 完成回补

这里继续复用现有 `ApplySettlement` 入口，不为 task 再发明第二条结算路径。

### gate 策略

这版不额外引入新的 reconcile service，也不为了 async task 单独改写共享结算入口的 gate 行为。

也就是说：

- detached finalize 的 canonical identity 仍然绑定 `task:<tasks.id>:finalize`
- async task 的 gate failure policy 保持与共享 `ApplySettlement` 一致，即 best-effort fail-open
- Redis gate 或类似 backend 出问题时，记录告警后继续写 truth
- 这意味着任务不会因为 gate backend 故障而卡住终态结算
- 代价是 gate 不可用时只剩弱防重；极少数并发 detached finalize 可能重复落账
- 不要求在 `tasks.properties` 里新增 reconcile 状态字段；系统日志足够，最终显式失败时再写 `fail_reason`

这是一条显式取舍：

- 获得什么：不影响现有共享结算逻辑，Redis gate backend 故障时任务仍能完成结算与退款
- 牺牲什么：gate 不可用时只剩弱防重，极少数并发 detached finalize 可能出现双扣、双退或重复 projection
- 为什么当前选择最合适：当前目标是压低改动面，不为 async task 额外分叉一套 settlement policy；这类风险继续作为小概率尾损接受

## 查询与状态推进

### fetch / continue

这版不改现有平台接口：

- `suno` 继续走 `suno` 自己的 submit / fetch / continue
- `kling` 继续走 `kling` 自己的 submit / fetch

任务定位仍然保持简单：

1. 对已拿到 provider `task_id` 的普通任务，继续按 `platform + user + task_id` 查本地任务
2. 找不到就直接报错
3. 不 silent fallback 到其他任务
4. 如果出现重复命中，必须 fail-close 报错，而不是继续取第一条

同时要明确：

- 这版不新增 `local_task_id` / public handle 之类的新查询语义
- 如果某个任务因为没有稳定 `task_id` 或 accepted 持久化异常而查不到，就直接报查不到
- 不 silent fallback，不远端补查，不猜测匹配其他任务

### 状态推进

为控制复杂度，这版把状态推进收敛成：

1. fetch 默认先返回本地状态
2. fetch 不触发远端刷新
3. sweeper 负责长期无人查询、普通非终态任务和少量内部保留但仍缺少 tracking handle 的长尾任务
4. callback 不纳入这版
5. 不要求统一读层
6. 不要求 per-task 分布式刷新编排

最小 freshness budget 直接收敛成最简单规则：

- fetch 返回的就是本地状态，可能滞后一个 sweeper 周期
- sweeper 维持固定低频轮询即可
- 没有 tracking handle 的内部保留任务不做自动远端刷新，只等待后续人工处理或 grace period 到期后的 lossy fail

最小状态规则保持如下：

- terminal 一旦 finalize，不再被非终态覆盖
- 不引入第二条状态推进源去和 sweeper 并发写同一行任务
- 出现重复命中时 fail-close，而不是猜测覆盖
- accepted 但没有 handle 的任务，允许在 grace period 后显式失败和退款

最后一条是这版和前一版最大的区别之一：

- 前一版试图给这类任务补自动恢复协议
- 这版直接承认：默认不做自动恢复，也不做读时刷新；只有极个别 provider 能低成本证明 accepted 时，内部才保留这类任务等待后续处理

## submit 幂等

这版不额外设计 task-scoped submit 幂等机制。

明确不承诺：

1. crash 后历史去重
2. 模糊失败后的防重提交
3. 不新增表前提下的强审计 submit ledger

因此允许接受的小问题包括：

- 客户端重试导致少量重复 submit
- provider 已接单但本地失败后又重提

但有一个配套约束必须补上：

- 允许自动重试的，只能是 `failed_or_ambiguous_before_acceptance`
- provider 已 accepted 但本地 accepted 持久化异常时，不新增专用客户端 contract；如果客户端把这类 5xx 当失败重试，导致重复 submit，属于这版明确接受的尾损

如果未来业务真的需要更强的 submit 幂等，再单独引入持久化模型；不要继续往当前 task 协调文档里叠加抽象。

## 这版明确保留的底线

虽然这版接受更多误差，但仍然保留下面几条底线：

1. submit 成功路径必须先冻结 settlement snapshot
2. async finalize 必须优先绑定 `tasks.id`
3. provider 已 accepted 的本地持久化异常不升级成新的对外状态；内部只记日志，不发明第三种 submit contract
4. fetch / continue 查不到任务时不能静默落到别的任务；查不到就报查不到
5. 同一 `platform + user + task_id` 重复命中时必须 fail-close
6. async task finalize 的 gate backend 故障时沿用共享结算入口的 fail-open 行为，不单独分叉第二套 policy
7. fetch 不承担远端刷新职责，sweeper 是唯一内建推进源
8. 不把 `tasks.properties` 继续扩成通用状态总线，并且必须遵守上面的写时序不变量

这些底线保留下来，是因为它们的收益仍然明显高于复杂度成本。

## 落地顺序

如果按最小实现成本落地，顺序应该是：

1. 保持现有 submit 占位 task + settlement snapshot 主路径
2. 保持 accepted 成功后回写 `tasks.task_id` 和最小 tracking 信息
3. 对 provider accepted 但本地 accepted 持久化异常的 submit，不新增 `202`；只保留内部日志和尾损接受边界
4. 只有 `failed_or_ambiguous_before_acceptance` 继续按失败路径处理
5. fetch 保持纯本地读取，并把重复命中改成 fail-close；查不到就直接报查不到
6. sweeper 负责所有内建状态推进，并明确 grace period 之后才允许 lossy 失败
7. 保持 async finalize 使用 `tasks.id`，并继续沿用共享结算入口的 fail-open gate 行为
8. 如果某个 provider 能低成本明确证明 accepted-but-no-handle，也先作为内部 provider-specific 特判处理；不要先上升成平台级 public contract

如果后续还要补，只补最低成本的硬化点：

1. 个别 provider 的 submit 成功 / 失败判定细化
2. 个别 provider 的最小人工排障入口

不要先做：

- handle 迁移
- 统一 task API
- recovery key 抽象
- reconcile service
- fetch 读时刷新
- callback 协调协议

## 自审结论

从“代码复杂度尽量低”的目标出发，这版方案比前一版更合适，原因是：

1. 它更接近当前代码真实能力边界
2. 它不再要求 fetch / callback / sweeper 三路并发推进同一任务，把状态推进收敛成一条最便宜的路径
3. 它不再为极小概率 accepted 持久化异常引入 submit 第三态、`202` 或新的公共查询句柄
4. 它只把 accepted-but-no-handle 留给个别 provider 的内部低成本特判，而不是上升成平台级通用承诺
5. 它把主要损失诚实地暴露为任务级误差，包括 5xx、not found 与最终丢失，而不是再扩一层公共状态机
6. 它保留了 `tasks.id` + settlement snapshot 这两个最值钱的结构性收益
7. 它不再为 async task 额外分叉 gate policy，而是继续复用共享结算入口，保持改动面最小

这版不是“最正确”的 async task 方案，但在你已经接受少量误杀、误退款、误记失败的前提下，它是当前复杂度收益比更好的点位。
