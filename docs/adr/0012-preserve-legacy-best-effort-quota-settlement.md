---
status: superseded by ADR-0030
---

# Preserve legacy best-effort quota settlement

ADR-0030 supersedes this decision: it retains the small pre-consumption amount model but removes local-estimate charging and Redis settlement authority.

GPT-5.6 production requests keep the established quota flow: admission uses the existing small pre-consumption and high-balance fast path, relay errors refund that pre-consumption, and successful responses adjust the balance from reported or locally estimated usage. Unary settlement does not gain a durable identity or outbox. This preserves existing billing behavior and avoids a new financial ledger, while explicitly accepting that ambiguous provider execution, concurrent high-balance requests, process failure, or a failed final adjustment can undercharge or leave only the initial pre-consumption; these audit findings are accepted deviations rather than completed fixes.

Redis deduplication uses explicit `pending` and `committed` states with the same 24-hour idempotency window. Only `committed` is a successful duplicate; `pending` is an in-progress or indeterminate truth attempt and every replay must fail loud until it expires. A SQL failure proved before commit releases its pending gate with owner-checked CAS under a fresh bounded context. Once commit has been attempted, an error or process crash keeps the gate pending and the system deliberately abandons recovery rather than risk charging twice. When Redis is enabled, gate acquisition failures fail closed; deployments that explicitly disable Redis retain the legacy single-process best-effort behavior. This at-most-once policy may undercharge at the crash boundary and does not add a SQL receipt, settlement table, migration, cleanup job, or dual-store consistency protocol.

ADR-0025 adds a narrow exception for Responses WebSocket turns that lose their transport before a lifecycle terminal. That unresolved intent preserves evidence while leaving pre-consumption as truth; it is not a final settlement, outbox, or general ledger.
