---
status: superseded by ADR-0022 for provider response headers
---

# Forward client headers on registered HTTP surfaces

Exact-wire HTTP relay forwards client request headers on explicitly registered API surfaces, except for hop-by-hop fields and credentials or routing selectors that the proxy itself owns. A configured channel endpoint is therefore inside the request's trust boundary and can observe those request headers; operators must not point a channel at an endpoint they do not trust. Provider response header exposure is governed separately by ADR-0022; this ADR no longer authorizes `Set-Cookie` or unknown response headers.

This choice preserves the supported request protocol. Interface scope is enforced separately: canonical resource paths must remain inside a registered route family, redirects are not followed, and the proxy replaces provider authentication fields with channel credentials.
