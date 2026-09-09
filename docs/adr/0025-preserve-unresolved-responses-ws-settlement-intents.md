---
status: superseded by ADR-0030
---

# Preserve unresolved ResponsesWS settlement intents

ADR-0030 supersedes this decision and removes unresolved intents: a turn without authoritative provider usage now Cancels its reservation with zero final charge.

A Responses WebSocket provider close, EOF, malformed event, timeout, or downstream cancellation without `response.completed`, `response.failed`, or `response.incomplete` is not final usage evidence. The relay therefore keeps the forced pre-consumption as quota truth and stores one bounded, expiring unresolved intent keyed by turn attempt ID with the response ID, owner/channel, observed usage, floor, and proposed settlement amount. It does not run final settlement until authoritative evidence is supplied by a future reconciliation path.

This deliberately adds neither a generic billing ledger nor an automatic reconciler. Permanently applying partial usage or the floor was rejected because it destroys the distinction between transport loss and provider completion; refunding was rejected because provider work may already exist. Records expire after 30 days and share the existing lifecycle cleanup job, so retained evidence has one SQL owner and a bounded lifecycle.

Intent insertion is idempotent on the turn attempt ID and compares the complete durable evidence payload on conflict. The actor retries only classified transient MySQL, PostgreSQL, SQLite, bad-connection, and network failures, using two short backoffs inside the existing detached lifecycle deadline. A final persistence failure increments a low-cardinality outcome metric, leaves pre-consumption untouched, and never invents a terminal settlement. This closes brief database-failure windows without adding an unbounded worker or changing the conservative billing policy.
