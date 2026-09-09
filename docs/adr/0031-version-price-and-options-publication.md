---
status: accepted
---

# Version Price and Options publication

Price and Options each use a monotonic database revision as their write-order truth and publish only validated complete values. One read observes one complete publication; separate stages may observe different revisions as decided by ADR-0034. Direct table edits are outside the supported surface, and an instance whose published revision differs from the database head is not ready.

Price and Options remain separate owners and only share small CAS and complete-publication primitives. Historical catalogues, request-level snapshots, distributed locks, event buses, and a common configuration platform were rejected because bounded polling detects stale instances while ADR-0034 permits later stages to read a newer publication.

Price readiness may additionally check the bounded `pricing.required_models` startup list. That list is deployment configuration rather than a persisted manifest or runtime management API.
