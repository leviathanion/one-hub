---
status: superseded by ADR-0030
---

# 以 SQL Hard Reservation 收敛单次消费结算

> ADR-0034进一步取代本文的 admission-time Price/Group 快照边界；下文保留为已废弃方案的历史记录。

one-hub 将 user/token 余额收敛为同一 SQL 事务拥有的唯一 truth。每个 Billing Attempt 只持有一个 Work Action：provider work 前计算完整可强制的 Customer Charge Quota 上界 `U`，要求 `U` 不超过 code-owned 单次错算预算，并建立 `reserved_quota = U` 的 Hard Reservation；共享 reducer 只决定一次 `Rollback / ExactCharge / KeepReserved`，final 只能退款或保持预扣，不再正向补扣。`U` 不表示也不约束 Upstream Provider Cost。Redis、cache、日志和 telemetry 都不拥有 admission、结算或恢复执行权。

Billing Attempt 进入 active 后只有一次 Application Submission 机会。`U > 0` 只有在 SQL reserve 明确提交后才能 active；`U = 0` 仍保留完整 Billing Attempt、submission claim 和 closure，只跳过余额写入。one-hub 不做应用层 retry、状态码 retry或 provider failover，ambiguous 不恢复；这个承诺不等于 provider-observable at-most-once，也不提供客户端请求幂等。客户端重复提交会创建新的 owner；provider 幂等键只服从 provider 自身契约，既不授权 one-hub 重试，也不充当本地计费 receipt。

短 owner 只存在于当前 request/actor，进程崩溃和 commit unknown 不恢复，歧义时最多保留 Hard Reservation。Async Task 等持久 owner 使用自己的 SQL aggregate，在业务状态与余额动作同一事务时重读确认；不建立通用 attempt 表、ledger、outbox、reconciliation 或 Redis lease。

多 action ResponsesWS/Realtime scope 不进入共享计费抽象。只有能在每个 action 前独立计算上界、独立 reserve 并独立关联 terminal evidence 的串行 Work Action 才能作为 Billing Attempt；provider-initiated work、自动计费功能、无法拆分的 multi-agent/inject 和上界超过单次错算预算的 operation 不支持。

结算关键的 model Price Policy 与 group ratio 在 admission 时从 SQL 权威数据构造一个一致性快照；并发管理事务在线性化点前后任选其一，已建立的 owner 不再读取 live policy。进程本地 price/group cache、默认价格、legacy option、远端 price service 响应和 telemetry 都不能直接进入 BillingPlan；远端价格必须先校验并持久化为权威 Price row。无唯一有效 Price Policy、wildcard 解析不确定或快照读取失败时，在 provider work 前失败。System Tool Price Catalog 仍是 hosted-tool 基础价的唯一 code-owned owner，不与 model Price Policy 混用。

## 考虑过的方案

- 小额 floor 后按实际费用补扣：会在并发请求下形成没有 SQL reservation 担保的累计 Customer Charge Quota，因此不采用。
- 为多 action session 建立进程内 Bounded Billing Scope：它仍需要 action claim、ACK barrier、aggregate evidence 和大额歧义预扣，复杂度与错算风险都不符合当前预算。
- 使用自定义 RoundTripper 证明 provider-observable at-most-once：客户端 transport 无法替代 provider 幂等契约，且该问题不属于余额 truth，因此只保留一次应用层调用规则。
- 建立 SQL receipt/ledger：能提供客户端级幂等、恢复和审计，但当前接受单 owner 有界错算，收益不足以覆盖持久状态与清理成本。

## 影响

- 所有支持的 operation 都必须建立完整 Hard Reservation；正数在 SQL 预扣，零值保留 owner 而不写余额，正常 terminal 只退款或 no-op。
- ambiguous、崩溃或 evidence 缺失可能让用户承担最多 `U` 的多扣，因此 `max_reservation_quota` 是产品支持边界，而不是观测阈值。
- 长连接只支持串行、可独立计费的 action；无法拆分的 scope 在 provider work 前失败。
- 本文原定的 Price/Group admission snapshot 已废弃；现行边界见 ADR-0034。
- 若未来要求客户端级幂等或多 owner 总错算上界，升级触发器是在 SQL 中引入稳定 request identity 与持久 owner/receipt，而不是重新加入 Redis billing state。
