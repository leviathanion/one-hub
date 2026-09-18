---
status: superseded by ADR-0018
---

# 把条件定价对象命名为 Rate Rules

> `RateRules` 仍是窄类型字段可接受的名称，但不再有 legacy-modifiers 兼容 schema。

条件档位与长上下文定价对象在 Go 中命名为 `RateRules`、在 canonical JSON 中命名为 `rate_rules`，因为它定义有效费率如何被选择，而不是应用通用 modifier。既有数据库列可以保留为物理兼容细节，legacy `modifiers` JSON 只作为同一状态派生出的 deprecated alias 被接受；canonical 与 legacy 输入冲突时拒绝。这避免破坏性数据迁移，同时防止出现两份可独立修改的表示。
