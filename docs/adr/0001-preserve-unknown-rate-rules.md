---
status: superseded by ADR-0018
---

# 保留来源不明的 Rate Rules

> ADR-0018 的保留设计只包含三条命名 Rate Rules，不再有未知维度或 provenance 链。

没有 provenance 的 legacy Rate Rules 视为管理员所有，启动或自动同步时不回填。Rate Rules 只有在显式 system provenance 或管理员采用系统规则后才变为系统管理；这会让旧模型缺少新引入的条件倍率，但避免升级和重启静默改单。既有基础价格同步不受影响。
