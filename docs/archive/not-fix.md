# Not Fixed

This document records review-report items intentionally left unchanged because
the proposed "fix" would violate `docs/dev/codex-official-upstream-architecture.md`.

## C-9 / M-38: Preserve Unknown Fields In `/responses/compact`

Decision: not fixed.

`/responses/compact` is not ordinary `/responses` raw-body preservation. The
architecture defines compact as a dedicated transform: extract identity from the
ordinary raw body, then build a compact-specific payload from the typed lens.
Preserving unknown ordinary fields in the compact body would blur the operation
boundary and can accidentally send fields upstream that only make sense on
ordinary create.

Trade-off: we lose forward preservation for unknown compact fields until they
are explicitly added to the compact schema. We keep the more important property:
compact has a small, auditable payload surface and never leaks ordinary
`client_metadata` or unrelated create-only fields.

## H-17: Raw Responses Reject `temperature` + `top_p`

Decision: not fixed.

The planner intentionally rejects `temperature` and `top_p` when both are
present on raw `/v1/responses` ingress. The architecture calls hidden semantic
repair out as forbidden. The chat adapter is the only sanctioned typed-to-raw
synthesis boundary and already resolves this conflict before planner entry.

Trade-off: some non-official Responses clients get a 400 instead of proxy repair.
The gain is a clean ownership rule: client-owned raw Responses bodies are not
silently rewritten; synthesized chat bodies are normalized only at the adapter
boundary.

## H-18 / M-28: Do Not Reintroduce System/Developer Merge For Responses Ingress

Decision: not fixed.

Raw `/v1/responses` ingress treats the client body as the upstream JSON truth.
Reintroducing system/developer merge or default instructions injection would be
a semantic rewrite after ingress and would contradict the "no repair" rule.
Chat-to-Responses may define its own conversion contract because the adapter
authors that synthesized Responses body.

Trade-off: clients that depended on legacy provider message surgery must use
the chat adapter path or send official Responses fields themselves. The gain is
that `/v1/responses` becomes inspectable and future-field preserving.

## H-19: Keep `store=false` On Codex Official Create

Decision: not fixed.

The architecture explicitly lists `store=false` as a provider-owned body patch
for Codex Official create. OpenAI provider behavior is a different dialect and
does not determine Codex behavior.

Trade-off: callers cannot use Codex Official create through one-hub to request
OpenAI API store semantics. The gain is parity with the Codex official upstream
path and avoidance of cross-dialect state assumptions.

## H-21: Do Not Fallback Codex ResponsesWS To HTTP Bridge

Decision: not fixed.

Codex Official ResponsesWS is native-only by design. `responses_http_bridge`
returns `426 responses_ws_unsupported_for_channel`; silently bridging would
reintroduce the dual-dialect behavior the architecture removes.

Trade-off: channels without native Codex WS support fail fast. The gain is a
single upstream protocol boundary and predictable handshake identity planning.

## H-20: Append `reasoning.encrypted_content` Only For Reasoning Requests

Decision: not fixed.

The Codex Official architecture explicitly keeps this body patch narrow:
`reasoning.encrypted_content` is appended only when the raw request contains a
`reasoning` object. Appending it to every raw Responses create would be a
provider-owned semantic expansion that is not tied to evidence in the raw
envelope.

Trade-off: callers that want encrypted reasoning continuation must send the
official `reasoning` object. The gain is that the planner does not advertise a
reasoning output include for requests that did not ask for reasoning.

## H-31 / M-22: Keep Legacy Realtime Bridge Headers Out Of Official Scope

Decision: not fixed.

The remaining `session_id` / `x-session-id` compatibility code lives in the
legacy `/v1/realtime` bridge path. Codex Official HTTP Responses and native
ResponsesWS now route through `providers/codex/wire`, and
`TestCodexOfficialStaticContract` blocks those legacy helpers and headers from
the official source files.

Trade-off: the legacy realtime bridge still carries historical one-hub session
headers until that transport is deliberately retired. The gain is a clean
separation: Official paths stay enforceably legacy-free without bundling a
larger legacy realtime deprecation into this review pass.

## M-37: Empty `residency` / `default_originator` Remain Valid

Decision: not fixed.

Empty `residency` means "omit the residency header"; empty
`default_originator` means "fall back to the built-in Codex originator". The
architecture example uses empty residency explicitly.

Trade-off: config validation cannot use non-empty string as a type invariant for
these fields. The gain is a compact policy format where optional fields can be
present but intentionally disabled.

## H-12 / L-21: Keep Header Decision Audit At Debug Level

Decision: not fixed.

Header decision audit is high-cardinality per-request diagnostic data. It is
useful when debugging Codex Official parity, but too noisy for default info
logging. The implementation now avoids building the JSON payload unless debug
logging is enabled.

Trade-off: operators must enable debug logging to see decision traces. The gain
is that normal production logs stay focused and do not accumulate per-request
protocol audit records by default.

## M-35: Do Not Hide Trusted-Attestation Policy Rejection

Decision: not fixed.

The architecture requires `trust_client_attestation=false` plus a client
`x-oai-attestation` header to fail before upstream. Returning a generic
`invalid_request_error` for the field is enough sanitization; silently dropping
the header or pretending the request failed for an unrelated reason would hide a
policy decision that must be enforceable at the official boundary.

Trade-off: a client can infer that a channel does not trust client attestation
by probing with that header. The gain is a crisp protocol rule: untrusted
attestation is rejected locally and never reaches upstream.

## L-26: Do Not Add Channel-Wide `responses_lite` Policy

Decision: not fixed.

`x-openai-internal-codex-responses-lite` is modeled as client identity or model
capability output, not as an operator-selected channel policy. Adding
`other.codex.responses_lite` would make the implementation easy, but it would
force a model-capability header across every model on the channel and mislabel
the source as model-derived.

Trade-off: the existing `wire.ChannelPolicy.ResponsesLite` input remains
reserved for a future model-capability resolver rather than becoming a
configurable channel knob. The gain is a cleaner authority model: channel policy
owns credential/residency/trust decisions, while model capability remains
outside `other.codex`.

## M-11: Keep JSON String Decoding For Metadata Header Values

Decision: not fixed.

`client_metadata` is JSON. When a reserved metadata key is represented as a JSON
string, decoding escape sequences is how the semantic header value is obtained;
the upstream HTTP header cannot carry the JSON escape syntax as bytes. Raw
preservation still applies to unknown metadata and to raw body/frame payloads,
not to the derived header value.

Trade-off: two JSON spellings such as `"abc-def"` and `"abc\u002ddef"` produce
the same upstream header. The gain is correct JSON semantics and one validation
path for header-derived and metadata-derived identity fields.

## H-13: Keep ResponsesWS Lifecycle Diagnostics Out Of Provider Objects

Decision: not fixed.

ResponsesWS open/close lifecycle is owned by the relay actor and
`common/responsesws` session implementations. Providers build native or bridge
upstreams; they do not own turn settlement, close classification, or diagnostic
correlation IDs. Adding separate provider lifecycle logs would create a second
partial timeline for the same session.

Trade-off: there is no provider-local open/close log pair. The gain is one
diagnostic authority for ResponsesWS lifecycle, with request/session/user
correlation attached at the relay boundary.

## M-9: Do Not Use Downstream Headers Or Principal In OpenAI ResponsesWS Open

Decision: not fixed.

`responsesws.OpenRequest` carries inbound headers and principal because Codex
Official needs them to construct official identity headers. OpenAI ResponsesWS
does not have that identity planner: upstream auth and account authority come
from the selected channel credential and provider configuration. Reusing
downstream headers or the one-hub principal there would create a second,
implicit upstream-authoring path.

Trade-off: the OpenAI provider ignores fields that are meaningful for Codex.
The gain is a sharper provider contract: shared request evidence can be present
without every provider being required to interpret it as upstream authority.

## M-14: Keep `HeaderPlan.Map()` As The Final Transport Conversion

Decision: not fixed.

`HeaderPlan` is an immutable ordered plan. The map exists only because the
current requester helper accepts `map[string]string`; moving maps earlier to
avoid this allocation would make the mutable transport representation part of
the planner contract.

Trade-off: each HTTP request allocates one small map at the transport boundary.
The gain is that the official planner remains inspectable, ordered, and
side-effect free.

## M-15: Keep Value Hashes Uniform In Header Decision Audit

Decision: not fixed.

Header decision audit deliberately records length and hash instead of raw
values. Special-casing constants or UUID-like values would create a second
classification policy in the logging path and increase the chance that a future
sensitive field is logged differently from the rest.

Trade-off: debug-enabled requests hash some low-risk values. The gain is a
simple redaction invariant: no header decision carries raw header values, and
all non-empty values are comparable by digest.

## C-12: Keep Raw Envelope Plus Typed Projection

Decision: not fixed.

The architecture explicitly separates body truth from local decision lenses:
raw JSON remains the upstream serialization source, while typed projection is a
read-only convenience for routing, quota estimates, and compatibility checks.
Removing the projection parse would either push ad hoc raw-field lookups through
relay/provider code or make typed structs the serialization authority again.

Trade-off: request ingress pays an extra decode for the typed lens. The gain is
the core invariant of the Codex Official path: unknown body fields are preserved
as bytes, and local decisions do not mutate the upstream envelope implicitly.

## M-1 / M-2: Do Not Merge Codex And OpenAI Response/Usage Dialects Prematurely

Decision: not fixed.

Codex and OpenAI Responses currently share shapes but not ownership rules.
Codex has provider-owned body patches, Codex-specific billing observations,
official identity planning, and native ResponsesWS frame metadata. A shared
"one true" response/usage abstraction would look tidy today but would either
grow dialect switches or hide Codex-only rules behind generic names.

Trade-off: some projection and usage code remains similar across providers. The
gain is a clearer boundary: shared leaf helpers stay in `common/responses`,
while provider dialect behavior remains local until duplication becomes stable
enough to extract without losing protocol clarity.

## L-4: Do Not Invent A Persistent Installation Secret

Decision: not fixed.

`GenerateProxyInstallationID` uses `CodexIdentitySecret` when configured and
falls back to `SessionSecret` only as a deployment compatibility path. Creating
another implicit persistent secret, deriving identity from public channel data,
or silently writing new secret material during request handling would make the
identity authority less obvious than the current rule.

Trade-off: deployments with a non-persistent `SessionSecret` and no
`CodexIdentitySecret` can see proxy-generated installation identity change after
restart. The gain is an explicit operator contract: stable generated identity
requires setting the dedicated long-lived `CodexIdentitySecret`.

## C-11: Keep `jsonobject.Clone` Semantically Complete

Decision: not fixed.

`Clone` copies `Raw` because it is a general raw-object value operation, not a
planner-only mutation helper. The create planner patches immediately afterward,
which clears `Raw` through `SetJSON`/`SetRaw`; that is the correct local
transition from raw-preserved object to patched object.

Trade-off: a create request briefly copies raw bytes that it will discard after
the first patch. The gain is a smaller, cleaner API: clone means clone, and the
mutation methods own the raw-invalidation rule.

## H-26 / M-33: Strict Affinity Remains Correctness-First

Decision: not fixed.

Strict affinity is documented as "do not reroute if the preferred channel cannot
be used". Automatically clearing or bypassing a strict binding after a transient
failure would turn a correctness rule into an availability hint and could send a
stateful continuation to the wrong channel.

Trade-off: a bad strict binding can keep failing until TTL cleanup, explicit
clear, or the preferred channel recovers. The gain is that strict really means
strict; non-strict rules already provide the availability-first behavior.

## M-19: Do Not Split Legacy Realtime Session In This Pass

Decision: not fixed.

`providers/codex/realtime_session.go` is still large, but it is the legacy
realtime/session-resume implementation, not the new Codex Official HTTP
Responses or native ResponsesWS planner boundary. A mechanical split would
create churn across a stateful subsystem while adding little protocol safety to
the official upstream work.

Trade-off: the file remains harder to navigate than ideal. The gain is keeping
this review pass focused on protocol correctness and avoiding a broad legacy
refactor that would be easy to get subtly wrong.

## M-24: Do Not Reject `x-codex-turn-state` On WS Handshake

Decision: not fixed.

The Codex Official planner validates `x-codex-turn-state` but omits it from WS
handshake output because turn state belongs in `response.create.client_metadata`
for native ResponsesWS. Rejecting a harmless client header merely because it is
not emitted would make header acceptance stricter than the architecture
requires.

Trade-off: clients do not get a 400 for a field that is ignored on the WS
handshake. The gain is a narrow rule: invalid reserved values fail closed, valid
but operation-inapplicable values are simply not projected.

## M-27: Keep Idle Timeout And Provider Close Settlement Classes Separate

Decision: not fixed.

`InboundIdle` is proxy-local liveness evidence; provider peer close is upstream
evidence. Collapsing them into one class would simplify tables but would hide
who ended the turn, which is exactly the evidence the ResponsesWS actor uses for
quota and diagnostics.

Trade-off: settlement code has more states to reason about. The gain is that
local timeout, local abort, provider close, and provider payload evidence remain
distinguishable.

## L-7: Empty Principal Produces No Principal Fingerprint

Decision: not fixed.

An empty one-hub principal means there is no authenticated stable caller
identity at that layer. The generated Codex installation ID still includes
channel and session identity; fabricating an "anonymous" HMAC would not add real
authority and would only make the identity source look stronger than it is.

Trade-off: unauthenticated requests do not get caller-stable generated
installation identity beyond channel/session evidence. The gain is honest
provenance: principal-derived identity is present only when a principal exists.

## L-8 / L-14: Do Not Centralize Generic Error Strings Or Rename Layer Locals

Decision: not fixed.

Strings such as `invalid_request_error` are protocol literals, and the current
local names (`apiErr`, `errWithCode`) mostly follow layer boundaries. A central
constant package or sweeping rename would reduce a few repeated tokens while
adding indirection to error construction sites.

Trade-off: some literals and local naming differences remain. The gain is
readable call sites where the OpenAI-compatible error type is visible without
chasing a generic constants layer.

## L-10: Keep `prepareResponsesOfficialHTTPRequest` As The Operation Boundary

Decision: not fixed.

The function takes the operation, optional path suffix, model, and already
planned body because it is the narrow crossing from body/header planners into
the transport request. Splitting those parameters into a one-use struct would
make the signature shorter but add another type with no independent invariant.

Trade-off: the helper has several explicit parameters. The gain is that every
piece of transport evidence is visible at the call site and no mutable request
builder object is introduced.

## L-11: Keep Tunable Realtime Durations As Vars

Decision: not fixed.

Some realtime durations are constants, while timeout values exercised by tests
or runtime knobs remain package variables. Collapsing them all to consts would
make tests slower or require dependency injection for simple timer overrides.

Trade-off: timeout declarations are not visually uniform. The gain is simple,
fast tests and no timer abstraction layer.

## L-17: Keep Panic Diagnostic Detail Sanitized

Decision: not fixed.

ResponsesWS diagnostic `DetailError` intentionally carries sentinel error text
instead of recovered panic values or raw adapter errors. The useful correlation
data is phase, panic class, channel/provider, and stack hash; raw panic text can
contain provider payloads or credentials.

Trade-off: diagnostics are less detailed than a raw panic dump. The gain is a
strong redaction invariant: panic diagnostics are useful for grouping without
leaking request contents.

## L-20: Do Not Normalize All Existing Log Message Style Here

Decision: not fixed.

Codex Official paths now have structured-enough, redacted diagnostics where it
matters. Normalizing every Codex/relay log prefix would touch unrelated legacy
paths and create noisy churn without changing behavior or protocol safety.

Trade-off: historical log message style remains mixed. The gain is keeping this
pass scoped to user-visible correctness and official protocol boundaries.

## L-24: Keep `visibleASCII` Scoped To HTTP/Header Validation

Decision: not fixed.

`visibleASCII` is a protocol grammar helper for JSON metadata and HTTP header
values. Shell metacharacters are not special in that context, and rejecting them
would be an undocumented protocol change. Any future shell use must introduce a
separate shell grammar instead of reusing this helper.

Trade-off: the helper name does not encode every possible future sink. The gain
is correct separation of grammars: HTTP-visible ASCII validation does not
pretend to be shell escaping.

## L-28: Keep `DecodeResponse` Generic At The Transport Layer

Decision: not fixed.

`DecodeResponse` is a low-level HTTP helper that decodes into the caller's
provided target. Encoding provider-specific result types into it would invert
the dependency direction and make the requester package know about API shapes.

Trade-off: the helper trusts callers to pass the right target type. The gain is
a small transport boundary that remains reusable across providers.

## L-30: Close Streams When Error Delivery Is Backpressured

Decision: not fixed.

If a stream consumer stops reading the error channel, the producer cannot both
deliver the error and stay non-blocking forever. The current behavior waits a
bounded time, logs the failed delivery, and closes the stream so the reader side
unblocks.

Trade-off: the exact error value can be lost when the consumer is already
non-cooperative. The gain is bounded goroutine lifetime and a clear shutdown
path instead of an unbounded blocked send.

## L-32: Keep Model Validation At Planner/Provider Boundaries

Decision: not fixed.

`commonresponses.Request` is the shared evidence envelope. It may exist before
model mapping, fallback selection, or provider-specific normalization completes.
Requiring model validity in that type would make a transport envelope enforce a
provider decision too early.

Trade-off: an empty model can exist briefly inside the relay pipeline. The gain
is proper layering: Codex planner rejects missing model at the point where the
official upstream request is authored.

## L-3 / L-5: Leave Benchmark-Only Helpers Outside Production Contract

Decision: not fixed.

The remaining `mustJSON` helper is in `hack/bench/relay_bench.go`, and the
self-hosted benchmark uses the typed projection only as a read lens while still
passing the raw envelope to the provider request. These files are benchmark
tools, not the official upstream implementation.

Trade-off: benchmark code keeps a few convenience shortcuts that production code
does not allow. The gain is no extra production abstraction or error plumbing
for code that is deliberately outside the request path.

## H-16: Keep Legacy Realtime Internal Errors On The Existing Error Logger

Decision: not fixed.

`logCodexRealtimeInternalError` still logs through the legacy error logger with
a detached background context. That path is the older `/v1/realtime`
execution-session subsystem, not the Codex Official HTTP Responses or native
ResponsesWS planner boundary. The function now redacts sensitive text and adds
caller metadata, while ResponsesWS adapter failures use the dedicated diagnostic
hook with request/session/user correlation.

Trade-off: legacy realtime internal logs do not get full request-context
correlation unless individual call sites already include it in the message. The
gain is avoiding a broad context-threading refactor through a stateful legacy
subsystem while preserving the important safety property: no raw credentials or
provider payloads are logged.

## M-32: Keep Channel Health Side Effects Asynchronous In HTTP Retry

Decision: not fixed.

`processChannelRelayError` remains asynchronous in the generic HTTP retry loop.
Making disable/cooldown updates synchronous would put database/cache work on
the user request's retry critical path. The same request already has local
skip-channel protection where it matters, so synchronous global propagation is
not required for correctness.

Trade-off: other concurrent requests can observe stale channel health for a
short window. The gain is lower tail latency and a simpler retry loop: local
request routing uses immediate evidence, while global health bookkeeping
settles out of band.

## M-39: Keep Native And Bridge `Recv` Drain Semantics Operation-Specific

Decision: not fixed.

Native ResponsesWS has a real bidirectional upstream connection and a separate
terminal queue so provider frames can drain before a local backpressure
terminal. The HTTP bridge adapts one HTTP stream at a time and does not have
the same provider close/control surface. Forcing both transports into identical
`Recv` drain mechanics would either invent fake native-like state for the
bridge or weaken native ordering guarantees.

Trade-off: transport tests must cover two drain shapes. The gain is more honest
evidence: native preserves connection-level terminal ordering, while bridge
preserves stream-level events and abort evidence without pretending to be a real
provider websocket.

## L-12: Keep Small Static Error Message Switches

Decision: not fixed.

The static Codex realtime error-message lookups stay as `switch` statements.
They are small, local protocol tables with no dynamic behavior. Converting them
to package-level maps would add mutable-looking global data and indirection
without improving the official upstream boundary.

Trade-off: adding a new static message edits a switch rather than a map
literal. The gain is a direct, allocation-free lookup that remains easy to read
at the call site.

## L-31: Keep `wire.ChannelPolicy` As A Plain Planner Input

Decision: not fixed.

`wire.ChannelPolicy` intentionally remains a simple input DTO rather than a
constructor-enforced value object. Save-time validation and provider runtime
parsing validate `residency` and `default_originator` before constructing the
official plan; empty strings remain meaningful optional values.

Trade-off: the struct type itself does not prove every invariant in isolation.
The gain is a smaller protocol planner API: callers can assemble evidence
without a builder layer, and validation stays at the boundaries where invalid
operator config can be reported with the correct field path.

## H-24: Preserve The Last Provider Error When ResponsesWS Candidates Exhaust

Decision: not fixed.

When the ResponsesWS retry loop has already observed a non-unsupported provider
failure, and the next channel selection attempt only proves there are no more
candidates, the provider failure remains the stronger user-facing evidence. A
generic channel-selection error would be true but less useful: it describes the
end of the search, not why the usable candidate failed.

Trade-off: the final error can come from the previous attempted channel rather
than the final `setProvider` call. The gain is a simpler evidence rule already
covered by tests: if no provider was attempted, report selection failure; if a
provider produced a real non-unsupported error, preserve that error after
fallback/unsupported scanning exhausts candidates.
