# ResponsesWS Bounded Settlement Core 与 Actor v2 架构设计

## 文档状态

- 状态：目标方案。建议作为 `docs/dev/responses-ws-architecture.md` 中计费、attempt、provider evidence、actor 状态章节的替换内容，也可以独立落盘为 `docs/dev/responses-ws-settlement-core-actor-v2.md` 并在 `docs/dev/index.md` 中引用。
- 适用范围：`GET /v1/responses` WebSocket ingress、ResponsesWS native upstream、ResponsesWS HTTP bridge upstream、turn attempt accounting、quota settlement、provider evidence、affinity side effect、actor 状态重构。
- 业务口径：ResponsesWS 不要求完全精准；允许有界小幅多计费；不能把可能进入 provider 的 turn 退成免费；不能用无界 worst-case 方式造成大偏差。
- 关键取舍：proxy 无法在所有 WebSocket / HTTP bridge / SSE 竞态中观测 provider 的真实事务边界。本方案不追求事务级精确，也不把 actor 扩展成完整工作流引擎；目标是把账务判断压缩到一个可测试、可审计、可回放的 bounded pure core：`decideResponsesWSSettlement(...)`，同时把 actor 状态整理成边界清晰的 turn domain model。

## 背景

ResponsesWS 当前实现已经把一个 turn 当作可 rollback / finalize 的账务单元处理，这是正确方向。但如果 actor 直接根据 send outcome、provider event、bridge error、close kind、pending phase 等大量状态分支做账务决策，长期会出现三个问题：

1. **复杂度膨胀**：send outcome × provider evidence × terminal × close timing 会变成一张分散在 actor handler 里的大矩阵。
2. **审计困难**：当用户或账务系统追问“这笔 floor 为什么扣”时，如果没有保留当时的输入、决策和真实扣费结果，后续只能从日志反推。
3. **安全假设漂移**：如果 settlement core 只对“投影理论上不会产生的输入”安全，一旦 projection bug 产生矛盾输入，账务核心可能向免费方向退化。

因此长期终态不是“把现有矩阵整理得更好看”，而是把真正影响钱的变量收敛为少数输入，并让核心函数对整个输入空间 total。这里的 total 不是追求极端防御代码，而是保证异常投影不会向免费方向静默退化：

```text
zeroChargeProof     是否有 0 扣费证明；这是弱证明，必须被 provider activity 压制
providerActivity    是否有 provider/generation 活动证据
observedBillable    已观测到的可计费 usage 对应 quota
terminalUsage       provider terminal 是否带权威 usage
preconsumeFloor     本地有界预扣 floor
```

所有 `DetailOrigin`、transport close detail、bridge event detail、native close kind、synthetic cancel、proxy-local error 都先投影成上面的 settlement input。它们可以继续用于诊断、日志、下游错误 payload 和 affinity guard，但不能直接绕过 settlement core 改钱。

## 目标边界

`GET /v1/responses` 是 Responses WebSocket 入站。客户端升级后首帧必须是 inline `response.create`，例如：

```json
{"type":"response.create","model":"gpt-5","input":"hi"}
```

入口只代理 ResponsesWS 语义。Codex CLI 只是验证客户端之一，不是协议边界；Codex provider 需要的 nested `response` shape 只允许出现在 provider adapter 边界。

本方案覆盖：

1. ResponsesWS turn attempt 的 quota reserve / rollback / finalize 语义。
2. native WS 与 HTTP bridge 的 transport send result / provider evidence 边界。
3. ambiguous / no-terminal / provider-evidence-without-terminal 的保守有界计费。
4. `decideResponsesWSSettlement(...)` bounded pure settlement core。
5. settlement trace 作为未来 audit/reconciliation 的预留接缝。
6. `ResponsesWSSessionActor` 从顶层平铺字段收敛为 turn slots 的结构方向。

非目标：

- 不把 `/v1/realtime` 改造成同一套 turn orchestrator。
- 不为未知私有代理方言做自动探测。
- 不在没有 replay proof 的前提下自动修复 continuation miss。
- 不承诺财务级 exactly-once ledger。
- 不承诺 proxy 能知道 provider 是否实际开始生成 token。
- 不在第一阶段引入 outbox、event sourcing、通用 workflow engine 或持久事件日志。
- 不把 quota、RPM、affinity、terminal side effect 下沉到 provider adapter 或 transport helper。
- 不让 reducer / pure core 执行 WebSocket write、close control、provider abort、数据库写入或真实 quota side effect。

## 第一性原理

ResponsesWS 入口同时面对六个事实：

1. **协议事实**：一条 WebSocket 上可以有多个 `response.create` turn，但同一时间只允许一个未 terminal 的 create。
2. **并发事实**：send result、provider terminal、provider close、客户端断开、timeout 可能乱序到达；业务状态不能跟随 goroutine 调度顺序漂移。
3. **账本事实**：RPM、quota、affinity、connection lease 是不同账本；它们的 commit/rollback 边界必须绑定到同一个 turn attempt。
4. **代理事实**：one-hub 是协议透明代理，转发真相是 raw frame；typed projection 只服务本地判断。
5. **观测事实**：`SendClient` nil 不等于 provider accepted；write error 也不等于 provider not seen；HTTP bridge 在 provider status 不可得时无法证明 provider 是否收到 request。
6. **业务事实**：ResponsesWS 不要求完全精准，但不能因为不可观测就把可能进入 provider 的 turn 退成免费；同时不能为了“绝不少扣”使用过大的 worst-case 扣费。

由此得到架构原则：

```text
每条 ResponsesWS 连接只有一个状态写入者 actor；
所有外部并发先转换为 actor event；
每个 turn attempt 是一个账务单元；
账务判断集中到 bounded pure function decideResponsesWSSettlement；
transport/provider detail 只作为 settlement input 的来源，不直接改钱；
zero-charge proof 是弱证明，必须被 terminal/provider activity/observed usage 压制；
不能证明 0 扣费时，不 rollback 成免费；
没有 terminal usage 时，用有界 floor / observed usage settlement；
未来如需审计或回放，只记录 settlement input/decision/applied result，而不是重放整个 actor。
```

架构取舍补充：**简洁不是少建类型，而是少建错误边界**。阶段三允许增加少量 struct、transition helper 和 projection helper，只要它们满足以下条件：

- 概念稳定：turn 生命周期、provider observation、settlement input、quota execution 的边界清楚。
- 所有权稳定：同一个事实只有一个写入口；缓存和 projection 可以存在，但不能成为第二套 truth。
- 控制流稳定：异常 detail 进入 diagnostics / warning / metric，不能随意新增绕过 settlement core 的账务分支。
- 偏差有界：社区项目可以接受少量 floor/observed 偏差，但不能接受重复 send、重复 merge usage 或明显免费退化。

## 核心设计：Settlement Core 是承重墙

长期终态的中心不是 reducer，也不是 actor 字段分组，而是下面这个纯函数：

```go
func decideResponsesWSSettlement(in ResponsesWSSettlementInput) ResponsesWSSettlementDecision
```

该函数必须满足：

- 不访问 WebSocket。
- 不访问 provider session。
- 不访问 gin.Context。
- 不访问数据库。
- 不调用 quota side effect。
- 不写日志。
- 不 record / clear affinity。
- 不根据 `DetailOrigin` 字符串直接分支。
- 只根据结构化 settlement input 产生 settlement decision。
- 对整个输入空间 total；即使 projection 产生矛盾输入，也只能向收费方向退化，不能向免费方向退化。

actor 的职责是：

```text
raw event / transport result / provider event
    -> update attempt-local observed state
    -> build ResponsesWSSettlementInput
    -> call decideResponsesWSSettlement
    -> execute decision as quota side effect
    -> separately decide downstream error/close/abort/affinity
```

因此，`DetailOrigin`、bridge error code、native close detail、synthetic cancel、queue-full、read pump EOF 等 detail 只能影响：

- settlement input 的构造；
- downstream 写什么 proxy-local error；
- 是否 close downstream；
- 是否 abort / detach upstream；
- diagnostic log / metric；
- affinity guard。

它们不能绕过 settlement core 直接 rollback/finalize quota。

## Settlement 输入模型

### ZeroChargeProof

`ZeroChargeProof` 表示 actor 认为当前 turn 有 0 扣费证明。它是**弱证明**：只有在没有 terminal、没有 provider activity evidence、没有 observed billable usage 时，才允许触发 rollback。

```go
type ResponsesWSZeroChargeProofKind int

const (
    ResponsesWSZeroChargeProofNone ResponsesWSZeroChargeProofKind = iota
    ResponsesWSZeroChargeProofPrepareFailed
    ResponsesWSZeroChargeProofRewriteFailed
    ResponsesWSZeroChargeProofQuotaRejected
    ResponsesWSZeroChargeProofClientClosedBeforeSend
    ResponsesWSZeroChargeProofTransportNotAttempted
    ResponsesWSZeroChargeProofProviderRejectedBeforeStream
)

type ResponsesWSZeroChargeProof struct {
    Kind   ResponsesWSZeroChargeProofKind
    Reason string
}

func (p ResponsesWSZeroChargeProof) Present() bool {
    return p.Kind != ResponsesWSZeroChargeProofNone
}
```

合法 zero-charge proof：

- actor 尚未创建 attempt 或尚未调用 `PreConsumeQuota()`。
- adapter `PrepareClientFrame` / local preflight 在任何 provider request/write 之前失败。
- send queue 在 actor 内同步拒绝，明确没有进入 upstream send worker。
- `TransportSendNotAttempted` / `SendOutcomeNotSent` 且 actor 没有任何 provider-originated/generation evidence。
- HTTP bridge 在 stream start 前收到 provider HTTP rejection，并且对应 `bridge_open_provider_error` event 已可靠进入 actor event path 或 transport recv queue。
- 本地 rewrite、quota preconsume、begin candidate、channel eligibility 等发送前失败。

以下不是 zero-charge proof：

- native write error。
- HTTP bridge 在 provider status 不可得时的 DNS/TLS/proxy/connect/read failure。
- actor 没收到 provider event。
- client close 与 send result 竞态。
- pending send 阶段 actor close。
- provider session EOF，除非此前已有明确 before-send boundary。

`ProviderRejectedBeforeStream` 应归入 `ZeroChargeProof`，不是 provider activity evidence。它表示 provider 明确在 stream/generation 前拒绝请求；如果同一个 settlement input 又包含 provider activity evidence，则输入矛盾，settlement core 必须 suppress zero-charge proof 并向收费方向退化。

Phase 2 的简化取舍：`bridge_open_provider_error` 只有在 pending before-stream 场景由 actor 显式传入时才是 `ProviderRejectedBeforeStream` proof。active attempt 收到该 event 视为 stale/protocol-conflict cleanup：按 active attempt 已有 terminal / observed / floor evidence 结算，写 provider error payload 后关闭 session，不从 observation candidate 自动补 proof。收益是不会把可能已经进入 provider 的 active turn 错退成免费；牺牲是极少数 active 异常路径可能按有界 floor 多扣一点。

### SettlementEvidence

Settlement core 的 evidence input 必须保持最小。它只包含 core 会读取的账务事实：

```go
type ResponsesWSSettlementEvidence struct {
    Terminal *ResponsesWSTerminalEvidence

    // 已观测到的可计费 usage 对应 quota。没有 usage 时为 0。
    ObservedBillableQuota int64

    // 任何说明 provider 已经开始处理/接受/产生活动的证据。
    // 这是由 projection 层从 stream opened、provider frame、usage seen、provider close after stream 等 detail 压缩而来。
    AnyProviderActivityEvidence bool
}

type ResponsesWSTerminalEvidence struct {
    Kind             ResponsesWSTerminalKind
    ResponseID       string
    HasTerminalUsage bool
    BillableQuota    int64
}
```

`ProviderStreamOpened`、`ProviderFrameSeen`、`ProviderUsageSeen`、`ProviderPeerCloseSeen`、`DetailOrigin`、transport close reason 等属于 projection diagnostics，不进入 settlement core 的 branch 条件。这样可以阻止 core 慢慢长回 16 值 detail origin 的大矩阵。

### ProjectionDiagnostics

Projection diagnostics 用于日志、metric、trace、下游错误 payload 和调试。它们不能影响 `decideResponsesWSSettlement(...)` 的分支。

```go
type ResponsesWSSettlementDiagnostics struct {
    ProviderStreamOpened   bool
    ProviderFrameSeen      bool
    ProviderUsageSeen      bool
    ProviderPeerCloseSeen  bool

    DetailOrigins   []string
    TransportStatus string
    CloseReason     string
    EventKind        string
}
```

### Preconsume floor

ResponsesWS no-terminal / ambiguous 路径需要有界 floor：

```text
floor = pre_consumed_quota
```

ResponsesWS attempt 必须强制建立 preconsume floor，不能因为用户余额很高而跳过预扣。floor 只允许由本地 prompt token 估算、小额固定 buffer / times-price 单次费用构成。禁止：

- 使用 `max_output_tokens` 作为 floor。
- 使用未知 completion 上限。
- 按连接时长放大。
- 使用 provider 最大可能账单作为 floor。

该 floor 只能保证“不把可能进入 provider 的 turn 变成免费”，不能保证在 proxy 完全没有 provider evidence 时覆盖 provider 真实账单。若未来业务要求严格不低于 provider 最终账单，必须引入 provider 账单异步对账，而不是在 actor 内假装可观测。

`ForcePreConsume()` 会让高余额 / trusted 用户也执行 ResponsesWS floor 预扣，通常会增加每个 ResponsesWS turn 的 quota truth/cache 写入。该开销是本业务口径的直接成本；实现后应增加 metric 观测 preconsume latency、quota contention 和 rollback/finalize ratio。

### SettlementInput / SettlementDecision

```go
type ResponsesWSSettlementInput struct {
    AttemptID string

    // ResponsesWS attempt 必须在进入 uncertain/no-terminal 路径前建立 floor。
    FloorQuota int64

    ZeroChargeProof ResponsesWSZeroChargeProof
    Evidence        ResponsesWSSettlementEvidence

    // Diagnostic-only. decideResponsesWSSettlement must not branch on this.
    Diagnostics ResponsesWSSettlementDiagnostics
}

type ResponsesWSSettlementAction int

const (
    ResponsesWSSettlementNoop ResponsesWSSettlementAction = iota
    ResponsesWSSettlementRollbackReserve
    ResponsesWSSettlementFinalizeExactUsage
    ResponsesWSSettlementFinalizeFloor
    ResponsesWSSettlementFinalizeObservedOrFloor
)

type ResponsesWSSettlementBasis int

const (
    ResponsesWSSettlementBasisNone ResponsesWSSettlementBasis = iota
    ResponsesWSSettlementBasisZeroChargeProof
    ResponsesWSSettlementBasisTerminalUsage
    ResponsesWSSettlementBasisFloor
    ResponsesWSSettlementBasisObservedOrFloor
)

type ResponsesWSSettlementFlag string

const (
    ResponsesWSSettlementFlagTerminalUsage             ResponsesWSSettlementFlag = "terminal_usage"
    ResponsesWSSettlementFlagProviderActivityEvidence  ResponsesWSSettlementFlag = "provider_activity_evidence"
    ResponsesWSSettlementFlagObservedBillableUsage     ResponsesWSSettlementFlag = "observed_billable_usage"
    ResponsesWSSettlementFlagNoProviderEvidence        ResponsesWSSettlementFlag = "no_provider_evidence"
    ResponsesWSSettlementFlagZeroChargeProof           ResponsesWSSettlementFlag = "zero_charge_proof"
    ResponsesWSSettlementFlagZeroChargeProofSuppressed ResponsesWSSettlementFlag = "zero_charge_proof_suppressed"
    ResponsesWSSettlementFlagContradictoryInput        ResponsesWSSettlementFlag = "contradictory_input"
    ResponsesWSSettlementFlagMissingSettlementFloor    ResponsesWSSettlementFlag = "missing_settlement_floor"
)

type ResponsesWSSettlementDecision struct {
    Action ResponsesWSSettlementAction
    Basis  ResponsesWSSettlementBasis

    // ExpectedFinalQuota 是账务 core 的唯一金额输出。
    // quota executor 必须按该值执行，不能重新用 attempt.Usage 计算另一套金额。
    ExpectedFinalQuota int64

    // Diagnostic-only. actor must not branch on Reason.
    Reason string

    // Accounting-only flags. Transport detail 不允许塞进这里。
    Flags []ResponsesWSSettlementFlag

    DecisionKey string
}
```

不要使用 `FinalQuotaFloor` / `FinalQuotaExact` 两个金额字段。settlement decision 只暴露一个 `ExpectedFinalQuota`，避免 trace 里记录一个金额、真实扣费执行另一套金额。

## Bounded settlement 决策规则

证据优先级固定为：

```text
1. provider terminal with usage -> exact usage
2. provider activity evidence or observed billable usage -> observed-or-floor / floor
3. zero-charge proof -> rollback
4. otherwise -> floor
```

解释：

- terminal with usage 是最强账务证据。它可以低于 preconsume floor，并触发退款。
- provider activity evidence / observed usage 会阻止 rollback。即使同时存在 zero-charge proof，也按矛盾输入处理，向收费方向退化。
- zero-charge proof 只有在没有 terminal、没有 provider activity evidence、observed billable quota 为 0 时才允许 rollback。
- 没有任何证据时，默认 uncertain no-provider-evidence，按 floor 结算。

`missing_settlement_floor` 只表示当前 settlement 选择了 floor / observed-or-floor fallback
路径，但可用 floor 为 0。terminal with usage 已经有 provider authoritative usage，不依赖
floor 计算最终金额，因此不打该 flag。

阶段三执行口径：`missing_settlement_floor` 在 pure core 里仍是诊断 flag，但 executor 的阶段一 fail-loud 不变量必须保留。当前代码已经规定：`ExpectedFinalQuota == 0`、非 `TerminalUsage` basis、且带 `MissingSettlementFloor` 时返回错误，不 finalize，也不伪造 applied equality。阶段三是结构还债，不应把这条账务安全边界改成 finalize-0。尤其是 provider activity 已经存在但 floor=0 的路径，不能被免费放行。

取舍：这会牺牲“单个 settlement flag 即可审计是否强制预扣”的便利性，换来 flag
语义更窄、更可操作，避免把 preconsume 执行不变量和本次结算金额风险混在一起。强制
preconsume 是否执行由 admission/preconsume 测试与指标覆盖。

参考伪代码：

```go
func decideResponsesWSSettlement(in ResponsesWSSettlementInput) ResponsesWSSettlementDecision {
    floor := in.FloorQuota
    if floor < 0 {
        floor = 0
    }

    observed := in.Evidence.ObservedBillableQuota
    if observed < 0 {
        observed = 0
    }

    flags := make([]ResponsesWSSettlementFlag, 0, 4)

    terminal := in.Evidence.Terminal
    hasTerminal := terminal != nil
    hasTerminalUsage := terminal != nil && terminal.HasTerminalUsage
    hasObservedUsage := observed > 0

    hasProviderActivity := in.Evidence.AnyProviderActivityEvidence ||
        hasObservedUsage ||
        hasTerminal

    hasZeroChargeProof := in.ZeroChargeProof.Present()

    if hasZeroChargeProof && hasProviderActivity {
        flags = append(flags,
            ResponsesWSSettlementFlagContradictoryInput,
            ResponsesWSSettlementFlagZeroChargeProofSuppressed,
        )
    }

    // Strongest evidence: provider terminal with usage.
    // This wins even if a zero-charge proof is present.
    if hasTerminalUsage {
        flags = append(flags, ResponsesWSSettlementFlagTerminalUsage)
        return newResponsesWSSettlementDecision(
            ResponsesWSSettlementFinalizeExactUsage,
            ResponsesWSSettlementBasisTerminalUsage,
            terminal.BillableQuota,
            "terminal_billable_usage",
            flags,
        )
    }

    // Any provider activity evidence prevents rollback.
    if hasProviderActivity {
        flags = append(flags, ResponsesWSSettlementFlagProviderActivityEvidence)

        if hasObservedUsage {
            flags = append(flags, ResponsesWSSettlementFlagObservedBillableUsage)
            flags = settlementFloorPathFlags(flags, floor)
            return newResponsesWSSettlementDecision(
                ResponsesWSSettlementFinalizeObservedOrFloor,
                ResponsesWSSettlementBasisObservedOrFloor,
                maxInt64(observed, floor),
                "provider_evidence_observed_or_floor",
                flags,
            )
        }

        flags = settlementFloorPathFlags(flags, floor)
        return newResponsesWSSettlementDecision(
            ResponsesWSSettlementFinalizeFloor,
            ResponsesWSSettlementBasisFloor,
            floor,
            "provider_evidence_floor",
            flags,
        )
    }

    // Only now may zero-charge proof rollback.
    if hasZeroChargeProof {
        flags = append(flags, ResponsesWSSettlementFlagZeroChargeProof)
        return newResponsesWSSettlementDecision(
            ResponsesWSSettlementRollbackReserve,
            ResponsesWSSettlementBasisZeroChargeProof,
            0,
            "zero_charge_proof",
            flags,
        )
    }

    // Default uncertain path: no proof of no-send, no provider evidence.
    flags = append(flags, ResponsesWSSettlementFlagNoProviderEvidence)
    flags = settlementFloorPathFlags(flags, floor)
    return newResponsesWSSettlementDecision(
        ResponsesWSSettlementFinalizeFloor,
        ResponsesWSSettlementBasisFloor,
        floor,
        "uncertain_no_provider_evidence_floor",
        flags,
    )
}

func settlementFloorPathFlags(flags []ResponsesWSSettlementFlag, floor int64) []ResponsesWSSettlementFlag {
    if floor == 0 {
        flags = append(flags, ResponsesWSSettlementFlagMissingSettlementFloor)
    }
    return flags
}
```

`newResponsesWSSettlementDecision(...)` 必须 canonicalize flags，构造稳定 `DecisionKey`，并保证 `ExpectedFinalQuota >= 0`。

## Projection：把复杂 detail 压缩成 core input

第一阶段不要求完成整个 `DetailOrigin` 系统的重构，但必须建立一个最小、可纯测试的 projection 边界：

```go
type ResponsesWSEvidenceProjectionInput struct {
    Terminal *ResponsesWSTerminalEvidence
    ObservedBillableQuota int64

    ProviderStreamOpened  bool
    ProviderFrameSeen     bool
    ProviderUsageSeen     bool
    ProviderPeerCloseSeen bool

    DetailOrigins   []string
    TransportStatus string
    CloseReason     string
    EventKind        string
}

func BuildResponsesWSSettlementEvidence(
    in ResponsesWSEvidenceProjectionInput,
) (ResponsesWSSettlementEvidence, ResponsesWSSettlementDiagnostics) {
    diagnostics := ResponsesWSSettlementDiagnostics{
        ProviderStreamOpened:  in.ProviderStreamOpened,
        ProviderFrameSeen:     in.ProviderFrameSeen,
        ProviderUsageSeen:     in.ProviderUsageSeen,
        ProviderPeerCloseSeen: in.ProviderPeerCloseSeen,
        DetailOrigins:         append([]string(nil), in.DetailOrigins...),
        TransportStatus:       in.TransportStatus,
        CloseReason:           in.CloseReason,
        EventKind:             in.EventKind,
    }

    observed := in.ObservedBillableQuota
    if observed < 0 {
        observed = 0
    }

    evidence := ResponsesWSSettlementEvidence{
        Terminal:              in.Terminal,
        ObservedBillableQuota: observed,
        AnyProviderActivityEvidence: in.ProviderStreamOpened ||
            in.ProviderFrameSeen ||
            in.ProviderUsageSeen ||
            in.ProviderPeerCloseSeen ||
            observed > 0 ||
            in.Terminal != nil,
    }

    return evidence, diagnostics
}
```

Projection 的测试必须断言：

- `ProviderRejectedBeforeStream` 不会被投影为 provider activity evidence。
- stream opened / provider frame / usage seen / peer close seen 会投影为 provider activity evidence。
- terminal without usage 也会阻止 rollback。
- diagnostics 的 detail 字段变化不改变 core decision，除非它们改变了 core evidence 字段。

## Settlement executor 与 trace

### Decision 是唯一金额来源

`ResponsesWSSettlementDecision.ExpectedFinalQuota` 是 quota executor 的唯一金额来源。executor 不能在执行时重新用 `attempt.Usage` 算出另一套 final quota。

禁止：

```text
trace.Decision.ExpectedFinalQuota = X
quota executor 实际按 attempt.Usage 扣 Y
```

如果底层 quota API 还不能按 fixed final quota 执行，Phase 1 必须新增等价能力。不能 fallback 到 `attempt.Usage` 后仍声称 trace 可审计。

建议执行接口：

```go
type ResponsesWSAppliedSettlement struct {
    AttemptID string

    Action ResponsesWSSettlementAction
    Basis  ResponsesWSSettlementBasis

    ExpectedFinalQuota int64
    AppliedFinalQuota  int64

    SettlementIdentity string
}

func (a *ResponsesWSTurnAttempt) ApplyResponsesWSSettlementDecision(
    c *gin.Context,
    decision ResponsesWSSettlementDecision,
) (ResponsesWSAppliedSettlement, error)
```

`SettlementIdentity` 是 quota ledger 的幂等 identity，用于 fixed-final 重复执行保护；它不是 provider response id、ResponsesWS turn id，也不参与 terminal / affinity 业务语义。trace 记录它只用于解释 executor 幂等结果。

执行规则：

- `RollbackReserve` 调用 rollback reserve，`AppliedFinalQuota = 0`。
- `FinalizeExactUsage` / `FinalizeFloor` / `FinalizeObservedOrFloor` 都必须按 `decision.ExpectedFinalQuota` 执行 fixed final quota settlement。
- fixed-final envelope 的金额必须由 `ExpectedFinalQuota` 覆盖；`UsageSummary` 只保留审计/展示明细，不参与金额计算。`FinalizeExactUsage` 传 terminal usage，`FinalizeObservedOrFloor` 只有在 observed usage 可计费时传 observed usage，纯 `FinalizeFloor` 不传 usage。
- 执行后必须校验 `AppliedFinalQuota == ExpectedFinalQuota`。
- executor 必须对真实执行前置条件 fail loud，例如 nil quota、空 model、identity mismatch、重复 finalize 金额不一致；这些是执行安全问题。
- `missing_settlement_floor + ExpectedFinalQuota == 0 + 非 TerminalUsage` 也是执行安全错误，必须保持阶段一现状：返回错误，不 finalize，不伪造 applied equality。若未来要放开合法 floor=0，只能作为独立 executor 补丁处理，并且必须至少区分 `no_provider_evidence` 与 `provider_activity_evidence`；后者仍应 fail-closed。
- 重复 finalize 只允许在已保存的 applied settlement 与当前 decision 完全一致时幂等返回；金额或 action/basis 不一致必须报错。
- 不允许在 no-terminal 路径中调用 `ConsumeWithIdentity(attempt.Usage)` 造成 usage < floor 时低于 floor。

### Trace 记录 decision 与 applied

Trace 不是“决策意图”记录，而是“决策与真实扣费”记录。

```go
type ResponsesWSSettlementTrace struct {
    AttemptID string
    OpeningID string
    ChannelID int

    Input    ResponsesWSSettlementInput
    Decision ResponsesWSSettlementDecision
    Applied  ResponsesWSAppliedSettlement

    CreatedAt time.Time
}
```

验收规则：

```text
decide(trace.Input).DecisionKey == trace.Decision.DecisionKey
trace.Decision.ExpectedFinalQuota == trace.Applied.AppliedFinalQuota
```

第一阶段不要求持久化 trace；可以进入 debug log、metric 或 test hook。但 trace 必须保留未来持久化的结构接缝。

## Actor 执行边界

actor 仍然是 ResponsesWS 连接的唯一状态写入者。settlement core 不替代 actor。

actor 负责：

- client frame read/write。
- upstream open / send / recv。
- provider adapter 调用。
- turn attempt 生命周期。
- quota side effect 执行。
- RPM / connection lease cleanup。
- downstream proxy-local error payload。
- upstream abort / detach。
- affinity record / clear。
- log / metric / trace hook。

actor 不允许：

- 在 handler 分支中直接调用 rollback/finalize，绕过 settlement core。
- 根据 `decision.Reason` 控制流程。
- 把 bridge/native/client-cancel 细节塞进 settlement flags。
- 在 ambiguous 后 retry 另一个 channel。
- 把 `DetailOrigin` 字符串作为账务分支条件。

### Reason 与 Flags

`Reason` 是 diagnostic-only 字段，不能参与控制流。actor 只能根据 typed `Action` / `Basis` / accounting-only `Flags` 执行账务 side effect。

`Flags` 只能表达账务事实，例如：

```text
terminal usage
provider activity evidence
observed billable usage
no provider evidence
zero charge proof
zero charge proof suppressed
contradictory input
missing settlement floor
```

`Flags` 不能表达 transport detail，例如：

```text
bridge_local_open_error
native_immediate_eof
ambiguous_write_failed
client_cancel_during_pending_send
```

下游 payload、close reason、native/bridge cleanup 由 actor 根据原始 transport context 决定。例如：

```text
ambiguous + no evidence:
    settlement core -> FinalizeFloor + no_provider_evidence
    actor -> 写 ambiguous_close_no_provider_evidence，然后 close

bridge local open error:
    settlement core -> FinalizeFloor + no_provider_evidence
    actor -> 保留精确 ws_request_failed payload，然后按 recoverable/non-recoverable 决定 close
```

这两条在账务 core 看来相同，但 downstream 表现不同。不要为了区分下游 payload 污染 settlement flags。

## 行为矩阵

| 场景 | ZeroChargeProof | Provider activity / observed / terminal | Settlement decision | Downstream / side effect |
|---|---:|---:|---|---|
| prepare / rewrite / preflight 失败 | yes | no | rollback | 写 local error；不 retry |
| quota preconsume 失败 | yes / no attempt | no | rollback/noop | 写 quota error |
| client closed before send worker 接收 | yes | no | rollback | close cleanup |
| `TransportNotAttempted` + no evidence | yes | no | rollback | 可按策略选择同 channel retry；不能 cross-channel ambiguous retry |
| provider rejected before stream | yes | no | rollback | 写 provider HTTP rejection；不生成 provider terminal |
| provider rejected before stream + provider activity evidence | yes | yes | floor / observed-or-floor / exact；proof suppressed | 记录 warning/metric；仅协议身份冲突才关闭当前 turn/session |
| terminal with usage | any | yes | exact usage | terminal side effect；success 才 record affinity |
| terminal without usage | any | yes | floor | terminal side effect；success/failure 按 provider terminal |
| provider evidence without terminal | no/contradictory | yes | observed-or-floor 或 floor | 不合成 provider terminal；不 record success affinity |
| ambiguous + provider evidence | no | yes | terminal exact 或 observed-or-floor/floor | 不 retry；等待或 close cleanup |
| ambiguous + no provider evidence | no | no | floor | 写 `ambiguous_close_no_provider_evidence`；close；不 retry |
| pending_send unknown close | no | no | floor | close cleanup；不 rollback |
| bridge local open error | no | no | floor | 保留精确 `ws_request_failed` payload |
| native EOF without evidence after ambiguous | no | no | floor | close cleanup；不 retry |
| proof conflict：NotSent + provider evidence | yes | yes | provider evidence 压制 rollback | 记录 warning/metric；不 retry |

## Retry policy

ResponsesWS actor 不允许在 ambiguous 后 retry。原因：

```text
ambiguous 表示 provider 可能已经看见请求；
如果 retry 另一个 channel，两个候选都可能被 provider 处理；
在 conservative floor policy 下，两个候选都可能产生 floor settlement；
这会把“允许小幅多计费”变成不可控重复计费。
```

允许 retry 的唯一前提是 `TransportNotAttempted` / no-send proof 且没有任何 provider evidence。即使允许 retry，也必须保证：

- 不跨 channel 污染 affinity。
- 不复用可能已经发送过的 provider attempt。
- 原 attempt rollback 后才能新建 attempt。

第一阶段不新增 retry 行为。当前已有 retry 行为必须通过测试证明不覆盖 ambiguous。

## Actor v2 数据结构方向

Settlement core 修正账务语义后，actor 仍需要还结构债。目标不是把 actor 拆成多个并发写入者，也不是为了少写代码继续保留平铺字段，而是把状态归属变清楚：turn lifecycle、provider observation、pending replay、active settlement fact、worker/close/watchdog lifecycle 分别有明确边界。

这版方向按阶段一/二后的代码重新校准：`common/responsesws.ProviderSettlementLogProjection` 已经存在，它就是 provider observation log 面向 settlement 的投影结果。阶段三不再新增 `ProviderEvidenceState` 之类的近义类型；如果 active 需要累积后续 provider event，应给既有 projection 增补 helper，而不是制造第三套 evidence/state 名称。

优雅结构的判断标准：

```text
一个事实只有一个写入口；
一个 lifecycle 只由一个 slot 拥有；
pending -> active 是显式 commit，不是字段复制；
replay payload 与 settlement observation 同源；
active 只保存既有 ProviderSettlementLogProjection，不保存可重复 append 的原始日志；
side effect 仍由 actor handler 执行，state helper 只做状态转换和 input 构造。
```

推荐落地顺序是先收敛 provider source，再迁移大块 turn slots。这样 pending 不再带独立 evidence 字段，active 不再带 observation log，搬家时要迁移的状态更少，也更不容易把旧双写模型搬进新结构。

目标形状：

```go
type ResponsesWSSessionActor struct {
    events chan ResponsesWSEvent
    done   chan struct{}

    io       responsesWSIOState
    snapshot responsesWSSnapshotState
    lease    responsesWSLeaseState
    upstream responsesWSUpstreamState

    turns   responsesWSTurnSlots
    workers responsesWSWorkerState
    closing responsesWSCloseState
    watchdog responsesWSWatchdogState
}
```

这些分组不是为了隐藏字段，而是为了让 review 时能快速判断：某段代码正在修改 IO、upstream、turn lifecycle、worker lifecycle 还是 close lifecycle。actor 仍然是唯一写入者；这些 struct 不引入新的 goroutine 所有权。

### Turn slots

```go
type responsesWSTurnSlots struct {
    opening *responsesWSOpeningTurn
    pending *responsesWSPendingTurn
    active  *responsesWSActiveTurn

    history responsesWSTurnHistory
}

type responsesWSTurnHistory struct {
    lastFinal                  *types.OpenAIResponsesResponses
    recentFinalizedResponseIDs []string
}
```

`opening` / `pending` / `active` 是互斥阶段，而不是三组可随意组合的字段。阶段三应引入小的 transition helper 来表达状态转换，例如：

```go
func (s *responsesWSTurnSlots) BeginOpening(opening responsesWSOpeningTurn) error
func (s *responsesWSTurnSlots) AttachPending(pending responsesWSPendingTurn) error
func (s *responsesWSTurnSlots) CommitPendingToActive(channelID int) (responsesWSActiveTurn, []responsesWSProviderJournalEntry, error)
func (s *responsesWSTurnSlots) FinishActive(result responsesWSTurnFinalization) error
func (s *responsesWSTurnSlots) ClearPending(reason string) responsesWSPendingCleanup
```

这些 helper 不做 WebSocket write、provider abort、quota side effect 或 DB 写入；它们只维护 actor 内存状态，并返回 actor handler 需要执行的 cleanup/replay 描述。

### Opening / pending / active

```go
type responsesWSOpeningTurn struct {
    openingID string
    firstFrame *responsesws.RawResponsesCreateFrame
    startedAt time.Time
    admission *ResponsesWSTurnAdmission
}

type responsesWSPendingTurn struct {
    phase     responsesWSPendingTurnPhase
    openingID string

    attempt *ResponsesWSTurnAttempt

    provider responsesWSPendingProviderState
    cancel   responsesWSPendingCancelState
}

type responsesWSActiveTurn struct {
    attempt  *ResponsesWSTurnAttempt
    evidence responsesws.ProviderSettlementLogProjection
    affinity *ResponsesTurnAffinity
    channelID int

    bridgeCancelPendingAttemptID string
}
```

`opening` 只表达首帧、admission 和准备中的 turn。`pending` 表达“已经有 attempt / send 或 provider opening 正在发生，但尚未 commit 为 active”。`active` 表达“actor 接下来按这个 attempt 接收 provider event、构造 settlement input 并最终结算”。

### Pending provider journal：一个来源，两种视图

pending provider state 不应同时维护 replay buffer 与另一份 evidence log。更优雅的做法是引入一个有序 journal：每条 provider 观测都只 append 一次；同一条 entry 可同时携带 downstream replay payload、failure replay payload 和 settlement observation。

journal 是 relay 本地类型，因为它携带 relay event replay payload；common 层使用已经存在的 `ProviderObservation` 与 `ProviderSettlementLogProjection`，避免 common 反向依赖 relay。

```go
type responsesWSPendingProviderState struct {
    journal responsesWSProviderJournal
    bytes   int

    bridgeOpenLocalErrorAttemptID string
    bridgeOpenProviderErr         *ResponsesWSEventBridgeOpenProviderError
}
```

示意类型：

```go
type responsesWSProviderJournal struct {
    entries []responsesWSProviderJournalEntry
    bytes   int
}

type responsesWSProviderJournalEntry struct {
    Observation responsesws.ProviderObservation

    Downstream *ResponsesWSEventProviderDownstream
    Failure    *ResponsesWSEventProviderRecvFailed
}
```

入口应收敛为少数方法：

```go
func (j *responsesWSProviderJournal) AppendDownstream(event ResponsesWSEventProviderDownstream, upstream responsesws.UpstreamEvent, maxBytes int) (buffered bool, overLimit bool)
func (j *responsesWSProviderJournal) AppendFailure(event ResponsesWSEventProviderRecvFailed, upstream responsesws.UpstreamEvent)
func (j *responsesWSProviderJournal) AppendLifecycle(upstream responsesws.UpstreamEvent)
func (j *responsesWSProviderJournal) Project() responsesws.ProviderSettlementLogProjection
func (j *responsesWSProviderJournal) Replay() []responsesWSProviderJournalEntry
```

`usage-only`、`stream-opened`、bridge open error 这类没有 downstream replay payload 的观测，也必须通过 `AppendLifecycle` 进入同一个 journal。这样会比“只留两个 slice”多一点代码，但边界更清晰：replay 集合、settlement evidence、diagnostics 都来自同一条时间线。

pending entries 有上限，`Project()` 默认直接从 entries 重算，避免把 `projection + dirty` cache 变成第二套 truth。如果未来确需 cache，必须完全封装在 journal 内部，并有从 entries 重建对齐的测试。

容量超限时必须保持当前顺序语义：先把 observation 计入 journal，再判定 downstream replay payload 是否超过容量并返回 `overLimit` 让 actor `failClosed`。否则 buffer 满触发 settlement 时，最后一条 provider activity 可能没有进入 projection。`bytes` 应归 journal 私有管理，避免容量状态与 entries 分离。

request-level rejection 是特殊 replay-only entry：它可以没有 settlement observation，因为 typed `ProviderRejectedBeforeAccept` proof 不等价于 provider activity；但 append 仍必须使用 journal 的事件数/字节数容量检查。超限时丢弃 replay payload 并 fail closed，不能绕过 cap，也不能为了保留 projection 把 request rejection 伪造成 provider frame evidence。

### Active evidence：保存既有 projection，不保存 observation log

active turn 不需要保存完整 observation log。pending commit 时将 journal 投影成 `ProviderSettlementLogProjection`，active 后续 provider event 直接 merge 到这个 projection。不要新增 `ProviderEvidenceState`；阶段二已经提供了等价且更准确的类型名。

```go
type ProviderSettlementLogProjection struct {
    Activity                  ProviderActivityFact
    Diagnostics               []DiagnosticDetail
    DetailOrigins             []RecvDetailOrigin
    ZeroChargeProofCandidates []ZeroChargeProofCandidate
}
```

如果需要便于 active 增量更新，可以给现有 projection 增补 helper：

```go
func (p *ProviderSettlementLogProjection) Observe(obs ProviderObservation)
func (p *ProviderSettlementLogProjection) Merge(other ProviderSettlementLogProjection)
func (p ProviderSettlementLogProjection) HasActivity() bool
func (p ProviderSettlementLogProjection) LastActivityOrigin() RecvDetailOrigin // must inspect activity, not just last DetailOrigin
func (p ProviderSettlementLogProjection) FirstZeroChargeProofCandidate() ZeroChargeProofCandidate
```

`ZeroChargeProofCandidates` 保持复数，settlement input builder 从候选列表选择第一个可用 proof candidate。不要为了适配 active path 把它改成单数字段。

`LastActivityOrigin()` 不得简单返回 `DetailOrigins` 的最后一个元素。`DetailOrigins` 是全量非空 origin，可能包含只用于 diagnostics、不能构成 activity 的 observation。helper 必须从 `Diagnostics` 末尾向前遍历，并按 `ProjectProviderObservationForSettlement` 的 activity 规则重判，或在 projection 内显式维护 `ActivityOrigins`。

active 保存 projection 的原因：

- settlement core 只需要 fact，不需要重放 provider 历史；
- 避免 pending commit 后 replay buffered event 又二次 append 到 active log；
- 后续新增 provider detail 时，只改 observation -> projection，不改账务 core；
- review active settlement path 时只看 `active.evidence -> settlement input -> decision -> applied`。

active projection 不是为了实现“财务级证明系统”。它只是 actor 内的 read model。contradictory input 默认进入 flag / warning / metric，并按收费方向有界退化；只有 attempt id、channel id、session generation、provider response id 这类身份不一致，才应关闭当前 turn 或 session。

### Settlement input builder 与 projection 边界

阶段三后，actor 主路径不应再把 `ProviderObservationLog` 传给 active settlement builder。当前 `ResponsesWSProviderEvidenceProjectionInput` 与 `ResponsesWSSettlementProjectionInput` 都带 `ObservationLog`；这两个 input 都应接收已经投影好的 provider fact：

```go
type ResponsesWSProviderEvidenceProjectionInput struct {
    Provider              responsesws.ProviderSettlementLogProjection
    Terminal              *ResponsesWSTerminalEvidence
    ObservedBillableQuota int64
    TransportStatus       string
    CloseReason           string
    EventKind             string
}
```

pending path 使用 `pending.provider.journal.Project()`；active path 使用 `active.evidence`。为了兼容已有测试或旧 helper，可以短期保留从 `ProviderObservationLog` 构造 projection 的 wrapper，但 actor 主路径不再依赖它。

### Worker / watchdog / close state

worker、watchdog、closing state 可以分组。这里的收益不是减少字段数量，而是减少 cleanup 的遗漏。

```go
type responsesWSWorkerState struct {
    runWG           sync.WaitGroup
    sendCommands    chan responsesWSSendCommand
    controlCommands chan responsesWSSendCommand
    sendOnce        sync.Once
    controlOnce     sync.Once

    setupCancelMu sync.Mutex
    setupCancel   context.CancelFunc
}

type responsesWSCloseState struct {
    closed              atomic.Bool
    clientClosed        atomic.Bool
    closeIntentPosted   atomic.Bool
    downstreamCloseSent atomic.Bool
    backpressurePosted  atomic.Bool
}

type responsesWSWatchdogState struct {
    lastActivityUnixNano atomic.Int64

    activeTurnMu       sync.Mutex
    activeTurnTimer    *time.Timer
    activeTurnTimerGen int64

    busyRejectWindowStart time.Time
    busyRejects           int
}
```

不改变并发所有权：worker goroutine 仍只投递 actor event；actor event loop 仍是业务状态转移入口。

## 类型收敛方向

### TransportSendResult 与 SettlementDecision 分层

长期命名应明确：

```text
TransportSendResult:
    transport 机械结果。只描述 send 是否未尝试、已尝试、stream 前拒绝、ambiguous。

SettlementDecision:
    账务决策。只描述 rollback/finalize exact/floor/observed-or-floor。
```

不要长期保留两个近似 send enum 互相转换。transport 不决定钱；actor 根据 transport result、zero-charge proof、evidence summary 构造 settlement input。

### DetailOrigin 降级为诊断

`DetailOrigin` 可以继续存在，但只用于：

- projection diagnostics；
- metric / log；
- debug；
- provider/native/bridge 问题定位；
- downstream payload 选择。

它不能直接驱动 quota rollback/finalize。

### Frame 类型收敛

长期只保留一个业务层 typed frame。Phase 2 选择 `common/responsesws.Frame` 作为 ResponsesWS typed frame；`runtime/session.Frame` 只属于 `/v1/realtime` 兼容面。wire `(messageType int, payload []byte)` 只应出现在 wsconn 边界。避免 ResponsesWS 业务层长期存在：

```text
common/responsesws.Frame
wire int + []byte
```

raw frame payload 应保持不可变语义；如需暴露 payload，应返回 copy 或只提供 `ClonePayload()`。

### Attempt identity 显式化

attempt id 是协议/账务语义，不应通过 `context.Context` value 隐式传递。长期形状：

```go
type SendRequest struct {
    AttemptID string
    Frame     responsesws.Frame
    DefaultPreviousResponseID string
}

SendClientWithResult(ctx context.Context, req responsesws.SendRequest) responsesws.ResponsesWSTransportSendResult
```

## Phase 策略

### Phase 1：Settlement core + 账务语义修正

最小可合入阶段：

- 新增 bounded settlement core。
- 新增 minimal pure projection。
- 新增 fixed final quota executor。
- trace 记录 input / decision / applied。
- ResponsesWS 强制 preconsume floor。
- actor 通过 settlement decision 执行 quota。
- ambiguous/no-terminal 不 rollback。
- ambiguous 不 retry 有测试。
- 更新相关测试和文档。

Phase 1 后，即使 actor 结构仍丑，账务语义必须自洽。

### Phase 2：语义压缩与重复状态收敛

- `SendResult` / `Outcome` 分层命名。
- `DetailOrigin` -> core evidence projection 收敛。
- Frame 类型收敛。
- attempt identity 从 context value 改显式 send request。

Phase 2 后，settlement core 的输入应由纯 projection 构造，不再依赖 actor 平铺字段的隐式组合。

Phase 2 的 transport 取舍：HTTP BridgeSession 仍保留 opening / active / cancel /
backpressure 等并发状态，收益是继续由 transport 边界保证 stream 生命周期安全；代价是
BridgeSession 还不是一个极简 pipe。本阶段只要求它产出规范 observation / upstream event，
不产出 accounting 语义；turn settlement sequencing 继续归 actor。

### Phase 3：Actor v2 状态模型还债

- 引入 turn slots 与显式 transition helper。
- 引入 pending provider journal，统一 replay payload 与 settlement observation 的来源。
- active turn 保存 projected evidence fact，不保存可重复 append 的 observation log。
- opening / pending / active / history 分别归位，减少非法字段组合。
- cancel、bridge error、worker、watchdog、close state 按生命周期归组。
- `contradictory_input` 作为诊断信号，不默认扩展成 fail-closed；`missing_settlement_floor` 的 executor fail-loud 行为保持阶段一现状，不在阶段三降级为 finalize-0。

Phase 3 不改变 settlement decision 的金额语义，也不改变 executor 已落地的 missing-floor fail-loud 行为。它允许增加少量结构代码，换取更清晰的所有权、转换边界和后续扩展点。

### Phase 4：可选 reducer / audit 持久化

Phase 4 不是承诺项，只有在满足以下条件时才做：

- settlement matrix 已稳定；
- actor 仍然因为 accounting state transition 难以 review；
- reducer 可以保持 accounting-only；
- reducer 不输出 WebSocket write / close / upstream abort 等 IO effect。

如果出现真实争议处理或对账需求，再持久化 `ResponsesWSSettlementTrace`。不要提前引入 outbox / event sourcing。

## 必须测试的矩阵

### Settlement core 纯测试

必须覆盖：

- terminal exact below floor：terminal usage = 10，floor = 100，final = 10。
- terminal without usage：final = floor。
- observed below floor：observed = 10，floor = 100，final = 100。
- observed equal floor：observed = 100，floor = 100，final = 100。
- observed above floor：observed = 150，floor = 100，final = 150。
- zero-charge proof alone：rollback。
- zero-charge proof + provider activity：不 rollback，floor，带 contradictory/proof_suppressed flag。
- zero-charge proof + observed usage below floor：observed-or-floor，final = floor。
- zero-charge proof + observed usage above floor：observed-or-floor，final = observed。
- zero-charge proof + terminal usage：exact terminal usage。
- zero-charge proof + terminal without usage：floor。
- no proof + no evidence：floor。
- negative floor / observed：clamp 到 0，并记录合适 flag。

### Projection 纯测试

必须覆盖：

- provider stream opened -> activity evidence。
- provider frame seen -> activity evidence。
- usage seen / observed quota -> activity evidence。
- peer close after stream -> activity evidence。
- provider rejected before stream -> zero-charge proof，不是 activity evidence。
- diagnostics 字段变化不影响 settlement decision。

### Actor 集成测试

必须覆盖：

- ambiguous + no provider evidence -> floor settlement，close，不 retry。
- ambiguous + provider evidence -> terminal/floor/observed-or-floor，不 retry。
- pending_send unknown close -> floor，不 rollback。
- bridge local open error -> floor，并保留精确 `ws_request_failed` payload。
- bridge open provider error -> rollback。
- NotSent + no evidence -> rollback。
- NotSent + provider evidence -> proof conflict，provider evidence 压制 rollback，记录 warning/metric。
- no-terminal usage < floor -> final = floor。
- terminal usage < floor -> final = usage。
- trace replay：`decide(trace.Input).DecisionKey == trace.Decision.DecisionKey`。
- trace/applied：`trace.Decision.ExpectedFinalQuota == trace.Applied.AppliedFinalQuota`。
- high-balance user ResponsesWS still force preconsume floor。

## 强不变量

1. 所有 ResponsesWS quota rollback/finalize 必须经过 settlement core decision。
2. `ZeroChargeProof` 永远弱于 terminal/provider activity/observed usage。
3. rollback/free 分支必须先确认没有 terminal、没有 provider activity evidence、observed billable quota 为 0。
4. ambiguous 不 retry。
5. no-terminal / ambiguous 不 rollback，除非存在 zero-charge proof 且无更强 evidence。
6. terminal with usage 按 provider usage exact 结算，允许低于 floor。
7. terminal without usage 按 floor。
8. provider evidence without terminal 按 `max(observed, floor)`。
9. `ExpectedFinalQuota` 是 executor 的唯一金额来源。
10. trace 必须同时记录 input、decision、applied。
11. trace decision amount 必须等于 applied amount。
12. actor 不能 branch `decision.Reason`。
13. settlement flags 只能表达账务事实，不能表达 transport detail。
14. `DetailOrigin` 只能作为 projection diagnostics，不能直接改钱。
15. ResponsesWS attempt 必须强制 preconsume floor。
16. floor 必须有界，不能使用 `max_output_tokens` / 连接时长 / worst-case provider bill。
17. provider adapter / transport helper 不碰 quota、RPM、affinity settlement。
18. actor 仍然是连接内状态唯一写入者。
19. pending provider replay payload 与 settlement observation 必须来自同一个 journal 入口。
20. active turn 保存 projected evidence fact，不保存可重复 append 的 observation log。
21. pending -> active commit 后 replay 不再次 observe，不再次 merge usage。
22. opening / pending / active 转换通过 turn slot helper 完成。
23. contradictory input 默认 warning/metric/trace，不直接绕过 settlement core；身份/归属冲突才关闭当前 turn/session。
24. replay `Surface` 遇到 accounting / provider accepted / downstream committed barrier 时，executor 必须先经过 settlement core，再 surface payload 和清 turn。
25. pending create cancel marker 是 attempt replay barrier，不能被 retry 分支清掉后重新发送 create。
26. pending request-level rejection append 必须执行 journal event/byte limits，即使该 entry 不进入 settlement observation projection。

## 需要同步修改的 docs/dev 文档

- `docs/dev/responses-ws-architecture.md`：把计费章节替换为本方案的 conservative bounded billing + settlement core 口径。
- `docs/dev/billing-settlement-architecture.md`：新增 ResponsesWS settlement 类型，说明 exact/floor/observed-or-floor/fixed final quota。
- `docs/dev/responses-ws-transport-boundary.md`：删除 ambiguous/no-evidence rollback 叙述，改为 floor settlement；明确 bridge local error 与 bridge provider rejection 的区别。
- `docs/dev/responses-ws-provider-contract.md`：把 “precise send result” 改为 “explicit transport send result”；说明 send result 不是 provider accepted proof。
- `docs/dev/index.md`：索引更新为 ResponsesWS conservative bounded billing / settlement core。
