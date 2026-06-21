# ResponsesWS Transport 边界重构方案

## 背景

本文以当前主线架构为基线，定义 ResponsesWS transport 边界的目标方案与迁移顺序。

当前 `GET /v1/responses` 的入口层已经收敛到统一 ResponsesWS actor：下游 WebSocket、首帧解析、连接容量、RPM、quota、affinity、terminal side effect 和关闭流程都由 relay 层串行处理。provider-facing 路径使用 `providers/base.ResponsesWSProvider.OpenResponsesWS(ctx, model, options)` 返回 `common/responsesws.Upstream`；`runtime/session.RealtimeSession` / `OpenRealtimeSessionWithOptions` 只保留给 `/v1/realtime`。

问题不在 ingress actor，而在 provider 内部边界：

- 历史迁移期 OpenAI ResponsesWS 曾复用 `openAIRealtimeSession` 作为传输和 turn 语义实现壳；当前 relay-facing contract 已收敛到 `OpenResponsesWS` + `responsesws.Upstream`，旧路径只作为背景问题记录。
- Codex realtime execution session 中也已有相似的 WS reader、attachment queue、turn usage、HTTP bridge 和 cleanup 逻辑。
- 若直接为 ResponsesWS 新增独立 upstream adapter，容易再次复制 upstream WS、recv queue、read pump、backpressure、close mapping、inflight 与 usage accumulator 等机械逻辑。

这说明“抽公共 transport”方向是对的，但必须先收敛真实重复点，而不是直接把三方法 `responsesws.Upstream` 当作完整替代品无限扩散。现有系统里还有 `runtime/session.RealtimeSession`、turn observer、Detach/Abort、provider close、usage event 等语义。若没有迁移边界，容易制造第四套相似抽象。

第一性原理下，ResponsesWS 需要的是“每条下游 WS 连接对应一个上游 responses transport”的统一抽象；它不需要跨请求 execution session，也不需要继承 `/v1/realtime` 的 resume/binding/revocation 语义。

## 边界定义

ResponsesWS 分四层：

| 层级 | 职责 | 是否 relay-facing |
| --- | --- | --- |
| Ingress / actor | 下游 WS、turn admission、quota、affinity、terminal side effect、关闭 | 是 |
| Transport | native upstream WS、HTTP/SSE bridge、send/recv/close/backpressure | 否 |
| Provider adapter | URL/header、payload shape、字段改写、provider event 过滤、usage 提取 | 否 |
| Realtime execution session | 跨请求 resume、binding、revocation、fallback cooldown | 否，属于 Codex realtime |

核心原则：

- ResponsesWS 不调用 Codex realtime execution session manager，不写 one-hub realtime binding。
- `/v1/realtime` 和 `/v1/responses` 不合并状态机；最多复用 `wsconn` 与 ResponsesWS transport helper。
- native WS 是默认 transport；HTTP bridge 是显式兼容模式，不做静默 fallback。
- provider adapter interface 可以是公共 Go interface，但不是 relay-facing contract。
- provider adapter 只处理 payload、usage、frame 过滤和 provider close/error 映射；quota、RPM、affinity、terminal side effect 由 actor 最终提交。

## Contract 策略

当前已有 `runtime/session` 的 session-facing 类型面；ResponsesWS provider contract 已收敛到 `OpenResponsesWS + responsesws.Upstream`，但 v1 仍控制类型分叉：

- `/v1/realtime` 兼容面：`runtime/session.Frame`、`RecvEvent`、`ProviderClose`、`RealtimeSession`
- ResponsesWS 目标面：`responsesws.Upstream`、`ResponsesWSProvider.OpenResponsesWS(...)`、OpenAI/Codex provider adapter、native transport helper

实施时按以下顺序处理：

1. **冻结 relay-facing surface**：列出 ResponsesWS actor 实际需要的能力：`SendClientWithResult(ctx, responsesws.SendRequest)`、`Recv`、`Abort`，以及可选 control lane `SendControl(ctx, responsesws.SendRequest)`。
2. **保持 `responsesws.Upstream` 为 ResponsesWS provider contract**：新 ResponsesWS native / HTTP bridge path 不再通过 legacy realtime open path 暴露。
3. **公共 transport 先作为内部 helper**：native WS helper 先服务 OpenAI/Codex provider adapter，不要求 relay 同时切换多套接口。
4. **类型归属短期单向适配**：如果 helper 位于 `common/responsesws`，只在边界转换 frame/event/close；不要长期让 `runtime/session` 和 `responsesws` 两套类型在业务代码里交叉传播。
5. **防止回退到 legacy path**：`runtime/session` 只服务 `/v1/realtime`；ResponsesWS 的 normalized transport 只进入 `OpenResponsesWS` 或返回 provider unsupported。

判断标准：抽 transport 的第一阶段只减少机械重复，不重写 actor 账本语义，不同时迁移 relay、OpenAI、Codex、runtime/session 四层。

### V1 实施硬约束

进入实现前，以下约束视为验收 checklist；任何一项不满足，都说明实现仍可能回到旧的 provider 内部状态机路径：

- dial ownership：v1 由 provider opener 完成 URL/header 构造、provider policy 校验和 `wsconn.DialManaged`；native helper 只接收已建立的 `ManagedConn`，不引入 `DialPlan`。
- send result contract：`ResponsesWSTransportSendStatus`、`ResponsesWSTransportSendResult`、`TransportSendCapable` 放在 `common/responsesws`；`responsesws.Upstream` 必须实现 `SendClientWithResult(ctx, responsesws.SendRequest)`，actor 直接使用该结果做 ResponsesWS send accounting。
- ambiguous close policy：`ResponsesWSTransportSendAmbiguous` 后 upstream 无法继续产出 evidence 时，actor 区分 `no_provider_evidence` 与 `provider_evidence_without_terminal` 只用于错误归因、日志、watchdog 与 affinity side effect；账务上二者都不能 rollback 成免费。`no_provider_evidence` 使用 preconsume floor；`provider_evidence_without_terminal` 使用 `max(observed_billable_usage_quota, preconsume_floor)`。
- result legality：`ProviderFrameResult` 组合由 helper 测试固定，adapter 不得自由组合 `Filtered`、`EmitFrame`、`Usage`、`Err`、`Origin`。
- event origin path：凡是影响 accounting、terminal side effect 或 provider evidence 判定的 origin，必须通过 event path 到 actor，不能只存在于日志或测试替身。
- legacy origin fallback：`DetailOrigin == ""` 有固定兼容规则；unknown non-empty detail origin 不能默默当作 provider terminal evidence。
- provider evidence helper：provider evidence / terminal evidence / proxy-local terminal 判断必须通过统一 helper，不能在 actor 多处手写 origin 表。
- inflight owner：actor 拥有 admission/accounting inflight；transport helper 只拥有连接和队列状态，HTTP bridge stream handle 只用于 cancel plumbing。
- bridge rollout：`http_bridge` 曾在未实现阶段作为合法但 unsupported 的配置返回 426；当前契约是 OpenAI/Codex 显式 `http_bridge` 进入 HTTP bridge，只有 provider/channel 不支持所选 transport 或缺少 HTTP Responses endpoint 时返回 426；非法配置仍返回 400。
- bridge upstream URL policy：HTTP bridge 的最终 HTTP request URL 必须执行 ResponsesWS 专用上游 policy。默认要求 `https` public host；`responses_ws_self_hosted=true` 才允许 `http` 与 private/local host；metadata host 与 metadata 解析结果永远拒绝。显式 proxy 模式也必须先做本地 DNS fail-closed 校验；无 proxy 时必须把 HTTP dial pin 到校验得到的 IP。该检查属于 provider opener / raw stream 边界，不下沉到全局 HTTP requester，避免改变普通 HTTP relay 的兼容性和 retry 语义。Trade-off：proxy 侧 split DNS 的灵活性降低，换取携带 provider 凭据的 bridge 请求不绕过 SSRF 边界。
- bridge open/cancel：stream open failure、stream read error、no-active cancel、cancel-terminal race 有固定 event 与 accounting 语义。
- native recv ordering：本地 backpressure / abort / read-close lifecycle event 不得越过已经进入 recv queue 的 provider frame / evidence。
- bridge malformed payload：HTTP bridge stream 中 malformed SSE `data:` payload 与 line-too-large 一样返回安全 `responses_ws_provider_protocol_error`，不得暴露 raw upstream line、headers、request body、session id 或 authorization material。
- panic safety：adapter panic recovery 不直接记录 recovered 原值，只记录安全摘要和可控 debug 信息。
- open context ownership：`OpenResponsesWS(ctx, model, options)` 的函数参数 `ctx` 是唯一 open/base context，`OpenOptions` 不携带 context。这样 retry/open worker、provider opener 与 bridge session 的 cancel source 保持单一。

### 2026-06-08 复审修复方案

第一轮多角度 review 后，v1 修复范围限定在 transport/evidence 边界，不重写 ingress actor 的账本状态机：

1. **pending close replay 复用 active evidence 入口**：close path replay buffered provider terminal 时，除 quota/affinity side effect 外，也调用与 active terminal 相同的 provider API error 处理入口；若没有 terminal，则第一个可向 client 表达的 buffered provider recv failure 用自身 `ReceivedAt` 标记完成并写同类 protocol-error payload。获得的是 close race 下 channel health、auto-disable、request_time 与失败归因一致；牺牲的是 replay path 多一次 dedupe/失败 payload 判断。dedupe 仍按当前 attempt 内的 `providerAPIErrorKeys` 控制，避免 close replay 与正常 active path 双报。
2. **native read pump panic fail-closed**：native helper 的 read-pump goroutine 自己 recover，投递 proxy-local `native_read_error` evidence、记录安全 diagnostic、关闭 transport。获得的是 goroutine 级 hard invariant；牺牲的是 panic 后不尝试恢复 upstream，因为 read state 已不可信。
3. **provider event 最小 schema 校验**：native OpenAI/Codex adapter 与 HTTP bridge stream 只要求 provider payload 是 JSON object 且顶层 `type` 为非空 string，不枚举所有官方 event type。获得的是 `{}`、`{"foo":1}`、`{"type":""}` 不再作为 provider evidence 透传；牺牲的是无法在 adapter 边界提前发现“未知但有 type”的未来事件，后者仍交给 actor terminal classifier 作为 non-terminal passthrough。
4. **native usage best-effort decode**：native OpenAI/Codex adapter 先完成最小 envelope 校验，再只对已知 usage 字段做 best-effort typed decode；typed decode 失败不关闭 transport。获得的是 native WS 与 HTTP bridge 对未来 provider event shape 的 passthrough 一致性；牺牲的是 adapter 边界不再因为已知字段 shape 异常提前 fail-fast，后续仍由 actor terminal classifier 和 provider error 解析处理可识别 evidence。
5. **known terminal response shape fail-closed**：`response.completed` / `response.done` / `response.failed` / `response.incomplete` / cancel terminal 若携带非 null `response`，该字段必须是可解析对象；未知/未来 event 即使携带当前 typed struct 不认识的 `response` shape 仍按 non-terminal passthrough。获得的是已知 terminal 不会因坏 shape 被误判成功并触发 accounting terminal side effect；牺牲的是如果未来官方把已知 terminal 的 `response` 改成非对象，会被 fail-closed，需要显式升级 schema。
6. **HTTP bridge proxy policy fail-closed**：ResponsesWS HTTP bridge 默认必须把已校验 IP 绑定到实际 dial。HTTP/HTTPS proxy 的 CONNECT/absolute-URL 目标由 proxy 侧解析，当前实现无法在不自定义代理协议栈的前提下同时保留原始 Host/SNI 又 pin 目标 IP，因此 v1 对 HTTP/HTTPS proxy fail-closed；SOCKS proxy 继续允许，但 dial 目标改为本地校验后的 IP，TLS SNI/HTTP Host 仍来自原始请求。获得的是携带 provider 凭据的 bridge request 不被代理侧 DNS split/rebinding 绕过；牺牲的是部分依赖 HTTP proxy 出网的 bridge 部署需要改用 SOCKS、直连，或显式自建 upstream 方案。普通 HTTP relay 不受影响。

### 2026-06-09 复审修复方案

第二轮多角度 review 后，修复继续限定在“证据边界先于状态结算”的原则内，不引入完整 JSON schema registry：

1. **send queue full 同步 NotSent**：actor 在 `SendProviderFrame` 返回 false 时已经知道 create 没进入 upstream send queue，必须同步进入 `SendOutcomeNotSent` 处理，不能再排队等待内部事件。获得的是 client close 先入队时也会 rollback 未发送请求；牺牲的是该路径从异步事件改成 actor 内联处理，但它仍复用 `handleSendResult` 的统一 accounting 分支。
2. **未计价 usage-only 仍保留 provider evidence**：`input_audio_transcription` usage 在未配置价格时不进入计费合并，但它仍是 provider activity evidence，必须先校验 response id 并更新 pending/active evidence。获得的是 ambiguous/not-sent close 不会把已发生的 provider activity 当作无上游证据；牺牲的是 evidence 与 billable usage 不再完全同义，后续读代码必须区分 provider activity evidence 与计费 usage。
3. **client event envelope 严格化**：`response.cancel` 与 `response.create` 共享顶层 JSON object、无重复 key、非空 string `type` 的最小解析边界。获得的是代理不会因 Go `json.Unmarshal` 的重复键后值覆盖，把上游可能解释为 create 的 payload 当作 cancel 原样转发；牺牲的是这里仍不做完整字段白名单，未来 client event 只要 envelope 合法仍可由 actor/provider-specific 分支决定是否支持。

### 2026-06-09 后续复审修复方案

第三轮事实核查后，修复继续控制在 provider/transport 边界，不把 HTTP bridge opener 扩成完整状态机：

1. **prepare/open panic fail-closed**：native `PrepareClientFrame` 与 HTTP bridge opener panic 不再只作为普通 pre-send error 返回，而是同步投递 proxy-local `adapter_panic` evidence 并关闭 helper。获得的是 adapter 状态不可信时连接不再复用；牺牲的是这类未发送请求也会关闭当前 upstream/session，但账本仍按 not-attempted rollback。
2. **bridge opener 分阶段错误分类**：provider adapter 在 HTTP request 构造、custom parameter merge、marshal、URL/config preflight 阶段的错误必须走 `OpenBridgeStream` 第三个 error 返回；只有 `SendResponsesHTTPBridgeRaw` 后已有 provider HTTP status 的错误才进入 `bridge_open_provider_error` / `RejectedBeforeStream`。获得的是 provider health、auto-disable、metrics 不被本地配置错误污染；牺牲的是 opener 代码多一次显式阶段拆分。
3. **cancel attempt 强绑定**：HTTP bridge cancel 对 opening/active stream 要求非空且匹配的 `attempt_id`；空 attempt 与 stale attempt 都是 no-op。获得的是迟到或无归属 cancel 不会跨 turn 关闭当前 stream；牺牲的是 helper 复用者必须显式传入 actor attempt context。
4. **open timeout 不放大非协作 opener 泄漏**：`openBridgeStreamWithTimeout` 超时后取消 ctx、关闭 timeout signal 并返回，不再额外创建等待 late result 的 cleanup goroutine。若 opener 晚返回 stream，opener goroutine 会在发送结果与观察 timeout signal 中二选一；timeout 已发生时由该 goroutine 自行 `Close()` late stream。获得的是协作但晚返回的 stream 不泄漏，且非协作 opener 仍最多只泄漏自身 goroutine；牺牲的是永不返回、严重违反 ctx contract 的 opener 仍无法被 helper 强制回收，需通过 provider opener 测试约束。

### ResponsesWS accounting invariant

ResponsesWS actor 是 `response.create` admission、quota reserve、RPM/affinity accounting 和 terminal finalization 的唯一 owner。native transport 与 provider adapter 只能返回证据：local send result、provider-originated frame、usage event、provider close 或 proxy-local error。

新的 ResponsesWS upstream、native transport、provider adapter 不得调用 `TurnObserver.AdmitTurn`、`RollbackTurnAdmission` 或 `FinalizeTurn`。迁移期如果仍经过 `runtime/session.RealtimeSession` 兼容路径，`SetTurnObserverFactory` 只作为旧路径兼容桥，不是目标 ResponsesWS contract 的依赖。

每个 `attempt_id` / `response_id` 最多产生一次 terminal accounting decision。若 send 结果为 ambiguous，actor 必须等待 provider-originated evidence 或连接关闭证据后再决定 finalize，provider adapter 不得自行提交账本或 rollback。

### Send outcome ownership

send outcome 由 actor 基于 transport 标准错误与 provider evidence 统一判定，provider adapter 不拥有 `LocalWriteOK / NotSent / Ambiguous` 的最终解释权。

send result 类型归属 `common/responsesws`，与 `responsesws.Upstream`、`responsesws.Frame` 和 provider evidence 类型同包。`runtime/session` 只服务 `/v1/realtime`，不声明 ResponsesWS 专用 send result 或 frame 类型。

```go
// common/responsesws
type ResponsesWSTransportSendStatus string
type ResponsesWSTransportSendReason string

const (
    ResponsesWSTransportSendNotAttempted         ResponsesWSTransportSendStatus = "not_attempted"
    ResponsesWSTransportSendAttempted            ResponsesWSTransportSendStatus = "attempted"
    ResponsesWSTransportSendRejectedBeforeStream ResponsesWSTransportSendStatus = "rejected_before_stream"
    ResponsesWSTransportSendAmbiguous            ResponsesWSTransportSendStatus = "ambiguous"
)

type ResponsesWSTransportSendResult struct {
    Status ResponsesWSTransportSendStatus
    Err    error
    Reason ResponsesWSTransportSendReason
}
```

relay-facing ResponsesWS surface 通过 `responsesws.Upstream` 强制要求 send result，避免把 `SendNotAttempted / SendAttempted / SendAmbiguous` 语义塞进普通 error 后丢失：

```go
type SendRequest struct {
    AttemptID string
    Frame     Frame
    DefaultPreviousResponseID string
}

type TransportSendCapable interface {
    SendClientWithResult(ctx context.Context, req SendRequest) ResponsesWSTransportSendResult
}
```

`responsesws.Upstream` 嵌入 `responsesws.TransportSendCapable`。actor 直接调用 `SendClientWithResult(ctx, responsesws.SendRequest)`；不再通过 plain `SendClient(ctx, frame) error` mapper 执行 ResponsesWS accounting。

语义约定：

- native WS `ResponsesWSTransportSendNotAttempted`：validation/preflight failed、context 在 write 前取消、connection 在 write 前已关闭，或 helper 明确未进入 `ManagedConn.WriteMessage`。
- native WS `ResponsesWSTransportSendAttempted`：`ManagedConn.WriteMessage` 返回 nil。
- native WS `ResponsesWSTransportSendAmbiguous`：已经进入 `ManagedConn.WriteMessage` 且返回 error；除非底层显式证明未写出，否则一律按 ambiguous 处理。

HTTP bridge 也必须实现同一 `ResponsesWSTransportSendResult` contract，但 send attempt boundary 是发起 `CreateResponsesStream` / HTTP request open，而不是 `ManagedConn.WriteMessage`：

- bridge `ResponsesWSTransportSendNotAttempted`：preflight/prepare failed、actor busy rejection、context 在发起 HTTP request 前取消、bridge 在发起 HTTP request 前已关闭。
- bridge `ResponsesWSTransportSendAttempted`：HTTP request 已可能到达 provider，且 provider stream 已成功打开。
- bridge `ResponsesWSTransportSendRejectedBeforeStream`：HTTP request 已到达 provider，但 provider 在 stream start 前返回 HTTP status/error response，例如 401/403/429/5xx。该状态表示 provider request attempted，但 upstream response stream 未 accepted/opened；它不产生 provider ResponsesWS terminal side effect。client-facing/provider-status error 通过 `BridgeEventResult` / `responsesws.UpstreamEvent` event path 上报，不能放进 send result `Err`。
- bridge `ResponsesWSTransportSendAmbiguous`：HTTP request 已发起，但在 provider status 可用前发生 local/proxy network failure，无法判断 provider 是否收到请求。

bridge open error classification 必须以“provider status 是否可得”为边界，而不是只看 `OpenAIErrorWithStatusCode` 是否非 nil。`HTTPClient.Do` / DNS / TLS / proxy / connect timeout 等没有 provider HTTP status 的失败是 proxy-local transport failure，必须标记为 `LocalError`，通过 `bridge_stream_error` event path 上报，并返回 `ResponsesWSTransportSendAmbiguous`。只有 provider 已返回 HTTP status/error body 的 rejection 才能走 `bridge_open_provider_error` 与 `ResponsesWSTransportSendRejectedBeforeStream`。当前实现可以在 ResponsesWS HTTP bridge raw stream 边界用 `MarkHTTPBridgeTransportError` 恢复该语义；不要把这个规则下沉到全局 `requester.SendRequestRaw`，否则会改变普通 HTTP relay 的 retry/side-effect 策略。Trade-off：bridge 层多一个轻量分类 helper，换取全局 requester 语义稳定和 accounting evidence 不被误污染。

native WS 与 HTTP bridge 共同原则：只要请求已经可能到达 provider，就不能轻易标成 `ResponsesWSTransportSendNotAttempted`。

bridge open provider error enqueue rule：provider 在 stream start 前同步返回 HTTP rejection 时，bridge 只有在对应 `bridge_open_provider_error` event 已可靠进入 transport recv queue 或 actor event path 后，才能返回 `ResponsesWSTransportSendRejectedBeforeStream`。如果 event 因 queue 满、context cancel 或内部错误无法入队，bridge 必须返回带 proxy-local transport error 的 `ResponsesWSTransportSendAmbiguous` 并关闭，不能静默返回 rejected，也不能退化为 `ResponsesWSTransportSendNotAttempted`。native WS attempted 后 provider evidence 本来异步到达，不需要这条同步入队要求；bridge open rejection 是同步产生的唯一 provider evidence，不能丢。

actor ordering rule：`bridge_open_provider_error` 进入 transport recv queue 不保证 actor 一定先于 send result 处理它；actor 在处理 `ResponsesWSTransportSendRejectedBeforeStream` 时必须保留足够的 attempt correlation state，直到对应 `bridge_open_provider_error` 已按 `AttemptID` 进入 actor 状态机并完成 client payload delivery/cleanup。

bridge local open error ordering rule：`bridge_stream_error` local open failure 同样可能晚于 `ResponsesWSTransportSendAmbiguous` 到达 actor。因为 bridge 在返回 ambiguous 前已经把 local error event 可靠入队，actor 处理该 ambiguous 时必须保留 `AttemptID` 关联状态，等待对应 `bridge_stream_error` 写出精确 `ws_request_failed` payload；不能提前退化为 `ambiguous_close_no_provider_evidence`。账务上 bridge local open error 不是 zero-charge proof，除非 transport 明确返回 not-attempted；因此该路径默认按 preconsume floor settlement。如果 local error event 无法入队，bridge 才能走 generic ambiguous/queue-full 路径。

HTTP bridge stream open success evidence：`CreateResponsesStream` 成功并返回已打开的 provider stream 时，actor 必须在读取第一条 SSE/stream event 前记录 provider activity evidence。实现通过 dedicated actor event 或扩展 recv pump 显式识别 `RecvDetailOriginBridgeStreamOpened` 来进入 actor event path；pending 阶段写入 `responsesWSProviderJournal`，active 阶段合并到 `common/responsesws.ProviderSettlementLogProjection`。不能把它作为会被 legacy recv pump 忽略的空 event 发送。

`bridge_stream_opened` open-success evidence 必须进入 actor 串行事件路径。bridge/helper goroutine 不得直接修改 pending journal 或 active projection；如果 hook 以 callback 形式存在，callback 也必须 enqueue actor event，或在 actor-owned serialization 下执行。

`bridge_stream_opened` evidence 成功进入 event path 并更新 actor-owned journal/projection 后，bridge 才能返回 `ResponsesWSTransportSendAttempted`。如果 evidence 因 queue 满、actor 已关闭或竞态无法记录，bridge 必须关闭 stream，并返回带 proxy-local transport error 的 `ResponsesWSTransportSendAmbiguous`；不得返回 `ResponsesWSTransportSendNotAttempted`。若随后 stream EOF/read error 在任何 `provider_stream` payload 前发生，ambiguous resolution 仍走 `provider_evidence_without_terminal`，不能当作 `no_provider_evidence`。

合法性约束：`ResponsesWSTransportSendAttempted` 与 `ResponsesWSTransportSendRejectedBeforeStream` 必须 `Err == nil` 且 `Reason == ""`；provider HTTP rejection、provider frame error 或 downstream client-facing error 不能塞进 send result 的 `Err`，必须走 recv/event path。`ResponsesWSTransportSendNotAttempted` 应携带 `Err` 或明确 `Reason`，其中非错误幂等 no-op（例如 bridge no-active cancel）使用 `Reason` 而不是 sentinel error；这增加了一个轻量字段，但避免把正常控制语义伪装成失败。`ResponsesWSTransportSendAmbiguous` 应携带 `Err` 且 `Reason == ""`，并且必须表示 native WS 已进入 `ManagedConn.WriteMessage` 后失败，或 bridge HTTP request 已发起但 provider status 不可得。未知 `ResponsesWSTransportSendStatus` 不能默默当作 attempted，actor 应按 ambiguous 保守处理或由测试拒绝。

provider adapter 可以在 `PrepareClientFrame` 阶段返回 client-facing payload error，例如 frame shape 不合法或 provider 不支持某个字段；但这类错误只说明“发送前校验失败”，不能伪装成 provider terminal event，也不能驱动 quota finalize。

`PrepareClientFrame` / preflight failure 表示 `ResponsesWSTransportSendNotAttempted`。如果 actor 已经为该 attempt 做了本地 quota/RPM reserve，actor 只 rollback reserve，不做 terminal finalize；adapter 不得为 prepare failure 生成 provider-originated terminal payload。更优顺序是 actor 在 reserve 前尽量调用 provider preflight；若 adapter-specific shape 只能在 send path prepare，必须按上述 rollback 规则处理。

`ResponsesWSTransportSendAmbiguous` 后 upstream 无法继续产出 evidence 时，连接关闭本身不能被解释为 provider terminal。v1 actor policy 按 provider evidence 拆成两类：

- `no_provider_evidence`：actor 没看到任何 provider-originated frame、usage、provider close 或 provider stream event。它只能说明 proxy 没观察到 provider evidence，不能证明 provider 没看到请求。actor 释放本地 inflight/capacity，按 preconsume floor 做 conservative settlement；RPM admission 不 rollback；affinity 不提交 provider terminal side effect；记录 proxy-local `ambiguous_close_no_provider_evidence` metric/log，不生成 provider-originated terminal payload。
- `provider_evidence_without_terminal`：actor 已看到 provider-originated non-terminal evidence，例如 `response.created`、usage-only evidence、provider close 或 provider stream event，但没有 provider terminal payload。actor 释放本地 inflight/capacity，不合成 provider failed/cancelled/completed；记录 proxy-local `upstream_lost_after_provider_evidence`。quota 按 `max(observed_billable_usage_quota, preconsume_floor)` 结算；RPM admission 不 rollback；affinity 不能记成 provider success/failure。

两类 ambiguous resolution 都不得把 synthetic failed/cancelled 当作 provider terminal，也不得让 provider adapter 自行 finalize 或 rollback。

SendResult accounting scope：`ResponsesWSTransportSendResult` 只有在绑定到 `response.create` attempt 且存在 `attempt_id` 时，才驱动 quota/RPM/affinity accounting。`response.cancel`、control frame、client ping/pong 或未来其它 client event 的 send result 只作为 transport delivery diagnostics；它们不得 reserve、rollback 或 finalize quota/RPM/affinity。synthetic cancel 不能覆盖已经观测到的 provider terminal evidence。

## 目标架构

### Native WS transport

新增公共 native WS transport helper，负责机械传输能力：

- 持有 `*wsconn.ManagedConn`。
- ResponsesWS 级 send sequencing、context/closed precheck、send result classification。
- raw WebSocket write serialization 与 write deadline 复用 `wsconn.ManagedConn`，不在 helper 中重复实现；只有在需要保护 `prepare -> write attempted flag -> result classification` 原子性时，helper 才维护 ResponsesWS 级 `sendMu`。
- read pump、bounded recv queue、backpressure close。
- HTTP/SSE bridge 在 ResponsesWS bridge 边界按 SSE blank-line 聚合 event，支持 `event:`/`id:`/`retry:`/comment 与 multi-line `data:`；不把该解析规则下沉到全局 HTTP requester。JSONL/raw JSON lines 不属于当前 bridge contract，自建 upstream 必须返回 SSE。Trade-off：ResponsesWS bridge 多一层轻量 assembler，且不覆盖 JSONL 兼容实现，但避免改变普通 stream relay 的兼容语义和扩大 bridge 协议面。
- closeOnce 与 `Abort` 幂等。
- provider close / local close / backpressure close 的统一事件化。
- adapter panic recovery：panic 转为 proxy-local error + close，不能杀死 pump goroutine 或泄露 conn；同时必须记录 provider、channel、transport、phase、safe error code 等安全摘要。测试应验证 panic 会触发 metric/log hook，而不只是返回 proxy-local error。
- adapter panic containment：provider adapter panic 必须在 native/bridge helper 内部 recover，不能逃逸到 relay-level goroutine recover。relay-level recover 是最后防线，不能作为 adapter panic sanitization 机制；helper 不得直接记录 `recover()` 原值，只记录 panic class、phase、provider/channel/transport、安全错误码和 stack hash。
- 不记录完整 `Authorization`、session id、raw payload；diagnostic logging 只允许安全摘要。

provider adapter 注入差异：

- `PrepareClientFrame(ctx, frame)`：校验 client frame，做 provider payload 改写。
- `HandleProviderFrame(ctx, messageType, payload)`：过滤 bootstrap/control frame，提取 usage，必要时重写下行 payload。
- `MapProviderClose(info)`：把 upstream close 映射为 provider close 或 proxy-local error。

`MapProviderClose(info)` 的输入必须携带底层 `wsconn.CloseInfo.Kind`。只有 `CloseKindPeerClose` 表示 upstream peer 确实发起 close，可映射为 `native_provider_close` provider evidence。`CloseKindWriteError`、`CloseKindReadError`、`CloseKindNormal`、`CloseKindGracefulShutdown`、`CloseKindAbort`、`CloseKindBackpressure` 等都是 proxy-local transport lifecycle，不能因为存在 close reason、wire code 或 `wsconn.CloseError` 就升级为 provider close。helper 应先按 close kind 决定是否允许调用 adapter close mapper；adapter 自身也应做同样守卫，避免未来复用时绕过 helper。Trade-off：旧代码里“有 code/reason 就转 provider close”的兼容宽松度降低，但 quota/ambiguous accounting 不再被本地 write/close failure 误保留。

```go
type ProviderCloseInfo struct {
    Kind   wsconn.CloseKind
    Code   int
    Reason string
    Err    error
}
```

OpenAI adapter 保持 inline `response.create`；Codex adapter 保留 nested payload 构造、bootstrap 过滤和 `codexTurnUsageAccumulator`。

v1 dial ownership：provider opener 负责 URL/header 构造、provider URL policy 校验与 `wsconn.DialManaged`。native helper 只接收已建立的 `*wsconn.ManagedConn` + adapter，不解析 raw channel `Other` JSON，不构造 provider URL，也不重新解释 self-hosted/private-IP/proxy policy。`ws://`、private IP、自定义 upstream、proxy、handshake timeout 等行为只能通过现有 provider policy 控制，不能因为抽公共 helper 绕过原有安全边界。若 OpenAI/Codex 迁移后仍存在大量重复 dial 逻辑，再另行引入 `DialPlan` contract。

provider binary data frame v1 默认视为 `RecvDetailOriginProviderMalformed` 并关闭 transport，除非 adapter 显式声明支持。native helper 可以把 `messageType` 传给 adapter，但 adapter 必须显式处理 binary；未处理即 proxy-local protocol error + close。

Native ResponsesWS 的 turn inflight/admission owner 是 actor，不是 transport helper。provider adapter 可以维护 provider parser state、usage accumulator、bootstrap filter state，但不能维护 quota/admission 语义上的 inflight。HTTP bridge 可以维护 current stream handle 用于 cancel plumbing，但该 state 只表示 transport cancellation target，不表示 accounting owner。

### Provider frame result contract

`ProviderFrameResult` v1 是 common native helper 与 provider adapter 之间的内部 contract，不作为 relay-facing API。字段类型归属 `common/responsesws`：frame 使用 `responsesws.Frame`，origin 使用 `responsesws.RecvDetailOrigin`，provider close 使用 `responsesws.ProviderClose`，避免 ResponsesWS 与 `/v1/realtime` 类型交叉传播。

v1 先采用单 result，不提前设计多 event fan-out。若未来 provider frame 确实需要拆成多个 downstream event，再升级为 slice。

```go
type ProviderFrameResult struct {
    EmitFrame      *Frame
    Usage          *types.UsageEvent
    Origin         RecvDetailOrigin
    Err            error
    CloseTransport bool
    Filtered       bool
}
```

字段语义：

- `EmitFrame`：需要下发给 downstream 的 ResponsesWS frame；为空表示不下发 payload。
- `Usage`：provider-originated usage evidence，交给 actor merge，不在 adapter finalize。
- `Origin`：native provider frame handling 的细分来源；普通 provider frame、provider malformed frame、adapter panic 必须可区分。
- `Err`：proxy-local adapter error 或 provider frame parse error；不能伪装成 provider terminal payload。
- `CloseTransport`：当前 frame 之后 transport 应关闭，例如 provider 明确 close 或 adapter 发现不可恢复协议错误。
- `Filtered`：bootstrap/control frame 被消费，不产生 downstream payload，也不代表 terminal。

合法组合由 helper 测试固定：

| 场景 | EmitFrame | Usage | Err | Filtered | CloseTransport | Origin |
| --- | --- | --- | --- | --- | --- | --- |
| 普通 provider frame | 可有 | 可有 | nil | false | false | `provider_frame` |
| bootstrap/control filter | nil | nil | nil | true | false | `provider_frame` |
| usage-only provider evidence | nil | 有 | nil | false | false | `provider_frame` |
| provider malformed frame | nil | nil | err | false | true | `provider_malformed_frame` |
| adapter panic | nil | nil | err | false | true | `adapter_panic` |

规则：

- `Filtered=true` 表示没有 downstream `EmitFrame`，且 v1 不携带 `Usage`。
- `Err != nil` 不得同时携带 `EmitFrame`。
- `Err != nil` 不得使用 `provider_frame` origin。
- `Usage != nil` 只允许 provider-originated evidence，v1 不允许 `Origin=proxy_local`。
- `CloseTransport=true` 只表示当前 provider data/control frame 处理导致 native transport 应关闭，例如 provider malformed frame、不可恢复 provider protocol error、adapter panic 或 provider 明确 control close，不能作为普通 frame 的隐式副作用。
- `ProviderFrameResult` 只建模 native provider WS frame 处理；HTTP bridge synthetic cancel、stream EOF、stream error 不得通过它表达。
- local abort、graceful detach、backpressure、read error、HTTP bridge stream EOF/error 不通过 `ProviderFrameResult.CloseTransport` 表达，而通过 transport/bridge event origin 表达。

### Bridge event result contract

`BridgeEventResult` v1 是 HTTP bridge stream-to-WS loop 的内部 result，不复用 `ProviderFrameResult`。bridge 没有 native provider WS frame、provider close/control frame，也不拥有 accounting inflight；它只表达 provider stream evidence、synthetic cancel 与 stream lifecycle。

```go
type BridgeEventResult struct {
    EmitFrame   *Frame
    Usage       *types.UsageEvent
    Err         error
    CloseStream bool
    Origin      RecvDetailOrigin
}
```

合法组合：

| 场景 | EmitFrame | Usage | Err | CloseStream | Origin |
| --- | --- | --- | --- | --- | --- |
| provider stream event | 可有 | 可有 | nil | false | `provider_stream` |
| provider stream opened before first event | nil | nil | nil | false | `bridge_stream_opened` |
| provider HTTP rejection before stream starts | nil | nil | err | true | `bridge_open_provider_error` |
| synthetic cancel | 有 | nil | nil | true/false | `synthetic_bridge` |
| stream EOF | nil | nil | nil | true | `bridge_stream_eof` |
| stream error | nil | nil | err | true | `bridge_stream_error` |

bridge normal provider stream event 与 usage 是 provider-originated evidence；synthetic cancel、stream EOF 和 stream error 不是 provider terminal evidence，也不得设置 `ProviderClose`。

`bridge_open_provider_error` 的 `BridgeEventResult.EmitFrame` 必须保持 nil。面向客户端的错误 payload 通过 `Err` 中的 `ClientPayloadError` 或单独 proxy-local error event 表达，不能作为 provider `EmitFrame` 下发，避免被 ResponsesWS terminal classifier 误判为 provider `response.failed` / `error` payload。

### Transport event origin model

native transport 与 HTTP bridge 都必须保留 event origin，但 native close origin 与 bridge synthetic/error origin 分开建模，避免 bridge 语义污染 native helper。凡是影响 actor accounting、terminal side effect 或 provider evidence 判定的 origin，都必须通过 event path 到达 actor，不能只存在于日志摘要或测试替身。

v1 在 `common/responsesws` 定义 typed detail origin，与 `UpstreamEvent` 同包，避免 actor 决策依赖裸字符串，也避免 ResponsesWS 类型泄露到 `runtime/session`：

```go
// common/responsesws
type RecvDetailOrigin string

const (
    RecvDetailOriginProviderFrame            RecvDetailOrigin = "provider_frame"
    RecvDetailOriginProviderStream           RecvDetailOrigin = "provider_stream"
    RecvDetailOriginProviderMalformed        RecvDetailOrigin = "provider_malformed_frame"
    RecvDetailOriginProxyLocal               RecvDetailOrigin = "proxy_local"
    RecvDetailOriginAdapterPanic             RecvDetailOrigin = "adapter_panic"
    RecvDetailOriginSyntheticBridge          RecvDetailOrigin = "synthetic_bridge"
    RecvDetailOriginBridgeStreamOpened       RecvDetailOrigin = "bridge_stream_opened"
    RecvDetailOriginBridgeOpenProviderError  RecvDetailOrigin = "bridge_open_provider_error"
    RecvDetailOriginBridgeStreamError        RecvDetailOrigin = "bridge_stream_error"
    RecvDetailOriginBridgeStreamEOF          RecvDetailOrigin = "bridge_stream_eof"

    RecvDetailOriginNativeProviderClose RecvDetailOrigin = "native_provider_close"
    RecvDetailOriginNativeProviderEOF   RecvDetailOrigin = "native_provider_eof"
    RecvDetailOriginNativeLocalAbort    RecvDetailOrigin = "native_local_abort"
    RecvDetailOriginNativeLocalDetach   RecvDetailOrigin = "native_local_detach"
    RecvDetailOriginNativeBackpressure  RecvDetailOrigin = "native_backpressure"
    RecvDetailOriginNativeReadError     RecvDetailOrigin = "native_read_error"
)

type RecvDetailPhase string

const (
    RecvDetailPhasePrepareClientFrame  RecvDetailPhase = "prepare_client_frame"
    RecvDetailPhaseHandleProviderFrame RecvDetailPhase = "handle_provider_frame"
    RecvDetailPhaseMapProviderClose    RecvDetailPhase = "map_provider_close"
)
```

helper 内部直接使用 `responsesws.RecvDetailOrigin`，不要再发明另一套长期传播的 origin enum。

ResponsesWS provider-facing event 使用 `responsesws.UpstreamEvent`：

```go
type UpstreamEvent struct {
    Frame         *Frame
    ProviderClose *ProviderClose
    Usage         *types.UsageEvent
    AttemptID     string
    ResponseID    string
    DetailOrigin  RecvDetailOrigin
    DetailPhase   RecvDetailPhase
    Err           error
}
```

`DetailPhase` 只在 `adapter_panic`、adapter error、provider close mapping error 等需要区分 phase 的事件上填写。`PrepareClientFrame` panic 发生在 write 前，应作为 send path 的 `ResponsesWSTransportSendNotAttempted`，不构成 provider evidence。`HandleProviderFrame` panic 说明已经收到 upstream provider frame，不是 provider terminal，但可以记录 provider activity evidence。`MapProviderClose` panic 是否构成 provider close evidence，取决于底层 close origin；不能因为 mapping panic 合成 provider terminal。

账务投影的取舍固定为：`handle_provider_frame` 阶段的 `adapter_panic` 表示 relay 已收到 provider bytes 但 adapter 崩溃，因此按 provider activity 保守结算；`prepare_client_frame` 阶段没有 provider activity proof，按 not-attempted/proxy-local 处理。收益是 provider bytes 不会被误判免费；代价是 adapter phase 必须准确传递，缺失 phase 时不能把 panic 自动升级为 provider activity。

derived payload origin 与 `DetailOrigin` 的合法组合固定如下：

| DetailOrigin | payload origin | provider-originated evidence | provider terminal evidence |
| --- | --- | --- | --- |
| `provider_frame` | `Provider` | 是 | 取决于 payload terminal classifier |
| `provider_stream` | `Provider` | 是 | 取决于 payload terminal classifier |
| `provider_malformed_frame` | `ProxyLocal` | 是，表示 upstream provider activity | 否 |
| `adapter_panic` | `ProxyLocal` | 否；若 `DetailPhase=handle_provider_frame`，可记录 provider activity | 否 |
| `synthetic_bridge` | `ProxyLocal` | 否 | 否 |
| `bridge_stream_opened` | `Provider` | 是，表示 provider stream 已打开 | 否 |
| `bridge_open_provider_error` | `Provider` | 是，表示 provider HTTP request rejection | 否 |
| `bridge_stream_error` | `ProxyLocal` | 否 | 否 |
| `bridge_stream_eof` | `ProxyLocal` | 否，除非此前已有 `provider_stream` evidence | 否 |
| `proxy_local` | `ProxyLocal` | 否 | 否 |
| `native_provider_close` | `Provider` | 是，表示 provider close evidence | 否 |
| `native_provider_eof` | `Provider` | connection evidence；是否是 request-level evidence 取决于 prior evidence 或 actor policy | 否 |
| `native_local_abort` | `ProxyLocal` | 否 | 否 |
| `native_local_detach` | `ProxyLocal` | 否 | 否 |
| `native_backpressure` | `ProxyLocal` | 否 | 否 |
| `native_read_error` | `ProxyLocal` | 否 | 否 |

`provider_malformed_frame` 的 payload origin 固定为 `ProxyLocal`：它表示 provider 有活动，但错误是 proxy-local protocol/parse error，不能触发 provider terminal side effect。

`native_provider_eof` 只表示 upstream connection 结束。plain EOF 前若没有任何 provider frame、usage、provider close code/reason 或 provider stream event，它是 connection evidence，不是 request-level provider evidence；`SendAmbiguous + immediate EOF` 默认仍走 `ambiguous_close_no_provider_evidence`。只有 actor policy 明确选择时，plain EOF 才能升级为 request-level provider evidence。带 provider close code/reason 的 `native_provider_close` 可作为 provider close evidence，但仍不是 provider terminal payload。

`DetailOrigin` zero-value fallback 用于兼容未迁移的旧 event producer：

- `DetailOrigin == "" && ProviderClose != nil`：actor 视为 `RecvDetailOriginNativeProviderClose`。
- `DetailOrigin == "" && (Frame != nil || Usage != nil)`：actor 视为 legacy native `RecvDetailOriginProviderFrame`。
- `DetailOrigin == "" && Err != nil`：actor 视为 `RecvDetailOriginProxyLocal`。
- unknown non-empty `DetailOrigin`：不允许默默当作 provider terminal evidence；actor 按 proxy-local ambiguous/protocol error 保守处理，除非该值被显式 allowlist。

provider evidence 判断必须封装成 helper，actor、测试和 adapter 不得各自手写 origin 表。transport/evidence origin-only helper 与需要判断 `response.completed` / `response.failed` / `response.cancelled` / `response.incomplete` / `error` payload 的 ResponsesWS terminal evidence helper 都归属 `common/responsesws`；relay lifecycle policy 仍留在 relay actor 层，不下沉到 common projection spec。

```go
func UpstreamEventHasProviderEvidence(event responsesws.UpstreamEvent) bool
func UpstreamEventHasProviderTerminalEvidence(event responsesws.UpstreamEvent) bool
func UpstreamEventIsProxyLocalTerminal(event responsesws.UpstreamEvent) bool
```

规则固定为：`provider_frame`、`provider_stream`、`bridge_stream_opened`、`native_provider_close`、带请求级 evidence 的 `native_provider_eof`、`provider_malformed_frame` 计为 provider activity evidence；`bridge_open_provider_error` 是 provider rejected before stream 的 zero-charge proof candidate，不是 provider activity evidence；`DetailPhase=handle_provider_frame` 的 `adapter_panic` 可记录 provider activity，但不是 provider terminal；只有 `provider_frame` / `provider_stream` payload 通过 terminal classifier 时，才计为 provider terminal evidence；`synthetic_bridge`、`bridge_stream_error`、`adapter_panic`、`native_backpressure`、`native_local_abort` 不计为 provider terminal evidence。

ambiguous resolution 不能只看最后一个 close/error event，必须读取 actor 累积的 provider evidence：pending send 阶段读取 `responsesWSProviderJournal` 的 replay/projection，active 阶段读取 `common/responsesws.ProviderSettlementLogProjection`。journal/projection 由 `responsesws.ProviderObservation` 投影得到 provider activity、usage、provider close、zero-charge proof candidate 和 detail-origin diagnostics；ResponsesWS accounting 不再维护另一套近义 evidence state。

pending journal 与 active projection 属于 relay actor-owned turn slot，不属于通用 `runtime/session`。旧粗粒度 activity 判断最多保留给 connection liveness / timeout refresh，不能再用于 provider evidence 或 terminal accounting。`bridge_stream_eof`、`bridge_stream_error`、`native_provider_eof` 本身未必是 request-level provider evidence；但如果此前 journal/projection 已经看到 `provider_stream`、`provider_frame`、usage 或 provider close evidence，后续 EOF/error 走 `provider_evidence_without_terminal`。若 `SendAmbiguous` 后直接看到 EOF/error 且没有 prior provider activity，则走 `no_provider_evidence`。

journal/projection 必须保留 provider activity 的 detail origin 或等价 diagnostic reason。`bridge_stream_opened` 产生 provider activity；`bridge_open_provider_error` 产生 before-stream rejection proof candidate 和 diagnostics。二者的 accounting metric、日志和后续分支不同，不能只剩一个 boolean 后丢失原因。

journal/projection 不自行决定 scope；actor admit 一个 `response.create` attempt 时初始化 pending slot，commit pending 时把 journal 投影到 active slot。只有 actor 事件路由已确认匹配当前 attempt/generation/channel 的 event 才能 append journal 或 merge projection；terminal accounting decision 或 attempt teardown 后通过 turn slot helper 清空。旧 generation 的迟到 event 可以记录诊断，但不得污染当前 turn。sequential `response.create` 必须使用新的 pending/active slot，不能继承上一 turn 的 provider evidence。

Send/evidence ordering：actor 必须在调用 `SendClientWithResult` 前初始化 attempt slot 和 pending journal。provider evidence 可能早于 send result 到达，尤其是 `bridge_open_provider_error` 或 provider 快速返回的 native frame；actor state machine 必须对同一 attempt/generation 的 evidence-before-send-result 乱序保持正确，不能假设 send result 一定先到。

Attempt/generation routing：所有可能更新 journal/projection 的 provider/bridge event 都必须关联 active `attempt_id` 与 upstream session generation。native/bridge helper 在 send/open 阶段绑定该 token，并在 `responsesws.UpstreamEvent` / relay internal event 上回传 `AttemptID`；actor 在更新 evidence 前校验 generation、channel 和 attempt id。generation 或非空 attempt id 不匹配当前 pending/active attempt 的迟到 event 只能作为 stale diagnostics，不能更新 current turn 的 accounting evidence。legacy helper 没有 attempt id 时只能走 generation+channel 兼容路径，不能作为新 transport 的目标形态。

HTTP bridge 从 provider stream 读取到的正常 `response.*` event 与 stream usage 使用 `RecvDetailOriginProviderStream`：它们是 provider-originated evidence，但不是 native provider WS frame，也不设置 `ProviderClose`。provider HTTP rejection before stream starts 使用 `RecvDetailOriginBridgeOpenProviderError`，是 provider rejected before stream 的 zero-charge proof candidate，不是 provider activity，也不是 ResponsesWS terminal payload。`bridge_stream_error`、`bridge_stream_eof`、`synthetic_bridge` 仍是 bridge-specific/proxy-local origin。

HTTP bridge stream error 不设置 `ProviderClose`，只生成 proxy-local error event；synthetic cancelled payload 也必须标记为 `RecvDetailOriginSyntheticBridge`，不能当作 provider-originated terminal evidence。

recv pump delivery rule：no-payload lifecycle failure event 必须投递为 `ProviderRecvFailed`，不能走 `ProviderBusinessError`。适用 origin 包括 `bridge_stream_error`、`bridge_stream_eof`、`native_read_error`、`native_backpressure`、`native_local_abort`、`native_local_detach`、`native_provider_eof`、`provider_malformed_frame` 和 `adapter_panic`。原因是 `ProviderRecvFailed` 会在 pending send 阶段先缓存 failure，并等待同一 attempt 的 `SendResult` 决定 no-send rollback、floor settlement 或 provider-evidence settlement；`ProviderBusinessError` 会立即 close actor，绕过 conservative billing policy。若 event 同时携带 authoritative provider frame 或 `ClientPayloadError` payload，仍按“先 payload，再 error event”的规则处理，避免重复下发。

`provider_malformed_frame` 的 downstream client payload 由 actor 在处理 `ProviderRecvFailed` 时合成 `responses_ws_provider_protocol_error`；provider adapter 只上报 typed origin 与安全 error evidence。Trade-off：actor 多一个 origin-specific failure 分支，但 pending send accounting 仍保持在 `ProviderRecvFailed` 路径，且 OpenAI/Codex adapter 不需要承担 client-facing protocol policy。

若短期某个 producer 不能直接填充完整 `responsesws.UpstreamEvent`，则 `ResponsesWSIOBridge` 的 relay internal event 必须携带等价 typed `DetailOrigin`，actor 决策仍读取 event path，不从日志推断。

### HTTP bridge transport

HTTP bridge 是 ResponsesWS 的可选兼容模式，不属于 Codex 专属能力；但 v1 不急于把它提升为完整公共 transport。第一阶段只抽共享的 stream-to-WS loop、cancel plumbing 和 event-origin 规范，OpenAI/Codex 的 request construction 与 stream parsing 继续留在 provider adapter。等两个 provider bridge 都稳定后，再判断是否提升为更宽的公共 HTTP bridge transport。

HTTP bridge v1 的共享职责：

- 接收 `response.create`，调用 provider 的 `CreateResponsesStream` 或等价 stream entrypoint。
- 将 SSE/stream event payload 转为 ResponsesWS text frame。
- 将 stream usage 作为 event usage 交给 actor。
- `response.cancel` 关闭当前 stream，并生成 proxy-local synthetic cancelled payload。
- stream error 映射为 proxy-local error，不伪装成 provider WS close。
- 同一 downstream WS 支持 sequential `response.create`：前一个 stream terminal/EOF 后可以接受下一次 create。
- 当前 stream active 时的第二个 `response.create` 必须由 actor admission/inflight 规则拒绝为 busy / `ResponsesWSTransportSendNotAttempted`，bridge helper 不私自排队，也不并发启动第二条 stream。

HTTP bridge request construction 是独立的协议适配边界，不能把 native WS 上游 payload 直接作为 HTTP `/responses` body 转发。ResponsesWS client frame 是 event envelope，HTTP `/responses` 接收的是 create body：

- `type`、`event_id` 属于 WebSocket envelope，必须在 HTTP body 中删除。
- `stream` 由 HTTP bridge 统一接管并强制为 `true`；client 显式 `stream=false` 必须在 provider request 前返回 prepare failure，避免静默把非流式语义改成流式语义。
- `background=true` 不进入 v1 HTTP bridge；它会改变 actor 对 active turn、cancel 和 terminal evidence 的假设，必须在 provider request 前显式拒绝。`background=false` / `null` / 缺省会被删除。
- `stream_options` 属于 HTTP `/responses` create-body 的低风险流式选项，bridge 应保留原始 JSON 值并透传。Trade-off：v1 不承诺 HTTP bridge 与官方 HTTP Responses 完全等价，但对会改变用户语义的字段选择显式拒绝而不是静默吞掉。
- `model` 可由 provider open options 回填。`previous_response_id` 的 open option 只作为 HTTP bridge 第一条 `response.create` 的兼容默认值；后续 turn 省略该字段时，由 actor 将最近一次 success terminal response id 作为 per-turn default 传给 bridge。client payload 中已有 `previous_response_id` 时不覆盖 continuation。
- provider-specific rewrite 仍在 provider adapter 内完成；例如 Codex 的 `store=false`、`temperature/top_p` 互斥、unsupported field stripping 和 prompt-cache derivation。
- 其它未知 top-level create-body 字段应保留 raw JSON，避免为当前 schema 做过窄 typed marshal 后丢失未来字段或大整数精度。

HTTP bridge stream open failure 与 stream read error 分开建模：

- `CreateResponsesStream` 返回 provider HTTP error/status，例如 401/403/429/5xx：bridge 上报 provider HTTP request rejection evidence，不合成 provider ResponsesWS terminal payload，也不设置 `ProviderClose`。它可以产生 client-facing proxy-local error payload，并在安全范围内保留 provider HTTP status/code/message；但该 payload 不能被分类为 provider ResponsesWS terminal，也不能伪装成 `response.failed`、`response.cancelled` 或 provider WS close。
- `CreateResponsesStream` 在 provider status 不可用前因 proxy/local network error 失败：bridge 上报 `bridge_stream_error` / proxy-local error。
- stream 已建立后的 read error：是 stream lifecycle error；若此前已有 `provider_stream` evidence，则 ambiguous resolution 走 `provider_evidence_without_terminal`，否则走 no-provider-evidence 路径。
- 正常 stream event：使用 `provider_stream` evidence，不能与 stream open failure 或 read error 混为一类。

`CreateResponsesStream` 成功打开 provider stream 后，即使第一条 SSE/stream event 尚未到达，也计为 provider activity evidence。该 evidence 必须通过 actor event path 写入 pending journal 或 active projection；随后若 stream 立即 EOF/read error，按 `provider_evidence_without_terminal` 处理。

`bridge_open_provider_error` 的 actor accounting policy 固定为：释放 inflight/capacity；rollback pending quota reserve；RPM admission 默认不 rollback，因为客户端已经发起一次 turn admission；记录 provider HTTP rejection metric/status，但不产生 provider terminal side effect。该路径不同于 ambiguous local/network failure：provider 已明确在 stream start 前拒绝请求，proxy 不应把它按模型生成工作计费。

若 `bridge_open_provider_error` 等价于 `previous_response_not_found`，actor 必须按当前 attempt 的实际 attempted `previous_response_id` 执行 continuation-miss side effect：清理 owner 匹配的当前请求 affinity binding；若该 id 来自 HTTP bridge default injection 而不是 client payload，也必须清理 response-id derived binding，并在匹配当前 `lastFinal.ID` 时清除 bridge default，避免后续 omitted turn 继续注入同一个 stale id。不 reroute、不 replay、不清空 client 原始 payload。

`bridge_open_provider_error` payload safety：面向客户端或日志的 provider HTTP rejection payload 只能包含 HTTP status、provider error code/type 和经过 redaction 的 bounded message。不得包含 raw response body、response headers、Authorization、upstream URL、session id、request body 或无长度限制的 provider 文本；message 必须复用 provider error normalization 的截断和脱敏策略。当前 redaction 覆盖裸 `sk-...` / `sk-proj-...`、`api_key=` / `token=` / `access_token=`、JWT-like token，以及 authorization/bearer/url/session/body 等字段；暂不做通用高熵字符串全量脱敏，避免误伤普通 provider 错误文本。

`bridge_open_provider_error` downstream WS close policy：provider HTTP rejection 是单次 `response.create` 的 request-level rejection，不默认等价于 downstream WS fatal error。默认可恢复集合包括 request validation 类 4xx、429 和 5xx：actor 写 client-facing error payload，rollback 当前 pending reserve，保持 WS 可用于下一次 `response.create`。401/403 或 channel auth/config 类错误不可恢复：actor 写安全错误 payload 后关闭 downstream WS。该策略牺牲了少量实现分支复杂度，换取 sequential create 的可用性，并避免将 provider rate limit/temporary 5xx 放大为整条 downstream session fatal。

HTTP bridge cancel policy：

- active stream：关闭当前 stream，并 emit proxy-local synthetic cancelled payload。该 payload 只是 downstream ACK，不是 provider terminal，不触发 quota finalize / RPM / affinity side effect。
- control lane 必须携带发起取消时的 attempt token；迟到 cancel 若与当前 active/opening attempt 不匹配，bridge 返回 stale no-op，不得关闭下一轮 turn 的 stream。
- recv queue 背压不能阻止 active stream 关闭。若 synthetic cancelled payload 不能立即入队，bridge 必须先取消/关闭 HTTP stream，再按 synthetic cancel、`bridge_stream_eof` 的顺序延迟投递 actor event；若 session 已关闭导致事件丢失，优先接受 ACK 丢失而不是让 provider stream 继续运行。Trade-off：背压 cancel 从“客户端重试后才释放资源”改为“资源立即释放、ACK 可能延迟”，但 actor accounting 仍只由有序事件收敛。
- queue-full cancel 不得在 bridge 内部压过已经解析出的 provider terminal。若 pump 已拿到 provider terminal，terminal payload/usage/response id 必须优先进入 actor；synthetic cancel fallback 只在没有 provider terminal 赢得该 stream 裁决时补投。Trade-off：bridge 多维护一个 per-stream cancel pending 标记，换取 terminal side effect 不被本地 cancel ACK 覆盖。
- cancel 后 matching `bridge_stream_eof` / expected stream close 到达时，actor 按 `max(observed_billable_usage_quota, preconsume_floor)` 结算；若没有 billable usage，则保留 preconsume floor，清 active turn，并保持 downstream WS 可继续下一次 sequential `response.create`。本地 cancel 导致的 `context.Canceled`、closed response body、closed network connection 等 HTTP stream read error 必须由 bridge helper 归一为 expected `bridge_stream_eof`；未被本地 cancel 标记的同类错误仍按普通 stream error 处理。
- opening/setup：`response.cancel` 必须先由 actor 取消当前 setup/open worker；HTTP bridge opening stream 还必须通过 control lane 调用 opening context cancel。迟到 open result 不得被 adopt，已创建 upstream 必须 cleanup；create send result 随后按 provider evidence 保守结算，不能合成 provider terminal。
- no active stream / already terminal / already EOF：`response.cancel` 作为 `NotSent` / no-op proxy-local control event，不 emit synthetic provider terminal，不影响 accounting。
- cancel 与 provider terminal 竞争：actor exactly-once terminal policy 胜出；synthetic cancel 不能覆盖已经观测到的 provider terminal。

HTTP bridge control lane 是内部 optional capability，不改变 `responsesws.Upstream` 主 contract。Trade-off：actor 增加一条只处理 control frame 的轻量 worker，换取 slow HTTP open/header 阶段 cancel 可达；native WS 仍保持原 write serialization，不为 cancel 引入并发 upstream write 风险。

native WS 下的 cancel 也遵循同一 accounting 原则：synthetic cancel 或 proxy-local cancel acknowledgement 不得覆盖 provider terminal evidence。

ResponsesWS active attempt watchdog 由 actor 持有并按 attempt/generation 关联。provider-originated activity evidence 刷新 watchdog；synthetic cancel、bridge EOF/error、local abort 不刷新。超时后 actor 写 proxy-local timeout payload，按 `max(observed_billable_usage_quota, preconsume_floor)` 完成 quota 结算，关闭 downstream WS 并 abort upstream，不生成 provider terminal side effect，也不提交 affinity success/failure。Trade-off：超时选择关闭 session 而不是恢复 idle，避免迟到 provider terminal 污染后续 sequential turn。

HTTP bridge 的 trade-off 必须显式暴露：

- 没有真实 upstream WS connection-local state。
- 不存在 provider close/control frame。
- `response.cancel` 只能关闭 HTTP stream 或发送 synthetic cancelled event。
- bridge request 以 raw `response.create` JSON 为转发真相，保留 unknown raw fields；只拒绝当前 bridge 明确不支持的 transport 字段，例如显式 `stream=false` 和 `background=true`。
- `previous_response_id`、prompt cache 或 provider-side session cache 的证明能力弱于 native WS。

因此 v1 不提供 `auto`，只允许显式启用。Trade-off：保留 unknown raw fields 提升未来 Responses 字段兼容性，但需要在 bridge 边界维护一小组明确拒绝字段；这条 ResponsesWS raw contract 不受普通 HTTP relay 的 `AllowExtraBody` 开关控制，`AllowExtraBody` 只约束普通 HTTP 请求体额外字段透传。

## 配置与错误语义

所有 channel 的 `Other` 非空时都必须是 JSON object。旧 plain string 不再是运行时合法配置格式；provider 不得把 `Other` 当裸字符串读取。持久化入口和 DB migration 会把可解释历史值 canonicalize 成 JSON object：OpenAI/Custom plain string 保存到 `vendor_extra.legacy_other`，只保留审计信息；provider-specific 字段必须放在 JSON object 中，例如 `{"api_version":"v1"}`、`{"region":"eastasia"}` 或 `{"dashscope_plugin":"..."}`；Codex 历史 `websocket_mode=required` 别名保存为 `force`。malformed JSON-like 值不猜测，继续 fail-closed。Trade-off：兼容迁移多一层包装逻辑，但避免把旧无语义字段误当新 provider 参数，同时不污染长期运行时 fallback。

在 channel `Other` JSON 中新增：

```json
{
  "responses_ws_transport": "native",
  "responses_ws_native": true
}
```

`responses_ws_transport` 由统一 helper 解析/校验为 normalized enum；relay open path 和 model schema validation 都调用同一 helper。provider 只接收 normalized transport mode，不直接读取 raw `Other` JSON。非法值必须在进入 provider open 前返回 `invalid_responses_ws_transport`，避免 OpenAI/Codex 各自发明默认值、错误码或兼容规则。

`responses_ws_native` 是第三方 OpenAI-compatible channel 的显式 native capability 声明，只允许 boolean。官方 OpenAI、Azure 与 Codex provider 由 provider 类型承诺 native 支持；custom/OpenAI-compatible provider 不得仅凭 HTTP `Responses` endpoint 非空推断 native WS 可用。Trade-off：用户需要为确实支持 native Responses WS 的兼容服务多填一个布尔字段，但可以避免默认尝试不被上游支持的 WebSocket endpoint。

classic Azure (`ChannelTypeAzure`) 的 `Other` 是更严格的 JSON schema：

```json
{
  "api_version": "2024-10-01-preview",
  "responses_ws_transport": "native",
  "self_hosted": false,
  "responses_ws_self_hosted": true,
  "extra": {},
  "vendor_extra": {}
}
```

`api_version` 必须是非空 string；`responses_ws_transport` 可选且只允许 `native` / `http_bridge`；`self_hosted` 可选且只允许 boolean，只用于 Azure `/v1/realtime` 私有/本地上游 URL policy；`responses_ws_self_hosted` 可选且只允许 boolean，只用于 ResponsesWS native/HTTP bridge upstream URL policy；`extra` / `vendor_extra` 是 opaque namespace，运行时不解释，批量更新 `api_version` 时必须原样保留。其它未知顶层字段仍 fail-fast。旧的 plain `Other="2024-10-01-preview"` 不再作为 runtime fallback；DB migration 只把可无损识别的历史 provider `Other` 字符串一次性转换为 JSON object，无法判断的旧值保持原样并由运行时校验 fail-closed。Azure V1 不使用该 schema。Trade-off：更新代码要保留 raw JSON object，复杂度略高于 struct round-trip；收益是 strict runtime contract 不放松，同时 opaque vendor data 不会被管理操作丢失。

Azure `api_version` 管理端搜索继续使用 JSON object 语义过滤，不退回 SQL `LIKE`，因此仍是 O(N) 候选扫描。实现先只读取 `id/other` 完成语义过滤和排序保序分页，再按当页 id 回查完整 channel 数据；候选超过 1000 时只写 warn，不硬失败。Trade-off：没有引入 DB-specific JSON index 或 schema 迁移，极大数据集的过滤 CPU 仍随候选数增长；收益是 compact/pretty JSON 行为一致、legacy plain string 不被误匹配，同时显著降低无关字段物化和内存占用。

取值：

| 值 | 行为 |
| --- | --- |
| 空 / `native` | 对官方 OpenAI、Azure、Codex 或显式 `responses_ws_native=true` 的兼容 provider 打开真实上游 ResponsesWS / provider native WS |
| `http_bridge` | 显式用 HTTP Responses stream bridge 模拟 ResponsesWS，需要 provider HTTP `Responses` endpoint 可用 |

OpenAI/Codex 的显式 `http_bridge` 通过同一 `OpenResponsesWS` provider contract 接入。未实现 `ResponsesWSProvider`、custom provider 未显式声明 native capability，或不支持所选 transport 的 provider 返回 `responses_ws_unsupported_for_channel` / HTTP 426；非法值仍返回 HTTP 400。这样合法但不受当前 provider 支持的模式与无效配置不会混在一起。

最终态不再使用迁移期 legacy `RealtimeSession` path 承载 ResponsesWS。normalized `responses_ws_transport` 只进入 `ResponsesWSProvider.OpenResponsesWS(ctx, model, responsesws.OpenOptions{Transport: ...})`；没有实现 `ResponsesWSProvider` 的 provider 返回 `responses_ws_unsupported_for_channel`，即使它仍实现 `/v1/realtime` 的 `OpenRealtimeSessionWithOptions`。

actor 需要判断“是否可以把上一轮 final response id 作为 bridge default previous_response_id”时，只依赖 `responsesws.BridgeContinuationDefaultCapable` 这类小 capability interface，不识别 `*responsesws.BridgeSession` 具体类型。Trade-off：多一个可选 interface，但 provider/helper 可以包装或替换 bridge session 而不破坏 continuation 语义；同时避免把 relay actor 重新耦合到某个 transport helper 的 concrete type。

曾评估过的 temporary compatibility flag 未落地，原因是它会让 ResponsesWS 同时存在 `OpenRealtimeSessionWithOptions(...ResponsesWS...)` 与 `OpenResponsesWS` 两条 provider 入口，扩大 accounting/evidence 语义分叉。删除条件与 owner 记录在 `responses-ws-provider-contract.md`：OpenAI/Codex native 与 HTTP bridge 均通过 `OpenResponsesWS`，relay/config 测试覆盖 normalized transport 不进入 legacy realtime open path 后，legacy ResponsesWS 入口视为删除完成。

历史迁移映射仅作为背景，不再是运行路径：

| transport | RealtimeOpenOptions |
| --- | --- |
| `native` | `PreferredTransport=TransportModeResponsesWS`, `RequireWS=true` |
| `http_bridge` | `PreferredTransport=TransportModeResponsesHTTPBridge`, `RequireWS=false` |

目标 ResponsesWS bridge implementation 不通过 Codex realtime execution session manager 实现；它直接调用 Responses stream entrypoint，并只产出 actor event。

错误约定：

- 非法 transport 值返回 `invalid_responses_ws_transport`，HTTP 400。
- provider 不支持所选 transport、custom/OpenAI-compatible provider 未声明 `responses_ws_native=true` 却请求 native，或显式 bridge 缺少 HTTP Responses endpoint，返回 `responses_ws_unsupported_for_channel`，HTTP 426。
- native WS 握手已返回 HTTP status 时保留 provider status：401/403 是认证错误，429 是 provider rate limit，5xx 是 provider websocket failure。
- 只有 404/426 或 provider 明确标记 endpoint unsupported 时映射为 `responses_ws_unsupported_for_channel`。
- DNS/TLS/proxy/local dial failure 没有 provider HTTP status，应映射为 proxy-local websocket dial error，不自动 bridge fallback；具体 500/502/503 需由 provider 错误规范统一，不在 transport helper 中各自发明。

Codex 兼容规则：

- 未设置 `responses_ws_transport` 时保持当前语义：`websocket_mode=off` 表示 ResponsesWS unsupported。
- 显式 `responses_ws_transport=http_bridge` 时走 bridge；这是用户主动选择的兼容模式，不受 native `websocket_mode` 关闭影响。
- Codex ResponsesWS 仍使用 connection-local upstream session id，不使用请求 `x-session-id` 复用 live upstream。

`responses_ws.unsupported_scan_limit` 默认保持 correctness-first：`0` / 未配置时按当前加载渠道数扫描 unsupported 候选，只有明确耗尽后才返回 `responses_ws_unsupported_for_channel`。显式配置正数且低于加载渠道数时，命中上限返回 `responses_ws_unsupported_scan_limited`，不伪装成 426。未显式配置且加载渠道数超过 warning 阈值时只打一条 warning，提示管理员可以用 scan limit 换取尾延迟上限。Trade-off：默认路径在大量 unsupported 渠道下尾延迟更高，但不会因为早停错过后续 WS-capable 渠道；需要低尾延迟的部署可以显式接受 scan-limited 语义。

## 实施顺序

### Step 0：冻结 relay-facing surface

先写清 ResponsesWS actor 对 upstream 的最小依赖，避免边抽 helper 边扩大 contract：

- `SendClientWithResult(ctx, responsesws.SendRequest) responsesws.ResponsesWSTransportSendResult`
- `Recv(ctx) (event, error)`
- `Abort(reason)`
- 如旧 `RealtimeSession` path 未完全移除，则兼容 `Detach(reason)`、`SetTurnObserverFactory(factory)`、可选 preflight。

这一步的产物是接口注释和测试替身，不改业务行为。

同时写死以下 invariant，并作为后续 PR 的验收条件：

- admission/finalize 唯一 owner。
- send outcome owner。
- `common/responsesws` owns `ResponsesWSTransportSendResult` / `TransportSendCapable`。
- `responsesws.Upstream` required `SendClientWithResult` contract；relay 不走 legacy error mapper。
- HTTP bridge `ResponsesWSTransportSendResult` 分类，明确 `CreateResponsesStream` / HTTP request open 是 bridge send attempt boundary。
- `bridge_open_provider_error` enqueue rule：只有 provider rejection event 已进入 recv/event path 后才能返回 `RejectedBeforeStream`。
- bridge stream open success evidence policy：provider stream opened before first SSE event 计为 provider activity，必须在第一次 read 前进入 actor event path 并更新 pending journal 或 active projection。
- `bridge_stream_opened` delivery rule：no-payload open-success evidence 不得走会被 legacy recv pump 忽略的空 event；必须用 dedicated actor event 或 DetailOrigin-aware recv pump。
- `bridge_stream_opened` hook serialization：open-success hook 必须通过 actor event serialization 更新 actor-owned journal/projection，不能由 transport goroutine 直接修改 actor state。
- bridge stream opened update rule：stream open success 只有在 `bridge_stream_opened` evidence 已记录后才能返回 attempted；无法记录时关闭 stream，并返回带 proxy-local transport error 的 ambiguous，不能返回 `NotAttempted`。
- native provider close/eof payload origin policy：`native_provider_close` 与 `native_provider_eof` 的 payload origin 使用 `responsesws.PayloadOriginProvider`，测试不依赖任何非枚举描述。
- `ResponsesWSTransportSendResult` accounting scope：只有 `response.create` attempt 驱动 accounting，cancel/control frame 只作 delivery diagnostics。
- ambiguous close actor policy，明确 capacity/quota/RPM/affinity 的处理差异。
- transport event origin model。
- typed `UpstreamEvent.DetailOrigin` 或 relay internal equivalent。
- `DetailOrigin` zero-value fallback 与 unknown-origin fallback。
- provider evidence helper、terminal evidence helper 与 actor-owned pending journal / active projection。
- provider evidence helper 替代 accounting 中的 legacy coarse activity detection；旧 activity detection 只能用于 liveness/timeout refresh。
- pending journal / active projection scoped by active `attempt_id` and upstream generation through actor routing.
- journal/projection 保留 provider activity origin 或等价 diagnostic reason，确保 `bridge_stream_opened` 与 `bridge_open_provider_error` 可区分。
- send/evidence ordering：actor 初始化 attempt/evidence state 后再调用 send，且允许同一 attempt/generation 的 provider evidence 早于 send result 到达。
- event attempt/generation routing：所有会更新 evidence state 的 event 必须关联 active attempt/generation；stale generation event 只作诊断。
- adapter panic phase。
- adapter panic containment：helper 内部 recovery，不能依赖 relay-level panic recovery 做 sanitization。
- `ProviderFrameResult` 合法组合矩阵。
- `BridgeEventResult` 中 `bridge_open_provider_error` 不通过 `EmitFrame` 表达 client-facing payload。
- `bridge_open_provider_error` payload safety：只允许 status/code/type/bounded sanitized message，不暴露 raw body/header/auth/session/upstream URL。
- HTTP bridge stream open failure 与 cancel/no-active policy。
- HTTP bridge stream line limit / malformed payload：bridge raw stream reader 使用 `config.RealtimeWebsocketReadLimit()` 作为单行上限；超过上限或 `data:` payload 不是合法 Responses JSON 时关闭 upstream，并通过 `bridge_stream_error` / safe protocol error 进入 actor。
- legacy `http_bridge` mapping feature gate 未落地；最终态由 `OpenResponsesWS` 替代。
- native helper 不拥有 dial；v1 由 provider opener 完成 `wsconn.DialManaged`。
- `responsesws.UpstreamEvent` literal audit：新增 `DetailOrigin` / `DetailPhase` 前必须检查所有 `UpstreamEvent` literal；若存在 unkeyed literal，先改为 keyed literal。
- raw field preservation policy。
- URL/dial security policy。
- temporary compatibility path 的删除条件：不实现 flag；以 `OpenResponsesWS` 测试覆盖 native/http_bridge 均不调用 legacy realtime opener作为删除完成条件。

### Step 1：抽 native transport，只迁机械逻辑

把 OpenAI/Codex 共有的机械逻辑迁到公共 helper：

- conn 持有与关闭
- ResponsesWS 级 send sequencing
- context/closed precheck
- send result classification
- read pump
- bounded recv queue
- backpressure close
- close mapping
- abort idempotency

不迁移 terminal classifier、quota finalize、affinity、RPM、provider evidence 规则，不抽 `DialPlan`，不让 helper 解析 raw channel `Other` 或 provider URL policy。

HTTP bridge skeleton 不放进这一步，避免把“native WS 机械去重”和“协议语义降级模拟”混在同一阶段。

### Step 2：先重接 OpenAI

OpenAI ResponsesWS 语义最接近 native passthrough，先迁移风险较低：

1. 构造 ResponsesWS URL 与 headers。
2. 在 OpenAI provider opener 内完成 URL policy 校验并 dial `wsconn.ManagedConn`。
3. 返回公共 native transport + OpenAI adapter。
4. 保留 `session.created` 过滤、inline `response.create`、usage/event 解析。

OpenAI `/v1/realtime` 继续使用 `openAIRealtimeSession`，不与 ResponsesWS 状态机合并。

迁移成功的标志不是只做到能发能收，而是 ResponsesWS path 不再依赖 `openAIRealtimeSession` 的 turn state 作为账本桥：usage 只作为 evidence 上报 actor，actor 完成 quota/finalize。

### Step 3：再拆 Codex native ResponsesWS

Codex 只拆 ResponsesWS native path，不一次性重构完整 realtime execution session：

- 保留 `prepareCodexRealtimeCreatePayload`。
- 保留 `handleRealtimeSupplierMessage` / `inspectCodexRealtimeSupplierEvent`。
- 保留 `codexTurnUsageAccumulator`。
- 保留 bootstrap/control frame 过滤。
- 去掉 `codexResponsesWSUpstream` 自有 pump/queue/write/close 重复逻辑。

必须保持：每条 downstream ResponsesWS 打开独立 upstream WS；相同 `x-session-id` 不共享 live upstream。

Codex ResponsesWS 不进入 execution session manager，不写 binding；但可以复用 Codex payload 构造、event inspect、usage accumulator 等纯函数。

### Step 4：HTTP bridge 后置

HTTP bridge 不和 native helper 放在同一阶段落地。它涉及配置、typed request、stream cancel、synthetic event、stream error origin、usage event 归属等语义。

第二阶段再实现：

- OpenAI bridge 调用现有 `CreateResponsesStream`。
- Codex bridge 调用 responses stream 创建逻辑，但不进入 execution session manager。
- bridge 只产出 event，由 actor 完成 usage merge/finalize。
- v1 只支持显式 `responses_ws_transport=http_bridge`。

### Step 5：保持 `OpenResponsesWS` 为 ResponsesWS provider contract

OpenAI/Codex 通过公共 helper 稳定后，`OpenResponsesWS(ctx, model, options)` + `responsesws.Upstream` 保持为唯一 ResponsesWS provider contract。

它替代旧的 `OpenRealtimeSessionWithOptions(...ResponsesWS...)` 路线，而不是长期并存；长期并存会让 `/v1/realtime` 与 `/v1/responses` 的边界更混乱。

删除条件：

- OpenAI/Codex native ResponsesWS 均迁到 common native helper。
- relay 测试覆盖 `OpenResponsesWS` path。
- 新 provider 不再实现 `OpenRealtimeSessionWithOptions(...ResponsesWS...)`。
- `OpenRealtimeSessionWithOptions(...ResponsesWS...)` 标记 deprecated，并在后续 PR 删除。

## 测试计划

### Contract 兼容

- 公共 native helper 包装后，`SendClientWithResult`、`Recv`、`Abort` 行为与 ResponsesWS actor contract 一致；`/v1/realtime` session 兼容能力不进入 ResponsesWS contract。
- `SendClientWithResult` 返回值只表达 local send result；actor 仍结合 provider-originated evidence 判断 `Attempted / NotAttempted / RejectedBeforeStream / Ambiguous`。
- `ResponsesWSTransportSendStatus`、`ResponsesWSTransportSendResult`、`TransportSendCapable` 位于 `common/responsesws`；`runtime/session` 不声明 ResponsesWS 专用类型。
- 新 ResponsesWS upstream 必须实现 `responsesws.TransportSendCapable`；actor 直接用 `SendClientWithResult(ctx, responsesws.SendRequest)`，不再通过 plain `SendClient(ctx, frame) error` mapper 执行 ResponsesWS accounting。
- `ResponsesWSTransportSendAttempted` 与 `ResponsesWSTransportSendRejectedBeforeStream` 必须 `Err == nil` 且 `Reason == ""`；`ResponsesWSTransportSendNotAttempted` 应携带 `Err` 或明确 `Reason`；`ResponsesWSTransportSendAmbiguous` 应携带 `Err` 且 `Reason == ""`，并且必须表示 native WS 已进入 `ManagedConn.WriteMessage` 后失败，或 bridge HTTP request 已发起但 provider status 不可得。
- provider HTTP rejection before stream starts 的 send result 是 `RejectedBeforeStream` 且 `Err == nil`；provider status/code/message 必须通过 `BridgeEventResult` / `responsesws.UpstreamEvent` event path 上报。
- provider HTTP rejection before stream starts 只有在 `bridge_open_provider_error` event 成功入队后才能返回 `RejectedBeforeStream`；若入队失败，必须返回带 proxy-local transport error 的 `Ambiguous` 并关闭，不能返回 `NotAttempted`。
- `CreateResponsesStream` 成功打开 provider stream 但第一条 SSE/stream event 前立即 EOF/read error 时，actor 已有 provider activity evidence，走 `provider_evidence_without_terminal`。
- `bridge_stream_opened` 不得作为 legacy recv pump 会丢弃的空 event 发送；必须通过 dedicated actor event、open-success hook 或 DetailOrigin-aware recv pump 到达 actor。
- `bridge_stream_opened` open-success hook 必须通过 actor event serialization 更新 journal/projection；测试应证明 transport goroutine 不能直接修改 actor-owned evidence。
- stream open success 只有在 `bridge_stream_opened` evidence 已记录后才能返回 `Attempted`；若 evidence 记录失败，必须关闭 stream 并返回带 proxy-local transport error 的 `Ambiguous`，不得返回 `NotAttempted`。
- 未知 `ResponsesWSTransportSendStatus` 不能默默当作 attempted；实现应按 ambiguous 保守处理或由测试拒绝。
- `ResponsesWSTransportSendResult` accounting scope 覆盖：只有 `response.create` attempt 驱动 reserve/rollback/finalize；`response.cancel`、control frame、ping/pong 的 send result 只作 delivery diagnostics。
- HTTP bridge opening cancel 覆盖：`response.create` 阻塞在 stream open 时，`response.cancel` 通过 control lane 取消 opening context；control send result 不驱动 quota/RPM/affinity accounting，create send result 后续按 no-provider-evidence / provider-evidence policy 结算。
- HTTP bridge active cancel 覆盖：本地 cancel 后 `context.Canceled` / closed response body / closed network connection 等 read error 归一为 matching `bridge_stream_eof`，但未被本地 cancel 标记的相同 read error 仍是 `bridge_stream_error`；line-too-large 等协议错误不得被归一为 expected close。
- 已进入 `ManagedConn.WriteMessage` 后返回 error 时，send result 必须按 ambiguous 处理；除非 helper 明确证明 write 未开始，否则不能给 actor `NotSent`。
- HTTP bridge send result 覆盖：preflight/busy/closed/cancel-before-request 是 `NotAttempted`；stream opened 是 `Attempted`；provider HTTP 401/403/429/5xx before stream starts 是 `RejectedBeforeStream`；HTTP request issued 后 provider status 不可用前发生 local/proxy failure 是 `Ambiguous`。
- preflight / prepare failure 为 `NotSent`；若 actor 已 reserve，则只 rollback reserve，不做 terminal finalize。
- `ResponsesWSTransportSendAmbiguous` + upstream 关闭且没有任何 provider-originated evidence：释放 inflight/capacity、按 preconsume floor conservative settlement、默认不 rollback RPM admission、记录 proxy-local `ambiguous_close_no_provider_evidence`，不合成 provider terminal。
- `ResponsesWSTransportSendAmbiguous` + upstream 关闭且已有 provider-originated non-terminal evidence：释放 inflight/capacity，不合成 provider terminal；quota 按 `max(observed_billable_usage_quota, preconsume_floor)` 结算，记录 `upstream_lost_after_provider_evidence`。
- native WS 与 HTTP bridge 均覆盖 active turn/stream 期间第二个 `response.create`：由 actor admission/inflight 规则拒绝为 busy / `NotSent`，transport helper 不排队、不并发发送。
- 迁移期兼容 path 不新增 turn observer 依赖；新 ResponsesWS upstream 不调用 `AdmitTurn`、`RollbackTurnAdmission` 或 `FinalizeTurn`。
- 每个 attempt/response 最多一次 terminal accounting decision。
- provider evidence helper 覆盖：`provider_malformed_frame` 与 `bridge_stream_opened` 计为 provider activity 但不是 terminal；`bridge_open_provider_error` 计为 zero-charge proof candidate，不计为 provider activity；`synthetic_bridge`、`adapter_panic`、`native_backpressure` 不计为 provider terminal evidence。
- active attempt watchdog 覆盖：provider activity evidence 刷新 watchdog；synthetic cancel 不刷新；stale attempt/generation timeout 被忽略；matching active timeout finalize quota、关闭 session、abort upstream，且不生成 provider terminal side effect。
- `bridge_stream_opened + bridge_stream_eof/read_error` 且此前无 `provider_stream` payload 时，仍走 `provider_evidence_without_terminal`，不能退化为 `no_provider_evidence`。
- provider evidence helper 替换 accounting 中的 legacy coarse activity detection；legacy activity detection 保留时只能用于 liveness/timeout refresh，不能参与 provider evidence / terminal accounting。
- provider evidence helper 包归属覆盖：origin-only helper 与 ResponsesWS terminal evidence helper 位于 `common/responsesws`；relay lifecycle policy 仍留在 relay。
- actor evidence accumulation 覆盖：`provider_stream + bridge_stream_error`、`provider_frame + native_provider_eof`、`handle_provider_frame adapter_panic` 都走 journal/projection，不只看最终 close/error event。
- journal/projection diagnostic origin 覆盖：`bridge_stream_opened` 设置 provider activity；`bridge_open_provider_error` 设置 zero-charge proof candidate / diagnostics，但不更新 latest provider activity origin。
- actor evidence scope 覆盖：journal/projection 绑定当前 `attempt_id` 和 upstream generation 的 actor routing；sequential create 不继承上一 attempt evidence；旧 generation 迟到 event 不更新当前 state。
- send/evidence ordering 覆盖：`bridge_open_provider_error` 或快速 native provider frame 先于 send result 到达时，actor 仍按匹配 attempt/generation 更新 evidence state 并正确结算。
- bridge continuation miss 覆盖：`bridge_open_provider_error(previous_response_not_found)` 在 event-before-send-result 和 send-result-before-event 两种顺序下都清理 owner 匹配 stale binding；若 attempted id 来自 bridge default `lastFinal.ID`，同时清除该 default。
- event attempt/generation routing 覆盖：缺失 generation 的 evidence event 不能更新 state；generation 不匹配的迟到 provider event 只记录诊断。
- event attempt token 覆盖：带非空 attempt id 的 provider/bridge event 只能更新同一 pending/active attempt；terminal 后迟到 EOF/read error 不得污染下一次 sequential create。
- native close/eof payload origin 覆盖：`native_provider_close` 与 `native_provider_eof` 使用 `responsesws.PayloadOriginProvider`，不使用非枚举描述。
- `SendAmbiguous + immediate native_provider_eof` 且此前无 frame/usage/provider close 时，默认走 `ambiguous_close_no_provider_evidence`，不能自动升级为 `provider_evidence_without_terminal`。

### 公共 transport

- send 成功、write entered then error、context canceled before write、provider close、local backpressure close、Abort 幂等。
- close origin 可区分：native provider close、local backpressure close、bridge stream error、synthetic cancelled event。
- adapter 过滤 bootstrap 后不产生 provider-originated event。
- provider frame 附带 usage 时，relay actor 仍只在 actor 内 merge/finalize quota。
- `ProviderFrameResult` 非法组合被 helper 拒绝或转为 proxy-local protocol error，包括 filtered 携带 frame、filtered 携带 usage、err 携带 frame、proxy-local usage evidence。
- `ProviderFrameResult.CloseTransport` 不承载 local abort/backpressure/read error/bridge EOF 等非 provider-frame 原因。
- `BridgeEventResult` 与 `ProviderFrameResult` 分开测试；synthetic cancel、stream EOF、stream error 不得经由 `ProviderFrameResult` 表达。
- `bridge_open_provider_error` 的 `BridgeEventResult.EmitFrame` 必须为 nil；client-facing payload 只能通过 `ClientPayloadError` / proxy-local error event 表达，不能进入 provider terminal classifier。
- `bridge_open_provider_error` payload safety 覆盖：client/log 只包含 status、provider code/type、bounded sanitized message；raw response body/header/auth/session/upstream URL/request body 不得出现。
- binary provider data frame 默认 malformed + close；adapter 显式支持时除外。
- event origin 细分值通过 typed `UpstreamEvent.DetailOrigin` 或 relay internal equivalent 到达 actor；日志/测试替身不能作为业务判定唯一来源。
- coarse `Origin` 与 `DetailOrigin` 合法组合被验证；例如 `synthetic_bridge` 不得使用 provider coarse origin，`provider_malformed_frame` 不得触发 provider terminal side effect。
- `DetailOrigin` zero-value fallback 覆盖：legacy provider frame/usage 视为 `provider_frame`，legacy `ProviderClose` 视为 `native_provider_close`，legacy proxy-local event 视为 `proxy_local`，unknown non-empty detail origin 不视为 provider terminal evidence。
- HTTP bridge normal stream event 使用 `provider_stream` origin，不设置 `ProviderClose`。
- adapter panic phase 覆盖：prepare phase 不计 provider evidence；provider frame handling phase 不计 provider terminal，但可记录 provider activity；close mapping phase 按底层 close origin 决定是否有 provider close evidence。
- `Abort` 与 read pump close 并发、`Recv` 阻塞时 `Abort`、backpressure close 与 provider close 同时发生，不泄露 goroutine。
- adapter panic 被 recovery 为 proxy-local error + close，并记录安全摘要 metric/log hook。
- adapter panic 不得逃逸到 relay-level goroutine recover；native/bridge helper 必须在 adapter boundary 内完成 panic containment。
- adapter panic recovery 不直接记录 recovered 原值；只记录 panic class、phase、provider/channel/transport、安全错误码和 stack hash。native 与 bridge session options 都必须接收 diagnostic hook 和 provider/channel/transport metadata；OpenAI/Codex production opener 必须填充这些 metadata。完整 stacktrace 如需记录必须受 debug 开关控制，且不得包含 raw payload/header/session id。
- race 验证覆盖 `./common/responsesws ./providers/openai ./providers/codex ./relay`。

### OpenAI

- native ResponsesWS 使用 inline `response.create`。
- `session.created` 被过滤，`response.created/completed` 正常下行。
- native handshake 错误状态保持规范映射。
- 不把 ResponsesWS turn 状态和 `/v1/realtime` session state 混用。
- private IP/self-hosted policy、proxy mode、invalid upstream URL、handshake timeout 不因 helper 迁移退化。
- auth header 不进入日志。
- OpenAI provider opener 负责 dial，native helper 只接收已建立的 `ManagedConn`。

### Codex

- 每条 downstream ResponsesWS 打开独立 upstream WS。
- 相同 `x-session-id` 的两条 ResponsesWS 不共享 live upstream。
- `websocket_mode=off` 且未显式 bridge 时返回 unsupported。
- nested payload 改写、unknown field 保留、usage accumulator、bootstrap 过滤通过。
- Codex provider opener 负责 upstream dial；transport helper 不解析 Codex `websocket_mode` 或 raw channel `Other`。

### HTTP bridge

- OpenAI/Codex 显式 `http_bridge` 可把 stream event 转为 WS text frame。
- `responses_ws.bridge_open_timeout_ms` 只保护等待 HTTP stream 打开的阶段；stream opened 后不再使用 opening timeout，继续由 active turn watchdog 管理；`<=0` 表示禁用 opening watchdog，保留慢 upstream 兼容性但扩大 opening 资源占用窗口。
- sequential 两个 `response.create`、active stream 期间第二个 create 被 actor 拒绝为 busy / `NotSent`、create 后 cancel、terminal 后 cancel、stream EOF、stream error、usage delta 累加均覆盖。
- `response.cancel` 在 active stream 存在时必须关闭 stream；queue full 时延迟投递 synthetic cancel 和 EOF，保持 actor 观察顺序，不保持 active stream 可重试。synthetic event 只作为 downstream ACK，不作为 provider terminal。
- 迟到 `response.cancel` 必须按 attempt token 判 stale；queue full cancel 与 provider terminal 竞争时 provider terminal wins，至少覆盖“terminal 已进入 pump 但 synthetic cancel 尚未入队”的测试。
- cancel 后 matching `bridge_stream_eof` 负责释放 active turn；按 `max(observed_billable_usage_quota, preconsume_floor)` 结算并保持 WS open。
- stream error 作为 proxy-local error，不触发 provider terminal side effect。
- stream open failure 与 stream read error 分开：provider HTTP 429/5xx before stream starts 使用 `bridge_open_provider_error`，不设置 `ProviderClose`、不合成 terminal；local network failure before provider status 是 `bridge_stream_error` / proxy-local；已有 `provider_stream` evidence 后 read error 走 `upstream_lost_after_provider_evidence`。
- stream open success evidence 覆盖：provider stream opened 后第一条 payload 前 EOF/read error，也走 `upstream_lost_after_provider_evidence`，不能按完全无 provider evidence rollback。
- `bridge_open_provider_error` downstream 表现：可发送 client-facing proxy-local error payload 并安全保留 provider HTTP status/code/message，但不能被 terminal classifier 当作 provider ResponsesWS terminal。
- `bridge_open_provider_error` accounting：释放 inflight/capacity；rollback pending quota reserve；默认不 rollback RPM admission；只记录 provider HTTP rejection metric/status，不产生 provider terminal side effect。
- provider 不支持 stream 时返回 426 unsupported。
- bridge stream error 不设置 `ProviderClose`。
- normal stream event/usage 使用 `provider_stream` origin；stream EOF/error/synthetic cancel 使用 bridge-specific origin。
- bridge stream event 必须按 SSE event 边界聚合后再校验 JSON；multi-line `data:` 使用 SSE 规则以 `\n` 拼接，event 总大小受 websocket read limit 保护。
- provider terminal frame 后 bridge pump 必须先关闭 active stream 并短暂 drain buffered data/error line，避免后续 `[DONE]` 或 EOF 阻塞 unbuffered stream reader。
- bridge stream line 长度超过 websocket read limit 时返回安全 protocol/stream error；错误 payload 不包含 raw upstream line、headers、request body、session id 或 authorization material。
- bridge current stream handle 只作为 cancel target，不拥有 accounting inflight。
- cancel race 覆盖：provider completed 后 cancel 时 provider terminal wins；cancel 与 provider completed 竞争时 exactly-once terminal decision；no active stream / already terminal / already EOF 时 cancel 无 accounting side effect。

### 配置与 raw preservation

- 空值、`native`、`http_bridge`、非法值、provider unsupported 均覆盖。
- HTTP bridge 当前契约：OpenAI/Codex 显式 `http_bridge` 进入 bridge implementation；只有 provider/channel 不支持所选 transport 或缺少 HTTP Responses endpoint 时返回 426 unsupported；非法值仍为 400。历史未落地阶段中“合法 `http_bridge` 一律返回 426”的测试已不再代表当前行为。
- HTTP bridge URL policy 覆盖：默认 local/private/plain HTTP 被拒绝并返回 typed local error；`responses_ws_self_hosted=true` 允许 local/plain HTTP；metadata IP/hostname 即使 self-hosted 也拒绝，至少覆盖 `169.254.169.254`、`100.100.100.200`、`fd00:ec2::254` 和 `metadata.google.internal`。
- normalized transport 不进入 legacy `RealtimeOpenOptions`；native / http_bridge 均只进入 `OpenResponsesWS` 或返回 provider unsupported。目标 bridge path 不通过 Codex execution session manager。
- 新增 `UpstreamEvent.DetailOrigin` / `DetailPhase` 前审计所有 `responsesws.UpstreamEvent` literal，确保没有 unkeyed literal 遗留。
- Codex `websocket_mode=off` + 显式 bridge 组合覆盖。
- `responses_ws_transport` 只由统一 helper 解析/校验，relay open path 与 model validation 不得各自手写取值表；provider 不直接读取 raw `Other` JSON。
- OpenAI inline 与 Codex nested payload 改写都验证 unknown raw fields 不丢失；该 raw preservation 不依赖、不读取普通 HTTP relay 的 `AllowExtraBody`。

建议验证命令：

```bash
go test ./common/responsesws ./providers/openai ./providers/codex ./relay
go test -race ./common/responsesws ./providers/openai ./providers/codex ./relay
```

## 明确不做

- 不合并 `/v1/realtime` 与 `/v1/responses` 状态机。
- 不把 execution session resume/binding/revocation 带入 ResponsesWS。
- 不做 native WS 到 HTTP bridge 的自动 fallback。
- 不把 quota、RPM、affinity、terminal side effect 下沉到 provider。
- 不长期保留两套相似 Frame/Close/Event 类型而不定义归属。
- 不承诺 HTTP bridge 与官方 native ResponsesWS 完全等价。
