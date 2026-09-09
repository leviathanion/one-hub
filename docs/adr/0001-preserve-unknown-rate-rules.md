# Preserve rate rules with unknown provenance

> Superseded by ADR-0018. The accepted design keeps only three named rate rules and no unknown-dimension or provenance chain.

Legacy Rate Rules without provenance are treated as administrator-owned and are not backfilled during startup or automatic synchronization. Rate Rules become system-managed only through explicit system provenance or an administrator action that adopts the system rules; this may leave old models without newly introduced conditional rates, but prevents upgrades and restarts from silently changing bills. Existing base-price synchronization is unaffected.
