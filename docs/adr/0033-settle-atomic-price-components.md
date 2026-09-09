---
status: accepted
---

# Settle atomic Price Components

This decision supersedes ADR-0030 only where ADR-0030 required one complete owner-wide usage object. One Settlement Decision still performs one balance action, but it independently prices atomic Price Components and confirms the sum of every Priceable component; Missing or Conflicting Evidence makes only that component free when another independent component remains Priceable.

A Price Component is the smallest evidence set that can produce a definite amount without borrowing from another component. Interdependent token totals and cache, media, TTL, input, or output partitions stay together; actual model, tier, and provider scope are dependencies, not zero-price components. At least one component containing a complete provider-originated billable quantity is required to Confirm, while a legitimate zero quantity can produce a zero-value Confirm.

Component rows, component ledgers, dynamic pricing expressions, and a second Billing Attempt state machine were rejected. Components exist only inside the owner reducer, and durable owners persist the final charge and diagnostics with their existing terminal transaction.
