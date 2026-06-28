---
title: "ResponsesWS Attempt Replay 架构设计方案"
layout: doc
outline: deep
lastUpdated: true
---

# ResponsesWS Attempt Replay 架构设计方案

## 文档状态

- 状态：当前实现。
- 适用范围：HTTP `/v1/responses` pre-commit retry、ResponsesWS native / HTTP bridge request-level rejection replay、attempt snapshot、replay barrier、rollback-before-retry。
- 文档口径：本文记录已经落地的 attempt replay 协议及其安全边界；不支持的场景应继续按 barrier fail closed，而不是在 provider/transport 分支里临时补 retry。

## 目标边界

`GET /v1/responses` 的 WebSocket 入站需要支持 provider request-level rejection 场景下的安全重放。该能力不是在 WS provider error 分支里补一个 retry loop，而是把 HTTP `/v1/responses`、ResponsesWS native upstream、ResponsesWS HTTP bridge upstream 的失败结果统一建模为 **attempt replay protocol**。

核心目标：

1. HTTP 与 WS 不按 transport 类型分别决策 retry，而按同一个 attempt snapshot 判断当前 attempt 是否仍可安全重放。
2. retry 之前必须先完成 quota reserve 的 zero-charge rollback，不能绕过 settlement core 直接重放。
3. 只有在 provider 未接受、downstream 未可见、账务可 rollback、raw create 可重放时，才允许跨 channel replay。
4. provider request-level rejection 与 response-scoped terminal 必须区分。顶层 `type:"error"` 可能是请求被拒绝；`response.failed` with response id 则应视为 provider 已接受后的 terminal。
5. continuation、strict affinity、explicit pin、ambiguous transport、usage evidence 都是 replay 的硬边界。

落地范围：

- 支持 first-turn raw `response.create` 的跨 channel replay；
- 支持 native WS 顶层 request-level `type:"error"` 的 pre-accept replay；
- 支持 HTTP bridge stream start 前的 provider rejection replay；
- 支持 HTTP `/v1/responses` response 写出前的 pre-commit retry；
- 不支持 response-scoped terminal replay；
- 不支持 `previous_response_id` continuation 跨 channel replay；
- 不支持 ambiguous send / ambiguous close 的乐观 replay；
- 不做 payload mutation，只重放原始 downstream frame。

## 第一性原理

ResponsesWS attempt replay 必须同时满足七个事实：

1. **协议事实**：`response.create` 是 turn 边界；一条 WebSocket 连接可以有多个 turn，但同一时间最多一个 active create。
2. **可见性事实**：任何 client-visible frame 或 close payload 一旦写出，客户端已经观察到当前 attempt 的结果，relay 不能再隐式把同一个 attempt 换成另一个 channel 的结果。
3. **provider 事实**：provider 发来一个 frame 不等于 provider 已经接受 request。顶层 request-level `type:"error"` 可能只是拒绝；`response.created`、response id、delta、usage、response-scoped terminal 才是接受或执行证据。
4. **账本事实**：retry 必须先经过 settlement core。没有 rollback 成功，就不能重放。
5. **并发事实**：send result、provider frame、bridge open error、provider close、client close 可以乱序到达；attempt state 必须由 actor 单写者串行推进。
6. **代理事实**：one-hub 是协议透明代理。replay 的真相是原始 raw `response.create` frame；typed struct 只能服务本地决策。
7. **亲和事实**：`previous_response_id`、strict affinity、explicit pin 是 correctness 约束，不是普通 retry hint。

由此得到架构原则：

```text
retry 是 attempt replay decision 的执行结果；
transport 只产出 event，不决定 retry；
provider adapter 只解析 payload / usage / terminal shape / API error，不执行 retry side effect；
actor 是 attempt state 的唯一写入者；
settlement core 是 retry 前 rollback 的唯一账务裁决入口；
一旦越过 downstream visibility / provider acceptance / accounting 任一 barrier，就不能跨 channel replay。
```

## 架构总览

目标链路：

```text
HTTP status / WS provider frame / bridge event / send result
    -> Attempt Event Normalizer
    -> Attempt State Reducer
    -> DecideResponsesAttemptReplay(snapshot)
    -> Settlement Command
    -> Retry / Surface / Close Executor
```

分层职责：

| 层 | 职责 | 禁止事项 |
| --- | --- | --- |
| Transport Adapter | 产生 send result、recv event、origin、payload、usage | 不做 retry、不改 quota、不写客户端 |
| Provider Adapter | 解析 provider payload、usage、terminal shape、API error | 不选 channel、不执行 settlement、不决定重放 |
| Attempt Normalizer | 把 HTTP / WS / bridge 事件归一成 attempt event | 不执行副作用 |
| Attempt Reducer | 单调更新 attempt state | 不访问 WebSocket、DB、provider session |
| Replay Policy | 纯函数决策 replay / surface / close | 不写日志、不 skip channel、不 rollback quota |
| Actor / HTTP Executor | settlement、abort upstream、skip channel、reopen、write downstream | 不在没有 policy decision 时私自 retry |

## 三道 replay barrier

### Downstream visibility barrier

`DownstreamCommitted` 表示客户端已经观察到当前 attempt 的不可撤销结果。包括：

- 写出任意 provider-origin data frame；
- 写出 attempt-correlated / turn-scoped proxy-local error payload；
- 写出 terminal payload；
- 写出 keepalive / synthetic payload，只要客户端可见；
- 发送 downstream close control；
- 任何会让客户端状态机不可回到“未收到本 turn 响应”的动作。

session-level proxy-local error（例如 `session_busy`、`invalid_event`、无归属
`response.cancel` 发送失败）只属于连接/控制面，不得 watermark 当前 attempt。

规则：

```text
Once downstream committed, replay is impossible.
```

### Provider acceptance barrier

`ProviderAccepted` 表示 provider 已经接受或开始执行 request。证据包括：

- `response.created`；
- 任意 response id：`response_id` 或 `response.id`；
- content delta / output item / tool event 等非 error provider event；
- response-scoped terminal：`response.completed`、`response.done`、`response.failed`、`response.incomplete`、`response.cancelled`；
- terminal response object；
- provider usage / terminal usage；
- HTTP bridge stream opened evidence。

不算 accepted 的事件：

- stream start 前的 bridge provider HTTP rejection；
- native WS 顶层 request-level `type:"error"`，且无 response id、usage、prior acceptance evidence；
- transport not-attempted proof。

规则：

```text
Once provider accepted, cross-channel replay is impossible.
```

### Accounting barrier

`Accounting` 表示当前 attempt 是否仍可通过 settlement core 得到 zero-charge rollback。usage、provider accepted evidence、finalized settlement 都会关闭 replay。

规则：

```text
Replay execution must be preceded by settlement decision and applied rollback.
```

## Attempt 状态模型

Attempt state 使用 enum 表达，不使用多个 bool 拼状态。这样可以减少非法组合，避免 `ProviderActivitySeen / TerminalSeen / BillableUsageSeen / Replayable` 这类布尔组合污染协议。

### Snapshot

```go
type ResponsesAttemptSnapshot struct {
    AttemptID string
    OpeningID string

    Upstream   ResponsesAttemptUpstreamDisposition
    Downstream ResponsesAttemptDownstreamDisposition
    Accounting ResponsesAttemptAccountingDisposition

    Failure *types.OpenAIErrorWithStatusCode
    Origin  ResponsesAttemptFailureOrigin

    Replay   ResponsesAttemptReplayCapability
    Affinity ResponsesAttemptAffinityMode
    Turn     ResponsesAttemptTurnKind

    Continuation ResponsesContinuationAnchor
    Watermark    ResponsesDownstreamWatermark
}
```

### Upstream disposition

```go
type ResponsesAttemptUpstreamDisposition int

const (
    ResponsesAttemptUpstreamUnknown ResponsesAttemptUpstreamDisposition = iota
    ResponsesAttemptUpstreamNotAttempted
    ResponsesAttemptUpstreamRejectedBeforeAccept
    ResponsesAttemptUpstreamAccepted
    ResponsesAttemptUpstreamFailedAfterAccept
    ResponsesAttemptUpstreamAmbiguous
)
```

含义：

| 状态 | 含义 | replay 口径 |
| --- | --- | --- |
| `Unknown` | 尚无足够事件 | 默认 surface / close |
| `NotAttempted` | 可证明 create 没有进入 provider local-write 边界 | 可进入 replay 判断 |
| `RejectedBeforeAccept` | provider 明确拒绝 request，且无 accepted evidence | 可进入 rollback / replay 判断 |
| `Accepted` | provider 已接受或开始执行 | 不 replay |
| `FailedAfterAccept` | response-scoped failure | 不 replay |
| `Ambiguous` | 无法证明 provider 是否看到 request | 不 replay |

### Downstream disposition

```go
type ResponsesAttemptDownstreamDisposition int

const (
    ResponsesAttemptDownstreamUncommitted ResponsesAttemptDownstreamDisposition = iota
    ResponsesAttemptDownstreamCommitted
)
```

### Accounting disposition

```go
type ResponsesAttemptAccountingDisposition int

const (
    ResponsesAttemptAccountingNoEvidence ResponsesAttemptAccountingDisposition = iota
    ResponsesAttemptAccountingZeroChargeProofAvailable
    ResponsesAttemptAccountingAcceptanceEvidenceSeen
    ResponsesAttemptAccountingUsageSeen
    ResponsesAttemptAccountingFinalized
)
```

### Replay capability

```go
type ResponsesAttemptReplayCapability int

const (
    ResponsesAttemptReplayNone ResponsesAttemptReplayCapability = iota
    ResponsesAttemptReplayRawCreateFirstTurn
)
```

`ResponsesAttemptReplayRawCreateFirstTurn` 表示 actor 仍持有原始 first frame，可以 exact replay。executor 不修改 payload。

### Affinity mode

```go
type ResponsesAttemptAffinityMode int

const (
    ResponsesAttemptAffinityFree ResponsesAttemptAffinityMode = iota
    ResponsesAttemptAffinityPreferred
    ResponsesAttemptAffinityStrict
    ResponsesAttemptAffinityExplicitPin
)
```

`Preferred` 不是硬绑定，可以在当前 channel 失败时换 channel。`Strict` 与 `ExplicitPin` 是硬边界，不跨 channel replay。

### Turn kind

```go
type ResponsesAttemptTurnKind int

const (
    ResponsesAttemptTurnFirst ResponsesAttemptTurnKind = iota
    ResponsesAttemptTurnContinuation
)
```

### Failure origin

```go
type ResponsesAttemptFailureOrigin int

const (
    ResponsesAttemptFailureOriginUnknown ResponsesAttemptFailureOrigin = iota
    ResponsesAttemptFailureOriginHTTPStatus
    ResponsesAttemptFailureOriginWSProviderRequestError
    ResponsesAttemptFailureOriginBridgeOpenProviderError
    ResponsesAttemptFailureOriginTransportNotAttempted
    ResponsesAttemptFailureOriginTransportAmbiguous
    ResponsesAttemptFailureOriginNoFirstProviderPayload
)
```

`Origin` 是 typed enum；日志可映射为字符串。policy 不比较 source string。

### Continuation anchor

```go
type ResponsesContinuationAnchor struct {
    PreviousResponseID string
    BoundChannelID     int
    BoundConnID        string
    HasFullContext     bool
    HasToolOutput      bool
    Strict             bool
}
```

当前规则：

```text
PreviousResponseID != "" -> no cross-channel replay
```

continuation recovery 属于语义恢复，不属于 attempt replay。它需要独立设计，不能混进 provider rejection retry。

### Downstream watermark

```go
type ResponsesDownstreamWatermark struct {
    AttemptID string
    Seq       uint64
    Committed bool
    Kind      ResponsesDownstreamCommitKind
}

type ResponsesDownstreamCommitKind int

const (
    DownstreamCommitNone ResponsesDownstreamCommitKind = iota
    DownstreamCommitProviderFrame
    DownstreamCommitProxyError
    DownstreamCommitSyntheticFrame
    DownstreamCommitKeepalive
    DownstreamCommitClosePayload
)
```

watermark 用于防止 policy 做出 retry decision 后，另一路径已经写出 client-visible payload。actor 是单写者时，watermark 可以轻量实现，但语义必须存在。

## Attempt Events

建议最小事件集：

```go
type ResponsesAttemptEvent interface {
    responsesAttemptEvent()
}

type ResponsesAttemptSendNotAttempted struct {
    Err error
}

type ResponsesAttemptSendAmbiguous struct {
    Err error
}

type ResponsesAttemptNoFirstProviderPayload struct {
    Phase   ResponsesAttemptPhase
    Elapsed time.Duration
    Cause   error
}

type ResponsesAttemptProviderRejectedBeforeAccept struct {
    APIError *types.OpenAIErrorWithStatusCode
    Origin   ResponsesAttemptFailureOrigin
    Payload  []byte
}

type ResponsesAttemptProviderAccepted struct {
    ResponseID string
    Reason     string
}

type ResponsesAttemptProviderFailedAfterAccept struct {
    APIError   *types.OpenAIErrorWithStatusCode
    ResponseID string
    Payload    []byte
}

type ResponsesAttemptUsageSeen struct {
    Usage *types.Usage
}

type ResponsesAttemptDownstreamCommitted struct {
    Kind   ResponsesDownstreamCommitKind
    Reason string
}

type ResponsesAttemptAccountingFinalized struct {
    Settlement ResponsesWSAppliedSettlement
}
```

Reducer 只允许状态单调推进：

```text
Unknown -> NotAttempted
Unknown -> RejectedBeforeAccept
Unknown -> Accepted
Accepted -> FailedAfterAccept
Any -> DownstreamCommitted
Any -> UsageSeen
Any -> AccountingFinalized
Any -> Ambiguous
```

非法回退必须 fail-closed 或保守 surface，不能向 retry 方向退化。

`ResponsesAttemptNoFirstProviderPayload` 默认不 retry。WS send 已 attempted 但没有首个 provider payload 时，provider 是否已经接受 request 不可证明，因此进入 `Ambiguous`。只有 transport 能证明 create 没送达时，才应产出 `SendNotAttempted`。

## Replay Policy

### Decision 类型

```go
type ResponsesAttemptReplayDecision int

const (
    ResponsesAttemptDecisionSurface ResponsesAttemptReplayDecision = iota
    ResponsesAttemptDecisionRollbackAndRetryNextChannel
    ResponsesAttemptDecisionRollbackAndSurface
    ResponsesAttemptDecisionNoRetryAmbiguous
    ResponsesAttemptDecisionClose
)
```

说明：

- `Surface`：把错误 / frame 透给客户端，保持协议透明性。
- `RollbackAndRetryNextChannel`：先 rollback 当前 quota reserve，再切 channel 重放。
- `RollbackAndSurface`：provider 明确拒绝且可 zero-charge，但因 pin / strict / continuation / 非 retryable status 等原因不能 retry；rollback 后把错误透给客户端。
- `NoRetryAmbiguous`：不可证明 provider 是否看到 request，不能重放。
- `Close`：协议或内部状态已不安全，关闭 session。

### Command 类型

policy 可以返回 decision 与 channel failure classification，executor 据此执行 side effect：

```go
type ResponsesReplayCommand struct {
    Decision ResponsesAttemptReplayDecision
    Failure  ResponsesChannelFailure
    APIError *types.OpenAIErrorWithStatusCode
    Barrier  ResponsesReplayBlockingBarrier
}

type ResponsesChannelFailure int

const (
    ChannelFailureNone ResponsesChannelFailure = iota
    ChannelFailureRateLimited
    ChannelFailureQuotaExhausted
    ChannelFailureTransient5xx
    ChannelFailureAuthRejected
    ChannelFailureTransportNotAttempted
    ChannelFailureAmbiguous
)

type ResponsesReplayBlockingBarrier int

const (
    ReplayBarrierNone ResponsesReplayBlockingBarrier = iota
    ReplayBarrierDownstreamCommitted
    ReplayBarrierProviderAccepted
    ReplayBarrierAccounting
    ReplayBarrierContinuation
    ReplayBarrierAffinity
    ReplayBarrierReplayCapability
    ReplayBarrierAmbiguous
    ReplayBarrierNonRetryableStatus
)
```

policy 只声明决策和原因，不执行 skip / cooldown / rollback / reopen。

### 纯函数草案

```go
func DecideResponsesAttemptReplay(s ResponsesAttemptSnapshot) ResponsesReplayCommand {
    if s.Downstream == ResponsesAttemptDownstreamCommitted || s.Watermark.Committed {
        return surfaceBlocked(ReplayBarrierDownstreamCommitted)
    }

    if s.Upstream == ResponsesAttemptUpstreamAmbiguous {
        return noRetryAmbiguous(ReplayBarrierAmbiguous)
    }

    if s.Upstream == ResponsesAttemptUpstreamAccepted ||
        s.Upstream == ResponsesAttemptUpstreamFailedAfterAccept {
        return surfaceBlocked(ReplayBarrierProviderAccepted)
    }

    if s.Accounting == ResponsesAttemptAccountingUsageSeen ||
        s.Accounting == ResponsesAttemptAccountingFinalized ||
        s.Accounting == ResponsesAttemptAccountingAcceptanceEvidenceSeen {
        return surfaceBlocked(ReplayBarrierAccounting)
    }

    if s.Continuation.PreviousResponseID != "" || s.Continuation.Strict {
        if s.Upstream == ResponsesAttemptUpstreamRejectedBeforeAccept ||
            s.Upstream == ResponsesAttemptUpstreamNotAttempted {
            return rollbackSurfaceBlocked(ReplayBarrierContinuation)
        }
        return surfaceBlocked(ReplayBarrierContinuation)
    }

    if s.Affinity == ResponsesAttemptAffinityStrict ||
        s.Affinity == ResponsesAttemptAffinityExplicitPin {
        if s.Upstream == ResponsesAttemptUpstreamRejectedBeforeAccept ||
            s.Upstream == ResponsesAttemptUpstreamNotAttempted {
            return rollbackSurfaceBlocked(ReplayBarrierAffinity)
        }
        return surfaceBlocked(ReplayBarrierAffinity)
    }

    if s.Replay != ResponsesAttemptReplayRawCreateFirstTurn ||
        s.Turn != ResponsesAttemptTurnFirst {
        if s.Upstream == ResponsesAttemptUpstreamRejectedBeforeAccept ||
            s.Upstream == ResponsesAttemptUpstreamNotAttempted {
            return rollbackSurfaceBlocked(ReplayBarrierReplayCapability)
        }
        return surfaceBlocked(ReplayBarrierReplayCapability)
    }

    switch s.Upstream {
    case ResponsesAttemptUpstreamNotAttempted:
        return retryNextChannel(ChannelFailureTransportNotAttempted, s.Failure)

    case ResponsesAttemptUpstreamRejectedBeforeAccept:
        if s.Accounting != ResponsesAttemptAccountingZeroChargeProofAvailable {
            return surfaceBlocked(ReplayBarrierAccounting)
        }
        if !responsesAttemptRetryableFailure(s.Failure) {
            return rollbackSurfaceBlocked(ReplayBarrierNonRetryableStatus)
        }
        return retryNextChannel(classifyChannelFailure(s.Failure), s.Failure)
    }

    return surfaceBlocked(ReplayBarrierNone)
}
```

`responsesAttemptRetryableFailure(...)` 保持与现有 HTTP `shouldRetry(...)` 的 status 口径兼容，但不能从 gin context 或 actor field 反向读取 pin / affinity；这些约束必须已经进入 snapshot。

`classifyChannelFailure(...)` 必须先识别 provider exact quota codes（`usage_limit_reached` / `insufficient_quota` 等），再按 generic HTTP `429` 归类为 rate limited。`HandleErrorResp` 可能把 quota exhaustion normalize 成 `429`，分类顺序不能抹掉 provider 的精确信号。

建议 retryable status：

- 429；
- 5xx，但 `408` / `504` / Cloudflare `524` 按 ambiguous timeout 处理，不跨 channel replay；
- 与现有 `/v1/responses` HTTP retry 兼容的少量 provider 400；
- 其他 status 默认 rollback + surface。

`307` 是 redirect，不是 transient failure，不能进入 attempt replay retryable 集合。`401` / `403` 不跨 channel replay；是否触发 channel disable 仍由 provider API side effect 策略决定。credential rotation 后重试需要独立协议，不能混进 attempt replay。

Preferred affinity 默认不是 correctness barrier，但规则显式配置 `skip_retry_on_failure` 时，snapshot 必须携带 `SkipRetryAfterPreferredFailure`，policy 返回 `RollbackAndSurface`。这样 HTTP Responses create 与 ResponsesWS 使用同一语义，不再绕过 preferred skip-retry。

## Provider Rejection 归一规则

### native WS 顶层 `type:"error"`

当 provider frame 满足以下条件时，normalizer 产出 `ResponsesAttemptProviderRejectedBeforeAccept`：

1. payload 是 provider-origin text frame；
2. JSON 顶层 `type == "error"`；
3. `ProviderAPIErrorFromPayload(payload)` 可提取 `OpenAIErrorWithStatusCode`；
4. payload 没有 `response.id`、没有 `response_id`；
5. 当前 attempt 之前没有 `response.created` / response id / delta / terminal response object / usage；
6. 当前 attempt 尚未 downstream commit；
7. event 没有 attached usage。

该事件不进入普通 `ProviderFrameSeen` accounting activity，不调用 response terminal side effect，不触发 response-scoped terminal settlement。它可以进入 diagnostics 和 provider API side effect，但账务上应作为 `ProviderRejectedBeforeAccept` zero-charge proof。

### `response.failed` / `response.incomplete`

满足任一条件时，必须归为 `ResponsesAttemptProviderFailedAfterAccept` 或 ordinary terminal surface：

- payload 带 `response.id`；
- 当前 attempt 已见过 `response.created`；
- 当前 attempt 已有 response id；
- payload 或 event 带 usage；
- classified terminal response 携带 response object。

这类失败不跨 channel replay，即使错误码是 `usage_limit_reached` / `rate_limit_error`。

### HTTP bridge open provider error

HTTP bridge 在 stream start 前拿到 provider status/error body 时，归一为：

```text
ResponsesAttemptProviderRejectedBeforeAccept
```

要求保持 send contract：bridge 只有在 provider error event 已可靠进入 recv queue / actor event path 后，才能返回 `ResponsesWSTransportSendRejectedBeforeStream`。如果入队失败，结果必须是 ambiguous，不能 retry。

### HTTP `/v1/responses`

HTTP `/v1/responses` 在 response 写出前拿到 provider error status 时，归一为同一类 failure event。HTTP streaming 一旦写出任意 chunk，必须标记 downstream committed，后续不 retry。

接入范围只限 `/v1/responses`；不要替换全局 `shouldRetry(...)`，避免影响 chat/completions、embeddings、images 等路径。

## Downstream Commit Barrier 实现

`ResponsesWSTurnAttempt` 增加：

```go
type ResponsesWSTurnAttempt struct {
    // existing fields...

    DownstreamCommitted    bool
    DownstreamCommittedAt  time.Time
    DownstreamCommitReason string
    DownstreamCommitKind   ResponsesDownstreamCommitKind
    DownstreamCommitSeq    uint64
}
```

actor 增加唯一 downstream 出口：

```go
func (a *ResponsesWSSessionActor) emitDownstream(
    attempt *ResponsesWSTurnAttempt,
    kind ResponsesDownstreamCommitKind,
    frame responsesws.Frame,
    mode ResponsesWSWriteMode,
    reason string,
) error {
    if attempt != nil && !attempt.DownstreamCommitted {
        attempt.DownstreamCommitted = true
        attempt.DownstreamCommittedAt = time.Now()
        attempt.DownstreamCommitReason = reason
        attempt.DownstreamCommitKind = kind
        attempt.DownstreamCommitSeq = a.nextDownstreamSeq()
    }
    return a.io.bridge.WriteClientTypedFrame(frame, mode)
}
```

proxy-local / close helper：

```go
func (a *ResponsesWSSessionActor) emitProxyLocalForAttempt(
    attempt *ResponsesWSTurnAttempt,
    payload []byte,
    reason string,
) error

func (a *ResponsesWSSessionActor) markDownstreamCloseCommitted(
    attempt *ResponsesWSTurnAttempt,
    reason string,
)
```

迁移约束：ResponsesWS actor 内部不应直接调用底层 `WriteClientFrame` / `WriteClientTypedFrame` 写 turn-scoped payload。所有 client-visible 写出必须经由 helper 标记 commit。

## Provider Acceptance Barrier 实现

`ResponsesWSTurnAttempt` 增加：

```go
type ResponsesWSProviderAcceptance struct {
    Accepted        bool
    Reason          string
    ResponseID      string
    FirstAcceptedAt time.Time
}
```

或直接增加字段：

```go
ProviderAccepted       bool
ProviderAcceptedAt     time.Time
ProviderAcceptedReason string
ProviderAcceptedID     string
```

以下位置必须推进 acceptance barrier：

- `response.created`；
- `RememberProviderResponseID(...)`；
- `MarkFirstProviderResponse(...)`；
- `MarkProviderTerminalEvidence(...)`；
- provider usage merge；
- bridge stream opened evidence。

request-level rejection pre-handler 必须运行在普通 evidence merge 与 acceptance 推进之前。

## Accounting Barrier 与 Settlement

### Zero-charge proof

新增 proof kind：

```go
const (
    // existing...
    ResponsesWSZeroChargeProofProviderRejectedBeforeAccept
)
```

可选短名：

```go
ResponsesWSZeroChargeProofReplayableProviderRejection
```

不建议只叫 `ProviderRejectedBeforeCommit`。commit 容易被理解为 downstream commit，但 replay 安全还依赖 provider acceptance barrier。一个 attempt 可以尚未 downstream commit，却已经 provider accepted；这种情况下不能 retry。

### Evidence 口径

request-level rejection 是 transport 上的 provider frame，但不是 provider acceptance evidence。它不应自我污染为 generic provider activity 后压制 zero-charge proof。

实现口径：

1. provider text frame 到达；
2. 在 `updateActiveProviderEvidence(...)` / `observeAndBufferPendingProviderEvent(...)` 之前尝试归一为 `ProviderRejectedBeforeAccept`；
3. 归一成功时，不把该 frame 作为普通 `ProviderFrameSeen` merge 到 accounting activity；
4. 构造 settlement input；
5. 根据 policy 决定 rollback + retry 或 rollback + surface。

`ProviderRejectedBeforeAccept` 的 zero-charge proof 必须由 request-level rejection normalizer 构造，settlement projection 不接受外部手写的同名 proof。这样会多一个很小的 typed evidence 字段，但换来一个更干净的边界：只有已经通过 payload 形态、response id、response object、usage/terminal evidence 检查的 request-level rejection，才能触发免费 rollback。错误调用者即使传入同名 proof，也会被 settlement core 拒绝。

长期 evidence 模型可以拆成：

```text
TransportSeen       诊断事实：确实收到 provider frame
AcceptanceEvidence  账务 / replay 事实：provider 是否接受或开始执行
```

settlement core 应只消费会影响账务的 acceptance / usage / finalized evidence，不直接把所有 provider-origin transport frame 都视为 accepted activity。

### Retry 前 settlement 顺序

`RollbackAndRetryNextChannel` 执行顺序固定：

```text
build settlement input
    -> decideResponsesWSSettlement(...)
    -> apply rollback reserve
    -> process provider API side effect / classify failed channel
    -> abort current upstream
    -> replay raw response.create
```

禁止：

```text
skip channel -> replay -> 之后再 rollback
```

rollback 失败时不能 retry，只能 surface settlement failure 并关闭或 fail closed。

### Surface 场景也要 rollback

当 provider 明确 `RejectedBeforeAccept`，但因为 explicit pin、strict affinity、continuation、不可重放或非 retryable status 不能 retry 时，仍应执行 `RollbackAndSurface`。

不 retry 不等于要按 floor 计费。provider 明确拒绝且未 accept，本次 attempt 应 zero-charge。

当 replay decider 返回 `Surface` 且 barrier 是 `accounting` / `provider_accepted` / `downstream_committed` 时，executor 不能直接写 payload 后清 turn。必须先让 settlement core 对当前 attempt 和 pending/active evidence 做 floor / observed-or-floor / exact settlement；只有 settlement 成功后，才能处理 provider API error side effect、surface payload、clear turn。settlement 失败必须 fail closed，不能把原 provider payload 变成 downstream-visible 的半提交结果。

## Pending Journal 竞态

provider frame 可能先于 send result 到达。actor 会把 pending 阶段 provider event 写入 pending provider journal，并在 send result `attempted` 后 replay。

目标行为：pending journal 中的 request-level rejection 不能在 commit 时直接 replay 给客户端；必须先走 attempt replay protocol。

新增 helper：

```go
func (a *ResponsesWSSessionActor) tryReplayPendingProviderRejectionBeforeCommit(
    attempt *ResponsesWSTurnAttempt,
) bool
```

调用位置：`commitPendingAttempt(...)` 在 `CommitPendingToActive(...)` 之前。

pending `response.cancel` 是 replay barrier。若 create send pending 期间客户端已经发出 cancel，send result 后不能清掉 cancel marker 再跨 channel 重新发送 create；request rejection / not-attempted 这类 rollbackable failure 只能 rollback 后 surface，或走已有 cancel settlement/cleanup 路径。

request-level rejection 的 pending journal entry 是 replay-only zero-charge proof，不应伪装成 provider activity observation；但它仍然占用 replay buffer，append 入口必须执行与普通 pending provider replay payload 相同的事件数和字节数上限，超限 fail closed。

流程：

1. 扫描 pending journal 中 attempt-correlated provider downstream entries；
2. 若存在且仅存在 request-level rejection，并且没有 usage / acceptance / malformed / peer close evidence，构造 `ResponsesAttemptSnapshot`；
3. 调用 `DecideResponsesAttemptReplay(...)`；
4. 若 `RollbackAndRetryNextChannel`：调用 settlement core rollback，然后执行 replay；
5. 若 `RollbackAndSurface`：rollback 后写出原 provider error；
6. 若无法证明唯一 request-level rejection：继续现有 commit + replay 逻辑。

只要 journal 中已经出现 `bridge_stream_opened`、usage、response id、delta、terminal response object、provider close、malformed evidence，就禁止 retry。

## Replay Executor

把现有 `retryFirstTurnAfterNotSent(...)` 泛化为：

```go
func (a *ResponsesWSSessionActor) retryFirstTurnAfterReplayableFailure(
    previous *ResponsesWSTurnAttempt,
    command ResponsesReplayCommand,
) bool
```

`not_attempted` 与 `provider_rejected_before_accept` 共享同一执行路径：

1. settlement rollback 已成功；
2. 对当前 channel 写入 request-scope skip；
3. 根据 `ResponsesChannelFailure` 执行 cooldown / auto-disable side effect；
4. abort 当前 upstream session；
5. 清空 upstream session / generation / recv armed；
6. 保留同一个 opening id、raw first frame、admission；
7. 清空 selected channel snapshot；
8. 切回 opening；
9. 重启 first-turn open worker；
10. 重放 exact raw `response.create`。

retry executor 不重新做 RPM admission。RPM admission 属于 turn admission，已经在 previous attempt 中消耗。quota reserve 必须在 retry 前由 settlement rollback 完成。

## Payload Replay 规则

replay input 固定为：

```text
exact raw response.create frame originally accepted from downstream
```

不做：

1. 删除 `previous_response_id`；
2. 删除 `prompt_cache_key`；
3. 修改 `include`；
4. 自动补 model / instructions；
5. transcript merge；
6. compact replay 展开；
7. provider-specific payload repair。

如果后续需要 payload mutation，应新增独立层：

```go
type ResponsesReplayPayloadTransform interface {
    Name() string
    Applicable(snapshot ResponsesAttemptSnapshot) bool
    Apply(raw []byte) (next []byte, audit ResponsesPayloadTransformAudit, ok bool)
}
```

并由 policy 显式返回 transform decision。retry executor 不能顺手改 payload。

## Channel Side Effect

provider API error side effect 继续复用现有 `processProviderAPIError(...)` 口径，但必须满足 exactly-once：

- 对同一个 attempt / channel / provider error key，只执行一次 metrics / cooldown / auto-disable side effect；
- retry path 执行 side effect 后，后续 surface path 不得重复；
- pending journal replay 被 retry 吞掉时，不能再把同一个 payload replay 到 downstream 并再次 process。

建议扩展 attempt-local key：

```go
func (a *ResponsesWSTurnAttempt) MarkProviderAPIErrorProcessed(
    channelID int,
    apiErr *types.OpenAIErrorWithStatusCode,
    origin ResponsesAttemptFailureOrigin,
) bool
```

key 至少包含：

```text
channel_id + status_code + type + code + origin
```

## HTTP `/v1/responses` 接入

HTTP `/v1/responses` 在 response 写出前拿到 provider error status 时，构造 attempt snapshot：

```go
snapshot := ResponsesAttemptSnapshot{
    Upstream:   ResponsesAttemptUpstreamRejectedBeforeAccept,
    Downstream: ResponsesAttemptDownstreamUncommitted,
    Accounting: ResponsesAttemptAccountingZeroChargeProofAvailable,
    Failure:    apiErr,
    Origin:     ResponsesAttemptFailureOriginHTTPStatus,
    Replay:     ResponsesAttemptReplayRawCreateFirstTurn,
    Affinity:   responsesAttemptAffinityFromContext(c),
    Turn:       responsesAttemptTurnFromRequest(req),
    Continuation: ResponsesContinuationAnchor{
        PreviousResponseID: strings.TrimSpace(req.PreviousResponseID),
    },
}
```

HTTP executor 继续使用现有 relay attempt loop。共享的是 `DecideResponsesAttemptReplay(...)`，不是 WS actor executor。

HTTP streaming 一旦写出任意 SSE/data chunk，必须视为 downstream committed。后续 provider error 不 retry。

## WS Actor 目标接口

### Attempt 字段

在 `ResponsesWSTurnAttempt` 中增加：

```go
DownstreamCommitted    bool
DownstreamCommittedAt  time.Time
DownstreamCommitReason string
DownstreamCommitKind   ResponsesDownstreamCommitKind
DownstreamCommitSeq    uint64

ProviderAccepted       bool
ProviderAcceptedAt     time.Time
ProviderAcceptedReason string
ProviderAcceptedID     string

ReplayFailure       *types.OpenAIErrorWithStatusCode
ReplayFailureOrigin ResponsesAttemptFailureOrigin
```

字段可以后续内聚为 nested struct；落地时优先减少迁移面。

### Provider frame pre-handler

在普通 provider evidence merge 之前识别 request-level rejection：

```go
if command, handled := a.tryHandleProviderRejectedBeforeAccept(event, accounting); handled {
    a.executeResponsesAttemptCommand(command, event)
    return
}
```

helper 只在以下情况下返回 handled：

- payload 是 provider-origin text frame；
- attempt 存在；
- downstream 未 commit；
- provider 未 accepted；
- event 无 usage；
- payload 是顶层 request-level API error；
- settlement input 可构造。

### Pending journal replay 前检查

`commitPendingAttempt(...)` 调整为：

```go
func (a *ResponsesWSSessionActor) commitPendingAttempt(attempt *ResponsesWSTurnAttempt) {
    if a.tryReplayPendingProviderRejectionBeforeCommit(attempt) {
        return
    }
    // existing CommitPendingToActive + replay
}
```

该 helper 只处理 pending slot 中的 replayable rejection；不能变更已 active 的其他 attempt state。

### Downstream emit helper

所有 turn-scoped client-visible write 必须经 helper：

- provider downstream binary / text frame；
- proxy-local error payload；
- provider close payload；
- malformed provider payload；
- quota settlement failure payload；
- ambiguous close payload。

直接调用底层 write 的位置应逐步清理，并用测试或静态 grep 防止新增。

## 强不变量

1. **downstream committed 后不可 replay**

   ```text
   Once downstream committed, retry is impossible.
   ```

2. **provider accepted 后不可跨 channel replay**

   ```text
   Once provider accepted, cross-channel retry is impossible.
   ```

3. **provider request-level rejection before accept 是 zero-charge proof**

   ```text
   Provider request-level rejection before accept can rollback reserve, but only before downstream commit and without usage / acceptance evidence.
   ```

4. **ambiguous transport 不 retry**

   ```text
   Ambiguous send/open/close outcome is never replayed.
   ```

5. **continuation 默认不跨 channel replay**

   ```text
   Continuation turn is not retried across channel by default.
   ```

6. **retry 前必须 settlement**

   ```text
   Replay execution must be preceded by settlement decision and applied rollback.
   ```

7. **policy 是纯函数**

   ```text
   DecideResponsesAttemptReplay does not access gin.Context, WebSocket, provider session, DB, logger, metrics, quota, channel state.
   ```

8. **payload replay 必须 exact raw replay**

   ```text
   Retry executor must not mutate response.create payload.
   ```

9. **provider API side effect exactly-once**

   ```text
   One attempt/channel/provider-error key can trigger cooldown/disable side effect at most once.
   ```

## 落地清单

### 1. Policy core

新增：

```text
relay/responses_attempt_replay_policy.go
relay/responses_attempt_replay_policy_test.go
```

包含：

- enum；
- snapshot；
- replay command；
- blocking barrier；
- channel failure classification；
- pure decision function；
- retryable status helper。

### 2. Downstream commit helper

- attempt 增加 downstream commit 字段；
- actor 增加 `emitDownstream(...)`；
- retry 相关写路径全部经 helper；
- 已写出 `response.created` 后的 provider error 不允许 retry。

### 3. ProviderRejectedBeforeAccept proof

- 新增 zero-charge proof kind；
- bridge open rejection 与 native request-level error 使用统一 proof；
- request-level rejection 不被普通 provider frame activity 自我压制。

### 4. native WS pre-handler

- 在 ordinary provider evidence merge 前识别顶层 `type:"error"`；
- 构造 snapshot；
- policy 返回 retry 时 rollback + replay；
- policy 返回 rollback surface 时 rollback + 写出原 payload；
- 不调用 response terminal side effect。

### 5. Pending journal race

- `commitPendingAttempt(...)` 前扫描 pending journal；
- 单一 replayable rejection 执行 policy；
- 混合 evidence / usage / response id 保持现有 surface。

### 6. HTTP `/v1/responses`

- 只接 `/v1/responses` pre-commit path；
- HTTP streaming first chunk 后标记 committed；
- 不替换全局 `shouldRetry(...)`。

### 7. Write path 收敛

- 所有 turn-scoped downstream write 收敛到 emit helper；
- 将 bool 字段逐步内聚为 `ReplayState` / `ProviderAcceptance` / `DownstreamCommit` nested struct；
- 删除不再需要的 bridge-specific retry 分支。

## 测试矩阵

### Policy 单元测试

| case | upstream | downstream | accounting | status | turn | affinity | expected |
| --- | --- | --- | --- | --- | --- | --- | --- |
| not attempted first turn | `NotAttempted` | uncommitted | zero proof | n/a | first | free | retry |
| not attempted pinned | `NotAttempted` | uncommitted | zero proof | n/a | first | explicit pin | rollback surface |
| provider 429 rejection | `RejectedBeforeAccept` | uncommitted | zero proof | 429 | first | free | retry |
| provider 503 rejection | `RejectedBeforeAccept` | uncommitted | zero proof | 503 | first | free | retry |
| provider 400 bad input | `RejectedBeforeAccept` | uncommitted | zero proof | 400 | first | free | rollback surface |
| downstream committed | `RejectedBeforeAccept` | committed | zero proof | 429 | first | free | surface |
| provider accepted | `Accepted` | uncommitted | acceptance seen | 429 | first | free | surface |
| usage seen | `RejectedBeforeAccept` | uncommitted | usage seen | 429 | first | free | surface |
| continuation | `RejectedBeforeAccept` | uncommitted | zero proof | 429 | continuation | free | rollback surface |
| strict affinity | `RejectedBeforeAccept` | uncommitted | zero proof | 429 | first | strict | rollback surface |
| ambiguous | `Ambiguous` | uncommitted | no evidence | n/a | first | free | no retry ambiguous |

### WS 集成测试

1. native WS 第一个 channel 返回顶层 request-level `usage_limit_reached`，客户端不应看到该 error；第二个 channel 收到同一 raw `response.create` 并成功。
2. native WS 第一个 channel 返回顶层 request-level `rate_limit_error` / 429，行为同上。
3. native WS 顶层 request-level `type:"error"` 但 explicit pin，rollback 后 surface，不 retry。
4. bridge open provider rejection / 429，客户端不看到第一个 channel error，第二个 channel 成功。
5. pending race：provider error 先入 pending journal，send result 后返回 attempted；actor 不 replay error，转为 policy decision。
6. 已写出 `response.created` 后收到 `type:"error"`，不 retry。
7. `response.failed` with `response.id` and `usage_limit_reached`，不 retry；provider API side effect 仍 exactly-once。
8. terminal usage 存在且为 0，也不 retry。
9. ambiguous send without provider evidence，不 retry，保持 conservative settlement。
10. `previous_response_id` turn，不跨 channel retry。
11. strict affinity，不跨 channel retry。
12. rollback 失败，不 retry，写出 quota rollback / settlement failure 并关闭。
13. 第一个 channel side effect exactly-once：retry path 与 surface path 不重复 auto-disable / cooldown。
14. 第二个 channel 成功后，affinity record / terminal response id 只记录成功 attempt，不把 rollback 的 rejected attempt 记录为 continuation truth。

## 指标与日志

建议新增指标：

```text
responses_ws_attempt_replay_decision_total{decision,origin,status,reason,barrier}
responses_ws_attempt_replay_executed_total{origin,status,failure_class}
responses_ws_attempt_replay_blocked_total{barrier,origin,status}
responses_ws_provider_rejected_before_accept_total{origin,status,code}
responses_ws_downstream_commit_total{kind,reason,frame_kind}
responses_ws_provider_acceptance_total{reason}
responses_ws_no_first_payload_total{phase,outcome}
responses_ws_replay_rollback_failed_total{origin,status}
```

trace / structured log 包含：

```text
attempt_id
opening_id
turn_id
channel_id
failure_origin
api_error.status
api_error.type
api_error.code
upstream_disposition
downstream_disposition
accounting_disposition
continuation_anchor
replay_decision
blocking_barrier
settlement.decision_key
replay_target_channel_id
```

日志不应成为 policy 输入。policy 决策所需字段必须来自 typed snapshot。

## 风险与约束

### provider rejection 误判为 pre-accept

约束：只接受顶层 `type:"error"` 且无 response id、无 usage、无 prior acceptance evidence。`response.failed` 一律不重试。

### pending journal 混合 evidence 被错误 retry

约束：pending journal 只在“唯一 request-level rejection 且无其他 acceptance / usage / close / malformed evidence”时 retry。任何混合 evidence 都回到现有 replay surface。

### rollback 失败后已经 skip channel

约束：执行顺序固定为 settlement rollback 先于 retry executor。rollback 失败不得 skip/reopen/replay。

### retry 后重复执行 provider API side effect

约束：attempt-local provider API error key 去重。side effect 与 downstream surface 分离。

### 直接 write 路径漏标 commit

约束：所有 turn-scoped client write 经唯一 emit helper。测试或静态 grep 禁止新增直接写路径。

## 非目标

- 不为 response-scoped terminal failover。
- 不为 continuation miss 自动清空 client payload 中的 `previous_response_id`，也不修改 payload 后重试；provider-reported continuation miss 在 settlement 成功后仍要执行 owner-guarded stale affinity binding / `lastFinal` cleanup。
- 不对 ambiguous transport 做乐观 retry。
- 不在 retry executor 中修改 payload。
- 不把 HTTP bridge 与 native WS 的 transport contract 合并成同一个底层实现。
- 不改变全局 HTTP relay 的 `shouldRetry(...)` 语义。
- 不引入持久 event sourcing / workflow engine。
- 不让 provider adapter 持有 quota、affinity、channel skip 或 retry executor。

## 最终形态

ResponsesWS retry 收敛为 attempt-level protocol：

```text
Attempt event
    -> Attempt state
    -> Replay decision
    -> Settlement decision
    -> Retry / Surface command
```

系统长期承重墙是三道 barrier：

```text
Downstream visibility barrier
Provider acceptance barrier
Accounting barrier
```

只有三者都未越过，且 raw create 仍可重放，actor 才能执行跨 channel replay。HTTP 与 WS 的 retry 语义统一到“当前 attempt 是否还能安全重放”，而不是继续按协议形态各自演化。
