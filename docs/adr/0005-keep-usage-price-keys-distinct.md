---
status: accepted
---

# 保持 usage 价格键彼此独立

> ADR-0036 取代本文的证据键命名与 TTL 价格回退表述：内部键改用 provider 原始字段名，`cache_creation_input_tokens` 与 TTL 拆分保持二选一计价。证据独立、价格独立、缺少证据各自回退的决定继续有效。

provider 以不同名称暴露的 usage 字段保持为独立定价键，包括 `cached_tokens`、`cache_read_input_tokens`、`cache_write_tokens` 和 `cache_creation_input_tokens`。每个键有自己的全局兜底价格和可选管理员覆盖；系统不在它们之间做别名、合并或迁移覆盖。这可能需要管理员显式配置新观察到的字段，但避免把不同的 provider 记账证据当作可互换。

Claude cache creation 有一个刻意收窄的价格读取例外。当 Claude TTL 键缺失时，`ephemeral_5m_input_tokens` 和 `ephemeral_1h_input_tokens` 可以各自读取显式 `cache_creation_input_tokens` 价格，然后在两者都没有覆盖时使用各自内置默认值 1.25 和 2。显式零仍然保留。该兜底只改变有效价格查找：它不别名 provider usage 证据、不合并 TTL 分区、不复制或迁移数据库覆盖，也不把通用价格作为额外费用。

内部定价证据不自动属于公共响应协议。Typed Responses 和 Chat 投影只输出各自公共 wire 契约定义的字段，而授权的 exact-wire 原始回放保留原始 provider JSON，包括未知的 future 与 provider-specific 字段。序列化不得擦除结算所用的内部证据。
