---
status: superseded by ADR-0030
---

# 保留 legacy best-effort 配额结算

ADR-0030 取代本决策：它保留小额预扣金额模型，但移除本地估算收费和 Redis 结算权威。

GPT-5.6 生产请求沿用既有配额流程：准入使用既有小额预扣和高余额快路径，relay 错误退回预扣，成功响应按上报或本地估算的 usage 调整余额。Unary settlement 不获得持久 identity 或 outbox。这保留既有计费行为并避免新财务账本，同时明确接受歧义 provider 执行、并发高余额请求、进程失败或最终调整失败可能少收费或只留下初始预扣；这些审计发现是被接受的偏差，而不是已完成的修复。

Redis 去重使用显式 `pending` 和 `committed` 状态，带同样的 24 小时幂等窗口。只有 `committed` 是成功重复；`pending` 是进行中或不确定的 truth attempt，每次重放都必须 fail loud 直到过期。commit 前已证明的 SQL 失败在全新的有界上下文中通过 owner-checked CAS 释放 pending gate。一旦尝试 commit，错误或进程崩溃会让 gate 保持 pending，系统故意放弃恢复而不是冒双重收费风险。Redis 启用时 gate 获取失败 fail closed；显式禁用 Redis 的部署保留 legacy 单进程 best-effort 行为。该 at-most-once 策略可能在崩溃边界少收费，且不新增 SQL receipt、settlement 表、迁移、清理任务或双存储一致性协议。

ADR-0025 为在生命周期 terminal 之前失去 transport 的 Responses WebSocket turn 增加一个窄例外。该 unresolved intent 保留证据，同时让预扣保持为 truth；它不是最终结算、outbox 或通用账本。
