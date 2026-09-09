---
status: accepted
---

# 让 Task aggregate 持久拥有异步 submission 与结算

> ADR-0030 取代本文的完整 Hard Reservation、`KeepReserved` 与 final 不正向补扣规则；Task identity、immutable channel binding 和 durable owner 决策继续有效。

每个 Async Task 持有一个 Work Action 的一次 Application Submission、Provider Task Identity 和 immutable `channel_id` binding。小额 Reservation 与 Task 创建同事务；`prepared → submit_started` CAS 后只有原请求可以发送；业务终态、Settlement Decision、owner close 和余额差额同事务提交。

不建立通用 workflow engine 或 Redis settlement gate。当前 Suno/Kling 不返回合格 usage，因此终态一律 Cancel；未来支持 Confirm 时，usage 与计价维度必须由 adapter 显式映射。

accepted Task不会因统一年龄自动关闭；无法长期观察terminal的provider operation不支持。closed行的物理保留服从明确的Task数据政策，删除不重新授权settlement。

Polling 通过同一 Task 行进入 finalizer，不为 correctness 引入 Redis lease；裸 ID 或绕过 owner CAS 的 direct Save 禁止。

## 影响

- submit crash 可能留下 Reservation，但不得因此重新提交 provider work。
- provider accepted 但 delivery barrier 无法确认时可能产生不可见孤儿。
- provider 不返回 usage 时，one-hub 承担实际上游成本。
