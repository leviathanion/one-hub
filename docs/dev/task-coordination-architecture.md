---
title: "one-hub Async Task 架构设计"
layout: doc
outline: deep
lastUpdated: true
---

# one-hub Async Task 架构设计

## 文档状态

- 状态：当前实现。
- 适用范围：Suno、Kling 异步任务。
- 计费共同语义见 [基于下游 Usage 的 TCC 计费](./usage-confirmed-tcc-billing-architecture.md)。

## 目标与边界

Task 行是异步提交、provider identity、后台状态推进和计费 finalization 的持久 owner。当前实现不建设通用 workflow engine、callback 协调协议或 one-hub public task handle。

模块职责：

| 模块 | 职责 |
| --- | --- |
| `relay/task/main.go` | reserve、创建 owner、claim submission、单次 provider submit、持久化 acceptance |
| `model/task_billing.go` | Task 状态 CAS 与余额在同一 SQL 事务内变更 |
| `relay/task/base/settlement.go` | 无 usage 的统一 Cancel 与失败状态映射 |
| `relay/task/task.go` | 后台扫描和状态推进 |
| `relay/task/suno`、`relay/task/kling` | provider 请求、响应解析和业务状态映射 |

## 持久状态

`tasks.id` 是 owner identity；`tasks.task_id` 是 provider 查询句柄。计费生命周期由以下字段表达：

- `provider_state`：`prepared -> submit_started -> accepted -> closed`；
- `reserved_quota`：Try 阶段的小额 Reservation；
- `charged_quota`：`NULL` 表示未 final，非空表示已 Confirm/Cancel；
- `settlement_decision`：当前为 `cancel`，未来只有 provider 返回合格 usage 时才能为 `confirm`；
- `token_quota_applied`：记录 Reservation 是否同时作用于有限额 token。

业务状态不能反推计费状态；`SUCCESS` 也不能替代 provider usage。

## Submit

provider work 前在一个 SQL 事务内：

1. 锁定 channel、user、token；
2. 按旧公式计算并预扣小额 `R`；
3. 插入 `prepared` Task owner；
4. CAS 到 `submit_started`，取得唯一 Application Submission 权。

之后只调用 provider 一次。transport error、timeout 或结果歧义都不重试、不换渠道。provider 返回 task ID 后，Task CAS 到 `accepted`；持久化失败则关闭为 `UNKNOWN`，由于没有 provider usage，Cancel Reservation。

## Fetch 与后台推进

用户 fetch 只读本地 Task，不顺手访问 provider。后台扫描器按 platform/channel 聚合 accepted task，调用对应 adaptor 获取状态并写回。`platform + user + task_id` 命中重复行时 fail closed。

## Finalize

Suno 与 Kling 当前都不返回可完整计价的 provider usage，因此成功、失败、timeout 和歧义终态统一执行：

```text
target_quota = 0
delta = -reserved_quota
settlement_decision = cancel
provider_state = closed
```

Task 终态、`charged_quota = 0` 与余额退款在同一 SQL 事务提交。poller、重复终态或失败清理再次进入时，`closed` 行直接返回第一次结果，不重复改变余额。

如果未来某个 async provider 提供合格 usage，必须先由 adapter 定义 usage 完整性与计价映射，再在同一个 Task 事务内 Confirm；不能把 accepted task ID、业务成功或固定次数价格当成 usage。

## 不变量

1. provider submit 前必须存在持久 Task owner 和 Reservation。
2. 每个 Task 只能 claim 一次 submission，provider work 后不重放。
3. Task 行而不是 Redis 持有 finalization 执行权。
4. 没有 provider usage 时一律 Cancel，即使 provider 已 accepted 或业务成功。
5. fetch 只读，后台扫描器是当前唯一内建状态推进源。
6. `charged_quota` 非空后，任何重复终态都不得再次改变余额。

## 接受的限制

- provider 已产生实际成本但不返回 usage 时，客户最终免费。
- submit 进程在 provider work 后崩溃，Task 可能停留在 `submit_started`；需要运维或未来 sweeper 规则关闭，但不得重新提交 provider work。
- 当前没有 callback 协调、强恢复 ledger 或公共 task handle。
