# ResponsesWS 架构说明

## 目标边界

`GET /v1/responses` 是通用 Responses WebSocket 入站。客户端升级后首帧必须是官方 inline `response.create`，例如：

```json
{"type":"response.create","model":"gpt-5","input":"hi"}
```

入口只代理官方 ResponsesWS 语义。Codex CLI 只是验证客户端之一，不是协议边界；Codex provider 需要的 nested `response` shape 只允许出现在 provider adapter 边界。

非目标：把 `/v1/realtime` 一并改造成同一套 turn orchestrator；为未知私有代理方言做自动探测；在没有 replay proof 的前提下自动修复 continuation miss。

## 第一性原理

ResponsesWS 入口要同时满足五个事实：

1. **协议事实**：一条 WebSocket 上有多个 `response.create` turn，但同一时间只允许一个未 terminal 的 create。
2. **并发事实**：`SendClient` 返回、provider terminal、客户端断开、timeout 可能乱序到达；业务状态不能跟随 goroutine 调度顺序漂移。
3. **账本事实**：RPM、quota、affinity、connection lease 是不同账本；它们的 commit/rollback 边界必须绑定到同一个 turn attempt。
4. **代理事实**：one-hub 是协议透明代理，转发真相是 raw frame；typed struct 只能服务本地决策。
5. **归属事实**：provider-originated payload 只有在存在 pending 或 active turn 时才有 quota/affinity 语义；session open 只能证明 transport 可用，不能单独成为 provider frame 的业务归属。

由此得到架构原则：**每条 ResponsesWS 连接只有一个状态写入者（actor），所有外部并发都先转换为事件；每个 turn attempt 是一个可提交或可回滚的事务；所有跨层语义必须通过类型表达，不能靠 sniff 或调用顺序约定。**

## 强不变量

### 协议与 fallback

1. `GET /v1/responses` 入站必须保持 WS 语义；provider 不能在该入口下静默切到 HTTP bridge。
2. 不支持 ResponsesWS 的 channel 在已升级连接上写 wrapped error frame：顶层 `type:"error"`、`status:426`、`error.code:"responses_ws_unsupported_for_channel"`；写入后发送 close control，close code `1000`，reason 可截断为 `responses_ws_unsupported_for_channel`。客户端 fallback 以最后解析到的 wrapped error frame 为准，不能只依赖握手 HTTP status 或 close code。
3. DNS/TLS/401/403/429/5xx/配置错误必须保留真实错误状态，不能包装成 426。
4. 显式 pin 或 strict continuation owner 的 channel 不支持 ResponsesWS 时写 wrapped fallback frame，不 fresh 到其它 channel；非 strict preferred affinity 只代表优先尝试，必须 skip 当前 channel 并继续选择其它 eligible WS-capable channel；只有所有 eligible candidate 都明确不支持 ResponsesWS 时，才写 wrapped fallback frame。
5. `SendOutcomeLocalWriteOK` 只表示 local-write-ok，不表示上游语义接受；写入后出现的 upstream rejection 不在 relay 层自动 retry，`SendOutcomeAmbiguous` 也不得自动 retry。

### 连接容量

6. API RPM 不等于连接数。Upgrade 不消耗 RPM。
7. pending first-frame slot 只保护"升级后迟迟不发首帧"的 socket，默认按 credential 限 96。pending lease 的 `Release` 必须幂等；获取成功后立即安装 `defer pendingLease.Release()`。
8. active ResponsesWS connection/session lease 是独立容量限制，按 credential、group 和 global 三层可配置；它由 actor 在打开首个 upstream session 前获取并挂到 actor 生命周期。
9. active lease 不得在第一帧成功发送后释放——连接可以 idle 30 分钟，established WS 必须由 active lease 持有到连接结束。

### Turn 生命周期

10. 一条 WebSocket 同一时间最多一个 active `response.create`。第二个 create 在 terminal 前到达返回 recoverable `session_busy`，不向上游发送，不消耗 RPM。
11. `candidate` 和 `active` 是生命周期状态，不是 `*channelAffinityState != nil`；affinity disabled 或没有 hint 时 turn 仍然存在。
12. turn 状态只能由 `ResponsesWSSessionActor` 的事件循环修改。handler、IO bridge、provider、wrapper、observer callback 都不能直接 commit/clear active turn，也不能在 actor 启动后直接写 actor fields。
13. 首帧 parse 成功后客户端 read pump 必须立即启动；actor 处理 `FirstTurnSetup` 时进入 pending-turn `opening` phase，该 phase 早于 active lease、upstream open、quota preconsume 和 RPM allow，已经算作 busy 并持有 `OpeningID`，后续 effect result 必须带回同一个 `OpeningID`。客户端 close 由 read pump 先设置取消信号，再投递 actor event。
14. 首帧 retry 边界不是 `SendClient` 返回 error，而是 actor 能证明该 candidate 没有被上游看到：未写入 upstream、没有 provider-originated event、没有 ambiguous outcome。
15. 后续 turn 已绑定当前 upstream session，不能重新选号。本地 binding 指向其它 channel 时直接返回 owner conflict，不发送上游。
16. `pending_prepare` / `pending_send` 是显式状态。RPM admission 在 turn entry 完成（首帧：pending-turn `opening` phase 内、channel selection 之前；后续 turn：busy check 通过、`PrepareResponsesWSTurnAttempt` 之前），独立于 candidate attempt。
17. `BeginCandidate` 之后、`SendProviderFrame` 之前的任何本地失败都必须走同一个 rollback cleanup：先调用 `RollbackBeforeLocalWriteOK(reason)`，成功后同时清空 `pendingAttempt`、`pendingProviderEvents`、provider evidence、`pendingTurnPhase`，并把 actor state 恢复为 `idle`。rollback 失败不能继续返回原始业务错误，必须写 `quota_rollback_failed` 并 fail closed。
18. `providerRecvPump` 只能在 actor `BeginCandidate` 成功、RPM admission 通过、pending attempt 和 `AttemptID` 存在后启动；provider-originated event 在 `SendResult` 之前到达时必须先缓存在 pending turn 上。buffered provider-originated event 本身是上游可能已处理该 turn 的证据，之后即使 `SendResult` 带 error，也不能按未发送 retry。
19. `SendOutcomeNotSent` 且无 buffered provider-originated event 时才允许 rollback/retry；`SendOutcomeAmbiguous` 不 retry、不 quota undo；`SendOutcomeNotSent` 与 buffered provider-originated event 同时出现是 proof 冲突，按 protocol violation fail closed（abort/detach session、关闭 turn/连接、不 retry、不重放 buffered event、不 classifier、不 record/clear affinity、不 quota undo）。
20. MVP 默认不做本地 `response.create` 队列；active turn 未 terminal 时返回 recoverable `session_busy`。后续如引入 `responses_ws.pending_create_queue_size=1`，入队前必须先完成 JSON、model 一致性、RPM、quota 和 frame size 校验。
21. 后续 `response.create.model` 必须映射到首 turn 锁定的 upstream model；连接内 model 切换 MVP 返回 recoverable local error，不重新选 channel/key。

### Upstream snapshot

ResponsesWS 需要冻结的是 one-hub 连接建立时的 immutable upstream snapshot，不是泛化的 upstream identity 大对象。snapshot 只记录 one-hub 当前路由、结算、日志和诊断真实依赖的字段，不能为了理论完整性强制引入当前数据模型没有的一等字段。

当前 snapshot 存储结构：

**顶层 key**（`ResponsesWSRequestSnapshot.Values` 直接存储）：

- `channel_id` — 选中的渠道 ID
- `channel_type` — 选中渠道的类型
- `original_model` — 客户端请求的原始 model（从 gin context 读取）
- `billing_original_model` — 是否使用原始 model 计费
- `new_model` — 上游实际使用的 provider model（等价于 upstream_model）

**嵌套对象** `responses_ws_selected_channel_snapshot`（类型 `SelectedChannelSnapshot`）：

- `ChannelID` / `ChannelType` — 渠道标识
- `PreCost` — 预扣成本倍数
- `ProviderModel` — 上游 model
- `BillingModel` — 计费 model
- `OriginalModel` / `BillingOriginalModel` — 原始 model 信息
- `Channel` — 完整的 `*model.Channel` 引用（从中可获取 `channel_name`、`resolved_base_url` 等）

此外，`responses_ws_selected_channel` key 保存了选中 channel 的裸引用。

明确不存储：

- `key_fingerprint` — 当前不生成也不存储。未来如需在 snapshot 中按 key 维度区分请求来源，应只存 hash/masked 值。
- `protocol` — 入口始终为 `GET /v1/responses`，无需冗余存储。

actor 后续 turn 使用当前 upstream session 和该 snapshot 做日志、诊断与结算，不按 `channel_id` 重新读取 live channel 配置来推导本连接当时的 base URL、key 或 model mapping。

### Affinity

22. affinity candidate 必须基于完整 Responses request projection 和 request hints，不能只复制 `model` / `previous_response_id`。
23. success terminal 才能 record affinity；failed / incomplete / cancelled / generic error 不 record。
24. continuation miss 只能清当前实际请求的 owner：cache 中该 key 的 `ChannelID` 必须等于 selected/current channel，才允许删除。

详细 clear 规则见 [Affinity 清理规则](#affinity-清理规则)。

### Terminal 语义

25. terminal 分类只允许通过 `ResponsesTerminalClassifier`。
26. `response.completed` / `response.done` 只有在没有 top-level `error`、没有 non-null `response.error`，且 status 缺失或等于 `completed` 时才算 success terminal。
27. `response.completed` / `response.done` 带 top-level error 或 `response.error` 时归类为 failed terminal，即使 status 缺失。
28. status 为 `failed` / `incomplete` / `cancelled` / `canceled` 的 completed/done alias 必须归类为 failed terminal。
29. normalize 只发生在下行边界：`response.done` + success 可转为 `response.completed`；cancelled/canceled/error-bearing done/completed 可转为 failed 形态。

### Raw protocol

30. raw frame 是 provider 转发源。`OpenAIResponsesRequest` 不能作为 re-marshal 的唯一来源。
31. typed projection 缺字段不能导致官方字段丢失；已知缺口如 `generate` 必须通过 raw map 保留。
32. model rewrite 只修改 raw frame 中的 `model` 字段或 provider adapter 需要的位置，不丢其它字段。
33. raw rewrite 只承诺语义级保留（unknown field value 不被 `float64` 改写），不承诺保留顶层对象 key 顺序、空白或重复 key 结构；parser 拒绝顶层重复 key。
34. Codex provider 内部要求嵌套 `{"type":"response.create","response":{...}}` 的 wrapping 只能在发送给 codex provider 的边界做，wrapper 内容来自 raw map，不能来自 typed struct 重组。
35. provider adapter 对 actor 暴露的必须是官方 ResponsesWS event surface；私有 bootstrap/control frame（`session.created`、内部 ack、transport greeting）必须在 adapter 内消费或转为本地协议错误。

### 并发与资源

36. actor event loop 是 turn/quota/affinity state 的唯一写入者。I/O goroutine、provider、wrapper、observer callback 只能投递事件。
37. proxy 对客户端 websocket 保持 single writer；provider frame 和 proxy-local error frame 都由 actor 下发 write command，再经同一个 write pump/write lock/write deadline 写出。
38. 上游 provider websocket 写也必须有 deadline。实际写上游 frame 的 adapter 在写锁内设置 `SetWriteDeadline(now+RealtimeWebsocketWriteTimeout())`，写后清零；deadline/partial write 失败默认映射为 `SendOutcomeAmbiguous`。
39. proxy-local error frame 不能触发 terminal sniff、active turn cleanup 或 affinity clear。
40. `session.Recv` 返回的 payload 来源必须通过类型表达：没有 provider-origin proof 的 `Recv` error payload 一律按 proxy-local 写；provider-origin `payload + err` 必须端到端保真，先 primary payload，再把 `ClientPayloadError` 携带的 client payload 作为 proxy-local error 补发；若 err 不携带 `ClientPayloadError`，则在 primary payload 之后投递 provider close/timeout。
41. gorilla read deadline 一旦设置会持续作用于后续 reads；first-frame deadline 只覆盖首帧读取，首帧成功后必须 `SetReadDeadline(time.Time{})`，established 阶段不得重新套用 `2 * realtime.websocket_ping_interval_ms` 这类隐式 read deadline。
42. gorilla ping/pong control frames 必须刷新 connection liveness，但不能刷新 actor business idle；普通客户端 data frame 才同时刷新 connection liveness 和 actor activity。
43. ResponsesWS established session 的 idle 语义以 `ResponsesWSIdleTimeout` 和业务 activity 为准。provider data frame、usage event、provider-originated error、客户端 data frame 都必须刷新 actor activity。OpenAI ResponsesWS 的 gorilla read-side API 只属于 read loop / read handler，writer 路径的 turn 状态变化不调用 `SetReadDeadline`，active-turn 无 provider/client data activity 时由业务 idle/后续独立 turn timer 关闭，不能复用客户端 socket read deadline。
44. handler 的 live `gin.Context` 不跨 goroutine 读写。turn prepare/record/clear 使用稳定 input 或 `c.Copy()`。
45. actor mailbox 事件必须分级：`SendResult`、actor 自身 panic/timeout 这类账本 proof / fail-closed 事件必须可靠投递；普通 provider data frame 可以受 mailbox backpressure 影响，但丢弃时必须最终投递 backpressure timeout 或关闭连接。
46. `pendingProviderEvents` 必须同时有事件数和字节数上限；上限触发时 fail closed，不能靠 mailbox 长度推导内存边界。
47. 所有 ResponsesWS goroutine 入口（actor loop、client read pump、provider recv pump、send worker、open worker、idle watchdog）都必须有 panic recovery，recovery 策略为 fail closed。

## 核心组件

| 组件 | 职责 |
| --- | --- |
| `ResponsesWSSessionActor` | 连接级单写者，唯一拥有 `sessionChannelID`、upstream snapshot、pending turn、active turn、active quota transaction、RPM charge state、last final response 和 close state。串行消费 `ClientFrame`、`SendResult`、`ProviderDownstream`、`ProxyLocalError`、`ClientClosed`、`Timeout` 事件。 |
| `ResponsesWSTurnAdmission` | 客户端 turn 级 RPM 账本；最多 `AllowRPMOnce` 一次，首帧 not-sent retry 复用同一个 admission。 |
| `ResponsesWSTurnAttempt` | 单 selected channel 的事务，承载 quota、candidate、session、send outcome。提供 `BeginCandidate`、`PreConsumeQuota`、`CommitLocalWriteOK`、`CommitAmbiguousAdmission`、`RollbackBeforeLocalWriteOK`。 |
| `ResponsesWSIOBridge` | protocol-aware bridge：拥有 goroutine、websocket read/write deadline、single writer、ping/pong activity、frame size limit 和 provider `Recv` loop；只做首帧/后续帧校验、model rewrite、provider event delivery 与 unknown event passthrough，不直接 sniff terminal/affinity/quota，把所有 I/O 结果投递给 actor 并执行 actor 返回的 write/open/abort 指令。 |
| `ResponsesAffinityManager` | responses-kind affinity 的 candidate、commit、record、clear 门面；所有调用来自 actor，所有 stale binding 清理必须带 owner 条件。 |
| `ResponsesTerminalClassifier` | 把 provider-originated 下行事件分类成 `non_terminal / success_terminal / failed_terminal`，并给出有限 normalize 结果；放在 `common/responsesws` 供 relay 和 provider 共用。 |
| `RawResponsesProtocolForwarder` | parse、projection、rewrite 和 provider adapter；raw bytes 是 no-op passthrough 的转发真相，`map[string]json.RawMessage` 承诺 JSON value 语义保留，不承诺 byte-for-byte transcript。实现在 `common/responsesws/raw_frame.go` 和 `common/responsesws/terminal_classifier.go`。 |
| `ResponsesWSConnectionLimiter` | 提供 `AcquireResponsesWSPendingSlot` / `AcquireResponsesWSActiveLease`，pending slot 默认按 credential 限 96，active lease 三层（credential/group/global）可配置。 |

## 与 WebSocket 底层复用的关系

ResponsesWS 需要专用 actor，不是因为底层 WebSocket I/O 特殊，而是因为每个 `response.create` 都是带 RPM、quota、affinity 和 terminal side effect 的 turn transaction。`RealtimeSessionProxy` 当前包含 pass-through policy，不能直接作为 ResponsesWS 的业务状态机复用。

目标重构是复用 `common/requester` 内稳定的 WebSocket safety primitive，而不是复用 `RealtimeSessionProxy` 的 policy，也不是新建一套完整 `wsrelay` 框架。第一阶段只复用 client writer、write deadline、read limit、close control、UTF-8 安全 close reason、ping/pong activity handler、bounded control/error writer、active counter guard 和已有 `WSControlFrameWriter`。client read pump、session recv pump 和 send worker 仍由 ResponsesWS actor/IOBridge 持有，除非后续观察证明机械重复足够大且不会引入业务 flag。

`LocalWriteOK / NotSent / Ambiguous` 不属于底层 helper。底层写 helper 只返回原始 error；ResponsesWS actor 结合 attempt phase、provider-originated evidence 和 session 状态合成 send outcome。这样可以让 `/v1/realtime` 继续保持 pass-through 语义，同时避免把 ResponsesWS 的账本概念泄漏到底层。

详细目标方案见 [WebSocket Transport 复用方案](./websocket-transport-architecture.md)。

## 连接账本

ResponsesWS 使用两类连接 lease：

| lease | 默认 | 生命周期 |
| --- | --- | --- |
| `responses_ws.pending_per_credential` | `96` (`-1` = unlimited) | Upgrade 前获取，首帧校验、active lease 和 first-turn RPM admission 完成后释放；早退由 defer 兜底 |
| `responses_ws.active_per_credential` | `128` | 打开首个 upstream session 前获取，actor close/handler return 后释放 |
| `responses_ws.active_per_group` | `128` | 同 active lease |
| `responses_ws.active_global` | `1024` | 同 active lease |
| `responses_ws.active_lease_redis_fail_open` | `true` | Redis 不可用时是否 fallback 进程内计数；`false` 则拒绝新 active lease |

active lease 不随首帧发送成功释放，因为 established websocket 可能 idle 很久。首帧 retry 其它候选 channel 复用同一个 active lease。

迁移 trade-off：旧实现中 `0/未配置` 等价于无限 active 连接，容易在单 token 长连接下耗尽 worker、provider session 或 quota 风险窗口。现在 `0/未配置` 使用保守默认，只有显式 `-1` 表示无限，并会写启动/运行日志，方便大并发部署确认自己选择了无限容量。

## 连接级限流

连接级限流只保护 `GET /v1/responses` 的 WebSocket 建连频率，目标是限制"握手成功但不发首帧 / 首帧非法 / 立刻断开"的空连接或短连接滥用。它不能替代 API RPM：同一 WS 会话内的每个 `response.create` 仍只在 turn-level `AllowRPMOnce(AllowCurrentUserRequest)` 消耗 API RPM。

| 配置 | 默认 | 语义 |
| --- | --- | --- |
| `responses_ws.connect_per_credential_per_minute` | `600` | 每个 credential 每分钟允许的 ResponsesWS 建连尝试数；单位是 per minute，窗口/Redis 共享语义与 API RPM limiter 一致；显式 `-1` 表示关闭 |
| `responses_ws.allow_anonymous_capacity_bucket` | `false` | 缺 token/user 时是否进入共享 `responses-ws-connect:anonymous` 桶；仅测试/本地诊断使用，生产保持 `false` |

入口位置：`middleware.AllowResponsesWSConnectionAttempt` 在 `relay.ResponsesWebSocket` 中位于 `EnsureCurrentUserRequestAllowed` 之后、`AcquireResponsesWSPendingSlot` 之前调用；默认值由 `common/config/config.go` 启动时通过 `viper.SetDefault` 注入。

- 调用顺序固定为：`EnsureCurrentUserRequestAllowed(c)` → `AllowResponsesWSConnectionAttempt(c)` → `AcquireResponsesWSPendingSlot(c)` → Upgrade。连接级限流失败时返回 `429 responses_ws_connection_rate_limited`，不得占用 pending/active lease。
- 限流 key 复用 `responsesWSCredentialKey(c)`：优先 token，其次 user，再其次稳定 auth namespace。`responses_ws.allow_anonymous_capacity_bucket=true` 时所有匿名请求共享 `responses-ws-connect:anonymous` 桶，因此默认 `600/min` 是全局匿名桶，不是 per-client。
- 故障模式 fail-open：Redis 不可用或 limiter 调用异常时退回进程内 limiter，并在降级路径 `logger.Warn` 一次。Trade-off：Redis 抖动期间会暂时失去跨实例共享，但避免把基础设施抖动放大成 handshake 全 429。
- 指标 counter `responses_ws_connection_rate_limited_total`，标签只允许低基数维度（`group`、`credential_kind`），不要直接把 credential id 放进标签。
- 默认值维持 `600/min`。这是为单 token 多人多设备场景预留重连风暴余量；代价是错误客户端或攻击流量能在一分钟内占用更多握手资源，所以部署侧仍应配合上游网关、API RPM 和 active lease 使用。

## 时间与大小配置

| 配置 | 默认 |
| --- | --- |
| `realtime.websocket_read_limit` | `32 MiB` |
| `realtime.websocket_ping_interval_ms` | `25000`（`<=0` 显式禁用，仅建议测试使用） |
| `realtime.websocket_write_timeout_ms` | `40000` |
| `responses_ws.first_frame_timeout_ms` | `30000` |
| `responses_ws.client_pong_timeout_ms` | `300000` |
| `responses_ws.idle_timeout_ms` | `1800000` |
| `responses_ws.max_lifetime_ms` | `3600000` |
| `responses_ws.pending_provider_events_max_bytes` | `2097152` |

首帧读取成功后会清除 first-frame read deadline，established 阶段的客户端连接活性由服务端 ping 和 `responses_ws.client_pong_timeout_ms` watchdog 维护，所有客户端入站 data/control frame 都刷新该 liveness；业务 idle 由 actor watchdog 维护，只由客户端 data frame 与 provider frame/usage/error 刷新。总连接寿命墙用于回收长期挂起连接，pending provider buffer 上限用于限制 send 结果确认前的内存占用。Trade-off：32 MiB 默认 read limit 降低 Codex 大上下文/文件负载误伤，但会提高单连接峰值内存预算；超限后 gorilla 连接不可复用，因此实现返回静态 `invalid_event` 后关闭连接。

## Turn 事务

每个 `response.create` 使用 `ResponsesWSTurnAdmission` 和 `ResponsesWSTurnAttempt` 表达两个账本：

- **admission**：本客户端 turn 的 RPM 最多 `AllowRPMOnce` 一次。`AllowRPMOnce` 在 turn entry 调用（首帧：`FirstTurnSetup` 处理后、active lease 成功之后、channel selection 之前；后续 turn：busy 和 model 一致性检查通过、`PrepareResponsesWSTurnAttempt` 之前）。Allow 成功后无论后续是否 unsupported、open 失败、`SendOutcomeNotSent`、ambiguous 或 candidate retry，都不退 RPM；首帧 retry 其它候选时复用同一个 admission，不重复扣 RPM。
- **attempt**：单 selected channel 上的 quota、candidate、session 和 send outcome。`BeginCandidate` 后的本地失败统一走 `RollbackBeforeLocalWriteOK`，只负责 quota/candidate/session 回滚，不退 RPM。

### 权威副作用顺序

| 步骤 | 可失败 | quota 副作用 | RPM 副作用 | provider recv | 失败处理 |
| --- | --- | --- | --- | --- | --- |
| parse / owner / busy / unsupported | 是 | 无 | 无 | 未启动 | local/proxy error，不进入 attempt |
| active lease（首帧 only） | 是 | 无 | 无 | 未启动 | 不创建 attempt、不消耗 RPM，写 wrapped 429/503 |
| `AllowRPMOnce`（turn entry） | 是 | 无 | 成功消费一次；失败不消费 | 未启动 | 不创建 attempt、不打开/不复用上游，写 wrapped 429 |
| open upstream session（首帧）/ 复用 session（后续 turn） | 是 | 无 | 已消费；retry 不重复扣 | 未启动 | abort/close session 或换候选 |
| `BeginCandidate` | 是 | 无 | 已消费 | 未启动 | 丢弃 candidate/session，不退 RPM |
| raw rewrite | 是 | 无 | 已消费 | 未启动 | `RollbackBeforeLocalWriteOK("rewrite_failed")` |
| `PreConsumeQuota` | 是 | atomic 或带 rollback handle | 已消费 | 未启动 | `RollbackBeforeLocalWriteOK("quota_preconsume_failed")` |
| `ArmProviderRecvPump` | 否 | 已 preconsume | 已消费 | 启动 | 只允许 actor 内构造非失败 command |
| `SendProviderFrame` / `SendResult` | 是 | local-write-ok/ambiguous 后不 undo；not-sent rollback | 不退 | 已启动，provider-originated event 先 buffer | 按 outcome commit / rollback / ambiguous admit |

`pending_prepare` 关闭可 rollback；`pending_send` 未收到 send result 时不能证明 not-sent，按 no-terminal settle 处理。no-terminal settle 必须保留至少 `PreConsumedQuota`，避免状态机不 rollback 但结算层退款，低估可能已经开始的 upstream write。

`BeginCandidate` 后的 rewrite、quota preconsume 等本地失败共享一个 cleanup path。这个路径的职责不只是 quota undo，还必须清 pending attempt、buffered provider events/evidence、pending phase 和 actor state；recoverable 的第二轮本地失败不会把连接永久留在 busy 状态。

Provider `Recv` 若返回 `payload + err`，payload 仍是第一权威帧。bridge 先投递该 frame，再把 `ClientPayloadError` 的 payload 作为 proxy-local error 投递；如果 err 没有 client payload，则投递 provider close/timeout。actor 不依赖 provider frame 上的 `Err` 字段来补发 client error，避免 provider-origin 错误因 frame 已转发而被吞掉。

Terminal side effects 顺序是 actor contract：先 merge terminal usage 并 finalize quota，再 record/clear affinity 和清 active，最后写 client frame。client write 失败不能回滚已经由 provider terminal 证明的 quota/affinity side effects；close path 若已有非 proof-conflict 的 buffered terminal evidence，必须先做内部 replay 以提取 usage/terminal side effects 再进入 no-terminal settlement（`NotSent + provider evidence` proof 冲突除外，不能 replay buffered events）。

财务验收标准：`pending_send` 无 send result、`SendOutcomeAmbiguous`、local-write-ok 后客户端断开等 no-terminal/uncertain 状态，不能 rollback，也不能把预扣额度退回。actor 必须使用 explicit no-terminal settlement，`FinalQuota >= PreConsumedQuota`；只有 terminal usage 明确给出更高费用时才超过预扣，只有可证明 not-sent 或 provider failed terminal 的普通 finalize 才允许低于预扣并退款。

## 主流程概览

### Upgrade 前

1. `EnsureCurrentUserRequestAllowed(c)`。
2. 校验当前请求必须是 WebSocket upgrade；非 upgrade 返回 `426 websocket_upgrade_required`，不消耗连接 attempt/pending capacity。
3. `AllowResponsesWSConnectionAttempt(c)`。
4. `AcquireResponsesWSPendingSlot(c)`。
5. 立即 `defer pendingLease.Release()`；`Release` 幂等。
6. Upgrade。
7. 通过 `RealtimeUserConn.SetReadLimit(config.RealtimeWebsocketReadLimit())` 设置 read limit，再设置 first-frame deadline。

### 首帧

1. 读取首帧（必须是 text frame）；读取成功后立即 `SetReadDeadline(time.Time{})`。
2. `ParseRawResponsesCreateFrame(raw)` 校验 text JSON + `type:"response.create"` + `model` 非空，得到 raw frame 和 typed projection。
3. 创建 `ResponsesWSSessionActor` 和 `ResponsesWSIOBridge`，启动 actor loop 与 client read/close monitor。
4. handler 投递 `FirstTurnSetup{OpeningID, raw, projection, pendingLease}` 后不再直接写 actor state；后续客户端 close、cancel 或第二个 create 都先投递给 actor。
5. actor 处理 setup event，生成 `OpeningID` 和首 turn `ResponsesWSTurnAdmission`，进入 pending-turn `opening` busy phase。
6. actor 执行 `PrepareResponsesTurnAffinity(...)`，校验 explicit pin 与 owner binding 是否冲突。
7. actor 获取 active lease 并挂入 actor 生命周期。
8. actor 调用 `admission.AllowRPMOnce(AllowCurrentUserRequest)`；失败不打开上游，不消耗 quota。
9. actor 主动释放 pending slot；早退路径仍由 lease 幂等释放兜底。
10. actor 发出 cancellable open worker command，进入 open-and-prime。后续所有 candidate 都 unsupported 或 open 失败，这次 RPM admission 不 refund。

### Open-and-prime

对每个候选 channel：

1. 按 pinned / owner affinity / fresh 规则选 channel。显式 pinned channel 可以 raw `fetchChannelById`；owner/preferred affinity 走 normal selector 的 strict preferred path，不能绕过 group/model/filter/cooldown/skip eligibility。
2. 若 channel 明确不支持 ResponsesWS：fresh 或非 strict preferred affinity skip 当前 channel；pinned 或 strict owner affinity 写 wrapped fallback frame。
3. 用 `RealtimeOpenOptions{PreferredTransport: TransportModeResponsesWS, RequireWS: true}` 打开 upstream session，禁止 HTTP bridge fallback。
4. actor 记录 `sessionChannelID`，只表示物理 session owner，不写共享 affinity。
5. actor 冻结本连接的 upstream snapshot：channel、resolved base URL、原始/上游/billing model、billing flag 与 pre-cost；不存储 `key_fingerprint`。后续结算、日志和 continuation miss 诊断使用 snapshot，不重新读取 live channel 配置推导本连接事实。
6. `PrepareResponsesWSTurnAttempt(...)` 完成 prompt-token 估算、quota 对象创建和 passive sink 准备，不执行 quota/RPM 副作用。
7. attempt 带 `openingID`，actor 调 `attempt.BeginCandidate(...)`，进入 `pending_prepare` 并分配 `AttemptID`。
8. 用 `RawResponsesCreateFrame` rewrite mapped protocol model；失败走 `RollbackBeforeLocalWriteOK("rewrite_failed")`。
9. 执行 `attempt.PreConsumeQuota()` 并安装 passive quota event sink；passive sink 只投递 actor event，不能 finalize quota。
10. actor 把 attempt 从 `pending_prepare` 推进到 `pending_send`，发出 `ArmProviderRecvPump(upstreamSessionGeneration, selectedChannelID, session)`。
11. actor 发出 `SendProviderFrame(attemptID, selectedChannelID, session, ...)`，IO bridge 调 `session.SendClient(...)`。
12. IO bridge 把 `SendResult(attemptID, selectedChannelID, outcome, err)` 投递回 actor。
13. `SendOutcomeLocalWriteOK` → `CommitLocalWriteOK`，pending commit 为 active，按到达顺序重放 `pendingProviderEvents`。
14. `SendOutcomeNotSent` 且无 buffered event → `RollbackBeforeLocalWriteOK("send_not_sent")`，abort session，再按规则尝试下一候选。
15. `SendOutcomeNotSent` 但已有 buffered event → protocol violation fail closed。
16. `SendOutcomeAmbiguous` → `CommitAmbiguousAdmission`，不 retry、不 quota undo；有 buffered event 就重放并等待 terminal，没有 event 就写 ambiguous-send proxy-local error 并关闭/detach。
17. sync continuation miss 仅在满足 sync-miss-clear guard 时按 candidate + selected channel 清 stale binding；ambiguous miss、有 provider-originated event 的 miss、attempt/channel 不匹配的 miss 都不自动清 binding。
18. all unsupported → 写 wrapped fallback frame `status:426`。

### Proxy 阶段

1. 后续客户端帧由 `clientReadPump` 投递 `ClientFrame` 事件，actor parse raw frame。
2. `response.create` 事件流程同首帧，但 channel 固定为 current session channel；后续 model 切换 MVP 拒绝，不重新选 channel/key；各步骤的 RPM/quota 副作用见 [失败矩阵](#失败矩阵)。
3. busy `response.create` 不计 RPM/quota，但单连接连续超预算时必须 fail closed，避免零成本 idle-refresh 攻击。
4. provider 下行 event：`providerRecvPump` 投递 provider-originated downstream event；actor 在 `pending_send` buffer，在 `in_flight` 调 classifier 决定 record / clear / active cleanup / normalize。
5. proxy-local error 只写给客户端，不做 terminal sniff，不改变 active turn；包括 `session.Recv` error 触发的 `ClientPayloadFromError(err)` synthetic payload。

### 关闭

1. 任一 pump 结束时投递 `ClientClosed` / `ProviderClosed` / `Timeout` 给 actor。
2. actor 决定 detach/abort provider session、close client websocket、rollback 可证明 not-sent 的 pending attempt，或对 local-write-ok / ambiguous admitted / proof-conflict turn 做 no-terminal settle。close 前若 pending buffer 中已有非 proof-conflict terminal evidence，actor 必须内部 replay terminal usage/affinity side effects。
3. release active lease。
4. 发送 close control：wrapped error frame 之后使用 close code 1000 + bounded reason；首帧非法/非 JSON 这类还没有 actor 状态的 upgrade 入口可以使用 1008/1003 等协议 close code。
5. best-effort record last final response 可保留，但正常路径应已逐 turn record；兜底必须幂等。

## Affinity 清理规则

| miss 来源 | 可清理条件 | clear state/channel |
| --- | --- | --- |
| sync `SendClient` miss | `SendOutcomeNotSent`、无 buffered provider-originated event、attempt/channel 匹配、cache owner 等于 selected/current channel | pending candidate + selected/current channel |
| provider-originated failed/error terminal miss | classifier 确认为 provider-originated failed terminal，且 cache owner 等于 active/current channel | active turn + current channel |
| ambiguous send miss / buffered frame 后 miss / proxy-local error | 不清理 | 无 |

显式 pin 不写共享 request-derived binding；成功 response id 可以记录到 pinned channel，因为它是后续 continuation 的真实 owner proof。pin 与已有 owner binding 冲突时返回 conflict，不发送上游，不清 binding。

## Provider 边界

### 通用约束

Provider 边界以 [OpenAI Responses WebSocket mode](https://developers.openai.com/api/docs/guides/websocket-mode) 和 [Azure Responses WebSocket mode](https://learn.microsoft.com/en-us/azure/foundry/openai/how-to/websockets) 的官方语义为准：真实 WS passthrough 是默认目标，HTTP/SSE bridge fallback 不是默认兼容策略。

- `RealtimeOpenOptions` 必须显式承载 `PreferredTransport: TransportModeResponsesWS` 和 `RequireWS: true`。`PreferredTransport` 是 caller preference，`RequireWS` 是 hard invariant：管理员 `websocket_mode=off` 优先级高于 `RequireWS`，结果是 `responses_ws_unsupported_for_channel` + wrapped 426，而不是强行打开 WS。
- `openRealtimeSessionWithFreshFallback` 不能在 `RequireWS` 为 true 时用 fresh retry 掩盖同一 channel 的 HTTP bridge fallback；fresh 只用于 retryable open/write failure。
- provider 必须把 `RequireWS` 复制到 session/runtime state，供 open-time、send-time reconnect、write-failure fallback 和 cancel path 共同读取，不能只在 open 入参里检查一次。
- provider attach/open 失败回滚必须把旧 session 的 transport flags 一并恢复（尤其 `requireWS` / `deferWSReader`），否则一次失败的 ResponsesWS attach 会污染原本可用的 HTTP-bridge session。
- provider 在 ResponsesWS 场景只做 transport、必要模型/字段改写和 provider-local session 管理，不能拥有独立账本语义。adapter 改写 request 时以 raw envelope / raw response object 为输入输出，只覆盖明确支持的字段，保留未知字段。

### OpenAI official

- ResponsesWS URL 从 Responses HTTP URL 派生：`https` → `wss`，`http` → `ws`。
- 默认上游 frame 是 inline `response.create`，不嵌套 `response`。
- 默认鉴权为 `Authorization: Bearer <key>`，合并 channel model custom headers，行为与普通 HTTP Responses 请求一致。
- `OpenAI-Beta: responses_websockets=...` 只在目标 provider 确实需要时配置，不作为官方 OpenAI 必需条件。

### Third-party OpenAI-compatible

- 第三方 OpenAI-compatible channel 默认不启用 ResponsesWS，不能仅凭 HTTP Responses 兼容就推断支持 WebSocket mode。
- 只有 channel 显式声明 `passthrough_supported` 时才允许按官方 inline `response.create` passthrough 打开上游 WS。
- 未声明或声明不支持时，返回 `responses_ws_unsupported_for_channel`，不得静默切到 HTTP/SSE bridge。
- HTTP/SSE bridge fallback 只能作为后续显式实验能力；启用时必须在日志和响应元数据中标注不保证 OpenAI connection-local previous-response cache。

### Azure OpenAI

- `ChannelTypeAzureV1` 的 ResponsesWS URL 是 resource-level `/openai/v1/responses`，model/deployment 在 `response.create.model` 中表达。
- classic `ChannelTypeAzure` 的 ResponsesWS URL 复用现有 Azure deployment/api-version builder，例如 `/openai/deployments/{deployment}/responses?api-version={Other}`，然后只转换 websocket scheme。
- 默认按 Azure ResponsesWS 文档使用 `Authorization: Bearer <channel key or Entra token>`。如后续实测 `api-key` header 也可用，只能作为显式兼容 fallback；默认不得同时发送 Bearer 和 `api-key` 两套凭据。
- 合并 channel custom headers 时必须过滤 hop-by-hop header 和下游敏感 header；如果 custom header 覆盖 `Authorization`，必须按 channel/provider 鉴权策略重新校验，避免用户配置造成双发或泄漏。
- Azure base URL 若已经包含 `/openai/deployments/...`，本地返回配置错误（classic Azure 不套用 resource-level guard）。

### Codex provider

- `PreferredTransport=ResponsesWS` + `RequireWS=true` 时，任何 open-time、send-time、write-failure fallback 都不能切到 HTTP bridge。
- Codex 现有内部 `response` nested shape 只能作为 adapter shape。adapter 从 `map[string]json.RawMessage` 构造 nested payload，保留 unknown fields。
- typed `OpenAIResponsesRequest` 不能成为 codex WS 转发的唯一来源。

### WS handshake error contract

`common/requester.WSRequester.NewRequest` 不能丢弃 gorilla `Dial` 失败时返回的 `resp`。dial 失败且 `resp != nil` 时读取并关闭 `resp.Body`，最多保留 4 KiB body snippet；header 只用于错误映射、`Retry-After` 等诊断。

- `401/403`、`429`、`5xx` 保留真实错误状态。
- 只有 `404/426` 或 provider 明确标记 unsupported endpoint 时才能映射为 `responses_ws_unsupported_for_channel`。
- DNS/TLS/connect timeout 没有 HTTP status，按真实 transport/config error 处理，不能伪装成 unsupported。

### Read timeout 分层

OpenAI ResponsesWS 的 timeout 分层：首帧用 gorilla read deadline 限制握手悬挂；首帧成功后立即清除 gorilla read deadline；客户端连接活性由 ping/pong liveness watchdog 判断，超时原因固定为 `client_pong_timeout`；业务 idle 由 actor watchdog 判断，provider frame、usage、provider-originated error 和客户端 data frame 刷新 actor activity。Trade-off：不再用 `2 * realtime.websocket_ping_interval_ms` 隐式杀 established 连接，代价是断开的客户端最多保留到 `responses_ws.client_pong_timeout_ms` 才回收；收益是长 Codex/ResponsesWS turn 不会因为下游 pong 抖动被 50 秒 socket deadline 误杀。

## 失败矩阵

| 场景 | RPM | 上游 | affinity | 客户端结果 |
| --- | --- | --- | --- | --- |
| Upgrade 前 credential 禁止请求 | 否 | 否 | 不动 | HTTP 403 |
| pending slot 满 | 否 | 否 | 不动 | HTTP 429/503 |
| 连接级建连限流过限 | 否 | 否 | 不动 | `429 responses_ws_connection_rate_limited` |
| 首帧超时 / 非 JSON / 非 create | 否 | 否 | 不动；pending defer 释放 | close 1008/1009 |
| 首帧 owner 与 explicit pin 冲突 | 否 | 否 | 不动 | wrapped conflict |
| active lease 满 | 否 | 否 | 不动 | wrapped 429/503 后关闭 |
| 首帧 `AllowRPMOnce` 失败 | 否 | 否 | 不动 | wrapped 429 后关闭 |
| all candidate unsupported | 是（admission 已前置） | 尝试前或握手处停止 | 不动 | wrapped fallback frame `status:426` + close 1000 |
| 首帧 quota precheck 失败 | 已消耗（RPM 已前置） | 是，随后 abort | 不动；partial preconsume 走 attempt rollback | wrapped quota error |
| session open 后、`ArmProviderRecvPump` 之前出现 provider event | 已消耗 | abort 当前 session | 不动；不 classifier、不 record、不 clear | fail closed，retry candidate 或 wrapped provider protocol error |
| 首帧 `BeginCandidate` / rewrite / `PreConsumeQuota` 失败 | 已消耗；不退 | 是，随后 abort | 不动或丢弃 candidate | begin/rewrite 无账本 rollback；preconsume 走 `RollbackBeforeLocalWriteOK` 后 wrapped error |
| 首帧 `SendOutcomeNotSent` 且无 buffered event | 已消耗；不退 | abort 当前 session | candidate 丢弃；满足 sync-miss-clear guard 时 owner 条件清理 | retry 或错误 |
| 首帧 `SendOutcomeNotSent` 但已有 buffered event | 已消耗；不退 | abort/detach | 不 record、不 clear、不 classifier | protocol violation fail closed |
| 首帧 `SendOutcomeAmbiguous` | 已消耗；不退 | abort/detach 由 actor 决定 | 不 record；admitted 后的 provider failed terminal miss 才可按 active owner clear | 不 retry；重放 buffered event 或 ambiguous-send error 后关闭 |
| 首帧 sync `previous_response_not_found` + not-sent + 无 buffered event | 已消耗；不退 | abort | 满足 sync-miss-clear guard 时清 candidate 且只清当前 channel owner | 透传或规范化为 WS error frame，记录 upstream snapshot；不 reroute、不 replay、不清空 `previous_response_id` 重试 |
| 后续 create 时 active turn 未结束 | 否 | 已有 | 不动 | recoverable `session_busy` |
| 后续 create model 映射后不同于首 turn upstream model | 否 | 已有，但不发送该 turn | 不动 | recoverable model switch error |
| 后续 create owner 指向其它 channel | 否 | 已有，但不发送该 turn | 不动 | owner conflict |
| 后续 turn `AllowRPMOnce` 失败 | 否 | 已有，但不发送该 turn | 不动；不创建 attempt | recoverable 429，连接继续 |
| 后续 quota precheck 失败 | 已消耗（per-turn admission） | 已有，但不发送该 turn | 不动；partial preconsume 走 rollback | recoverable/terminal quota error |
| 后续 `BeginCandidate` / rewrite / `PreConsumeQuota` 失败 | 已消耗；不退 | 已有 | candidate 丢弃；quota 走 rollback | recoverable local error |
| 后续 `SendClient` sync miss + not-sent + 无 buffered event | 已消耗；不退 | 已有 | 满足 sync-miss-clear guard 时清 candidate 且只清 current channel owner | recoverable/terminal error |
| provider terminal 早于 `SendResult` | 已按 attempt 决定 | 已有 | 先 buffer；commit 后才 record/clear；not-sent 与 buffered event 冲突 fail closed | 顺序化后写给客户端，或 proof 冲突时 fail closed |
| provider 下行 `response.completed` 无 error | 已消耗 | 已有 | record active | success terminal |
| provider 下行 completed/done 带 error | 已消耗 | 已有 | 不 record；miss 则 clear active | failed terminal |
| provider 下行 `type:"error"` miss | 已消耗 | 已有 | clear active owner | failed terminal |
| proxy-local invalid event / 429 / session_busy | 否 | 不发送 | 不动 | recoverable local error，连接继续 |
| 客户端 first open-and-prime 期间断开 | not-sent 可 rollback；ambiguous/admitted 不 undo；已 Allow 的 RPM 不 refund | 可能已有 | 不新增 record；不在 actor 外 finalize quota | actor 收到 `ClientClosed` 后 abort/detach，禁止 fresh retry |
| 客户端断开 | 已接纳 turn 可能已消耗 | 已有 | 不新增 record；actor 做 no-terminal settle | detach/abort |
| idle timeout | 不额外消耗 | 已有 | 不新增 record | abort session, release active lease |

## 关键 Trade-off

1. **raw passthrough 优先于 typed safety**：rewrite 用 `map[string]json.RawMessage` 牺牲编译期字段约束，不能 byte-for-byte transcript；收益是不会因 typed struct 滞后丢失官方字段。
2. **active connection limiter 独立于 RPM**：多一个 lease 生命周期；收益是能真正限制 30 分钟 idle established WS。
3. **首 turn pending opening phase 优先于 pipeline queue**：第二个 `response.create` 在首 turn 打开阶段只能拿到 recoverable `session_busy`，极端客户端 pipeline 少一次自动排队；收益是 client close、quota preconsume、session abort 都能归属到 actor-owned phase，消除首帧 parse 到 `BeginCandidate` 之间的无主窗口。后续如引入 `responses_ws.pending_create_queue_size=1`，必须先验证 model/RPM/quota/frame size，并保持默认值为 `0`。
4. **`SendClient` nil 不等于 upstream accepted**：`pending_send` 期间需要 buffer provider-originated event，ambiguous/proof-conflict 不自动 retry，proof 冲突 fail closed 会对单次异常 overcount；收益是不把本地写成功误判为 provider 语义接受，避免重复上游副作用和 underbilling。
5. **upstream write deadline 放在 provider adapter 实际写点**：每个 ResponsesWS provider adapter 都必须接入同一个 write-deadline helper；deadline/partial write 只能按 ambiguous 处理；收益是 `SendClient` 一定能在有界时间内产出 `SendResult`，actor 不会长期停在 `pending_send` 并持有 quota/lease。
6. **quota finalization 回到 actor，而不是沿用 realtime observer**：需要把现有 observer 改成 passive event sink；收益是 quota、terminal、affinity 的提交顺序和 rollback 边界一致，避免 provider recv goroutine 在 actor 之前 finalize。
7. **binding 命中 fail-closed，binding 缺失 best-effort**：TTL/restart 后缺 binding 仍可能发到非 owner 并由上游返回 miss；收益是不会把已知 owner 的 continuation 错发到其它 channel。
8. **terminal normalize 有限进行**：客户端看不到少数 provider 原始 terminal alias；收益是计费、affinity、客户端看到一致的 success/failed 语义。
9. **只重构 ResponsesWS，不统一 `/v1/realtime`**：两个 WS 入口的 limiter 语义暂时不同；收益是改动面可控。
10. **ResponsesWS 使用专用 actor，但只复用稳定 I/O primitive**：多一层 event/command 样板，不能直接套用 `RealtimeSessionProxy` 的 pass-through policy；收益是客户端帧、send raw result、provider event、close/timeout 被统一线性化，状态正确性收敛到 actor 状态机，同时 writer/deadline/close/control-frame 这类底层风险不再重复实现。read/recv/send pump 暂不抽，避免 callback helper 变成换名 proxy。
11. **删除 RPM dry-run，接受 unsupported 多扣一次 RPM**：首帧已通过本地校验和 active lease 后会先消耗一次 RPM；若之后所有 candidate 都 unsupported 或 open 失败，用户仍会被扣 1 RPM。收益是不再要求 count、sliding-window、token-bucket、memory 四套 limiter 实现严格 mirror 的 `PeekN`，也不需要维护 dry-run/allow 一致性测试。
12. **Redis active lease 默认 fail-open，但必须显式可关**：默认回退进程内计数保留旧部署可用性；多实例 Redis 抖动时容量可能按实例数放大。`responses_ws.active_lease_redis_fail_open=false` 可改为 fail-closed，适合容量约束高于可用性的生产集群。

## 明确不做

- 不实现 connection pool / prewarm。
- MVP 不支持 pipelined `response.create` 排队（首 turn 打开阶段第二个 create 返回 `session_busy`）；未来小队列必须显式配置且默认关闭。
- 不支持连接内 model 切换；后续 `response.create` 必须映射到首 turn 锁定的 upstream model。
- 不在写入后 upstream rejection 上自动跨 channel replay。
- 不把 affinity record 下沉到 provider 层。
- 不把 Redis affinity 升级为 distributed owner / lease / fencing 协议。
- 不为 byte-for-byte raw transcript 增加 raw terminal mode。
- 不顺手改变 `/v1/realtime` 的连接级 limiter 语义。
- 不把 `LocalWriteOK / NotSent / Ambiguous` 下沉到 WebSocket 底层 helper；这是 ResponsesWS actor 的账本语义。
