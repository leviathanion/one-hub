---
title: "one-hub Async Task 架构设计"
layout: doc
outline: deep
lastUpdated: true
---

# one-hub Async Task 架构设计

## 文档状态

- 状态：当前实现。
- 适用范围：Suno、Kling、后台 Responses 与 OpenAI Batch 异步任务。
- 计费共同语义见 [基于下游 Usage 的 TCC 计费](./usage-confirmed-tcc-billing-architecture.md)。

## 目标与边界

Task 行是异步提交、provider identity、后台状态推进和计费 finalization 的持久 owner。当前实现不建设通用 workflow engine、callback 协调协议或 one-hub public task handle。

模块职责：

| 模块 | 职责 |
| --- | --- |
| `relay/task/main.go` | reserve、创建 owner、claim submission、单次 provider submit、持久化 acceptance |
| `model/task_billing.go` | Task 状态 CAS 与余额在同一 SQL 事务内变更 |
| `relay/task/base/settlement.go` | Suno/Kling 无 usage 的 Cancel 与失败状态映射 |
| `relay/responses_background*`、`model/responses_background.go` | 后台 Responses 的原始交付、查询观察和持久 evidence |
| `relay/batch.go`、`model/openai_batch.go` | Batch 提交、结果有界扫描及单 Task 结算 |
| `relay/task/task.go` | 后台扫描和状态推进 |
| `relay/task/suno`、`relay/task/kling` | provider 请求、响应解析和业务状态映射 |

## 持久状态

`tasks.id` 是 owner identity；`tasks.task_id` 是 provider 查询句柄。计费生命周期由以下字段表达：

- `provider_state`：`prepared -> submit_started -> accepted -> closed`；
- `reserved_quota`：Try 阶段的小额 Reservation；
- `charged_quota`：`NULL` 表示未 final，非空表示已 Confirm/Cancel；
- `settlement_decision`：缺少合格 usage 时为 `cancel`；后台 Responses/Batch 的可归属组件可以形成 `confirm`；
- `token_quota_applied`：记录 Reservation 是否同时作用于有限额 token。

业务状态不能反推计费状态；`SUCCESS` 也不能替代 provider usage。

## Submit

provider work 前在一个 SQL 事务内：

1. 锁定 channel、user、token；
2. 按该任务 family 的准入规则计算并预扣小额 `R`，价格读取当前发布；
3. 插入 `prepared` Task owner；
4. CAS 到 `submit_started`，取得唯一 Application Submission 权。

之后只调用 provider 一次。transport error、timeout 或结果歧义都不重试、不换渠道。provider 返回 task ID 后，Task CAS 到 `accepted`；身份持久化失败不能触发重发；按已取得的可信 evidence 与该 family 的歧义收尾规则执行一次结算。后台 Response 的 Task/ResponseOwner 及 Batch 的 Task/资源 owner/派生槽位分别在 acceptance 事务内绑定，不能只接受 Task 后再补资源归属。

## Fetch 与后台推进

Suno/Kling 的用户 fetch 只读本地 Task。后台 Responses 的 GET/cancel/恢复流与 Batch 的 retrieve/cancel 按已授权固定渠道转发并观察真实回执，不新建收费 owner。后台扫描器按 platform/channel 处理 accepted Task；新增 OpenAI family 使用固定上游 ID 查询，不重新 POST。`platform + user + task_id` 命中重复行时 fail closed。

Batch 已终态但已知结果 File 的派生归属仍未观察完成时，继续使用原 accepted Task、槽位及最小证据，在 48 小时窗口内只读恢复；完成或到期后才一次收尾。缺 usage 本身不延长 Task，不新增关闭后状态机。

## Finalize

Suno 与 Kling 当前都不返回可完整计价的 provider usage，因此成功、失败、timeout 和歧义终态统一执行：

```text
target_quota = 0
delta = -reserved_quota
settlement_decision = cancel
provider_state = closed
```

Task 终态、`charged_quota = 0` 与余额退款在同一 SQL 事务提交。poller、重复终态或失败清理再次进入时，`closed` 行直接返回第一次结果，不重复改变余额。

后台 Responses 与 Batch 已由各自 adapter 提取合格 usage，通过同一个 Task 事务 Confirm。Batch 不为 item 新建 Attempt，按稳定 custom_id 关联独立可计价组件后一次原子余额终结；全部或部分文件已删除导致证据不足，只影响相应收费，不拦截用户删除。价格在每次决策读取当前发布，不保存价格快照。

Background Responses/Batch 的首次 Task 关闭事务明确提交后，使用共享投影入口尽力写消费日志、渠道用量和用户请求次数，不再调用余额结算。重复关闭和 commit_unknown 回读恢复不重复投影；投影失败或提交后崩溃可能漏记，不回滚余额、不建设第二账本或 outbox。

## 不变量

1. provider submit 前必须存在持久 Task owner 和 Reservation。
2. 每个 Task 只能 claim 一次 submission，provider work 后不重放。
3. Task 行而不是 Redis 持有 finalization 执行权。
4. 没有 provider usage 时一律 Cancel，即使 provider 已 accepted 或业务成功。
5. 查询、流观察与后台扫描只竞争同一 Task 的版本及 finalization；不能相互覆盖新证据或重复收费。
6. `charged_quota` 非空后，任何重复终态都不得再次改变余额。

## 接受的限制

- provider 已产生实际成本但不返回 usage 时，客户最终免费。
- submit 进程在 provider work 后崩溃，Task 可能停留在 `submit_started`；现有歧义清理只终结本地追踪，不能重发或宣称上游没有工作。
- 当前没有 callback 协调、强恢复 ledger 或公共 task handle。

## 新增 OpenAI family 的留存边界

后台 Responses 每用户最多 32 个未收尾 Task，追踪窗口 24 小时；显式 store=true 使用普通 ResponseOwner 留存，其他后台响应授权元数据覆盖追踪窗口及额外 24 小时。Batch 每用户最多 32 个未收尾 Task，追踪窗口 48 小时。两类已结算 Task 留存 90 天，清理不触及仍未收尾的预扣；本地追踪截止不等于上游失败。

Task 的最小 evidence、身份和调度版本有界持久化，不保存完整生成内容。资源 owner 和 Task 分别拥有访问授权与执行结算：owner 的删除观察或清理不撤销已经接受的 Task，Task 也不通过文件租约约束用户调用。新增表和 worker 应随同版本启用；回滚先停止新 family 并保留能收尾的 worker，不能恢复旧库抹掉已发生消费。具体范围与验证见 [实施方案](./openai-transparent-relay-implementation-plan.md)。
