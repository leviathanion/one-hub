---
status: accepted
---

# 使用下游 Usage 确认 TCC 计费

> ADR-0033取代本文“必须形成一份owner-wide完整usage才能Confirm”的部分规则；一次Application Submission、一次Settlement Decision、provider-originated evidence、无本地估算和无第二次余额动作等边界继续有效。

one-hub 采用业务级 TCC 处理消费额度：Try 沿用旧方案的小额预扣 `R`，Confirm 只接受可归属、可形成 ADR-0033 原子 Price Component 的下游 provider evidence，并按 Confirm 时读取的当前完整价格发布得到最终费用 `C`；没有任何 Priceable component 时 Cancel 并释放全部预扣。Reservation 不是 Charge；provider success、transport 歧义、本地 token 估算和固定费用推测都不能替代下游 evidence。最终允许 `C > R` 的正向补扣和少量并发透支，以避免完整费用上界侵入 exact-wire 协议边界。

所有 one-hub 能在 provider work 前观察到的 Work Action 都必须先完成 Try。唯一例外是 ADR-0008 定义的 provider-initiated OpenAI Realtime turn：VAD 或 idle-timeout 可在没有客户端 `response.create` 的情况下使 provider 先开始工作，one-hub 只能从首个关联 `response.created` 得知该 turn。该路径使用 `Reservation=0`、无 Submission Claim 的 post-work observation owner，按首次关联事件确定 principal 与执行归属，并且只允许同一 turn 的 owner-correlated evidence执行一次ADR-0033 Settlement Decision；没有Priceable component时收费为`0`。它不是 Try，不授权工作或重试，也不得扩展到客户端 `response.create`、HTTP、Responses WebSocket、Async Task 或其他可事前观察的操作。

价格、倍率与其他可配置策略的跨阶段读取遵循 ADR-0034；Try 与 Confirm 可以观察不同发布版本，已发生的 Reservation 和 provider evidence 仍是执行事实。

短请求/turn 只保留进程内一次 submission 与一次 Confirm/Cancel guard；Async Task 由 Task aggregate 持久化相同执行权。SQL 是 user/token 余额事实源，Redis 只做缓存，不承担 admission、结算防重或恢复执行权。provider work 可能发生后不重试或换渠道；无 usage 的歧义执行仍然 Cancel，这是明确接受 provider 成本可能无法向客户收回的产品取舍。

本 ADR 完整取代 ADR-0026，并取代 ADR-0012 中“本地估算可以结算”以及 ADR-0025 中“无 terminal usage 保留预扣并建立 unresolved intent”的计费决策。ADR-0027 的 Async Task identity、channel binding 和 durable owner 继续有效，其完整 Hard Reservation、`KeepReserved` 与 final 不正向补扣规则由本 ADR 取代。

## 考虑过的方案

- **完整上界 `U`**：能够保证 `0 <= C <= U`、避免正向补扣和正常 Confirm 后的负余额；但缺失 usage 时保留 `U` 与当前收费事实冲突，而且计算可强制上界需要封闭未知字段、强制最大输出/时长/工具次数并缩小支持面，因此不采用。
- **完全后付费，不做 Try**：金额路径最简单，但失去低余额准入和并发风险缓冲；用户明确要求保留 TCC，因此不采用。
- **原样恢复 legacy settlement**：保留了小额预扣，但本地估算、floor 和 Redis gate 仍会在无 usage 时收费或形成第二事实源，因此只复用其价格计算与差额结算，不原样恢复全部语义。

## 影响

- provider 实际执行但未返回 usage 时不收费，one-hub 主动承担该上游成本。
- 最终费用可超过预扣，余额可能短暂或永久为负；若未来要求余额绝不为负，再重新评估完整上界或分段 re-authorization。
- fixed-price operation 没有 provider usage/billable-units evidence 时只能免费或在 provider work 前拒绝。
- 短 owner 仍接受 crash/commit-unknown 的小额错算；风险超过预算时引入 SQL receipt，不把 Redis 升级为账务 owner。

用户已消费额度参与自动分组，因此 `used_quota += C` 与最终余额在同一 SQL 事务提交，并按余额加已消费重算组；Cancel 退回预扣后同样重算。请求次数和展示日志仍为异步投影。预扣期间允许临时分组偏差，异常时以零额度管理操作手动重算，不新增分布式恢复状态。
