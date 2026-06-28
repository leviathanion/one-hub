---
title: "Codex Official Upstream 架构设计"
layout: doc
outline: deep
lastUpdated: true
---

# Codex Official Upstream 架构设计

## 文档状态

- 状态：目标方案。
- 适用范围：one-hub 中 Codex provider 的 `/v1/responses` HTTP、`/v1/responses/compact` HTTP、`GET /v1/responses` WebSocket upstream 请求构造。
- 设计取向：一步到位重构；不保留 `legacy_onehub` header 兼容层；不做 Codex / PI / one-hub legacy 的混合智能推断。
- 参考文档：
  - `docs/dev/codex-pi-header-parity.md`
  - `docs/dev/responses-ws-architecture.md`
  - `docs/dev/responses-ws-transport-boundary.md`
  - `docs/dev/wsconn-architecture.md`
  - `docs/dev/channel-affinity-architecture.md`

本文把 Codex provider 定义为一个明确的 upstream dialect：**Codex Official Upstream**。它的职责不是把客户端 header 原样代理给上游，而是用 one-hub 已持有的渠道 OAuth 凭证，构造一份与 Codex 官方客户端直接 OAuth 请求同构的 upstream 请求。

这里的“同构”只覆盖应用层可控协议：headers、JSON body、ResponsesWS frame metadata。TLS、ALPN、HTTP/2 pseudo-header、header 顺序、Go transport 自动 header、WebSocket `Sec-*` 握手字段不属于本文控制面。

## 事实修正

`docs/dev/codex-pi-header-parity.md` 当前有一个需要在本方案中修正的点：

- 普通 HTTP `/responses` 会携带 body `client_metadata`。
- HTTP `/responses/compact` 不携带普通 `/responses` 的 `client_metadata` body。Codex compact 会构造 compact 专用 payload，并把关键兼容身份投影到 header，尤其是 `x-codex-installation-id`。

因此：

```text
/responses          -> raw body preservation + body client_metadata parity + HTTP header parity
/responses/compact  -> compact body transform + compact header projection parity
ResponsesWS native  -> WS handshake parity + response.create client_metadata parity
```

不要把 ordinary `/responses` 的 body preservation 规则机械套到 `/responses/compact`。

## 真实问题

当前 Codex provider 的 header / body 构造散落在多处：

- `providers/codex/base.go:getRequestHeaderBag`
- `providers/codex/base.go:filterAndPassthroughClientHeaders`
- `providers/codex/chat.go:applyDefaultHeaders`
- `providers/codex/realtime.go:getRealtimeHeaders`
- `providers/codex/responses.go:getResponsesOperationRequestWithSession`
- `providers/codex/responses_ws_upstream.go:getResponsesWSBridgeRequest`

它们把四类语义混在了一张 mutable header map 里：

1. 渠道 OAuth 权威字段。
2. Codex 官方客户端字段。
3. one-hub legacy execution-session 字段。
4. PI / Realtime / HTTP bridge 的历史行为。

这导致几个结构性问题：

- 上游会出现 Codex 官方路径不该出现的 `session_id`、`x-session-id`、`Conversation_id`、`version`、HTTP `OpenAI-Beta`、`Connection`。
- Codex 官方路径应该出现的 `session-id`、`thread-id`、`x-codex-window-id`、`x-codex-installation-id`、`x-openai-subagent` 等字段缺失。
- 普通 HTTP `/responses` 用 typed struct 重组 body，`client_metadata` 和未来未知字段会丢。
- ResponsesWS native 和 HTTP bridge 的 header 语义混用，`x-codex-turn-state` 可能出现在 WS handshake 中。
- `model_headers` 可以静态注入业务 header，但它不能表达 per-session / per-thread / per-turn 的官方客户端身份。

本方案解决的不是“补几个字段”。要解决的是：**Codex upstream 请求应该有唯一的协议边界、唯一的身份解析、唯一的 header 构造器、唯一的 body 保真策略。**

## 设计目标

1. Codex provider 只说 Codex Official Upstream dialect。
2. 删除 legacy one-hub header 兼容层，不再生成或转发 `session_id`、`x-session-id`、`Conversation_id`。
3. 删除 smart allowlist。header 构造改为 operation-specific 的显式规则表。
4. 普通 `/responses` 使用 raw envelope 作为序列化真相；typed request 只是本地决策 lens。
5. `/responses/compact` 使用 compact 专用 body，不保 ordinary `client_metadata` body，但从 raw metadata 中投影 compact header。
6. ResponsesWS 只走 native Codex upstream；不为 Codex Official path 静默降级到 HTTP bridge。
7. `Authorization`、`ChatGPT-Account-ID`、FedRAMP、residency 等权威字段只来自 one-hub channel credential / channel policy。
8. 客户端传空的必需 Codex identity 字段按官方语义补齐；补齐来源必须是同一请求的官方 header/body metadata 或 proxy-generated identity，不读取 legacy header。
9. 任意非空但格式非法的 Codex 字段 fail closed，不静默吞掉。
10. 规则可测试、可审计、可 diff；不靠调用顺序或隐式 side effect。

## 非目标

- 不追求 wire-level 完全一致。
- 不把 PI 行为和 Codex 行为揉进一个 provider。
- 不支持旧 `session_id` / `x-session-id` 作为 Codex upstream header。
- 不保留 `legacy_onehub` profile。
- 不让 `model_headers` 覆盖 Codex Official protocol 字段。
- 不做 ResponsesWS native 到 HTTP bridge 的自动 fallback。
- 不让 provider adapter、transport helper 或 header builder 参与 quota、affinity、RPM、settlement 决策。

## 第一性原理

Codex provider 需要三个分离的真相：

```text
credential truth  = one-hub channel credential
request truth     = client raw request envelope
upstream truth    = Codex Official dialect plan
```

三者不能互相污染：

- 客户端 `Authorization` 是 one-hub ingress credential，不是 upstream credential。
- 客户端 raw body 是请求语义真相，不能被 typed struct 吃掉未知字段。
- upstream header 是由 Codex Official dialect 构造出来的结果，不是客户端 header map 的子集。

因此架构固定为：

```text
Ingress raw request
    -> RawEnvelope + TypedLens
    -> CodexIntent
    -> CodexIdentity
    -> CodexHeaderPlan
    -> PreparedUpstreamRequest
    -> requester / wsconn
```

每一层只做一件事。

## 已选型结论

### 1. `CodexIntent` 是唯一输入语义

新增一个 leaf package：

```text
providers/codex/wire
```

该 package 不 import Gin、不 import requester、不读取数据库、不持有 provider。

它只处理协议：

```go
type Operation string

const (
    OpResponsesCreate  Operation = "responses.create.http"
    OpResponsesCompact Operation = "responses.compact.http"
    OpResponsesWSOpen  Operation = "responses.ws.open"
)

type Intent struct {
    Operation Operation
    Headers   HeaderSnapshot
    Body      *RawEnvelope
    Policy    ChannelPolicy
    Clock     Clock
}
```

`CodexIntent` 不读取 legacy session header。它只读取官方 Codex surface：

- official inbound headers，例如 `session-id`、`thread-id`、`x-client-request-id`。
- ordinary `/responses` body `client_metadata`。
- ResponsesWS `response.create.client_metadata`。
- channel policy 中的 account / fedramp / residency / trusted attestation policy。

### 2. `RawEnvelope` 是 body 序列化真相

普通 `/responses` 不能再以 `types.OpenAIResponsesRequest` 为 upstream body 序列化源。

新增公共 raw JSON object 工具：

```text
common/jsonobject
```

提供：

```go
type Object struct {
    Raw    json.RawMessage
    Fields map[string]json.RawMessage
}

func Parse(raw []byte) (*Object, error)          // reject duplicate top-level keys
func (o *Object) Clone() *Object
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
写出：Object 是 upstream body 基底，只 patch one-hub 明确拥有的字段。
```

这避免未来 Codex 新增 body 字段时被 one-hub 吃掉。

### 3. `CodexIdentity` 是唯一身份解析结果

新增：

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
    SourceChannel      Source = "channel_policy"
    SourceGenerated    Source = "proxy_generated"
)
```

身份解析只接受官方名称：

```text
header: session-id
body:   client_metadata.session_id
```

不接受：

```text
header: session_id
header: x-session-id
header: Conversation_id
```

这些字段从 Codex Official path 中删除。

### 4. 空值是 missing，非法值是错误

字段状态显式建模：

```go
type FieldState int

const (
    FieldMissing FieldState = iota
    FieldEmpty
    FieldPresent
    FieldInvalid
    FieldMultiple
)
```

规则：

```text
missing / empty -> 进入官方 fallback 链
present invalid -> 400 invalid_request_error
multiple singleton -> 400 invalid_request_error
```

这保留了“客户端传空时 fallback”的需求，但不把非法输入静默改写成另一种行为。

### 5. Header plan 是不可变输出，不是 mutable map 传来传去

新增：

```go
type HeaderPlan struct {
    entries   []HeaderEntry
    decisions []Decision
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

这样测试可以直接断言：

```text
operation=responses.create.http
must have: session-id, thread-id
must not have: session_id, x-session-id, Conversation_id, Connection, OpenAI-Beta
```

### 6. `model_headers` 不进入 Codex Official protocol 字段

Codex Official path 不再调用 `applyCommonRequestHeaders`。

渠道配置只允许通过结构化 `channel.Other` 表达 Codex policy：

```json
{
  "codex": {
    "fedramp": false,
    "residency": "",
    "default_originator": "codex_cli_rs",
    "trust_client_attestation": false
  }
}
```

`model_headers` 不再作为 Codex upstream header 注入机制。配置中出现 protected / Codex-reserved header 应 fail-fast，而不是运行时静默忽略。

保留 `model_headers` 给通用 OpenAI-compatible provider 可以，但 Codex Official provider 不消费它。

### 7. Codex ResponsesWS 不走 HTTP bridge

在 Codex Official path 下：

```text
GET /v1/responses downstream -> Codex native WS upstream
```

不再允许：

```text
GET /v1/responses downstream -> HTTP /responses bridge upstream
```

HTTP bridge 是另一个 transport dialect，可以继续服务通用 provider，但它不属于 Codex Official parity 目标。Codex 官方客户端 direct OAuth 的 WS 路径是 native WS，one-hub Codex provider 应保持同一层语义。

### 8. Prompt cache 只存在于 body / routing hint，不投影为 legacy header

保留 `prompt_cache_key` 的两个合法位置：

1. 请求 body `prompt_cache_key`。
2. provider 选择前的 routing hint。

删除 upstream header 投影：

```text
Conversation_id
session_id = prompt_cache_key
```

`docs/dev/channel-affinity-architecture.md` 的核心原则仍保留：影响 routing 的稳定 hint 必须在 provider 选择前可见。区别是：hint 不再伪装成 Codex official header。

## 架构总览

```text
relay/http_responses
  └─ parse raw body -> responses.RequestEnvelope
       ├─ Projection: typed lens for relay decisions
       └─ Raw:       upstream serialization base

providers/codex
  └─ CodexUpstreamFactory
       ├─ PrepareResponsesCreate
       ├─ PrepareResponsesCompact
       └─ OpenResponsesWS

providers/codex/wire
  ├─ HeaderSnapshot
  ├─ MetadataSnapshot
  ├─ IdentityResolver
  ├─ HeaderPlanBuilder
  ├─ BodyPlanner
  └─ Validators

requester / wsconn
  └─ transport only
```

边界：

| 层 | 职责 | 禁止 |
| --- | --- | --- |
| relay | 解析 raw body、建立 typed lens、路由、quota、affinity | 构造 Codex upstream header |
| provider/codex | 绑定渠道 credential、调用 wire planner、发送 upstream | 从 Gin header map 直接拼 upstream header |
| provider/codex/wire | 生成 Codex Official request plan | 读取 DB、改 quota、做路由 |
| requester/wsconn | HTTP/WS I/O | 理解 Codex 业务字段 |

## Provider contract 调整

当前 `providers/base.ResponsesInterface` 以 `*types.OpenAIResponsesRequest` 为主参数。目标架构改为 raw envelope contract：

```go
type ResponsesRequest struct {
    Operation Operation
    Body      *RawEnvelope
    Headers   http.Header
    Model     string
    Stream    bool
}

type ResponsesProvider interface {
    CreateResponses(ctx context.Context, req *ResponsesRequest) (*types.OpenAIResponsesResponses, *types.OpenAIErrorWithStatusCode)
    CreateResponsesStream(ctx context.Context, req *ResponsesRequest) (requester.StreamReaderInterface[string], *types.OpenAIErrorWithStatusCode)
    CompactResponses(ctx context.Context, req *ResponsesRequest) (*types.OpenAIResponsesResponses, *types.OpenAIErrorWithStatusCode)
}
```

说明：

- `Headers` 是 inbound client headers snapshot，不是 upstream headers。
- `Body.Raw` 是 upstream serialization base。
- `Body.Projection` 是本地 typed lens。
- provider 必须显式决定如何使用 raw body；不能靠 `json.Marshal(Projection)` 偷懒。

这会影响所有 Responses provider，但这是正确的断点。协议代理系统的核心 contract 应该表达 raw preservation，而不是把 typed struct 当请求真相。

ResponsesWS 已经通过 `responsesws.Upstream` 表达了类似边界；本方案让 HTTP Responses 和 ResponsesWS 的协议哲学一致。

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
    Operation: responses.OpCreate,
    Body: &responses.RawEnvelope{Object: obj, Projection: projection},
    Headers: c.Request.Header.Clone(),
}
```

Relay 仍然可以读取：

- `model`
- `stream`
- `previous_response_id`
- `prompt_cache_key`
- `input`

用于路由、quota、affinity、stream 判断。但 relay 不再用 typed struct 重组 upstream body。

### Provider 阶段

`PrepareResponsesCreate` 做四件事：

1. `ResolveIdentity`。
2. `BuildHTTPCreateHeaders`。
3. `PlanResponsesCreateBody`。
4. 构造 `http.Request`。

body patch 规则：

```text
set model                      = mapped upstream model
set stream                     = true for Codex
set store                      = false
set include                    = include reasoning.encrypted_content if needed
normalize temperature/top_p    = keep temperature, delete top_p when both present
remove context_management      = Codex upstream rejects it
remove truncation              = Codex upstream rejects it
preserve client_metadata       = yes
preserve unknown fields        = yes
```

不再设置：

```text
headers[Conversation_id]
headers[session_id]
headers[x-session-id]
```

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
| `traceparent` / `tracestate` | valid W3C tracing header or local tracing injection |

Forbidden in upstream HTTP create:

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

## HTTP `/responses/compact` 链路

### 输入

`/responses/compact` 仍然先读 raw envelope，因为 compact header 需要从 ordinary request metadata 中提取 `x-codex-installation-id`。

### Compact body

Compact body 是 compact 专用 schema。它不携带 ordinary `/responses` 的 `client_metadata`。

规则：

```text
extract identity from raw body before compact transform
build compact payload from typed lens
set mapped model
strip fields compact upstream rejects
send compact body
```

### Compact header set

Required 同 HTTP create，但 `Accept` 为 `application/json`。

Compact-specific:

| Header | Source |
| --- | --- |
| `x-codex-installation-id` | official header, else body `client_metadata.x-codex-installation-id`, else generated or omitted by policy |

建议默认：没有 installation id 时生成 proxy scoped installation id，而不是省略。原因是 compact 是 Codex 官方明确投影该字段的路径；生成值应带稳定作用域：

```text
installation_id = stable_uuid(channel_id, downstream_credential_id, session-id)
```

该值只表达 one-hub upstream 请求 identity，不伪装成客户端真实 installation id。audit source 记为 `proxy_generated`。

Forbidden 同 HTTP create，另加：

```text
body.client_metadata  // compact body 中不保留 ordinary metadata
```

## ResponsesWS native 链路

### 输入

```text
GET /v1/responses
```

下游首帧必须是 official inline：

```json
{"type":"response.create","model":"gpt-5","input":"hi"}
```

这已经由 `docs/dev/responses-ws-architecture.md` 定义。Codex provider 不应引入 nested Codex private frame 作为 relay-facing 协议。

### Upstream open

`OpenResponsesWS(ctx, model, options)` 调用 Codex provider。Codex provider：

1. 从 first `response.create` raw frame 建立 `RawEnvelope`。
2. 从 inbound handshake headers + frame `client_metadata` 解析 `CodexIntent`。
3. 构造 WS handshake header plan。
4. 用 `wsconn` 打开 native Codex ResponsesWS upstream。

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

## 身份 fallback 规则

这里的 fallback 不是兼容旧协议，而是 official dialect completion：当官方客户端字段为空时，proxy 生成一份可解释的 upstream identity。

### `User-Agent`

```text
header User-Agent non-empty valid
> default Codex UA
```

默认 UA 应从一个集中常量生成：

```text
codex_cli_rs/<one-hub-codex-dialect-version> (<os>; <arch>) one-hub
```

不要继续使用 `codex-tui/0.135.0 ...`。

### `originator`

```text
header originator non-empty valid
> channel policy default_originator
> codex_cli_rs
```

不再根据 UA 猜 `pi` 或 `codex-tui`。

### `session-id`

```text
header session-id
> body/frame client_metadata.session_id
> generated UUID
```

不读取：

```text
header session_id
header x-session-id
prompt_cache_key
```

### `thread-id`

```text
header thread-id
> body/frame client_metadata.thread_id
> generated UUID
```

不要默认等于 `session-id`。Codex direct 中 session 和 thread 是两个独立 identity。

### `x-client-request-id`

```text
header x-client-request-id
> resolved thread-id
```

### `x-codex-window-id`

```text
header x-codex-window-id
> body/frame client_metadata.x-codex-window-id
> omit
```

window id 没有强制生成。缺失时 omit 比伪造更干净。

### `x-codex-installation-id`

普通 `/responses`：

```text
body client_metadata.x-codex-installation-id
> generated stable proxy installation id
```

`/responses/compact`：

```text
header x-codex-installation-id
> body client_metadata.x-codex-installation-id
> generated stable proxy installation id
```

ResponsesWS body：

```text
frame client_metadata.x-codex-installation-id
> generated stable proxy installation id
```

## Validation contract

### Singleton header

以下字段必须最多一个有效值：

```text
User-Agent
originator
session-id
thread-id
x-client-request-id
x-codex-window-id
x-codex-parent-thread-id
x-openai-subagent
x-openai-memgen-request
x-responsesapi-include-timing-metrics
x-openai-internal-codex-responses-lite
x-codex-inference-call-id
x-codex-installation-id
traceparent
```

多值直接 400。

### Size limits

建议默认：

```text
single header value               <= 4 KiB
x-codex-turn-metadata              <= 16 KiB
client_metadata total serialized   <= 64 KiB
tracestate                         <= W3C limit, otherwise 512 bytes local cap
```

超限是 invalid request，不做截断。

### 字符集

- ID 类字段：只允许可打印 ASCII，不允许控制字符。
- bool 类字段：只允许 `true` / `false`。
- `x-codex-turn-metadata`：必须是 ASCII JSON string，且 JSON decode 后是 object。
- `traceparent`：必须符合 W3C 基本格式。
- `tracestate`：必须符合 W3C list 基本格式。
- `client_metadata`：如果存在，必须是 JSON object。

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

下游请求里的 `Authorization` 是 one-hub API credential，只在 ingress middleware 消费。它不是 `CodexIntent` 的一部分。

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
| channel credential 缺 token | 401/502，沿用 provider token error 语义 |
| channel policy 非法 | channel unavailable / config error |
| Codex ResponsesWS upstream 不支持 native | 426 wrapped error；不 bridge |

## 删除旧路径

目标架构删除或停止用于 Codex Official path 的逻辑：

```text
providers/codex/base.go:filterAndPassthroughClientHeaders
providers/codex/chat.go:applyDefaultHeaders
providers/codex/realtime.go:codexRealtimeCompatibilityHeaderKeys
providers/codex/realtime.go:codexRealtimeRequestOverrideHeaderKeys
providers/codex/responses.go 中 Conversation_id / session_id prompt_cache projection
providers/codex/responses_ws_upstream.go Codex Official bridge path
codexHeaderBag 作为跨函数 mutable header carrier
```

可以保留的东西：

```text
OAuth token refresh
usage accumulator
SSE parser
model normalization
prompt_cache_key routing hint resolver
ResponsesWS actor / settlement / wsconn transport boundary
```

但这些模块不能再拥有 Codex Official header 规则。

## 新文件布局

建议：

```text
common/jsonobject/
  object.go
  object_test.go

relay/responses_request.go
  parse HTTP raw envelope

providers/base/responses_contract.go
  new raw-envelope Responses provider contract

providers/codex/wire/
  operation.go
  snapshot.go
  metadata.go
  identity.go
  headers.go
  body_create.go
  body_compact.go
  ws_metadata.go
  validate.go
  decision.go
  testdata/
    codex_http_create.golden.json
    codex_http_compact.golden.json
    codex_ws_handshake.golden.json

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
```

## 代码形态示例

### Prepare HTTP create

```go
func (p *CodexProvider) prepareResponsesCreate(ctx context.Context, req *responses.Request) (*http.Request, error) {
    policy, err := p.codexChannelPolicy()
    if err != nil {
        return nil, err
    }

    token, err := p.GetToken()
    if err != nil {
        return nil, err
    }

    plan, err := wire.Plan(wire.PlanInput{
        Operation: wire.OpResponsesCreate,
        Headers:   wire.NewHeaderSnapshot(req.Headers),
        Body:      wire.NewMetadataSnapshot(req.Body.Object),
        Policy:    policy,
        Credential: wire.Credential{
            AccessToken: token,
            AccountID:   p.Credentials.AccountID,
        },
        Model: p.mapModel(req.Model),
        Clock: wire.RealClock{},
    })
    if err != nil {
        return nil, err
    }

    body, err := wire.PlanResponsesCreateBody(req.Body.Object, wire.BodyPatch{
        Model:  p.mapModel(req.Model),
        Stream: true,
    })
    if err != nil {
        return nil, err
    }

    return p.Requester.NewRequest(
        http.MethodPost,
        p.responsesURL(""),
        p.Requester.WithRawBody(body),
        p.Requester.WithHTTPHeader(plan.HTTPHeader()),
    )
}
```

### Resolve identity

```go
func ResolveIdentity(in IdentityInput) (Identity, []Decision, error) {
    var d []Decision

    ua := in.Headers.RequiredString("User-Agent").OrDefault(DefaultUserAgent, &d)
    originator := in.Headers.RequiredString("originator").OrDefault(in.Policy.DefaultOriginator, &d)

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

    return Identity{...}, d, nil
}
```

关键点：没有 `session_id` header，没有 `x-session-id` header，没有 prompt cache key fallback。

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

### Body preservation tests

1. ordinary `/responses` 保留 `client_metadata`。
2. ordinary `/responses` 保留未知 top-level 字段。
3. ordinary `/responses` patch `model` / `stream` / `store` 后不重排丢字段。
4. duplicate top-level key 400。
5. `client_metadata` 非 object 400。

### Compact tests

1. compact body 不包含 ordinary `client_metadata`。
2. compact 从 raw body `client_metadata.x-codex-installation-id` 生成 header。
3. compact 缺 installation id 时按 policy 生成 stable proxy id。
4. compact 不发送 HTTP `OpenAI-Beta`。

### WS tests

1. WS handshake 不包含 `Content-Type`、`Accept`、`Connection`、`x-codex-turn-state`。
2. WS handshake 包含 `OpenAI-Beta: responses_websockets=2026-02-06`。
3. WS body 保留原始 `client_metadata`。
4. WS body 缺 `session_id` / `thread_id` 时用 resolved identity 补齐。
5. WS body 每次 send 前 stamp `x-codex-ws-stream-request-start-ms`。
6. Codex Official path 不走 HTTP bridge。

### Config tests

1. Codex channel `model_headers` 中出现 Codex reserved header 时配置校验失败。
2. `channel.Other.codex` unknown top-level key fail-fast。
3. `trust_client_attestation=false` 时客户端 `x-oai-attestation` 被拒绝或忽略为 invalid policy，不上游。
4. `fedramp=true` 时上游设置 `X-OpenAI-Fedramp: true`。

### Integration tests

建立 fake upstream，捕获最终 HTTP request / WS handshake / WS frame，断言：

```text
没有 legacy headers
raw body metadata preserved
operation-specific headers correct
credential headers from channel, not client
```

## 落地顺序

这是一次目标架构重构，不设置 legacy 兼容 profile。但实际提交可以按可编译 checkpoint 切分。

### Step 1：抽 `common/jsonobject`

- 从 `common/responsesws/raw_frame.go` 提取 duplicate-key object parser。
- 保持 ResponsesWS 行为不变。
- 新增 object parser tests。

### Step 2：升级 HTTP Responses relay contract

- `relay/responses.go` 读取 raw body，生成 `RawEnvelope`。
- `types.OpenAIResponsesRequest` 继续作为 projection。
- provider contract 改为接收 `responses.Request`。
- 所有 provider 编译适配。

### Step 3：实现 `providers/codex/wire`

- HeaderSnapshot。
- MetadataSnapshot。
- IdentityResolver。
- HeaderPlanBuilder。
- BodyPlanner。
- Validators。
- Golden tests。

这一步不碰 requester，只做纯函数测试。

### Step 4：替换 Codex HTTP create / compact

- 删除 `getRequestHeaderBag` 在 `/responses` 路径上的使用。
- 删除 prompt cache header projection。
- ordinary `/responses` 使用 raw body。
- compact 使用 compact body planner。

### Step 5：替换 Codex ResponsesWS native open

- `OpenResponsesWS` 使用 wire header planner。
- WS frame send 前使用 wire metadata planner。
- Codex Official path 删除 HTTP bridge。

### Step 6：收紧配置和 CI

- Codex provider 不消费 `model_headers`。
- reserved header 配置 fail-fast。
- depguard / lint 禁止 Codex Official path 调用旧 header helper。

建议 CI 加规则：

```text
providers/codex/*responses*.go may not call applyDefaultHeaders
providers/codex/*responses*.go may not call getRequestHeaderBag
providers/codex/wire may not import gin/model/requester
```

## 验收线

1. `/responses` 上游 body 保留 Codex `client_metadata` 和未知字段。
2. `/responses/compact` 上游 body 不含 ordinary `client_metadata`，但 header 含 compact `x-codex-installation-id`。
3. Codex HTTP upstream 不出现 `session_id`、`x-session-id`、`Conversation_id`、`Connection`、HTTP `OpenAI-Beta`。
4. Codex WS handshake 不出现 `Content-Type`、`Accept`、`Connection`、`x-codex-turn-state`。
5. `session-id`、`thread-id`、`x-client-request-id` 在三条 Codex operation 上都有确定来源。
6. 客户端传空 required identity 字段时 fallback；传非法非空字段时 400。
7. 客户端 `Authorization` 不可能进入 upstream；upstream `Authorization` 必然来自 channel token。
8. Codex provider 不再通过 `model_headers` 注入 official protocol headers。
9. ResponsesWS Codex Official path 不 bridge。
10. Header decisions 可审计，且不记录敏感值。

## 取舍

这个方案会破坏旧 one-hub Codex provider 的一些隐式行为：

- 旧客户端如果只发送 `session_id` / `x-session-id`，不再影响 Codex upstream identity。
- 依赖 `model_headers` 注入 Codex protocol 字段的渠道配置会失效或保存失败。
- ResponsesWS 不再自动切 HTTP bridge。
- PI 行为不再被 Codex provider smart 推断。

这是有意的。协议代理最怕“看起来能用”的混合语义。Codex Official path 应该像一个小工具一样：输入 raw request，输出唯一的 upstream request plan。没有全局魔法，没有隐式兼容，没有跨层偷读。

最终形态可以概括为：

```text
raw envelope in, official plan out
```

这是这次重构的核心。
