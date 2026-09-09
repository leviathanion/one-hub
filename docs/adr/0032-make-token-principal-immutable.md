---
status: accepted
---

# Make the Token principal immutable

A Token ID remains bound to one User for its entire lifetime. Changing the principal requires creating a new Token and revoking the old one; online transfer and any cross-instance active-attempt registry are not supported.

This makes authorization, reservation, settlement, long-lived session capacity, and audit records agree on one principal without coordinating all active requests. Preserving mutable transfer was rejected because a management write cannot atomically move every in-flight and durable owner across processes.
