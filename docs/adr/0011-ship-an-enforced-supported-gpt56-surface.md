# Ship an enforced supported GPT-5.6 surface

> Operation support and channel configuration in this decision are superseded by ADR 0023. Operation support is now model-independent and derived from adapters; model names only participate in ordinary channel model selection and model mapping.

The first production release guarantees a deliberately bounded Supported Contract Surface rather than partially exposing the entire GPT-5.6 API. Production `/v1` routes remain an Enforced Relay Surface and use Legacy Quota Settlement rather than observe-only accounting, while capabilities that do not yet have complete routing and lifecycle semantics fail closed before provider work begins. This trades immediate breadth for a smaller explicit contract without making half-open resource passthrough part of the public product.

The initial Responses surface includes create, Native Responses WebSocket, Stored Response retrieve/delete/input-items, `/responses/compact`, and `/responses/input_tokens` as explicit operations with their own capability and representability gates. Existing resource operations that require an administrator to pin an exact channel remain available only as Administrator-Pinned Resource Relay; they are not promoted into the ordinary-user contract and do not imply proxy-managed ownership.
