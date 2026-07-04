---
title: "Codex Official Upstream 架构设计"
layout: doc
outline: deep
lastUpdated: true
---

# Codex Official Upstream 架构设计

## 文档状态

- 状态：目标架构方案，**无迁移态**。
- 适用范围：one-hub 中 Codex provider 的 `/v1/responses` HTTP、`/v1/responses/compact` HTTP、`GET /v1/responses` WebSocket upstream 请求构造。
- 影响范围：本文重写的 `providers/base` Responses / ResponsesWS provider contract 是全仓唯一 contract。OpenAI 等非 Codex Responses provider 同步切换到同一 contract，同样以 raw object 为 body 基底，不得回退 typed 真相；但各自的 body patch 规则属于各自 dialect，本文只定义 Codex 的 planner 规则。
- 设计取向：一次性切到干净协议边界；不保留 legacy profile；不设置 optional raw interface；不设置 typed fallback；不做 Codex / PI / one-hub legacy 的混合推断。
- 参考文档：
  - `docs/dev/codex-pi-header-parity.md`
  - `docs/dev/responses-ws-architecture.md`
  - `docs/dev/responses-ws-transport-boundary.md`
  - `docs/dev/wsconn-architecture.md`
  - `docs/dev/channel-affinity-architecture.md`

本文把 Codex provider 定义为一个明确的 upstream dialect：**Codex Official Upstream**。它的职责不是把客户端 header 原样代理给上游，而是用 one-hub 已持有的渠道 OAuth 凭证，构造一份与 Codex 官方客户端直接 OAuth 请求同构的 upstream 请求。

这里的“同构”只覆盖应用层可控协议：headers、JSON body、ResponsesWS frame metadata。TLS、ALPN、HTTP/2 pseudo-header、header 顺序、Go transport 自动 header、WebSocket `Sec-*` 握手字段不属于本文控制面。

## 硬约束

本设计只有一个运行态：

```text
raw envelope + control + policy in, official plan out
```

对应的硬约束：

1. **一个 dialect**：Codex provider 只实现 Codex Official Upstream，不实现 legacy one-hub Codex dialect。
2. **一个 HTTP Responses contract**：provider-facing Responses contract 是 `RawEnvelope + Control + Policy`，不再接收 `*types.OpenAIResponsesRequest` 作为请求真相。
3. **一个 ResponsesWS open contract**：Codex WS upstream open 必须拿到 inbound headers 和 first `response.create` raw frame；不允许把 handshake identity 延迟到 send path。
4. **一个 header planner**：所有 Codex upstream header 只能由 `providers/codex/wire` 生成；旧 allowlist 和 mutable header bag 不在 Codex path 中出现。
5. **一个 body planner**：HTTP body 以 raw JSON object 为序列化真相；typed projection 只读，不作为 upstream marshal 源。唯一例外是 chat-to-responses 适配边界的一次性合成（见「Chat-to-Responses 适配边界」）：chat ingress 没有 raw Responses body 可保，适配器在 planner 之前合成 raw envelope，合成之后走与 Responses ingress 完全相同的 raw path。
6. **一个 control plane**：下游响应形态是本地控制事实，不进入 upstream JSON。Chat-to-Responses streaming conversion 用 `Control.DownstreamDialect` 表达，不使用 body magic field。
7. **一个 policy plane**：channel affinity / prompt-cache 这类代理策略是 policy 事实；policy 可以产生明示 body patch，但 provider 不在 planner 之后做隐藏生成或二次猜测。
8. **一个身份解析器**：`session-id`、`thread-id`、`x-client-request-id`、`x-codex-window-id` 等身份字段只在 `IdentityResolver` 中解析。
9. **一个错误口径**：空 required field 可以 fallback；非空非法 field 直接 `400 invalid_request_error`；多值 singleton header 直接 `400 invalid_request_error`。
10. **无双轨**：没有 `RawResponsesInterface` optional branch，没有 typed adapter，没有 legacy fallback flag，没有按 UA 自动切协议。
11. **无修补**：proxy 不做 semantic repair。proxy 拥有的字段可以 set；显式 unsupported input 直接 `400 invalid_request_error`；未知 future field raw-preserve。

这不是“在旧系统旁边挂一条新路径”。这是把 Codex provider 的协议边界重写成一个小而确定的 Unix 风格工具：输入证据，输出计划。

## 事实修正

`docs/dev/codex-pi-header-parity.md` 中关于 `/responses/compact` 的描述需要在本方案中修正：

- 普通 HTTP `/responses` 会携带 body `client_metadata`。
- HTTP `/responses/compact` 不携带普通 `/responses` 的 `client_metadata` body。Codex compact 会构造 compact 专用 payload，并把关键身份字段投影到 header，尤其是 `x-codex-installation-id`。

因此三条路径的目标不同：

```text
/responses          -> raw body preservation + body client_metadata parity + HTTP header parity
/responses/compact  -> compact body transform + compact header projection parity
ResponsesWS native  -> WS handshake parity + response.create client_metadata parity
```

不要把 ordinary `/responses` 的 body preservation 规则套到 `/responses/compact`。

## 当前问题

当前 Codex provider 的 header / body 构造散落在多处：

- `providers/codex/base.go:getRequestHeaderBag`
- `providers/codex/base.go:filterAndPassthroughClientHeaders`
- `providers/codex/chat.go:applyDefaultHeaders`
- `providers/codex/realtime.go:getRealtimeHeaders`
- `providers/codex/responses.go:getResponsesOperationRequestWithSession`
- `providers/codex/responses_ws_upstream.go:getResponsesWSBridgeRequest`

这些函数把四类语义混在一张 mutable header map 里：

1. 渠道 OAuth 权威字段。
2. Codex 官方客户端字段。
3. one-hub legacy execution-session 字段。
4. PI / Realtime / HTTP bridge 的历史行为。

结果是结构性错误，而不是字段遗漏：

- 上游会出现 Codex 官方路径不该出现的 `session_id`、`x-session-id`、`Conversation_id`、`version`、HTTP `OpenAI-Beta`、`Connection`。
- Codex 官方路径应该出现的 `session-id`、`thread-id`、`x-codex-window-id`、`x-codex-installation-id`、`x-openai-subagent` 等字段缺失。
- 普通 HTTP `/responses` 用 typed struct 重组 body，`client_metadata` 和未来未知字段会丢。
- ResponsesWS native 和 HTTP bridge 的 header 语义混用，`x-codex-turn-state` 可能出现在 WS handshake 中。
- `model_headers` 是绕过 HeaderPlan 的任意 header 注入口；即使不覆盖 reserved header，也会破坏 Codex Official path 的单一协议边界。

本方案解决的不是“补字段”。要解决的是：**Codex upstream 请求必须有唯一协议边界、唯一身份解析、唯一 header 构造器、唯一 body 保真策略。**

## 设计目标

1. Codex provider 只说 Codex Official Upstream dialect。
2. 删除 legacy one-hub header 逻辑，不再生成或转发 `session_id`、`x-session-id`、`Conversation_id`。
3. 删除 smart allowlist。header 构造改为 operation-specific 的显式规则表。
4. 普通 `/responses` 使用 raw envelope 作为序列化真相；typed request 只是本地决策 lens。
5. `/responses/compact` 使用 compact 专用 body，不保 ordinary `client_metadata` body，但从 raw metadata 中投影 compact header。
6. ResponsesWS 只走 native Codex upstream；Codex path 不存在 HTTP bridge。
7. `Authorization`、`ChatGPT-Account-ID`、FedRAMP、residency 等权威字段只来自 one-hub channel credential / channel policy。
8. 客户端传空的必需 Codex identity 字段按 official completion 补齐；补齐来源必须是同一请求的官方 header/body metadata 或 proxy-generated identity，不读取 legacy header。
9. 任意非空但格式非法的 Codex 字段 fail closed，不静默吞掉。
10. 规则可测试、可审计、可 diff；不靠调用顺序或隐式 side effect。

## 非目标

- 不追求 TLS / HTTP2 / WS transport wire-level 完全一致。
- 不把 PI 行为和 Codex 行为揉进一个 provider。
- 不支持旧 `session_id` / `x-session-id` 作为 Codex upstream header。
- 不保留 `legacy_onehub` profile。
- 不保留旧 typed Responses provider contract。
- 不设置 raw/typed 双接口。
- Codex Official channel 不允许配置 `model_headers`；Codex policy 只能通过结构化 `channel.Other.codex` 表达。
- 不做 ResponsesWS native 到 HTTP bridge 的自动 fallback。
- 不让 provider helper、transport helper 或 header builder 参与 quota、affinity、RPM、settlement 决策。

## 第一性原理

Codex provider 需要分离不同平面的真相：

```text
credential truth  = one-hub channel credential
body truth        = client raw request envelope
control truth     = local downstream response behavior
policy truth      = routing / channel / affinity decisions
upstream truth    = Codex Official dialect plan
```

三者不能互相污染：

- 客户端 `Authorization` 是 one-hub ingress credential，不是 upstream credential。
- 客户端 raw body 是 upstream JSON 候选真相，不能被 typed struct 吃掉未知字段。
- `Control` 不进入 upstream，决定 downstream codec，例如 `responses_sse` 或 `chat_completions_sse`。
- `Policy` 不来自客户端 JSON，来自 routing、channel policy、affinity planner；它只能通过 planner 产生明示 patch，例如 `prompt_cache_key`。
- upstream header 是由 Codex Official dialect 构造出来的结果，不是客户端 header map 的子集。

固定数据流：

```text
Ingress raw request
    -> HeaderSnapshot + RawEnvelope + Control + PolicyInput + TypedLens
    -> responses.Request evidence + wire planner inputs
    -> CodexIdentity
    -> CodexHeaderPlan + CodexBodyPlan
    -> PreparedUpstreamRequest
    -> requester / wsconn
```

每一层只做一件事。

## 包边界

新增 leaf package：

```text
providers/codex/wire
```

`wire` package 不 import Gin、不 import requester、不读取数据库、不持有 provider。它只处理 Codex protocol planning。

建议文件布局：

```text
common/jsonobject/
  object.go
  object_test.go

common/requestctx/
  principal.go

common/responses/
  request.go
  raw_envelope.go

common/responsesws/
  open_request.go
  raw_frame.go

providers/base/
  responses_contract.go
  responses_ws_contract.go

providers/codex/wire/
  operation.go
  snapshot.go
  metadata.go
  principal.go
  identity.go
  headers_http.go
  headers_ws.go
  body_create.go
  body_compact.go
  ws_metadata.go
  validate.go
  decision.go
  errors.go
  testdata/
    responses_create_header.golden.json
    responses_create_body.golden.json
    responses_compact_header.golden.json
    responses_ws_open_header.golden.json
    responses_ws_frame.golden.json

providers/codex/
  responses_create.go
  responses_compact.go
  responses_ws.go
  credential_policy.go
```

原则：

```text
wire package: pure protocol planner
codex provider: credential + URL + requester/wsconn
relay: ingress + routing + accounting
transport: bytes only
```

## Raw JSON object

普通 `/responses` 不能再以 `types.OpenAIResponsesRequest` 为 upstream body 序列化源。

新增公共 raw JSON object 工具：

```go
type Object struct {
    Raw    json.RawMessage
    Fields map[string]json.RawMessage
}

func Parse(raw []byte) (*Object, error)          // reject duplicate top-level keys
func (o *Object) Clone() *Object
func (o *Object) SetRaw(name string, value json.RawMessage)
func (o *Object) SetJSON(name string, value any) error
func (o *Object) Delete(name string)
func (o *Object) MarshalJSON() ([]byte, error)
```

`common/responsesws/raw_frame.go` 已经有同类思想：raw frame 是真相，typed projection 只服务本地 admission / rewrite。这个思想上移到 HTTP `/responses`。

新增：

```go
type RawEnvelope struct {
    Object     *jsonobject.Object
    Projection types.OpenAIResponsesRequest
}
```

规则：

```text
读取：Projection 用于 model、stream、prompt_cache_key、quota、routing。
写出：Object 是 upstream body 基底，只 patch Codex Official 明确拥有的字段。
控制：Control 是本地下游输出控制面，不进入 upstream body。
策略：Policy 是 routing / channel / affinity 决策面，只通过 BodyPlanner 产生明示 patch。
```

未来 Codex 新增 body 字段时，one-hub 默认保留。

## HeaderSnapshot

禁止在 Codex Official path 中使用：

```go
req.Header.Get("session-id")
```

原因是 `Header.Get()` 会把 missing、empty、multiple 等状态压扁。Codex identity 字段必须有完整状态。

定义：

```go
type FieldState int

const (
    FieldMissing FieldState = iota
    FieldEmpty
    FieldPresent
    FieldInvalid  // 仅由 validation 层（携带 per-field grammar 读取时）产生；raw snapshot 永不产出
    FieldMultiple
)

type HeaderField struct {
    CanonicalName string
    Values        []string
}

type HeaderSnapshot struct {
    Fields map[string]HeaderField // lower-case key -> full values
}
```

singleton header 的规则：

```text
missing             -> FieldMissing
present but ""      -> FieldEmpty
one non-empty value -> FieldPresent -> 交给 validation 层做 grammar 判断
multiple values     -> FieldMultiple -> 400 invalid_request_error
non-empty invalid   -> FieldInvalid  -> 400 invalid_request_error（validation 层判定）
```

分层约束：raw snapshot 只描述形态（missing / empty / present / multiple），不做任何 grammar 判断；`FieldInvalid` 只能由 validation 层套用 per-field grammar 后产生。同一字段的合法性只在一处判定。

## Provider contract

### HTTP Responses

旧 contract 删除：

```go
CreateResponses(ctx, request *types.OpenAIResponsesRequest)
CreateResponsesStream(ctx, request *types.OpenAIResponsesRequest)
CompactResponses(ctx, request *types.OpenAIResponsesRequest)
```

新 contract 是唯一 contract：

```go
type ResponsesOperation string

const (
    ResponsesCreate  ResponsesOperation = "responses.create.http"
    ResponsesCompact ResponsesOperation = "responses.compact.http"
)

type ResponsesRequest struct {
    Operation ResponsesOperation
    Headers   wire.HeaderSnapshot      // inbound client headers
    Body      *responses.RawEnvelope   // raw body + typed lens
    Control   responses.Control        // local downstream behavior, never upstream JSON
    Policy    responses.PolicyInput    // routing/channel/affinity decisions
    Principal requestctx.Principal     // authenticated downstream subject
    ChannelID int
    Model     string                   // relay-selected model name
}

type DownstreamDialect string

type Control struct {
    DownstreamDialect DownstreamDialect
    Stream            bool
}

const (
    DownstreamResponses       DownstreamDialect = "responses"
    DownstreamChatCompletions DownstreamDialect = "chat_completions"
)

type PolicyInput struct {
    PromptCache *PromptCacheDecision
}

type PromptCacheDecision struct {
    Key    string
    Source PromptCacheSource
}

type ResponsesProvider interface {
    CreateResponses(ctx context.Context, req *ResponsesRequest) (*types.OpenAIResponsesResponses, *types.OpenAIErrorWithStatusCode)
    CreateResponsesStream(ctx context.Context, req *ResponsesRequest) (requester.StreamReaderInterface[string], *types.OpenAIErrorWithStatusCode)
    CompactResponses(ctx context.Context, req *ResponsesRequest) (*types.OpenAIResponsesResponses, *types.OpenAIErrorWithStatusCode)
}
```

说明：

- `Headers` 是 inbound client headers snapshot，不是 upstream headers。
- `Body.Object` 是 upstream serialization base。
- `Body.Projection` 是本地 typed lens。
- `Control.DownstreamDialect` 决定 downstream response codec；`chat_completions` 表示 upstream 使用 Responses，但下游输出 ChatCompletion chunks。
- `Policy.PromptCache` 是 prompt-cache / affinity planner 的唯一显式输入；provider 不在 body planner 之后调用 fallback helper。
- provider 必须显式决定如何使用 raw body；不能 `json.Marshal(Projection)`。
- 所有 Responses provider 都实现这个 contract。不存在旧 typed contract 并行存在的状态。

### ResponsesWS

当前 `OpenResponsesWS(ctx, model, options)` 无法从 first frame 构造 WS handshake identity，因为 `OpenOptions` 没有 inbound headers，也没有 first frame。Codex Official path 不允许把 native open 延迟到 first send。

旧 contract 删除：

```go
OpenResponsesWS(ctx context.Context, modelName string, options responsesws.OpenOptions)
```

新 contract 是唯一 contract：

```go
type OpenRequest struct {
    InboundHeaders     wire.HeaderSnapshot
    FirstFrame         *responsesws.RawResponsesCreateFrame
    Principal          requestctx.Principal

    SelectedModel      string
    UpstreamSessionID  string
    PreviousResponseID string
    Transport          runtimesession.TransportMode
    ChannelID          int
    Diagnostics        DiagnosticHook
}

type ResponsesWSProvider interface {
    OpenResponsesWS(ctx context.Context, req *responsesws.OpenRequest) (responsesws.Upstream, *types.OpenAIErrorWithStatusCode)
}
```

规则：

```text
relay 必须先读取 first response.create raw frame
relay 必须把 inbound headers 和 first frame 一起交给 provider
provider 在 OpenResponsesWS 中完成 WS handshake header plan
provider 不允许在 Send path 中临时构造 handshake identity
```

`OpenRequest.Transport` 在 Codex Official path 中只是 validation-only 输入，不是 upstream dialect selector。

Allowed:

```text
empty
responses_ws
```

Forbidden:

```text
responses_http_bridge
realtime
legacy
any unknown value
```

Provider 不允许根据 `Transport` 分支到另一个 upstream dialect。Forbidden transport 返回 `400 invalid_request_error` 或 `426 responses_ws_unsupported_for_channel`，但永远不 fallback 到 HTTP bridge。

`FirstFrame` 仍然不等于“已经发送给 upstream”。open 阶段只用它解析 identity 和 handshake。后续 send 阶段使用同一 `Identity` 对 first frame metadata 做 raw-preserving patch。

## Request Evidence

Codex Official 的输入语义由 relay 组装出的 `responses.Request` 和
provider 构造的 wire planner input 共同表达；代码中不需要额外的
request-intent 聚合类型：

```go
type Operation string

const (
    OpResponsesCreate  Operation = "responses.create.http"
    OpResponsesCompact Operation = "responses.compact.http"
    OpResponsesWSOpen  Operation = "responses.ws.open"
)

// Conceptual shape only; current code passes responses.Request plus focused
// wire inputs instead of materializing this aggregate.
type RequestEvidence struct {
    Operation Operation
    Headers   HeaderSnapshot
    Body      *RawEnvelope
    FirstFrame *responsesws.RawResponsesCreateFrame
    Policy    ChannelPolicy
    Principal requestctx.Principal
    Clock     Clock
}
```

这个 evidence 不读取 legacy session header。它只读取官方 Codex surface：

- official inbound headers，例如 `session-id`、`thread-id`、`x-client-request-id`。
- ordinary `/responses` body `client_metadata`。
- compact transform 前的 ordinary raw body metadata。
- ResponsesWS first `response.create.client_metadata`。
- channel policy 中的 account / fedramp / residency / trusted attestation policy。
- ingress authentication 后得到的 opaque principal。

## Principal 和 proxy-generated identity

Codex provider 不能读取 downstream raw `Authorization`。下游凭证只在 ingress auth middleware 消费，并转成 opaque principal。

定义：

```go
type Principal struct {
    Kind     string // user | api_key | service_account
    StableID string // internal opaque id, never sent upstream
}
```

Codex wire planner 只接收 principal fingerprint：

```go
type PrincipalFingerprint struct {
    Kind string
    HMAC string // HMAC(server_secret, Kind + ":" + StableID)
}
```

`x-codex-installation-id` 缺失时的 proxy-generated 规则：

```text
installation_id = uuidv5(
  namespace = onehub_codex_installation_namespace(channel_id),
  name      = principal_fingerprint + ":" + session-id,
)
```

约束：

```text
不使用 raw downstream Authorization
不把真实 API key hash 原样发给 upstream
channel scoped
principal scoped
session scoped
stable
non-reversible
audit 中只记录 source=proxy_generated
```

HMAC key 的运维约束：

```text
key 是 server 级 secret，轮换会改变所有 proxy-generated installation id
"stable" 承诺的作用域是该 secret 的生命周期，不是永久
该 key 不得与高频轮换的 session/cookie secret 共用轮换周期；应使用独立、长周期的 codex identity secret
```

文档中不再使用未定义的 `downstream_credential_id`。

## ChannelPolicy

`model_headers` 不进入 Codex Official protocol 字段。更严格地说：Codex Official channel 的 `model_headers` 必须为空；任何非空 `model_headers` 都是配置错误。

原因是 `model_headers` 的抽象是“静态注入 header”，而 Codex Official path 的抽象是“从 raw evidence 生成唯一 official plan”。即使 `model_headers` 中没有 reserved header，它仍然绕过 `HeaderPlan`，让协议面出现第二个 header 作者。

Codex provider 不调用 `applyCommonRequestHeaders`。

渠道配置只允许通过结构化 `channel.Other.codex` 表达 Codex policy：

```json
{
  "codex": {
    "fedramp": false,
    "residency": "",
    "default_originator": "codex_cli_rs",
    "trust_client_attestation": false,
    "generate_proxy_installation_id": true
  }
}
```

配置校验规则：

```text
unknown codex policy key -> config error
channel.type == codex && model_headers != empty -> config error
policy.fedramp 只能来自 channel policy / credential claim
policy.residency 只能来自 channel policy
trust_client_attestation=false 且客户端发送 x-oai-attestation -> 400 invalid_request_error
```

配置错误的故障域是渠道，不是进程：保存/更新时 fail-fast 拒绝写入；启动或运行时发现存量非法配置（例如升级前遗留的非空 `model_headers`），该渠道标记不可用并告警，进程正常启动。一条脏渠道配置不允许放大为全服务不可用。

## IdentityResolver

唯一输出：

```go
type Identity struct {
    UserAgent       string
    Originator      string
    SessionID       string
    ThreadID        string
    ClientRequestID string
    WindowID        string
    InstallationID  string

    TurnMetadata    string
    ParentThreadID  string
    Subagent        string
    MemgenRequest   string
    TurnState       string
    ResponsesLite   string
    InferenceCallID string

    Sources map[string]Source
}

type Source string

const (
    SourceClientHeader Source = "client_header"
    SourceBodyMetadata Source = "body_client_metadata"
    SourceFrameMetadata Source = "frame_client_metadata"
    SourceChannel      Source = "channel_policy"
    SourceGenerated    Source = "proxy_generated"
)
```

身份解析只接受官方名称：

```text
header: session-id
header: thread-id
body:   client_metadata.session_id
body:   client_metadata.thread_id
frame:  client_metadata.session_id
frame:  client_metadata.thread_id
```

不接受：

```text
header: session_id
header: x-session-id
header: Conversation_id
```

这些字段从 Codex Official path 中删除。

### Fallback 规则

这里的 fallback 不是旧协议兼容，而是 official dialect completion：当官方客户端字段为空时，proxy 生成一份可解释的 upstream identity。

#### `User-Agent`

```text
header User-Agent non-empty valid
> default Codex UA
```

默认 UA 从集中常量生成：

```text
codex_cli_rs/<one-hub-codex-dialect-version> (<os>; <arch>) one-hub
```

#### `originator`

```text
header originator non-empty valid
> channel policy default_originator
> codex_cli_rs
```

#### `session-id`

HTTP create / compact：

```text
header session-id
> body client_metadata.session_id
> generated uuid
```

WS open：

```text
header session-id
> first frame client_metadata.session_id
> generated uuid
```

#### `thread-id`

HTTP create / compact：

```text
header thread-id
> body client_metadata.thread_id
> generated uuid
```

WS open：

```text
header thread-id
> first frame client_metadata.thread_id
> generated uuid
```

#### `x-client-request-id`

```text
header x-client-request-id
> resolved thread-id
```

依据：Codex 官方客户端在普通 `/responses` 上按 `responses_metadata.thread_id` 生成该字段（见 `codex-pi-header-parity.md`）。fallback 到 resolved thread-id 是 official completion，不是代理发明的每请求随机 id 语义。

#### `x-codex-window-id`

```text
header x-codex-window-id
> client_metadata.x-codex-window-id
> omit
```

Window id 不默认生成。没有真实客户端窗口语义时，省略比伪造更安全。

#### `x-codex-installation-id`

普通 `/responses`：

```text
body client_metadata.x-codex-installation-id
> header x-codex-installation-id
> omit
```

`/responses/compact`：

```text
header x-codex-installation-id
> body client_metadata.x-codex-installation-id
> proxy-generated installation id when policy.generate_proxy_installation_id=true
> omit
```

WS frame metadata：

```text
frame client_metadata.x-codex-installation-id
> proxy-generated installation id when policy.generate_proxy_installation_id=true
> omit
```

三条路径的 precedence 不同不是随意选择：precedence 跟随官方客户端在该 operation 上的权威投影面。普通 `/responses` 官方以 body `client_metadata` 携带该字段、header 只是兼容投影，所以 body 优先；`/responses/compact` 官方显式把它投影为 header，所以 header 优先（证据见 `codex-pi-header-parity.md`）。修改任一 precedence 前必须先更新 parity 证据。

## Validation grammar

所有 grammar 都必须可单元测试。没有“看起来有效”的隐式判断。

### Singleton headers

这些字段是 singleton：

```text
User-Agent
originator
session-id
thread-id
x-client-request-id
x-codex-window-id
x-codex-turn-metadata
x-codex-parent-thread-id
x-openai-subagent
x-openai-memgen-request
x-codex-beta-features
x-responsesapi-include-timing-metrics
x-openai-internal-codex-responses-lite
x-oai-attestation
x-codex-inference-call-id
x-codex-installation-id
traceparent
tracestate
```

多值直接 400。

### Grammar table

| 字段 | Grammar | 空值 | 非空非法 |
| --- | --- | --- | --- |
| `User-Agent` | visible ASCII，禁止 CR/LF/CTL，`1..256` bytes | fallback | 400 |
| `originator` | token：`[A-Za-z0-9._-]{1,64}` | fallback | 400 |
| `session-id` | id：visible ASCII，禁止 CR/LF/CTL，`1..128` bytes | fallback | 400 |
| `thread-id` | id：visible ASCII，禁止 CR/LF/CTL，`1..128` bytes | fallback | 400 |
| `x-client-request-id` | id：visible ASCII，禁止 CR/LF/CTL，`1..128` bytes | fallback | 400 |
| `x-codex-window-id` | id：visible ASCII，禁止 CR/LF/CTL，`1..128` bytes | omit | 400 |
| `x-codex-parent-thread-id` | id：visible ASCII，禁止 CR/LF/CTL，`1..128` bytes | omit | 400 |
| `x-openai-subagent` | token：`[A-Za-z0-9._:-]{1,128}` | omit | 400 |
| `x-openai-memgen-request` | `true` or `false` | omit | 400 |
| `x-codex-beta-features` | comma-separated token list，每项 `[A-Za-z0-9._:-]{1,64}`，总长 `<=512` | omit | 400 |
| `x-responsesapi-include-timing-metrics` | `true` or `false` | omit | 400 |
| `x-openai-internal-codex-responses-lite` | `true` or `false` | omit | 400 |
| `x-oai-attestation` | trusted policy only；base64url/JWT-like，`<=4096` bytes | omit | 400 |
| `x-codex-inference-call-id` | id：visible ASCII，禁止 CR/LF/CTL，`1..128` bytes | omit | 400 |
| `x-codex-installation-id` | UUID / id，visible ASCII，禁止 CR/LF/CTL，`1..128` bytes | compact 可 fallback | 400 |
| `traceparent` | W3C basic：`00-<32hex>-<16hex>-<2hex>`，trace/span 非全零 | omit | 400 |
| `tracestate` | W3C list basic，禁止 CR/LF/CTL，`<=512` bytes | omit | 400 |

### Metadata grammar

`client_metadata` 如果存在，必须是 JSON object。reserved metadata keys 使用同名 header grammar：

```text
session_id                         -> session-id grammar
thread_id                          -> thread-id grammar
x-codex-window-id                  -> x-codex-window-id grammar
x-codex-parent-thread-id           -> x-codex-parent-thread-id grammar
x-openai-subagent                  -> x-openai-subagent grammar
x-codex-installation-id            -> x-codex-installation-id grammar
x-codex-turn-metadata              -> JSON object string, <=16 KiB
x-codex-turn-state                 -> JSON object string or opaque visible ASCII, <=16 KiB
ws_request_header_traceparent      -> traceparent grammar
ws_request_header_tracestate       -> tracestate grammar
x-codex-ws-stream-request-start-ms -> unix milliseconds string
```

Unknown metadata keys are preserved if total serialized `client_metadata` size is `<=64 KiB` and no key/value contains control characters where string grammar applies。

实现上，reserved header / metadata 的 grammar、singleton 属性、metadata alias、operation 输出位置应收敛到一张 field contract 表，并由表驱动测试覆盖。

Trade-off：field contract 表只承载字段事实，不把 `session-id` / `thread-id` 的生成、`x-codex-installation-id` 的 operation precedence、policy fallback 等上下文流程改写成全声明式引擎。这样获得单一协议契约和可测试边界，同时避免为了“纯表驱动”引入收益不成比例的抽象复杂度。

## HeaderPlan

Header plan 是不可变输出，不是 mutable map。

```go
type HeaderPlan struct {
    Entries   []HeaderEntry
    Decisions []Decision
}

type HeaderEntry struct {
    Name  string
    Value string
}

type Decision struct {
    Name   string
    Action string // set | copy | fallback | omit | reject
    Source Source
    Reason string
}
```

只有最后一步转换为 `http.Header` / `map[string]string`。

测试直接断言：

```text
operation=responses.create.http
must have: session-id, thread-id
must not have: session_id, x-session-id, Conversation_id, Connection, OpenAI-Beta
```

## Authority fields

这些字段永远不从客户端透传：

| 字段 | 来源 |
| --- | --- |
| `Authorization` | channel OAuth token |
| `ChatGPT-Account-ID` | channel credential |
| `X-OpenAI-Fedramp` | channel policy / credential claim |
| `x-openai-internal-codex-residency` | channel policy |
| `Host` | transport |
| `Content-Length` | transport |
| `Connection` | transport |
| `Upgrade` / `Sec-WebSocket-*` | wsconn / websocket runtime |
| `Cookie` | never forwarded |
| `Forwarded` / `X-Forwarded-*` | never forwarded |

下游请求里的 `Authorization` 是 one-hub API credential，只在 ingress middleware 消费。它不是 Codex Official request evidence 的一部分。

## HTTP `/responses` 链路

### 输入

```text
POST /v1/responses
Content-Type: application/json
```

body 必须是 JSON object。重复 top-level key 直接 400。

### Relay 阶段

```go
raw := ReadReusableBody(c)
obj := jsonobject.Parse(raw)
projection := DecodeProjection(obj.Raw)
req := responses.Request{
    Operation: responses.ResponsesCreate,
    Body: &responses.RawEnvelope{Object: obj, Projection: projection},
    Control: responses.Control{
        DownstreamDialect: responses.DownstreamResponses,
        Stream: projection.Stream,
    },
    Policy: responses.PolicyInput{
        PromptCache: route.PromptCacheDecision,
    },
    Headers: wire.NewHeaderSnapshot(c.Request.Header),
    Principal: requestctx.PrincipalFromContext(c),
    Model: route.SelectedModel,
}
```

Relay 仍然可以读取：

```text
model
stream
previous_response_id
prompt_cache_key
input
```

用于路由、quota、affinity、stream 判断。但 relay 不再用 typed struct 重组 upstream body。

### Chat-to-Responses 适配边界

`/v1/chat/completions` 通过 Responses provider fallback 时使用同一个 contract，但它的 ingress 没有 raw Responses body 可保。适配边界的规则：

1. **唯一 sanctioned 的 typed→raw 合成点**。适配器把 ChatCompletion 请求转换为 typed Responses request，在转换边界一次性合成 raw envelope（typed marshal → `jsonobject.Parse`），然后交给与 `/v1/responses` ingress 完全相同的 planner。合成发生在 planner 之前，planner 及其之后不存在任何 typed marshal。`/v1/responses` ingress 永远不经过该路径。
2. **converter 是合成 body 的作者，拥有冲突解决权**。合成 body 必须预先满足 BodyPlanner 的全部 reject 规则；planner 不为任何来源放宽。chat 客户端常规地同时携带 `temperature` 和 `top_p`，converter 在转换边界显式取 `temperature`、丢弃 `top_p`，并产生 `source=chat_adapter` 的 decision。这不是 hidden repair：repair 禁令针对的是"代理修改客户端拥有的 Responses body"；chat 路径的 Responses body 本来就是代理合成的。
3. **instructions 归属随 ingress 变化**。"client owns instructions" 只对 Responses ingress 成立；chat 客户端无法表达 Codex instructions，system/developer message 到 `instructions` / `input` 的映射是转换契约的一部分，必须显式定义并测试，不允许在 planner 之后隐式注入。

control plane 差异：

```go
req.Control.DownstreamDialect = responses.DownstreamChatCompletions
req.Control.Stream = true
```

这个控制位不序列化进 upstream body。旧 `ConvertChat bool json:"-"` 只表达了同一事实，但在 typed-to-raw marshal 过程中会丢失，因此不再作为 provider contract 的一部分扩散。

### Provider 阶段

`PrepareResponsesCreate` 做四件事：

1. `ResolveIdentity`。
2. `BuildHTTPCreateHeaders`。
3. `PlanResponsesCreateBody`。
4. 构造 `http.Request`。

### HTTP create header set

Required:

| Header | Source |
| --- | --- |
| `Authorization` | channel OAuth token |
| `ChatGPT-Account-ID` | channel credential account id, when present |
| `Content-Type: application/json` | protocol |
| `Accept: text/event-stream` | Codex streaming requirement |
| `User-Agent` | client official header, else Codex default |
| `originator` | client official header, else `codex_cli_rs` |
| `session-id` | official header, else body `client_metadata.session_id`, else generated |
| `thread-id` | official header, else body `client_metadata.thread_id`, else generated |
| `x-client-request-id` | official header, else resolved `thread-id` |

Optional:

| Header | Source |
| --- | --- |
| `X-OpenAI-Fedramp` | channel policy only |
| `x-openai-internal-codex-residency` | channel policy only |
| `x-codex-window-id` | official header or body metadata |
| `x-codex-turn-metadata` | official header or body metadata |
| `x-codex-parent-thread-id` | official header or body metadata |
| `x-openai-subagent` | official header or body metadata |
| `x-openai-memgen-request` | official header |
| `x-codex-beta-features` | official header |
| `x-responsesapi-include-timing-metrics` | official header |
| `x-codex-turn-state` | official HTTP header only |
| `x-openai-internal-codex-responses-lite` | official header or model capability |
| `x-oai-attestation` | trusted client policy only |
| `x-codex-inference-call-id` | official header |
| `x-codex-installation-id` | body `client_metadata.x-codex-installation-id`, else official header |
| `traceparent` / `tracestate` | valid W3C tracing header or local tracing injection |

Forbidden:

```text
session_id
x-session-id
Conversation_id
version
OpenAI-Beta
Connection
Cookie
Host
Content-Length
Proxy-*
Forwarded
X-Forwarded-*
Sec-WebSocket-*
```

### HTTP create body planner

`BodyPlanner` 接收 raw object 和显式 patch input，输出已序列化的上游 JSON：

```go
type CreateBodyInput struct {
    Model       string
    Stream      bool
    PromptCache *responses.PromptCacheDecision
}
```

Allowed top-level rewrites:

```text
set model   = mapped upstream model
set stream  = true
set store   = false
set include = append reasoning.encrypted_content only when reasoning is present
preserve client_metadata
preserve prompt_cache_key when client sent it
set prompt_cache_key from Policy.PromptCache when body does not contain prompt_cache_key
preserve unknown fields
```

Prompt cache patch 规则：

```text
1. body 已有 prompt_cache_key：
   - 保留客户端值。
   - routing / affinity 必须使用同一个 key。

2. body 没有 prompt_cache_key，但 Policy.PromptCache.Key 非空：
   - BodyPlanner 写入 /prompt_cache_key。
   - 这个 key 必须是 relay hint 或 channel policy 已经作出的同一个 PromptCacheDecision。
   - provider 不允许在 body planner 之后重新生成。

3. body 没有，Policy.PromptCache 也没有：
   - 不生成 prompt_cache_key。
```

Explicit rejects:

```text
temperature and top_p both present -> 400 invalid_request_error
context_management present         -> 400 invalid_request_error
truncation present                 -> 400 invalid_request_error
client_metadata not object         -> 400 invalid_request_error
```

No silent semantic repair.

以上 reject 对所有进入 planner 的 raw body 生效，包括 chat 适配器合成的 body。converter 必须在转换边界预先解决冲突（见「Chat-to-Responses 适配边界」），planner 不为任何来源放宽。

### Existing Codex request adaptation disposition

| Existing behavior | Final decision | Reason |
| --- | --- | --- |
| `request.Model = normalizeCodexModelName` | keep as `set /model` | provider model mapping is an upstream authority decision |
| `stream=true` | keep as `set /stream` | Codex official responses path is streaming |
| `store=false` | keep as `set /store` | official ChatGPT Codex path does not use OpenAI API store semantics |
| `ensureCodexIncludes` | keep narrowly | only append `reasoning.encrypted_content` when `/reasoning` exists |
| `temperature/top_p` prefers temperature | move to chat adapter boundary | Responses ingress 上是 hidden repair，双字段返回 400；chat 适配器作为合成 body 的作者，在转换边界显式取 temperature、弃 top_p |
| strip `context_management` | delete | hidden repair; unsupported field returns 400 |
| strip `truncation` | delete | hidden repair; unsupported field returns 400 |
| `ensureStablePromptCacheKey` | delete from HTTP Official body planning | prompt cache key decision must be explicit `Policy.PromptCache` before BodyPlanner; no provider-late mutation |
| `normalizeCodexBuiltinTools` | delete | Codex official client already speaks official tool schema; proxy does not mutate tools |
| `adaptCodexCLI` | delete | provider does not infer CLI-ness from instructions |
| system/developer merge | delete from Responses ingress | semantic rewrite outside protocol boundary；chat 适配器的 message 映射属于转换契约，另行显式定义 |
| default Codex instructions injection | delete from Responses ingress | client owns instructions；chat ingress 的 instructions 合成归 converter 契约所有 |
| typed marshal upstream body | delete | raw object is serialization truth |
| prompt cache header projection to `Conversation_id` / `session_id` | delete | legacy header pollution |

This table is part of the protocol contract. If a future upstream change requires a rewrite, it must be added here as an explicit JSON patch with tests.

## HTTP `/responses/compact` 链路

### 输入

`/responses/compact` 先读 raw envelope，因为 compact header 需要从 ordinary request metadata 中提取 `x-codex-installation-id`。

### Compact body

Compact body 是 compact 专用 schema。它不携带 ordinary `/responses` 的 `client_metadata`。

规则：

```text
extract identity from raw body before compact transform
build compact payload from typed lens
set mapped model
reject unsupported fields explicitly
send compact body
```

Forbidden body content：

```text
client_metadata  // compact body 中不保留 ordinary metadata
```

### Compact header set

Required 同 HTTP create，但 `Accept` 为 `application/json`。

Compact-specific:

| Header | Source |
| --- | --- |
| `x-codex-installation-id` | official header, else body `client_metadata.x-codex-installation-id`, else proxy-generated by policy |

没有 installation id 时默认生成 proxy scoped installation id，因为 compact 是 Codex 官方明确投影该字段的路径。生成值只表达 one-hub upstream 请求 identity，不伪装成客户端真实 installation id。

## ResponsesWS native 链路

### 输入

```text
GET /v1/responses
```

下游首帧必须是 official inline：

```json
{"type":"response.create","model":"gpt-5","input":"hi"}
```

Codex provider 不引入 nested private frame 作为 relay-facing 协议。

### Upstream open

Codex provider 在 `OpenResponsesWS(ctx, req)` 中完成：

1. 从 `req.FirstFrame` 建立 raw frame envelope。
2. 从 `req.InboundHeaders + first frame client_metadata` 解析 identity evidence。
3. 构造 WS handshake header plan。
4. 用 `wsconn` 打开 native Codex ResponsesWS upstream。
5. 把 resolved `Identity` 存入 upstream object，供后续 frame planner 使用。

### WS handshake header set

Required:

| Header | Source |
| --- | --- |
| `Authorization` | channel OAuth token |
| `ChatGPT-Account-ID` | channel credential account id, when present |
| `User-Agent` | client official header, else Codex default |
| `originator` | client official header, else `codex_cli_rs` |
| `OpenAI-Beta: responses_websockets=2026-02-06` | protocol |
| `session-id` | official header, else frame `client_metadata.session_id`, else generated |
| `thread-id` | official header, else frame `client_metadata.thread_id`, else generated |
| `x-client-request-id` | official header, else resolved `thread-id` |

Optional:

```text
X-OpenAI-Fedramp
x-codex-window-id
x-codex-turn-metadata
x-codex-parent-thread-id
x-openai-subagent
x-openai-memgen-request
x-codex-beta-features
x-responsesapi-include-timing-metrics
x-oai-attestation  // trusted only
```

Forbidden in WS handshake:

```text
Content-Type
Accept
Connection
Host
Content-Length
session_id
x-session-id
Conversation_id
version
x-codex-turn-state
x-codex-installation-id
traceparent
tracestate
```

`x-codex-turn-state` 在 Codex official WS 中属于 `response.create.client_metadata`，不是 handshake header。

### WS frame body preservation

继续沿用 `common/responsesws/raw_frame.go` 的原则：raw frame 是真相，只 patch provider 明确拥有的字段。

发送 `response.create` 前：

```text
preserve existing client_metadata
preserve unknown body fields
set model to mapped upstream model
inject previous_response_id only when relay actor owns that default
stamp x-codex-ws-stream-request-start-ms
```

Metadata patch 规则：

| Metadata key | 策略 |
| --- | --- |
| `x-codex-installation-id` | preserve; missing 可由 identity resolver 生成 |
| `session_id` | preserve; missing 用 resolved `session-id` |
| `thread_id` | preserve; missing 用 resolved `thread-id` |
| `x-codex-window-id` | preserve; missing 用 resolved window id if any |
| `x-codex-turn-state` | preserve; 不从 handshake header 搬运 |
| `ws_request_header_x_openai_internal_codex_responses_lite` | preserve or set when model capability requires |
| `ws_request_header_traceparent` / `ws_request_header_tracestate` | preserve or inject local trace context |
| `x-codex-ws-stream-request-start-ms` | 每次发送前 stamp 当前毫秒 |

## Audit

`HeaderPlan` 输出 redacted decision log：

```json
{
  "dialect": "codex_official",
  "operation": "responses.create.http",
  "field": "session-id",
  "action": "fallback",
  "source": "body_client_metadata",
  "reason": "client-header-empty",
  "value_len": 36,
  "value_hash": "sha256:..."
}
```

不记录：

```text
Authorization value
ChatGPT-Account-ID raw value
client_metadata raw JSON
prompt/input text
principal StableID
```

记录：

```text
字段名、action、source、reason、长度、hash、operation、channel_id、request_id
```

## 错误语义

| 场景 | 行为 |
| --- | --- |
| body 不是 JSON object | 400 `invalid_request_error` |
| duplicate top-level JSON key | 400 `invalid_request_error` |
| `client_metadata` 存在但不是 object | 400 `invalid_request_error` |
| singleton official header 多值 | 400 `invalid_request_error` |
| official header 非空但非法 | 400 `invalid_request_error` |
| required official field missing/empty | official fallback chain |
| `temperature` 和 `top_p` 同时存在 | 400 `invalid_request_error` |
| unsupported body field `context_management` | 400 `invalid_request_error` |
| unsupported body field `truncation` | 400 `invalid_request_error` |
| channel credential 缺 token | 401/502，沿用 provider token error 语义 |
| channel policy 非法 | 保存时拒绝写入；存量配置在启动/运行时降级为 channel unavailable + 告警，不阻断进程 |
| Codex ResponsesWS upstream 不支持 native | 426 wrapped error；不 bridge |

## 删除旧路径

Codex Official path 删除或停止调用：

```text
providers/codex/base.go:filterAndPassthroughClientHeaders
providers/codex/base.go:getRequestHeaderBag for Responses path
providers/codex/chat.go:applyDefaultHeaders
providers/codex/realtime.go:codexRealtimeCompatibilityHeaderKeys
providers/codex/realtime.go:codexRealtimeRequestOverrideHeaderKeys
providers/codex/responses.go 中 Conversation_id / session_id prompt_cache projection
providers/codex/responses.go:ensureStablePromptCacheKey in upstream body planning
providers/codex/responses.go:normalizeCodexBuiltinTools in Codex Official path
providers/codex/responses.go:adaptCodexCLI
providers/codex/responses_ws_upstream.go Codex Official bridge path
codexHeaderBag as cross-function mutable header carrier
```

可以保留的模块：

```text
OAuth token refresh
usage accumulator
SSE parser
model mapping
prompt_cache_key routing hint resolver
ResponsesWS actor / settlement / wsconn transport boundary
```

但这些模块不能再拥有 Codex Official header 或 body rewrite 规则。

## 代码形态示例

### Prepare HTTP create

```go
func (p *CodexProvider) prepareResponsesCreate(ctx context.Context, req *responses.Request) (*http.Request, error) {
    policy, err := p.codexOfficialChannelPolicy()
    if err != nil {
        return nil, err
    }

    token, err := p.GetToken()
    if err != nil {
        return nil, err
    }

    metadata, err := wire.MetadataFromResponsesBody(req.Body.Object)
    if err != nil {
        return nil, err
    }

    identity, decisions, err := wire.ResolveIdentity(wire.IdentityInput{
        Operation: wire.OpResponsesCreate,
        Headers:   req.Headers,
        Metadata:  metadata,
        Policy:    policy,
        Principal: p.codexPrincipalFingerprint(req.Principal),
        ChannelID: req.ChannelID,
        Clock:     wire.RealClock{},
    })
    if err != nil {
        return nil, err
    }

    headers, err := wire.BuildHeaders(wire.HeaderPlanInput{
        Operation: wire.OpResponsesCreate,
        Headers:   req.Headers,
        Credential: wire.Credential{
            AccessToken: token,
            AccountID:   p.Credentials.AccountID,
        },
        Policy:   policy,
        Identity: identity,
    })
    if err != nil {
        return nil, err
    }
    headers.Decisions = append(decisions, headers.Decisions...)

    body, err := wire.PlanResponsesCreateBody(req.Body.Object, wire.CreateBodyInput{
        Model:       p.mapModel(req.Model),
        Stream:      true,
        PromptCache: req.Policy.PromptCache,
    })
    if err != nil {
        return nil, err
    }

    return p.Requester.NewRequest(
        http.MethodPost,
        p.responsesURL(""),
        p.Requester.WithBody(body),
        p.Requester.WithHeader(headers.Map()),
        p.Requester.WithContext(ctx),
    )
}
```

### Resolve identity

```go
func ResolveIdentity(in IdentityInput) (Identity, []Decision, error) {
    decisions := make([]Decision, 0, 16)

    ua := in.Headers.RequiredString("User-Agent").OrDefault(DefaultUserAgent, &decisions)
    originator := in.Headers.RequiredString("originator").OrDefault(in.Policy.DefaultOriginator, &decisions)

    sessionID := FirstNonEmpty(
        in.Headers.String("session-id"),
        in.Metadata.String("session_id"),
        GeneratedUUID(),
    )

    threadID := FirstNonEmpty(
        in.Headers.String("thread-id"),
        in.Metadata.String("thread_id"),
        GeneratedUUID(),
    )

    clientRequestID := FirstNonEmpty(
        in.Headers.String("x-client-request-id"),
        threadID,
    )

    return Identity{
        UserAgent: ua,
        Originator: originator,
        SessionID: sessionID,
        ThreadID: threadID,
        ClientRequestID: clientRequestID,
        Sources: ...,
    }, decisions, nil
}
```

关键点：没有 `session_id` header，没有 `x-session-id` header，没有 provider-late prompt cache fallback。prompt cache key 只能来自 raw body 或 `Policy.PromptCache`。

## 测试计划

### Golden header tests

为每个 operation 建 golden：

```text
responses.create.http
responses.compact.http
responses.ws.open
```

断言：

- required header 全存在。
- forbidden header 全不存在。
- header value source 符合 decision log。
- 空 required field 进入 fallback。
- 非空非法 field 400。
- singleton 多值 400。

### Body preservation tests

1. ordinary `/responses` 保留 `client_metadata`。
2. ordinary `/responses` 保留未知 top-level 字段。
3. ordinary `/responses` patch `model` / `stream` / `store` 后不丢字段。
4. duplicate top-level key 400。
5. `client_metadata` 非 object 400。
6. 同时存在 `temperature` 和 `top_p` 返回 400。
7. 存在 `context_management` 返回 400。
8. 存在 `truncation` 返回 400。
9. 不再注入默认 instructions。
10. 不再 merge system/developer messages。
11. body 已有 `prompt_cache_key` 时保留客户端值，不被 policy 覆盖。
12. body 没有 `prompt_cache_key` 且 `Policy.PromptCache` 存在时，写入同一个 decision 的 key。
13. body 没有 `prompt_cache_key` 且无 `Policy.PromptCache` 时，不生成 key。
14. 不再 normalize tools。

### Compact tests

1. compact body 不包含 ordinary `client_metadata`。
2. compact 从 raw body `client_metadata.x-codex-installation-id` 生成 header。
3. compact 缺 installation id 时按 policy 生成 stable proxy id。
4. compact 不发送 HTTP `OpenAI-Beta`。
5. compact generated installation id 不依赖 raw downstream Authorization。

### WS tests

1. `OpenResponsesWS` 没有 first frame 时直接拒绝。
2. WS handshake 不包含 `Content-Type`、`Accept`、`Connection`、`x-codex-turn-state`。
3. WS handshake 包含 `OpenAI-Beta: responses_websockets=2026-02-06`。
4. WS body 保留原始 `client_metadata`。
5. WS body 缺 `session_id` / `thread_id` 时用 resolved identity 补齐。
6. WS body 每次 send 前 stamp `x-codex-ws-stream-request-start-ms`。
7. Codex Official path 不走 HTTP bridge。
8. `OpenRequest.Transport=responses_http_bridge` 直接拒绝，不触发 bridge fallback。

### Chat 适配器测试

1. chat 请求同时携带 `temperature` 和 `top_p` 时，合成 Responses body 只含 `temperature`，并产生 `source=chat_adapter` decision，不触发 planner 400。
2. 合成 body 满足 planner 全部 reject 规则；planner 对 chat 合成 body 与 Responses ingress body 使用同一套规则，无放宽分支。
3. `/v1/responses` ingress 不经过 typed→raw 合成路径；合成只发生在 chat 适配边界。
4. system/developer message 到 `instructions` / `input` 的映射符合转换契约。

### Config tests

1. Codex channel `model_headers` 非空时保存校验失败；存量非法配置使该渠道不可用，不影响进程启动。
2. `channel.Other.codex` unknown top-level key fail-fast。
3. `trust_client_attestation=false` 时客户端 `x-oai-attestation` 返回 400，不上游。
4. `fedramp=true` 时上游设置 `X-OpenAI-Fedramp: true`。

### Integration tests

建立 fake upstream，捕获最终 HTTP request / WS handshake / WS frame，断言：

```text
没有 legacy headers
raw body metadata preserved
operation-specific headers correct
credential headers from channel, not client
no typed marshal path for Responses ingress（chat 适配边界是唯一 sanctioned 合成点，且在 planner 之前）
no HTTP bridge path for Codex WS
```

## 切换方式

这是单态重构。没有运行时开关，没有 legacy profile，没有 typed fallback，没有按 channel 灰度的协议分叉。

提交可以按可编译断点拆分，但每个断点都朝同一个最终 contract 前进，不引入第二套线上行为：

1. 引入 `common/jsonobject` 和 `HeaderSnapshot`。
2. 替换 HTTP Responses provider contract 为 raw envelope contract。
3. 替换 ResponsesWS provider contract 为 `OpenRequest` contract。
4. 实现 `providers/codex/wire` pure planner。
5. 替换 Codex HTTP create / compact。
6. 替换 Codex ResponsesWS native open / frame metadata planner。
7. 删除旧 Codex header helpers 和 bridge path。
8. 收紧配置校验和 CI 规则。

CI 规则：

```text
providers/codex/*responses*.go may not call applyDefaultHeaders
providers/codex/*responses*.go may not call getRequestHeaderBag
providers/codex/responses.go may not call ensureStablePromptCacheKey for HTTP Official create/compact body planning
providers/codex/*responses*.go may not call adaptCodexCLI
providers/codex/*responses*.go may not import responses_ws_upstream bridge for Codex Official path
providers/codex/wire may not import gin/model/requester
providers/base must not expose typed Responses provider contract
providers/base must not expose OpenResponsesWS(ctx, modelName, options) contract
```

## 验收线

1. `/responses` 上游 body 保留 Codex `client_metadata` 和未知字段。
2. `/responses/compact` 上游 body 不含 ordinary `client_metadata`，但 header 含 compact `x-codex-installation-id`。
3. Codex HTTP upstream 不出现 `session_id`、`x-session-id`、`Conversation_id`、`Connection`、HTTP `OpenAI-Beta`。
4. Codex WS handshake 不出现 `Content-Type`、`Accept`、`Connection`、`x-codex-turn-state`。
5. `session-id`、`thread-id`、`x-client-request-id` 在三条 Codex operation 上都有确定来源。
6. 客户端传空 required identity 字段时 fallback；传非法非空字段时 400。
7. 客户端 `Authorization` 不可能进入 upstream；upstream `Authorization` 必然来自 channel token。
8. Codex channel `model_headers` 必须为空；Codex provider 不再通过 `model_headers` 注入任何 upstream header。
9. ResponsesWS Codex Official path 不 bridge。
10. Header decisions 可审计，且不记录敏感值。
11. HTTP Responses provider contract 不再暴露 typed request 作为请求真相。
12. ResponsesWS open contract 明确携带 inbound headers 和 first frame。
13. ResponsesWS `Transport` 在 Codex Official path 只做 validation，不作为 alternate dialect selector。
14. chat→responses 合成 body 满足与 Responses ingress 相同的 planner 约束；`temperature` / `top_p` 冲突在转换边界解决，不触发 400。

## 取舍

这个方案会破坏旧 one-hub Codex provider 的隐式行为：

- 旧客户端如果只发送 `session_id` / `x-session-id`，不再影响 Codex upstream identity。
- Codex channel 中任何非空 `model_headers` 配置都会保存失败；存量配置在升级后使该渠道不可用（进程不受影响）。
- ResponsesWS 不再自动切 HTTP bridge。
- PI 行为不再被 Codex provider smart 推断。
- 非 Codex official client 依赖 provider 自动注入 instructions、merge messages、normalize tools 的行为在 Responses ingress 上消失。需要兼容语义的客户端应走 `/v1/chat/completions` 适配边界——那里是显式转换契约，而不是 Responses ingress 的隐式 repair。

这是有意的。协议代理最怕“看起来能用”的混合语义。Codex Official path 必须是单一 raw envelope protocol：relay 只收集证据，provider 只生成 official plan，transport 只发送 bytes；不兼容旧 header 拼接、typed marshal、semantic repair、WS bridge、`model_headers` 注入。

最终形态：

```text
raw envelope + control + policy in, official plan out
```
