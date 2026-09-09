---
status: accepted
---

# Read configurable policy at each use

Price, group ratio, Options, and other configurable policy are read from the current complete publication at each decision point; a request, billing attempt, session, or asynchronous action does not pin a configuration revision across stages. Reserve and settlement may therefore use different prices, and other stages may observe an update mid-operation, while already executed amounts, selected upstream ownership, provider evidence, and other action facts remain stable.

This deliberately accepts within-operation inconsistency when configuration changes at runtime; operators may instead use a stop-the-world update when they want to avoid observing that behavior. The decision avoids historical catalogues, request snapshots, version fences, and duplicated lifecycle plumbing; control-plane CAS prevents lost writes but does not promise request-level consistency. A future requirement for legally auditable or externally promised point-in-time policy would be the trigger to introduce a durable policy identity for that specific boundary.
