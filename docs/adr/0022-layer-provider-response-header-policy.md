---
status: accepted
---

# Layer provider response header exposure by operation

Provider response headers use a deny-by-default policy: safe request metadata is combined with headers required by the current operation and unmodified body representation. Exact-wire preserves ordinary provider status, body, error, and stream semantics but does not authorize provider cookies, account/project identity, upstream rate-limit state, hop-by-hop fields, or unknown future headers. A confirmed shared-account authentication or quota failure keeps the provider HTTP status while replacing its body with the stable public account-error envelope; this is a credential boundary, not cross-protocol normalization. Full passthrough and blacklist approaches were rejected because shared provider credentials make account metadata a separate security boundary; dedicated-account exceptions require a future explicit channel capability rather than becoming the default.
