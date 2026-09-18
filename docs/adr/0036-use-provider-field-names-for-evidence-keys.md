---
status: accepted
---

# 内部证据键使用 provider 原始字段名

计费与定价使用的内部证据键直接采用 provider 原始字段名：OpenAI 用 `cache_write_tokens`，Claude 用 `cache_creation_input_tokens`、`cache_read_input_tokens` 和 `cache_creation.ephemeral_5m/1h_input_tokens`，不再自造 `cached_*` 别名。这些键只作为内部证据和管理端计价键；公共响应继续按各自协议白名单投影，不暴露 provider 私有字段。

Claude 的 `cache_creation_input_tokens` 是总额，`cache_creation` 的两个 TTL 字段是它的拆分，官方保证总额等于拆分之和。计价在总额与拆分之间二选一：拆分完整且与总额一致时按两个 TTL 键计价，否则按总额键计价，绝不重复计算。同一条 usage 同时出现两个写方言键时各按自己的证据计价，不静默合并或取最大值。

## 影响

- 升级通过一次性迁移改写 `prices.extra_ratios` 与 `rate_rules.extra_multipliers` 中的旧键；迁移之外不再识别旧键，价格表与接口配置必须使用新键名。
- 这是一次停机变更：迁移完成后旧二进制不再识别价格配置中的证据键，滚动升级不要与旧实例混跑。
- 历史消费日志保留旧键，统计回填迁移负责汇总列；前端明细只渲染新键。
- 本决策取代 ADR-0005 的键名与 TTL 价格回退表述；证据独立、价格独立、缺少证据各自回退的决定继续有效。
