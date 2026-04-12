---
title: "one-hub Async Task 架构设计"
layout: doc
outline: deep
lastUpdated: true
---

# one-hub Async Task 架构设计

## 文档状态

- 状态：当前实现
- 适用范围：Suno、Kling 两类异步任务链路
- 目的：描述当前代码已经采用的 task 架构、边界和 trade-off，而不是保留未来 task coordinator 草案

## 这套架构真正解决什么

当前 one-hub 的 async task 并没有做成一个“完整 task coordinator”。

它解决的是更窄但更重要的三件事：

1. submit 之前先有本地 task 行和冻结后的结算快照
2. provider accepted 之后，finalize 使用稳定的本地 identity 收口结算
3. fetch、sweeper、finalize 围绕同一条本地任务记录推进，而不是到处现查现算

它明确不解决：

1. 通用 `/tasks` 平台抽象
2. callback / fetch refresh / sweeper 三路并发协调
3. accepted-but-untracked 的强恢复
4. crash-safe submit ledger
5. 公共 `public_handle` / `recovery_key`

当前系统的真实定位是：

`provider-facing async relay + local task row + local settlement snapshot + sweeper-driven status convergence`

## 当前实现的总链路

当前 async task 已经收敛为：

`prepare quota -> insert placeholder -> freeze snapshot -> provider submit -> persist acceptance -> local fetch / sweeper -> finalize settlement`

对应模块：

| 模块 | 职责 |
| --- | --- |
| `relay/task/main.go` | submit 主流程、placeholder 创建、accepted 持久化、失败清理 |
| `relay/task/base/settlement.go` | settlement snapshot、tracking、finalize、失败回补 |
| `relay/task/task.go` | 后台 sweeper、批量同步与空 `task_id` 长尾回收 |
| `relay/task/suno/*` | Suno submit / fetch / status 映射 |
| `relay/task/kling/*` | Kling submit / fetch / status 映射 |
| `model/task.go` | `tasks` 表读写与 fail-close 查询 |

## 当前数据模型

### `tasks.id`

- 本地数据库主键
- async finalize 的 canonical identity 来源
- 只服务于内部结算、防重和排障

当前代码在 task finalize 时优先使用：

```text
task:<tasks.id>:finalize
```

只有在本地行 ID 不可用时，才退化到 `platform + user + channel + task_id` 组合 identity。

### `tasks.task_id`

- provider 原生 task id
- 当前对外 fetch 的主查询句柄
- 仍然保持 provider-facing 语义

这意味着当前接口没有迁移到 one-hub 自己的 public handle。

### `tasks.properties`

当前 `tasks.properties` 已经被收敛成一份 task settlement snapshot，包含三部分：

1. `SettlementEnvelope`
2. settlement status：`reserved / committed / rolled_back`
3. tracking：`provider_accepted`、`provider_task_id`

它不再被当成“未来可能扩展成万能协调总线”的预留位，而是当前 task 架构里的最小冻结快照。

### `tasks.data`

- 保存 provider 返回的业务数据
- 用于 fetch 响应和后台同步后的展示
- 不参与结算 truth

## Submit 架构

### 1. 先持有额度，再写本地 placeholder

当前 submit 路径固定为：

1. 创建 `Quota`
2. async task 调用 `ForcePreConsume()`
3. 执行 `PreQuotaConsumption()`
4. 创建或复用本地 `Task`
5. 把任务置为 `SUBMITTED`
6. 清空 `task.TaskID`
7. 写入 `submit_time`、`channel_id` 等基础字段

这一步的目标不是“先把状态机做完整”，而是确保 provider submit 之前，本地已经有可挂接 settlement snapshot 的稳定行。

### 2. submit 前冻结 settlement snapshot

在真正调用 provider 前，当前代码会执行：

```text
BuildTaskSettlementSnapshotProperties(quota, task)
```

冻结内容包括：

- async task finalize identity
- user / token / channel / model 上下文
- `pre_consumed_quota`
- `final_quota`
- projection / cleanup 参数

随后把 snapshot 持久化到：

- `tasks.properties`
- `tasks.quota`

这意味着 async task 的价格和回补边界已经在 submit 时冻结，不依赖 finalize 时的 live config。

### 3. provider submit 成功后再写 accepted tracking

provider submit 成功时，当前代码不会立刻 finalize，而是先：

1. 把 provider `task_id` 放进 `task.TaskID`
2. `MarkTaskProviderAccepted(...)`
3. 回写 `provider_accepted` / `provider_task_id`
4. 持久化 accepted task
5. 激活后台 sweeper

accepted 之后的任务状态仍保持 `SUBMITTED`，等待后续 fetch / sweeper 推进到终态。

## 当前 accepted 边界

### accepted 前失败

如果 submit 返回失败，当前代码会：

1. 删除 placeholder；删除失败时把本地行标记为失败
2. 清空 snapshot
3. 执行 `Undo()` 回补预扣额度

这里的语义是：

- 只有“无法证明 provider 已 accepted”的失败，才允许删除 placeholder + `Undo()`

### accepted 后持久化失败

如果 provider 已返回成功、但 accepted 持久化失败，当前代码不会再 `Undo()`。

它会：

1. 先尝试完整持久化 `task_id + properties + status`
2. 若失败，退化到只持久化 `properties + status`
3. 若退化持久化也失败，则返回 `500`

这条边界很关键：

- 一旦本地已经拿到 provider accepted 的正证据，就不能再按“普通 submit 失败”处理
- 即使这会带来 query handle 不完整的长尾任务，也不能为了省事执行错误退款

### 当前 trade-off

- 获得什么：避免 provider 已接单但本地又 `Undo()` 的系统性错账
- 牺牲什么：极端情况下会留下 accepted 但没有稳定 `task_id` 的任务
- 为什么当前最合适：账务 truth 错误比偶发的 task 查询缺失更危险

## Finalize 架构

### 1. finalize 只基于 snapshot，不重算 live config

当前 `FinalizeTaskSettlement(...)` 的逻辑是：

1. 解析 `tasks.properties`
2. 只处理 `status == reserved` 的 snapshot
3. success：沿用 submit 时冻结的 `final_quota`
4. failure：把 `final_quota = 0`
5. 调用统一 `ApplySettlement(...)`
6. 根据结果把 snapshot 标记为 `committed` 或 `rolled_back`
7. 回写 `tasks.properties` 和 `tasks.quota`

因此 async task finalize 已经不再依赖：

- live pricing
- live group ratio
- live model mapping
- finalize 时临时推导的上下文

### 2. finalize 幂等依赖本地 identity

当前 async task finalize 的 identity 优先绑定 `tasks.id`。

这使得 detached finalize 可以复用共享 settlement gate，而不是再发明 task 专用账务入口。

### 3. failure finalize 是“回补 reserve”，不是第二套退款实现

task 失败时，当前主路径是：

1. `FinalizeTaskSettlement(..., false)`
2. 通过 `final_quota = 0` 得到 `delta = -pre_consumed_quota`
3. 由统一 settlement truth 完成 reserve 回补

只有旧任务没有 snapshot 时，才会退回 legacy refund 兼容逻辑。

## Fetch 架构

### 当前 fetch 只读本地任务

Suno / Kling 的 fetch handler 当前都只做本地查询：

- `GetTaskByTaskId(platform, user, task_id)`
- `GetTaskByTaskIds(platform, user, task_ids)`

不会在用户查询时顺手触发 provider refresh。

### 查询边界

当前查询 contract 很明确：

1. 按 `platform + user + task_id` 查本地任务
2. 命中重复记录时 fail-close，返回冲突错误
3. 查不到就返回 `task_not_exist`

### 当前 trade-off

- 获得什么：接口行为简单，fetch 不承担副作用
- 牺牲什么：查询结果可能滞后一个 sweeper 周期
- 为什么当前最合适：当前系统已经有后台同步器，没必要把读路径也变成推进源

## Sweeper 架构

### 1. sweeper 是唯一内建状态推进源

当前 `relay/task/task.go` 的后台 goroutine 被 accepted task 激活后，会循环：

1. 扫描未完成任务
2. 按平台分组
3. 按渠道聚合 provider `task_id`
4. 拉取 provider 状态
5. 回写本地任务
6. 在 success / failure 时触发 finalize

轮询周期当前固定为 `15s`。

### 2. 空 `task_id` 长尾任务会被延迟回收

对 accepted 但没有 tracking handle 的任务，当前代码不会立刻失败，而是：

1. 判断 `provider_accepted == true`
2. 如果 `task_id` 仍为空，等待一个 grace period
3. grace period 当前为 `1m`
4. 超时后执行 `FailTaskWithSettlement(...)`

这里的设计不是强恢复，而是有限等待后的有损收敛。

### 当前 trade-off

- 获得什么：给 accepted 持久化异常留出最小恢复窗口
- 牺牲什么：超过窗口后，任务会被本地判定为失败，即使上游可能已成功
- 为什么当前最合适：当前系统没有 callback、没有 recovery ledger，也没有额外 handle 协议，继续无限悬挂只会放大运维成本

## 平台 adaptor 的职责边界

当前 Suno / Kling adaptor 只负责平台相关部分：

1. submit 请求组装与响应解析
2. provider 状态到本地 `TaskStatus` 的映射
3. 平台返回数据写入 `tasks.data`

它们不负责：

1. 自己实现第二套结算逻辑
2. 自己实现防重 gate
3. 自己决定 fetch 是否远端刷新

这使得 task 架构的稳定边界留在公共层，而不是继续散落到 provider adaptor。

## 当前架构的关键不变量

1. provider submit 前必须先有本地 task 行和 settlement snapshot
2. async finalize 必须优先绑定 `tasks.id`
3. 只要已有 provider accepted 正证据，就不能执行 placeholder 删除 + `Undo()`
4. fetch 只读本地状态，不承担刷新职责
5. sweeper 是唯一内建状态推进源
6. task 终态一旦完成 settlement，就不能再被另一份终态覆盖
7. `platform + user + task_id` 重复命中时必须 fail-close

## 当前架构不做什么

1. 不新增 task 专用表
2. 不引入 one-hub public task handle
3. 不在 fetch 时顺手刷新 provider 状态
4. 不引入 callback 协调协议
5. 不承诺 accepted-but-untracked 一定能恢复
6. 不把 `tasks.properties` 扩成通用状态总线

## 结论

当前 one-hub 的 async task 已经不是“实现草案”，而是一套已落地的最小架构：

- 本地 `tasks` 行承接 submit、fetch、sweeper 和 finalize
- `tasks.properties` 冻结 settlement snapshot 与最小 tracking
- `tasks.task_id` 继续承担 provider-facing 查询语义
- sweeper 负责唯一的内建状态推进
- finalize 通过统一 settlement 入口完成成功扣费或失败回补

这套架构没有追求完整 workflow engine 能力，但它已经把当前系统最重要的边界固定下来：先有本地真相，再调 provider；accepted 后不误退款；async finalize 不重算 live config；读路径和写路径的职责不再混在一起。
