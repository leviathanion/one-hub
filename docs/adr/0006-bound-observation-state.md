# Bound observation state

> Superseded for the GPT-5.6 remediation by ADR-0011, ADR-0012, ADR-0014, and ADR-0018. The new plan may retain bounded wire projections, but not this ADR's durable observation identity, first-model lock, or observation-led accounting design.

> ADR-0030 and ADR-0034 supersede this ADR's billing decisions: only Authoritative Provider Usage can Confirm a charge, and each billing stage reads the current complete pricing publication instead of retaining an admission-time policy.

Relay observation keeps bounded memory and never delays provider payload delivery for unbounded parsing or durable accounting. A later ordinary terminal event can recover evidence after an oversized non-terminal event, but an individually oversized terminal may remain unknown and lose local observation billing. This rare failure is accepted instead of disk spooling, an unbounded parser, or a durable outbox; delivery failures remain observable through metrics and logs.

For native Responses WebSocket passthrough, a parsed provider terminal is enqueued even if delivery to the downstream client fails. If the provider connection closes before a terminal carrying a stable response identity arrives, the relay does not manufacture a durable per-turn record; connection diagnostics cover that rare case because durable incomplete-turn correlation would require an attempt journal. Observation identity is the channel ID plus provider response/resource ID, so different wire projections deduplicate while extreme provider ID reuse can collide. Existing usage-derived identities are not backfilled, so the first post-upgrade re-observation of an old provider resource can create one canonical row.

When a provider returns a different model name, an explicitly configured exact or wildcard price is resolved at settlement. A missing actual-model price may use the request model's current explicit policy; no global fallback price is fabricated. Concurrent catalogue updates may be observed by later billing stages as accepted by ADR-0034.

本文原先允许无 token usage 的固定费用推断，该计费决定已由 ADR-0030 取代：缺少 Authoritative Provider Usage 时必须 Cancel。Native Responses WebSocket 的连接模型约束不受此替代影响。
