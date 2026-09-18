---
status: accepted
---

# 按原子 Price Component 结算

本决策仅在 ADR-0030 要求一个完整 owner-wide usage 对象之处取代 ADR-0030。一次 Settlement Decision 仍执行一次余额动作，但它独立定价原子 Price Component，并确认每个 Priceable component 之和；当另一个独立 component 仍可定价时，Missing 或 Conflicting Evidence 只让该 component 免费。

Price Component 是无需借用另一个 component 即可产生确定金额的最小证据集。相互依赖的 token 总量和 cache、media、TTL、input 或 output 分区保持在一起；actual model、tier 和 provider scope 是依赖，而不是零价格 component。Confirm 至少需要一个包含完整 provider-originated billable quantity 的 component，而合法的零数量可以产生零值 Confirm。

Component 行、component 账本、动态定价表达式和第二个 Billing Attempt 状态机被拒绝。Component 只存在于 owner reducer 内，持久 owner 用其既有 terminal 事务持久化最终费用和诊断。
