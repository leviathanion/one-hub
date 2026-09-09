# API Proxy Pricing and Relay Context

This context defines the business language used when routing provider requests, relaying provider protocols, and applying price policy without silently changing administrator intent.

## Language

**Price Policy**:
The complete pricing behavior attached to a model, including base prices, conditional rates, and long-context rules.
_Avoid_: Modifier, pricing extras

**Original Model Billing**:
A channel-mapping policy under which the client-requested model's complete Price Policy remains authoritative through final settlement. Provider-reported usage and service tier remain billing evidence, but the mapped or reported model cannot replace or partially mix that policy.
_Avoid_: Response-model override, mixed-model pricing

**Rate Rules**:
价格策略中显式声明的条件倍率调整，根据本次操作的计费事实决定各计量项的价格变化；没有适用规则时不作调整。
_Avoid_: Modifier, generic pricing expression

**统一倍率（Uniform Rate Multiplier）**:
一条条件规则内部各计量项共用的默认调整倍率，允许更具体的输入、输出或计量项倍率覆盖。
_Avoid_: 系统全局折扣、总账单倍率、叠加到局部倍率上的额外倍率

**Price Component**:
The smallest owner-scoped provider-evidence set that can produce a definite customer charge without referring to, subtracting, or inferring another component. Interdependent total, cache, media, TTL, input, and output partitions remain one component; model, tier, and provider scope are pricing dependencies rather than components.
_Avoid_: Usage bucket, Rate Rule, Price row, attribution component

**Adapter Operation Support**:
A code-owned, model-independent statement that an adapter can represent an operation through a declared data path. Work Action policy, billing evidence, charge policy, ownership, and egress remain separate owners.
_Avoid_: Channel capability, upstream capability declaration, model capability, operation kernel

**Turn Cancellation**:
Termination of the proxy's current in-flight attempt because its downstream connection or request context ended. It cancels only that attempt and closes its upstream transport; it does not create a provider protocol event.
_Avoid_: Response cancellation, synthetic provider cancellation

**Provider Client Event**:
A client-to-provider protocol frame. In native passthrough it remains ordered wire data interpreted by the provider; the proxy's internal control channel does not give it additional semantics.
_Avoid_: Proxy control command

**Queued Payload Budget**:
The maximum payload bytes retained by one relay connection while frames wait for processing. It bounds transient queue memory, not cumulative traffic; ordinary frames fail closed when the budget is full, while a small bounded projection of closure-relevant facts remains deliverable.
_Avoid_: Connection traffic quota, frame count limit

**Replayable Request Body**:
A request body that the relay can open again from the beginning without rereading the downstream connection. Replayability supports pre-send candidate selection and safe local processing; it does not prove that a failed `client.Do` produced no provider side effect.
_Avoid_: Small request, idempotent request

**Work Action**:
One independently claimable command that can create or enlarge provider work, cost, durable state, or another irreversible side effect. One Billing Attempt owns exactly one Work Action.
_Avoid_: Billing owner, transport attempt, polling call

**Application Submission**:
The Billing Attempt's one invocation of its configured requester, SDK, or adapter for a Work Action. It prevents one-hub application retry or failover after the attempt becomes active, but does not claim provider-observable at-most-once without a provider idempotency contract.
_Avoid_: Provider receipt, TCP write, client-request idempotency

**Observation/Control Call**:
A bounded auxiliary provider call whose operation capability proves it cannot create or enlarge billable provider work, change owner/channel, or produce chargeable provider usage.
_Avoid_: Application Submission, method-based read assumption

**Streaming Request Body**:
A one-shot body forwarded directly from the downstream connection. It never authorizes another Application Submission of the same Work Action.
_Avoid_: Unreliable request, upload exception

**Exact-Wire Relay**:
An operation whose public request and response wire can reach the selected provider unchanged apart from routing, provider credentials, and transport framing.
_Avoid_: Passthrough provider, same API family

**Provider Response Header Policy**:
A deny-by-default exposure policy that combines safe response metadata with operation-specific representation headers. It is independent of Exact-Wire Relay because provider account, cookie, and transport metadata cross proxy-owned security boundaries.
_Avoid_: Full header passthrough, header blacklist

**Provider Response Security Policy**:
The shared boundary that removes proxy-owned credentials and upstream account metadata from headers, bodies, stream events, WebSocket frames, and close reasons after evidence extraction and before downstream delivery.
_Avoid_: Billing Evidence sanitizer, exact-wire bypass, error-only redaction

**Supported Contract Surface**:
The public operations and capabilities for which the proxy promises compatible relay behavior, authorization, routing, and enforced billing. Anything outside this surface is rejected before provider work begins.
_Avoid_: Best-effort surface, partially supported surface

**Enforced Relay Surface**:
A production relay surface that performs quota admission and runs the configured settlement policy against the caller's balance. Observation may diagnose its behavior but never replaces enforcement.
_Avoid_: Observe-only production surface, free relay

**Unsupported Capability**:
An operation or request feature that the proxy cannot faithfully relay or explicitly convert. It fails locally before provider work; an operation rejected by the real upstream is a Provider Error instead.
_Avoid_: Upstream unsupported response, inferred provider limitation

**Candidate Representability Mismatch**:
A pre-provider-work fact that one normally scheduled channel cannot faithfully represent the current request. Ordinary scheduling may skip that candidate and continue; it becomes an Unsupported Capability only after all eligible candidates are exhausted. Durable ownership, administrator pinning, and any attempt already observed by a provider prohibit this fallback.
_Avoid_: Invalid request, provider retry, global capability failure

**Billing Owner**:
The lifecycle authority with one possibly-zero Quota Reservation, one Application Submission opportunity, and one final Confirm/Cancel decision for one Work Action. It is a short Billing Attempt or a durable Async Task owner.
_Avoid_: Billing ledger, protocol handler, balance transaction

**Customer Charge Quota**:
The customer-balance amount selected by the Settlement Decision for one Work Action. It is independent of the provider's upstream price, cost, margin, or account exposure.
_Avoid_: Provider cost, upstream spend, vendor liability

**Upstream Provider Cost**:
The provider-side economic cost borne by one-hub. Customer Charge Quota and Quota Reservation neither represent nor bound this cost; missing provider usage can therefore leave this cost uncharged to the customer.
_Avoid_: Customer charge, reserved quota, billing exposure

**Quota Reservation**:
The possibly-zero customer quota held during TCC Try for one Work Action. It reduces admission exposure but is neither a Charge nor a complete upper bound; Confirm may charge above it and Cancel releases it in full.
_Avoid_: Hard Reservation, Billing Floor, final charge, Redis reservation

**Billing Attempt**:
The billing owner for one independently admitted Work Action, with one possibly-zero Quota Reservation, one Application Submission opportunity, and one final Confirm/Cancel action. A request/actor owns a short attempt, while an Async Task durably owns the same lifecycle.
_Avoid_: Provider business action, idempotency-key scope, request transmission

**Token Principal Binding**:
The immutable association from one Token ID to one User for the Token's lifetime. Changing the principal creates a new Token and revokes the old one.
_Avoid_: Mutable token owner, active-attempt transfer, token reassignment

**Authoritative Provider Usage**:
Provider-originated, owner-correlated usage or billable-unit evidence with presence preserved for every field it actually reports. Only a complete Price Component built from this evidence can be charged; local estimates, provider success, and transport facts never substitute for it.
_Avoid_: Estimated usage, observed output, provider cost, success proof

**Billing Evidence**:
The owner-scoped Authoritative Provider Usage and attribution facts consumed by the settlement reducer. Each Price Component becomes Priceable, Not Applicable, Missing Evidence, or Conflicting Evidence without changing another independent component or granting another Application Submission.
_Avoid_: Attempt disposition, HTTP retry status, transport error class, local token estimate

**Settlement Decision**:
The shared settlement reducer's one owner-wide balance target: Confirm the sum of all Priceable components, including a legitimate zero sum, or Cancel when none contains a complete provider-originated billable quantity.
_Avoid_: Balance result, retry decision, Keep Reserved

**Closure Cut**:
The guarded boundary that freezes all evidence eligible for one Settlement Decision. Evidence observed after it is diagnostic only and never authorizes another balance action.
_Avoid_: Downstream close, provider terminal alone, retry point

**Balance Apply Outcome**:
The observed delivery fact for one final balance action: no write required, applied, not applied, or commit unknown. It never authorizes a second balance action.
_Avoid_: Settlement Decision, billing evidence, reconciliation state

**Provider Task Identity**:
The scoped identity that binds one asynchronous provider task to one local owner, composed of its provider namespace, Provider Task Scope Incarnation, and exact provider task ID. Channel identity is execution provenance rather than part of this identity.
_Avoid_: Provider Task Key, global task ID, channel-scoped task ID

**Public Task Lookup Identity**:
The user-scoped identity accepted by a public asynchronous task API for lookup and derived actions. It includes the API's task family, the owning `UserId`, and the exact provider task ID, and remains distinct from Provider Task Identity.
_Avoid_: Provider Task Identity, global task ID, local row ID

**Provider Task Scope Incarnation**:
The stable scope in which local task identity is deduplicated. It is the verified provider-owned namespace instance when available, otherwise a conservative provider-namespace-wide scope that may reject unrelated collisions but cannot split one real instance across channels.
_Avoid_: Channel Generation, credential fingerprint, provider account unless the provider defines the same boundary

**Async Execution Binding**:
The frozen immutable Channel ID used to submit and observe an asynchronous provider task. It selects credentials and routing provenance but does not establish provider task uniqueness.
_Avoid_: Provider Task Identity, preferred channel

**Channel Account Incarnation**:
One immutable Channel row representing a single upstream account boundary. Account-defining changes create a new Channel ID; a provider-proven refresh of credentials for the same subject does not.
_Avoid_: Channel Generation, config revision, credential timestamp

**Async Task Owner**:
The Task aggregate that durably owns one provider submission, its Provider Task Identity and Async Execution Binding, and the Billing Attempt finalized with its terminal state.
_Avoid_: Task billing ledger, provider task cache

**Payment Order**:
The durable credit aggregate created before gateway work that freezes the payer, gateway incarnation, amount, currency, and quota and owns idempotent callback crediting.
_Avoid_: Billing Attempt, payment callback lock

**Non-Authoritative Usage Projection**:
A consumption log or aggregate derived after balance truth is decided. It may be missing, delayed, or duplicated, never authorizes a balance/permission change, and cannot be presented as an authoritative invoice or audit record.
_Avoid_: Billing ledger, quota truth

**Ephemeral Response Affinity**:
A bounded, user-scoped provenance proof for non-persistent response continuations. It may carry a routing preference but is not Stored Response Ownership; an independent continuation may enter ordinary scheduling only while this proof is valid.
_Avoid_: Durable ownership, permanent binding

**Stored Response**:
A provider Response whose `store` field is omitted or true and which can be referenced or managed across requests for the provider's retention period.
_Avoid_: Connection-local response, cached response

**Compacted Response**:
The `response.compaction` operation result returned by `/responses/compact`. Its `output` is continuation material that the client replays as later input; the result's `id` alone is not evidence of a Stored Response lifecycle or retrievability.
_Avoid_: Stored Response, implicit ownership, ID-prefix inference

**Stored Response Ownership**:
The durable binding from a Stored Response ID and owning `UserId` to the Channel ID that created it. The binding keeps that ID; it does not freeze the channel’s upstream configuration. The creating `TokenId` is audit evidence rather than the authorization owner. The binding begins when `response.created` first exposes the ID and governs continuation and lifecycle operations.
_Avoid_: Response affinity, preferred channel

**Response Scope Incarnation**:
The provider-proven namespace instance in which a Stored Response ID is unique; when the provider cannot prove a narrower namespace, the whole provider namespace is used conservatively. It is not a Channel ID or credential fingerprint.
_Avoid_: Channel scope, credential scope, guessed account namespace

**Stored Response Ownership Miss**:
A `previous_response_id` or lifecycle target for which this proxy has no durable ownership record or valid user-scoped ephemeral proof. Ordinary users fail closed to prevent cross-tenant probing through shared upstream accounts; only an administrator-pinned relay may attempt an unknown ID.
_Avoid_: Ordinary scheduling, implicit ownership

**Ownership Delivery Barrier**:
The boundary at which ownership of a provider resource or asynchronous task must become durable before its identifier becomes visible to the caller. Failure at this boundary can leave an upstream orphan but cannot expose an unowned resource.
_Avoid_: Eventual ownership, asynchronous owner write

**Ownership Tombstone**:
A negative Stored Response ownership record retained after successful deletion through the original ownership horizon. It prevents a deleted ID from degrading into an unknown ID.
_Avoid_: Cache miss, immediate owner deletion

**Stored Lifecycle Relay Support**:
The combination of adapter operations and proxy-owned authorization, ownership, and exact-channel routing needed to relay the Stored Response lifecycle. It does not promise that the selected upstream endpoint or account will accept every lifecycle operation.
_Avoid_: Upstream lifecycle guarantee, declared channel capability

**OpenAI Data Residency Endpoint**:
An OpenAI regional API domain whose product contract carries the Data Residency pricing uplift. It is outside the current Supported Contract Surface. Azure, AWS, and Google Cloud deployment regions are not instances of this term.
_Avoid_: Regional provider channel, generic processing scope

**Prompt Cache Affinity**:
A non-owning preference that keeps the same cache key and canonical model on a previously successful upstream account when available. Failure of the preference permits normal fallback.
_Avoid_: Cache ownership, strict channel lock

**Connection-Local Response**:
A non-persistent Response created with `store:false` whose continuation state may exist only inside the same Native Responses WebSocket connection.
_Avoid_: Stored Response, durable response

**Resource-Independent Tool**:
A tool whose supported execution does not require the proxy to create, authorize, route, or manage an independently addressable provider resource across requests. It may still require exact wire handling, provider capability checks, and separate usage or per-call billing.
_Avoid_: Free tool, stateless request

**System Tool Price Catalog**:
The code-owned base-price catalogue for hosted tool actions. Final user charging additionally applies the existing group ratio; there is no dynamic global option or per-channel tool-price override.
_Avoid_: Runtime tool-price setting, channel quote

**Account-Scoped Tool Resource**:
A provider resource such as a File, Vector Store, Container, or uploaded Skill whose identifier and lifecycle belong to one upstream account. Accepting a reference requires principal ownership and exact-channel routing for the complete lifecycle.
_Avoid_: Opaque tool parameter, routing hint

**OpenAI-Compatible Fallback Channel**:
A channel whose established adapter or provider fallback accepts an OpenAI-compatible request shape even though the channel is not an official OpenAI or Azure OpenAI endpoint.
_Avoid_: Native OpenAI channel, exact-wire channel

**Native Responses WebSocket**:
A Responses WebSocket session backed by a real provider WebSocket operation rather than an HTTP/SSE bridge. Native describes the transport boundary, not wire equivalence: each provider operation is still classified as exact-wire, same-dialect, or cross-protocol.
_Avoid_: HTTP bridge, exact-wire guarantee

**Exact Channel Model Admission**:
The Native Responses WebSocket rule that a turn's wire model must be explicitly covered by the selected channel's model set, including an administrator-configured wildcard, and must not be changed by model mapping. Similar names or provider aliases do not establish equivalence.
_Avoid_: Inferred model family, automatic alias equivalence, model rewrite

**Responses Streaming Event Model**:
The server-event vocabulary shared by Responses HTTP streaming and Responses WebSocket, including distinct completed, failed, and incomplete lifecycle events. It is not the Realtime event model.
_Avoid_: Realtime events, generic response events

**Realtime Event Model**:
The session-oriented event vocabulary of the Realtime API, where `response.done` closes a response and its nested status describes the outcome. It is separate from every Responses API transport.
_Avoid_: Responses WebSocket events, Responses streaming events

**Provider Supplier Dialect**:
A provider-private upstream event vocabulary interpreted only by that provider's adapter. Similar event names do not make it an instance of Realtime or the Responses Streaming Event Model.
_Avoid_: OpenAI-compatible events, shared lifecycle aliases

**Same-Dialect Adapter**:
An operation that keeps the public protocol dialect but still requires provider-specific URL, authentication, normalization, or narrow field transformation.
_Avoid_: Exact passthrough, cross-protocol translation

**Cross-Protocol Adapter**:
An operation that converts between different public protocol dialects and therefore has an explicit representability contract in both directions.
_Avoid_: Generic translate flag, lossy fallback

**Provider Lifecycle Normalization**:
A narrow mapping from a Provider Supplier Dialect to a public terminal whose meaning is established by that provider contract. Generic protocol layers never infer the mapping from similar event names; unrepresentable outcomes fail explicitly after preserving provider-work and billing evidence.
_Avoid_: Unknown-field filtering, synthetic success, transparent passthrough

**Effective Service Tier**:
The service tier reported by the provider for the completed request and used for final pricing. A requested `auto`, `fast`, or other tier is admission input, not final billing evidence.
_Avoid_: Requested tier, configured default tier

**实际执行速度（Effective Speed）**:
上游实际执行本次操作所使用的速度属性，是独立于服务档位的计费事实；客户端请求的速度不能替代它。
_Avoid_: 请求速度、Fast/Priority 合并档位

**Ambiguous HTTP Create Attempt**:
An HTTP create attempt whose transport failure does not prove whether the provider executed it. A replayable body does not make this attempt safe to retry or safe to refund.
_Avoid_: Safe retry, unsent request, replayable failure

**Administrator-Pinned Resource Relay**:
An operator-only resource request that names an exact channel and bypasses ordinary multi-channel resource selection. It is an operational escape hatch rather than part of the ordinary-user Supported Contract Surface.
_Avoid_: Public resource lifecycle, ownership routing

**渠道连接配置**：
管理员为渠道指定的上游类型、地址、凭据及组织/项目等账号定位信息；渠道 ID 标识管理记录，不承诺这些信息永久不变。
_Avoid_: 不可变账号 incarnation、资源身份快照

**渠道编辑确认**：
管理员保存渠道修改前对影响范围的明确确认，涵盖任务、已存储响应、其他上游资源及后续请求；不判断资源是否存在，不承诺这些资源在修改后仍可访问。
_Avoid_: 任务占用检查、无占用证明、资源安全保证

**渠道上游接口配置**：
管理员对渠道某个上游接口的使用许可和请求目标设置；关闭保留目标配置，不声明适配器支持面，也不授予协议转换能力。
_Avoid_: 上游能力声明、客户端入口开关、自定义参数

**晋级额度**：
判断用户自动分组区间时使用的余额与已消费额度之和；充值、兑换、赠送和管理员余额调整均参与此口径。
_Avoid_: 累计实付金额、独立充值累计
