# Accept the bounded cross-instance price administration race

Price administration uses a process-local mutex, database transactions, and a unique `prices(model)` index. It does not use a distributed lock. Concurrent bulk price writes from different instances can therefore observe stale rows and end with a last-writer result or a constraint error; administrators refresh and retry. This does not introduce persistent Price Groups or inheritance.

This is accepted because price writes are a low-frequency control-plane operation, while a distributed lock would add availability dependencies and require lease fencing to be correct. If concurrent administration becomes common, the next mechanism is an explicit persisted revision/CAS contract with conflict reporting, not an unfenced lock.

Legacy databases can already contain duplicate model rows from a release that did not enforce the index. Upgrade preserves those rows and starts the rest of the service, while the duplicated exact model is unavailable to runtime billing. Index creation is retried as an independent invariant check on every start rather than relying on a one-shot migration ID; after an administrator removes the ambiguous rows, the next start installs the constraint.
