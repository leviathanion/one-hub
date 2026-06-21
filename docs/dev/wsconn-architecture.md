# wsconn — WebSocket 唯一传输边界架构方案

## 文档状态

**已落地。**

本文描述 one-hub 的 WebSocket 传输层已从"共享 safety primitives + 业务各自组装"的形态，演进为"`common/wsconn` 作为唯一传输边界、业务层不直接接触 `github.com/gorilla/websocket`"的硬边界形态。

该方案取代 [WebSocket Transport 复用方案](./websocket-transport-architecture.md) 描述的旧实现路径，而不是叠加在其上。`common/requester/ws_*.go` 中的旧 primitives 已被吸收进 `common/wsconn`，原文件已删除。

## 真实问题

落地前，`common/requester/ws_*.go` 曾经抽出若干 safety primitives（`WSClientWriter`、`WSControlFrameWriter`、`SafeWSCloseMessage`、`ApplyWSReadLimit`、`InstallWSActivityHandlers`、`WSActiveCounterGuard` 等），但它们是**散落的工具集**，不是一个连贯的连接抽象：

1. 业务层（`relay/responses_ws.go` 3000+ 行、`relay/realtime.go` 580+ 行、`providers/openai/realtime_session.go`、`providers/codex/realtime.go`、`providers/xunfei/chat.go`）曾**直接持有** `*websocket.Conn`、直接调用 `ReadMessage`/`WriteMessage`/`SetReadDeadline`/`SetPongHandler`。
2. 每个业务点重复实现 read pump、ping loop、liveness watchdog、send worker，且实现细节互有差异；当时 ResponsesWS 的 `client_pong_timeout_ms` 实际是 inbound activity-based，名字却像 pong miss。
3. close reason 在不同 goroutine 各自归类，缺少统一来源；metrics 维度容易漂移。
4. 14 个生产文件、13 个测试文件直接 import `github.com/gorilla/websocket`。任何一处接触 transport 细节都可能引入并发写、漏 deadline、UTF-8 close reason 溢出、close 路径分叉等已知反复出错的 bug 类。

结果是：抽象层次没到位，业务文件被传输层细节稀释；同样的传输 bug 要在多处分别修复；新增配置（如 pong timeout）会出现"在一个模块叫这名字、行为却是另一个含义"的语义漂移。

primitives 路线本身没错，但**到此为止已达上限**。要继续降低传输层 bug 密度，必须把 primitives 升级为一个**强制性边界**：业务层连 `*websocket.Conn` 都拿不到。

## 设计目标

1. **唯一传输边界**：`common/wsconn` 是项目里唯一允许 import `github.com/gorilla/websocket` 的位置（含测试支持子包 `common/wsconn/wstest`）。
2. **业务层不再持有 `*websocket.Conn`**：连握手产生的裸连接也不出 `wsconn` 包外。
3. **关闭原因单源**：任何连接的 close reason 由"第一个写入者"定义，后续观察者只能读，不可覆盖。
4. **liveness 语义分离**：ping/pong 死连接检测与业务 idle 超时是两个独立配置，不再共用一个变量名。
5. **协议表面收紧**：业务感知不到 ping/pong 帧；写消息只能是 Text/Binary；关闭只通过 `Close(CloseInfo)`，是否发送 close frame 由 wsconn 根据 CloseInfo.Kind 决定。
6. **不分阶段、不留兼容层**：一次性切换，避免双轨期。

## 完整方案的复杂度边界

本方案不分阶段、不保留兼容层；落地完成时生产和测试代码都不再直接 import `github.com/gorilla/websocket`。

但"不分阶段"不等于把所有能力都塞进 `common/wsconn`。复杂度按职责归属分配：

1. **`common/wsconn`** 只承接 WebSocket 传输层复杂度：握手、读写、liveness、CloseInfo、DialError、安全默认、gorilla import 隔离
2. **`runtime/session`** 承接 provider 会话语义：typed Frame、ProviderClose、usage、provider error
3. **`relay/realtime`** 和 **`relay/responses_ws`** 承接业务编排：Actor、首帧 gate、lease、quota、settlement、fallback
4. **测试支持** 由 `common/wsconn/wstest` 提供，覆盖业务测试所需场景但不做任意网络混沌
5. **不引入 watchdog pre-close hook**；watchdog 关闭不抢发业务 payload，只通过 `CloseInfo.Kind` / close code / metrics 表达。InboundIdle best-effort 发 1001 GoingAway，PongMiss 直接释放

这五条决定本方案在哪里加复杂度、在哪里减复杂度，是"甜蜜点"的硬边界。任何新增需求都先按这五条归类，归不进的视为越界。

## 非目标

- **不抽通用 Bridge 公共包**。Realtime 和 Responses-WS 都不是纯转发场景，统一走 Actor + Pump 范式即可，不引入第二种编排抽象
- **不重写业务状态机**：`ResponsesWSSessionActor`、turn admission、quota 预扣、settlement、fallback、affinity 选择等业务规则保持原貌，只把 IO 层换成 `wsconn`
- **不改变 `runtime/session` 的业务能力边界**：能做什么不变；但接口的 frame 表达从裸 `int mt` 改为 typed `session.Frame`（见 [runtime/session typed Frame](#runtimesession-typed-frame)），以保证"协议表面收紧"目标贯穿到 session 层
- **不引入新的传输协议**（如 HTTP/2 stream 直连）。本方案只重构 gorilla 之上的封装层
- **不把 provider/relay 业务概念放入 `common/wsconn`**。`common/wsconn/` 包内永远不出现 session / turn / quota / billing / provider / model 这类业务关键词（CI 兜底，见架构契约 2）

## 整体分层

```
┌─────────────────────────────────────────────────────────────────────┐
│ relay/realtime         relay/responses_ws (Actor)                   │
│   持有: *wsconn.ManagedConn (client 侧)                             │
│         session.RealtimeSession (provider 侧, 不变)                 │
│   读路径: MessagePump → handler → 业务事件                          │
│   写路径: ManagedConn.WriteMessage                                  │
│   关闭:   ManagedConn.Close(CloseInfo)                              │
└──────────────────────────┬──────────────────────────────────────────┘
                           │
┌──────────────────────────┴──────────────────────────────────────────┐
│ providers/openai · providers/codex · providers/xunfei               │
│   RealtimeSession 业务能力边界不变；接口签名随 typed Frame 切换       │
│   实现内部持有 *wsconn.ManagedConn 管理上游 WS                       │
│   不再 import gorilla                                                │
└──────────────────────────┬──────────────────────────────────────────┘
                           │
┌──────────────────────────┴──────────────────────────────────────────┐
│ common/wsconn  ←  唯一允许 import github.com/gorilla/websocket      │
│   ManagedConn      lifecycle / IO / liveness 全部职责                │
│   MessagePump      读循环 + 派发 + panic recovery + ctx 退出         │
│   CloseInfo        统一 close 分类，first-write-wins                  │
│   AcceptManaged    upgrade + 包装一体化                              │
│   DialManaged      dial + 包装一体化                                 │
│   MessageType      Text / Binary 两值（无 Ping/Pong/Close）          │
│   Subprotocols     gorilla websocket.Subprotocols 透传              │
│   IsUpgrade        gorilla websocket.IsWebSocketUpgrade 透传        │
└─────────────────────────────────────────────────────────────────────┘
                           │
                           ▼
                  github.com/gorilla/websocket

common/wsconn/wstest/  ← 仅测试支持包允许 import gorilla
   Pair(t)             内存中两端连通的 ManagedConn
   Server(t, handler)  httptest 包装的真握手 ManagedConn 来源
   WithClock(clock)    fake clock 注入
```

## wsconn 公共表面

### ManagedConn

单连接所有权对象。回答四个问题，且只回答这四个：连接是否还活着、读写是否安全、何时关闭、关闭原因是什么。

```go
type ManagedConn struct { /* unexported */ }

type Config struct {
    Label                  string                // 日志/metrics 用
    Clock                  Clock                 // nil = 真实时间（time.Now / time.NewTimer）；测试注入 fake clock
    PingInterval           time.Duration         // 0 = 不发 ping
    PongMissTimeout        time.Duration         // 发出 ping 后未收到对应 pong 多久判死，0 = 不监控 pong 回执
    InboundActivityTimeout func() time.Duration  // 任意 inbound control frame 或完整 data message 多久未到达判死，nil 或返回 0 = 关闭
    ReadLimit              int64                 // 0 = wsconn 内置默认值（16 MiB）
    WriteTimeout           func() time.Duration  // 每次写帧前评估，nil = 5s
    OnActivity             func(time.Time)       // 任意 inbound control frame 或完整 data message 到达；仅 transport hook，不允许写连接
}

// Provider/relay 调用方可以在创建 Config 时捕获配置值；已建立连接使用创建时
// 的 timeout/liveness 快照，不承诺热重载。这样避免每次 timer reset / write 都
// 重新读取全局配置带来的锁、漂移和测试复杂度。

// Clock 抽象 wsconn 内部的 Go runtime timer 操作。
//
// 走 Clock：
//   - ping timer / ticker
//   - pong miss timer
//   - inbound activity timer
//   - CloseInfo.At
//   - cleanup goroutine 等待窗口
//   - slow-handle observation
//
// 不走 Clock（必须用真实 time.Now()）：
//   - gorilla WriteControl deadline 参数
//   - net.Conn.SetWriteDeadline
//   - net.Conn.SetReadDeadline
//
// fake clock 不控制 kernel deadline；涉及 socket deadline 的测试要 mock net.Conn
// 或用极短真实 timeout。
type Clock interface {
    Now() time.Time
    NewTimer(d time.Duration) Timer
    AfterFunc(d time.Duration, f func()) Timer
}

type Timer interface {
    Chan() <-chan time.Time
    Stop() bool
    Reset(d time.Duration) bool
}
```

**`Config` 不再提供 `OnClosed` 回调**。所有 close 后业务处理（metrics、log、actor post、状态机迁移、资源释放）通过 `Pump.OnClose` 唯一入口完成，避免双回调无顺序保证带来的资源释放坑。`OnBeforeWatchdogClose` 同样不存在。

**`WriteTimeout` / `InboundActivityTimeout` closure 契约**：

- 必须快速（< 1 µs 量级）、无阻塞、无副作用
- wsconn 可能在入口 validation 与运行期 timer / write 路径**多次**调用
- 禁止在闭包内查数据库、拿锁、做远程调用

**`InboundActivityTimeout` 是 transport liveness 兜底**，不是业务态 idle timeout。任何 inbound control frame（ping、pong、close）或完整 data message 到达都会刷新计时器；outbound ping/data 不计入。粒度是 message 级（`ReadMessage` 返回时刷新），不是 frame 级 —— 如果业务需要 frame 级刷新，需要切换 `NextReader` 实现（不在本方案范围）。

"客户端长时间没业务操作"这种业务层 idle 不通过 Config 表达，由 Actor 内部用 `time.AfterFunc + conn.Close` 自行实现（参见 [turn-level deadline 不属于 wsconn](#turn-level-deadline-不属于-wsconn)）。

#### Config 校验

无效组合在 `AcceptManaged` / `DialManaged` 入口直接返回 `ErrInvalidConfig`，**不**降级运行：

```go
var ErrInvalidConfig = errors.New("wsconn: invalid config")
```

明确拒绝的组合：

| 组合 | 原因 |
|---|---|
| `PingInterval < 0` 或 `PongMissTimeout < 0` | 负数 duration 无意义 |
| `PingInterval <= 0` 且 `PongMissTimeout > 0` | 没有 ping 永远不可能有"对应 pong"，PongMiss 永不触发 |
| `ReadLimit < 0` | 必须 ≥ 0；0 表示用 wsconn 默认值 |
| `WriteTimeout != nil` 且首次调用返回负 | 闭包初始值非法 |
| `InboundActivityTimeout != nil` 且首次调用返回负 | 闭包初始值非法 |

注意：`PingInterval >= PongMissTimeout` 不是非法组合。`PongMiss` 使用 per-ping `Clock.AfterFunc` + generation 校验实现（详见 [Pong generation 算法](#pong-generation-算法精确实现)），两者完全正交，无大小约束。

允许的零值组合（zero-value 友好）：

- 全零 Config（讯飞 chat 场景）：不发 ping、不启 watchdog、读到 EOF 自然结束
- 仅 `PingInterval > 0`：发 ping 但不监控 pong 回执（探测网络中间设备状态用）

#### 运行期 closure 负数兜底

入口 validation 只能检查 `WriteTimeout` / `InboundActivityTimeout` 首次返回值。运行期 closure 返回负数时 wsconn 做**最小兜底**：

```go
// WriteTimeout
d := cfg.WriteTimeout()
if d < 0 {
    log.Warn("wsconn: WriteTimeout returned negative; falling back to default")
    d = defaultWriteTimeout // 5s
}

// InboundActivityTimeout
d := cfg.InboundActivityTimeout()
if d < 0 {
    log.Warn("wsconn: InboundActivityTimeout returned negative; disabling watchdog")
    d = 0
}
```

`InboundActivityTimeout` 负数 → 0 是 **fail-open**：watchdog 被禁用，死连接可能更晚释放。这是有意识选择——比"配置 bug 立即关闭所有活跃连接"更稳，也比维护 `lastValid` per-connection 状态更简单。**调用方配置层仍应保证不返回负数**，wsconn 仅做防御性兜底。

```go
// 入站握手配置。wsconn 内部据此构造 gorilla websocket.Upgrader；
// 业务层不引用 gorilla 任何类型。
//
// 入站握手超时通过传入 r.Context() 的 deadline 控制，不在 AcceptOptions
// 单独配置（gorilla websocket.Upgrader 本身没有 HandshakeTimeout 字段，
// 这跟 Dialer 不同）。
type AcceptOptions struct {
    CheckOrigin       func(*http.Request) bool // nil = gorilla 默认 same-host Origin 策略，不是允许所有来源
    ResponseHeader    http.Header
    ReadBufferSize    int
    WriteBufferSize   int
    EnableCompression bool
    Subprotocols      []string
    Error             func(w http.ResponseWriter, r *http.Request,
                            status int, reason error)
}

// 入站：upgrade + 包装一体化。AcceptOptions 用纯 Go 字段表达
// gorilla Upgrader 的全部配置，业务层永远见不到 *websocket.Conn
// 也不需要 import gorilla。
func AcceptManaged(
    w http.ResponseWriter,
    r *http.Request,
    cfg Config,
    opts AcceptOptions,
) (*ManagedConn, error)

// 出站 dial 可选项。严格禁止暴露任何 gorilla 类型（如 *websocket.Dialer）。
type DialOption func(*dialConfig)

// WithProxyURL 设置出站 proxy。**fail-closed 语义**：
//   - rawURL 为空 → 不使用 proxy（与当前行为一致）
//   - rawURL 非空但 url.Parse 失败 / scheme 不支持 → DialManaged 直接返回
//     ErrInvalidProxyURL，**不退化为直连**（旧 common/requester/ws_client.go:30-34
//     是 fail-open 行为，在配置错误时静默直连，这条改成 fail-closed 才能让
//     "通过 proxy 出网"的部署约束真正生效）
//   - scheme 支持的范围在 wsconn 内白名单（http/https/socks5/socks5h），其它走 Err
func WithProxyURL(rawURL string) DialOption
func WithSubprotocols(protos ...string) DialOption
func WithHandshakeTimeout(d time.Duration) DialOption // d <= 0 使用 wsconn 默认 5s；生产调用方应传 config.ConnectTimeout()
func WithTLSConfig(cfg *tls.Config) DialOption
func WithNetDialContext(f func(ctx context.Context,
                               network, addr string) (net.Conn, error)) DialOption

// DialManaged 默认拒绝 insecure ws、私网/metadata IP，并在直连路径把已校验 DNS
// 结果固定到 NetDialContext，避免校验与连接之间再次解析。配置 HTTP/SOCKS
// proxy 时不做目标 IP pin；proxy 部署由 proxy 本身承担目标解析和出网策略。

// 出站握手失败时返回的诊断信息。完整保留现有 WSDialHandshakeError 字段，
// 并把 CloseInfo 内联进来（DialFailed 时没有 ManagedConn 承载 CloseInfo）。
// 业务通过 errors.As(err, &wsconn.DialError{}) 根据 StatusCode 做错误分类
// （404/426/401/403/429/5xx）。
type DialError struct {
    URL           string       // 脱敏诊断 URL；已移除 userinfo / query / fragment
    StatusCode    int
    Header        http.Header
    BodySnippet   []byte
    BodyTruncated bool
    BodyReadErr   error
    Err           error
    CloseInfo     CloseInfo  // Kind 固定为 CloseKindDialFailed
}

func (e *DialError) Error() string
func (e *DialError) SafeURL() string
func (e *DialError) Unwrap() error

`DialError.Error()` 和 `DialError.URL` 都只使用脱敏 URL：必须移除 userinfo、query、fragment。
取舍是牺牲少量现场诊断细节，换取公共字段被 `%+v` 或结构化日志直接打印时也不泄漏凭据。

// 出站：dial + 包装一体化。握手失败时返回 *DialError；
// 成功时 ManagedConn 已就绪可立即读写。
func DialManaged(
    ctx context.Context,
    rawURL string,
    header http.Header,
    cfg Config,
    opts ...DialOption,
) (*ManagedConn, error)

// 出站安全策略；默认值见下方"DialSecurityPolicy 与错误脱敏"小节
type DialSecurityPolicy struct {
    AllowInsecureWS bool     // 默认 false；仅本地/测试场景显式打开
    AllowPrivateIP  bool     // 默认 false（防 SSRF）；部署可按 provider 打开
    MaxBodySnippet  int64    // 默认 4 KiB；DialError.BodySnippet 上限
    RedactHeaders   []string // 进入 DialError 前需擦除的 header，默认含 Authorization / Cookie / Sec-WebSocket-Protocol

    // HostFilter 是 host/ip 准入的补充决策；返回 false 即拒绝。
    // 默认 nil = 应用内置规则（拒 RFC 1918 / loopback / link-local / metadata IP 169.254.169.254）；
    // AllowPrivateIP=true 时内置规则放过 self-hosted 常用的 loopback / RFC 1918 / link-local，
    // 但 metadata IP 仍在 HostFilter 之前硬拒。
    // 业务可注入自定义 filter 收紧默认（例如 self-hosted 白名单、企业 VPN 段允许、严格 SaaS 拒所有内网）。
    // wsconn 不感知 provider / channel 语义，所有特定策略通过这个 hook 注入。
    HostFilter func(host string, ips []net.IP) bool
}

func WithDialSecurityPolicy(p DialSecurityPolicy) DialOption

// 业务可用 API（完整列表）
func (c *ManagedConn) WriteMessage(mt MessageType, payload []byte) error
func (c *ManagedConn) ReadInitial(ctx context.Context) (MessageType, []byte, error)  // 同步读首帧（仅轻量校验，重 admission 在 actor）
func (c *ManagedConn) Close(info CloseInfo)              // 唯一关闭入口；幂等；first-write-wins；快速返回
func (c *ManagedConn) Done() <-chan struct{}             // cleanup 完成后 closed（不是 Close 调用瞬间）
func (c *ManagedConn) CloseInfo() CloseInfo              // 仅在 Done() closed 后保证已定型
```

**业务无法**调用：`ReadMessage`、`WriteControl`、`WriteClose`、`SetReadDeadline`、`SetPongHandler`、`SetPingHandler`、`SetCloseHandler`、`WriteJSON` —— 这些要么不存在于 ManagedConn API，要么是 unexported。

读路径**只通过** `MessagePump`（持续读）或 `ReadInitial`（同步读首帧）。其它读 API 不暴露。

写关闭路径**只通过** `Close(info)`，不分离出独立的 `WriteClose`。是否真正发送 close frame 由 wsconn 根据 `info.Kind` 决定（详见 [CloseInfo](#closeinfofirst-write-wins) 节"Close 内部步骤"小节），业务无法越过这个决策。

#### ReadInitial 的特殊性

`ReadInitial` 是 wsconn 公共表面里**唯一的同步读 API**，专为 ResponsesWS 这类"首帧需要在业务接管前同步读到"的场景设计。Pump.Run 启动后异步读，会立刻调度 Handle → 读第 2 帧，业务还没拿到首帧；同步读首帧是更直接的接入点。

```go
mc, _ := wsconn.AcceptManaged(...)

// 同步读首帧；带 first-frame timeout
firstCtx, cancel := context.WithTimeout(r.Context(), firstFrameTimeout)
defer cancel()
mt, payload, err := mc.ReadInitial(firstCtx)
if err != nil {
    mc.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort, Reason: "first_frame_timeout"})
    return
}

// 业务对首帧做**轻量**校验（帧类型 + 协议 JSON parse）：
//   - 非 Text 帧：直接 Close(CloseUnsupportedData)
//   - JSON parse 失败：写 JSON error → Close(ClosePolicyViolation)
// **重业务校验（quota / lease 升级 / RPM / model 准入）一律不在这里做** —— 见下方"职责边界"。
parsedFrame, err := parseFirstFrameJSON(payload)
if err != nil {
    writeJSONError(mc, "invalid_first_frame", err)
    mc.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindNormal, Code: wsconn.ClosePolicyViolation, Reason: "invalid_first_frame"})
    return
}

// 立刻启动 actor + Pump，把首帧作为事件投递给 actor channel；
// 所有 heavy admission 走 actor 单线程处理。
actor := newActor(...)
actor.Start()
pump := &wsconn.Pump{Conn: mc, Handle: actor.onClientFrame, OnClose: actor.onClientClose}
go pump.Run(ctx)
actor.PostReliable(FirstTurnSetup{Frame: parsedFrame, ReceivedAt: time.Now()})
```

##### 职责边界（关键）

ReadInitial **只做两件事**：

| 做 | 不做 |
|---|---|
| 同步读首帧（带 ctx deadline） | quota 预扣、lease 升级、RPM admission |
| 轻量帧类型校验（拒非 Text / 拒 oversized） | model 准入、affinity owner 检查 |
| 协议 JSON parse（拒结构非法） | settlement / observer factory 安装 |

为什么 heavy admission 不放在 ReadInitial 与 Pump.Run 之间：

1. **当前代码语义就是这样**：`relay/responses_ws.go:2410-2453` 显示首帧 parse 后立刻 `actor.Start + StartClientReadPump`，admission 通过 `FirstTurnSetup` 事件交给 actor 处理。改成"外层同步 admission"会引入与 actor 状态机并发的第二条业务路径
2. **admission 期间客户端 ping / close / context cancel 都不可达**：Pump 没起来，没有 read 循环消费控制帧
3. **actor 单线程处理 admission 才能与后续 turn / quota / settlement 共享同一锁边界**：拆出来等于做了两套状态机
4. **admission 失败的清理路径已经存在**：actor 收到 FirstTurnSetup 后判 admission 失败 → 走 `requestCloseIntent("admission_failed")` → finalize + lease release + Close downstream，整条路径已经覆盖了 quota refund / lease release / settlement，外层无需重复

约束：

- `ReadInitial` 必须在 `Pump.Run` 之前调用；同一 ManagedConn 上**只能调用一次**
- `ReadInitial` 不触发 PongMissTimeout / InboundActivityTimeout watchdog —— 这些 watchdog 在 Pump.Run 启动时才武装。首帧 timeout 由调用方通过 ctx deadline 控制
- 控制帧（ping/pong/close）在 ReadInitial 读循环中也走自定义 PingHandler / PongHandler，与 Pump 路径一致；但 InboundActivity 不刷新（无 watchdog）
- gorilla `SetReadLimit` 在 ReadInitial 路径**不启用**；改用应用层 `NextReader + io.LimitedReader`，超限时业务**还能写自定义 JSON error**（这是 first-frame 场景最需要的能力）

##### 错误分类（关键）

`ReadInitial` 区分两类错误：

**业务失败（返回 error，不 panic）**：

- ctx deadline / cancel
- oversized first frame（应用层 LimitedReader 超限）
- peer 在握手后直接发 close（返回 `*wsconn.CloseError`，**不**返回 `*websocket.CloseError`）
- 协议读取失败（IO error）

调用方拿到 error 后**必须**：
1. 可选 `WriteMessage` 写业务 JSON error
2. 调用 `Close(CloseInfo)` 释放连接（read state 进入 `terminal` 不会自动 cleanup）
3. 不允许再启动 `Pump.Run`

**非法状态转换（panic）**：

- 重复 `ReadInitial`
- `Pump.Run` 已启动后再 `ReadInitial`
- `ReadInitial` 期间并发再调 `ReadInitial`
- `Pump.Run` 退出后再启动任何 reader

这些都是程序员错误，让 bug 在第一次出现时立刻暴露，比返回 sentinel error 让调用方写"不可能进入"的分支更稳。**不存在 `ErrReadInitialAfterPump`**。

##### ReadInitial 成功后业务校验失败

`ReadInitial` 已经返回 payload，但调用方做轻量校验（非 Text 帧、JSON parse 失败、event type 不合法）后决定拒绝：

1. 可选 `WriteMessage` 写业务 JSON error
2. 必须调用 `Close(CloseInfo)`
3. 不允许再启动 `Pump.Run`

这一类失败 wsconn 无法用 read state 自动 enforce（JSON parse 是业务逻辑）。`beginPump()` 实现层会额外检查 `closeStarted`，提供 fail-fast：

```go
func (c *ManagedConn) beginPump() {
    if c.closeStarted.Load() {
        panic("wsconn: Pump.Run called after Close")
    }
    // read state transition
}
```

为什么 ReadInitial 用应用层 LimitedReader：参见 [ErrReadLimit 处理决定](#errreadlimit-处理决定) —— gorilla `SetReadLimit` 超限时已发 1009 close，业务写 JSON 不可靠。ReadInitial 是首帧场景，业务需要返回精确的 JSON 错误（"frame too large"、"invalid event type"、"model required"）。所以这一处**双轨**是有意为之，不是不一致。

##### ReadInitial ctx deadline / cancel 实现契约

gorilla `NextReader / ReadMessage` 不能被 ctx 直接打断。Pump.Run 采用 ctx watcher 关闭底层 socket 的方式打断 read，但 **ReadInitial 不能这么做**——一旦关 socket，"调用方还能写 JSON error"的目标就破产了。

ReadInitial 内部用**临时 net.Conn read deadline** 实现 ctx 语义：

- **ctx 有 deadline**：内部把 ctx deadline 映射到 `net.Conn.SetReadDeadline(time.Now().Add(remaining))`；deadline 用真实 `time.Now()`，**不走** `Config.Clock`
- **ctx cancel 无 deadline**：起一个内部 watcher goroutine，ctx done 时调 `net.Conn.SetReadDeadline(time.Now())` 立即打断 read
- gorilla `ReadMessage` 因 deadline 返回 `i/o timeout` 时，ReadInitial 内部转译为 `context.DeadlineExceeded` 或 `context.Canceled`，**不把 net.Error 透传给业务**
- **ReadInitial 在所有返回路径用 defer 清理临时 read deadline**：无论成功、ctx deadline、ctx cancel、peer close、oversized first frame、IO error，返回前都尽力 `SetReadDeadline(time.Time{})`。read deadline 留在过去虽不直接影响 `WriteMessage`，但会污染后续调试与 Pump 接管时序
- **wire 上不发送任何 close frame**：业务自己决定写 JSON error 再 `Close`
- **fake `Config.Clock` 不控制这个 socket deadline**（与 Clock 边界一致）

实现形态：

```go
func (c *ManagedConn) ReadInitial(ctx context.Context) (mt MessageType, payload []byte, err error) {
    c.beginReadInitial()

    // ctx-cancel watcher：watcher 生命周期必须 ≤ ReadInitial 调用生命周期。
    // 若 watcher 在 ReadInitial 返回后仍存活，等到原 ctx cancel 时会调
    // SetReadDeadline(time.Now())，把已经交给 Pump 的读循环误打断。
    stopWatcher := make(chan struct{})
    if ctx.Done() != nil {
        go func() {
            select {
            case <-ctx.Done():
                _ = c.rawNetConn.SetReadDeadline(time.Now())
            case <-stopWatcher:
            }
        }()
    }

    defer func() {
        close(stopWatcher)                                // 先停 watcher
        _ = c.rawNetConn.SetReadDeadline(time.Time{})     // 再清 read deadline
        c.finishReadInitial(err)                          // 最后转移 read state
    }()

    // ctx 有 deadline -> 临时 SetReadDeadline(real time.Now()+remaining)
    // gorilla ReadMessage -> net.Error 时转译为 ctx.Err()
    // ...
}
```

defer 顺序固定为"先 close watcher → 再清 read deadline → 再转移 read state"：watcher 退出后才清 deadline，避免 watcher 刚好在 deadline 已清后又 set 一次；read state 最后转移让外部观察者看到一致状态。

这条契约是"业务还能写 JSON error"成立的实现前提，必须有验收线覆盖（详见 ReadInitial 行为验收线）。

##### ReadInitial 期间的 control handler 与 OnActivity 行为

ReadInitial 期间，wsconn 已安装自定义 Ping / Pong handler（与 Pump 路径一致），但**watchdog 尚未武装**。为避免业务被首帧 gate 前的"假活跃"误导，行为契约写死：

- ✓ PingHandler 自动回 pong（走 wsconn 内部 controlWriter，与 Pump 期间一致）
- ✓ PongHandler 解析 generation；可更新内部 `lastInboundActivity` 观测值
- ✗ **不启动 / reset `InboundActivityTimeout` watchdog**（watchdog 在 Pump.Run 启动时才 arm）
- ✗ **不触发 `Config.OnActivity` 回调**：`OnActivity` 是 runtime liveness / metrics hook，让业务感知"连接还活着、可以 refresh business idle"；ReadInitial 阶段连接尚处于首帧 gate，触发 `OnActivity` 会让业务误以为 Pump 已接管

`OnActivity` 在 Pump.Run 启动后正常派发（任意 inbound control frame 或完整 data message 到达）。

### MessagePump

```go
type Pump struct {
    Conn *ManagedConn

    // Handle 必须 < 1ms 返回。任何业务处理（actor channel post、JSON unmarshal 重活、
    // quota 检查、数据库查询）都要通过非阻塞 channel post 投递异步完成。
    // 详见下方"Handle 非阻塞契约"。
    Handle func(ctx context.Context, mt MessageType, payload []byte)

    // close 后通知：CloseInfo 已定型，连接已完全释放，禁止再写。
    // 这是业务感知 close 的**唯一**回调入口（Config.OnClosed 已删除）。
    // metrics、log、actor post、状态机迁移、资源释放都通过此 hook 完成。
    OnClose func(CloseInfo)
}

func (p *Pump) Run(ctx context.Context)
```

#### Pump.Run 持有 read lifecycle

```go
func (p *Pump) Run(ctx context.Context) {
    p.Conn.beginPump()

    // 最早注册的 defer 最晚执行：先让下面的 fallback Close 跑 →
    // 然后等 ManagedConn cleanup 完全完成（<-Done）→ 派发唯一 close 回调。
    // 这是 Pump 派发 OnClose 的唯一路径；ManagedConn 自己不主动回调业务。
    defer func() {
        p.Conn.finishPump()
        <-p.Conn.Done()
        if p.OnClose != nil {
            p.OnClose(p.Conn.CloseInfo())
        }
    }()

    // ctx watcher：ctx done 时强制 Close，让阻塞的 ReadMessage 解阻塞
    watchDone := make(chan struct{})
    go func() {
        select {
        case <-ctx.Done():
            p.Conn.Close(wsconn.CloseInfo{Kind: CloseKindAbort, Reason: "ctx_done"})
        case <-watchDone:
        }
    }()
    defer close(watchDone)

    // invariant fallback：正常路径不应赢
    defer p.Conn.Close(wsconn.CloseInfo{
        Kind:   wsconn.CloseKindAbort,
        Reason: "pump_exit_without_close",
    })

    for {
        mt, payload, err := p.Conn.readMessage()
        if err != nil {
            // readMessage 返回前已完成 PeerClose / ReadError / ErrReadLimit / WriteError /
            // HandlerPanic / ctx_done 分类 Close；defer Close 走 CAS 失败分支
            return
        }
        p.safeHandle(ctx, mt, payload)
    }
}
```

契约：

- **Pump 拥有 read lifecycle**。Pump.Run 一旦启动就负责到底，调用方不需要额外绑定 `ctx.Done() → Close`
- **read path 错误前置分类**：`readMessage` 在返回 error 之前必须完成 `Close(classifiedInfo)`，把 PeerClose / ReadError / ErrReadLimit / WriteError / HandlerPanic / ctx_done 等分类信息写进 `CloseInfo`。defer 的 `pump_exit_without_close` 仅作为 invariant fallback，正常路径不应赢
- **主循环不 select ctx.Done**：ctx cancel 由 watcher goroutine 通过 `Close` 关闭底层连接打断 read，read path 顺序统一
- 一个 ManagedConn 同一时刻只允许一个 Pump.Run 在跑 —— **由 read state 5 态机强制**（见下方"read state 5 态"），重复启动直接 panic
- watcher goroutine 通过 `watchDone` channel 在 Pump.Run 正常退出时一并退出，不泄漏 goroutine

#### read state 5 态

ManagedConn 用一个 5 态 enum 表达 read lifecycle，不与 close state 耦合：

```go
type readState uint32

const (
    readIdle readState = iota
    readInitialActive
    readInitialReady   // ReadInitial 成功；Pump.Run 可以接管
    readPumpActive
    readTerminal       // ReadInitial 失败 / Pump.Run 已退出；任何后续读都是 bug
)

type ManagedConn struct {
    // ...
    readState atomic.Uint32 // 持有 readState 值
}
```

状态机集中在四个 helper 内，所有合法 from→to 对在此处维护：

```go
func (c *ManagedConn) beginReadInitial() {
    if !c.readState.CompareAndSwap(uint32(readIdle), uint32(readInitialActive)) {
        panic("wsconn: ReadInitial called in invalid read state")
    }
}

func (c *ManagedConn) finishReadInitial(err error) {
    if err == nil {
        c.readState.Store(uint32(readInitialReady))
        return
    }
    c.readState.Store(uint32(readTerminal))
}

func (c *ManagedConn) beginPump() {
    // fail-fast：业务在 ReadInitial 成功后做轻量校验失败 → Close 后误启 Pump
    if c.closeStarted.Load() {
        panic("wsconn: Pump.Run called after Close")
    }
    for {
        st := readState(c.readState.Load())
        switch st {
        case readIdle:
            if c.readState.CompareAndSwap(uint32(readIdle), uint32(readPumpActive)) {
                return
            }
        case readInitialReady:
            if c.readState.CompareAndSwap(uint32(readInitialReady), uint32(readPumpActive)) {
                return
            }
        default:
            panic("wsconn: Pump.Run called in invalid read state")
        }
    }
}

func (c *ManagedConn) finishPump() {
    c.readState.Store(uint32(readTerminal))
}
```

转换规则：

```text
ReadInitial:
- readIdle           -> readInitialActive
- success            -> readInitialReady
- failure            -> readTerminal
- 其它状态调用 ReadInitial -> panic

Pump.Run:
- readIdle           -> readPumpActive
- readInitialReady   -> readPumpActive
- finish             -> readTerminal
- 其它状态调用 Pump.Run -> panic
```

为什么 panic：read state 转换违反是程序员错误，让 bug 在第一次出现时立刻暴露。返回 error 会让调用方写"不可能进入"的分支，反而掩盖问题。

#### ctx watcher 与 net.Conn 强中断

gorilla `ReadMessage` 阻塞在 underlying `net.Conn.Read` 上，**`ctx.Done()` 不能直接打断它**（gorilla API 没有 read-with-context 形式）。Pump.Run 主动起 ctx watcher goroutine，在 ctx done 时调 `Close` 关闭底层 socket。gorilla `ReadMessage` 从 closed socket 读会返回 `use of closed network connection` 错误，read path 走分类 Close（CAS 失败，CloseInfo 已被 watcher 写入 `ctx_done`）。

这才是真正打断阻塞 read 的机制 —— ctx watcher 通过 Close 间接达到效果，不是绕过 gorilla 自己捅 socket。

#### 回调职责

| 回调 | 触发时机 | 允许做什么 | 不允许做什么 |
|---|---|---|---|
| `Config.OnActivity` | 任意 inbound control frame 或完整 data message 到达 | metrics、refresh business idle | 写连接、阻塞调用 |
| `Pump.OnClose` | cleanup 完成后（Pump.Run 退出前最后一步） | metrics、log、actor post、状态机迁移、资源释放 | 写连接、阻塞调用 |

`Pump.OnClose` 是业务感知 close 的**唯一**回调。`Config.OnClosed` / `Config.OnPongMiss` / `Config.OnInboundIdle` / `OnBeforeWatchdogClose` 全部**不存在**——业务从 `OnClose(info)` 读 `info.Kind` 完全能区分 PongMiss / InboundIdle / PeerClose 等关闭原因，不需要独立 hook，也不需要双回调引入顺序问题。

#### close 路径序列

```text
任何关闭路径（业务主动 / PeerClose / ReadError / WriteError / HandlerPanic /
               watchdog 判死 / Pump.Run 退出）
    → Close(info) CAS 抢闸成功者定型 CloseInfo
    → go cleanup(info)
       → 按 Kind 决定是否发 close frame
       → 停 watchdog / ping loop
       → 关底层 *websocket.Conn
       → close(c.done)
    → Pump.Run 监听 <-Done() 后，由 Pump 派发 OnClose（唯一回调）
```

`OnClose` 派发**不在 close 锁内同步调用**。这是 wsconn 实现层的硬性 invariant：cleanup goroutine 内不持锁派发回调。删除了 `Config.OnClosed` 后，**不再有"两个回调无顺序保证"的问题**。

#### Pump 只关心 I/O 层事件

- 底层 read I/O error（含 `ErrReadLimit`、deadline 超时、网络异常）→ 转 `CloseInfo{Kind: CloseKindReadError, Code: <按错误类型映射>}` 走 `Close()`，**不**调用 Handle
- 收到 close frame → 转 `CloseInfo{Kind: CloseKindPeerClose, Code, Reason}` 走 `Close()`，**不**透传到 Handle（gorilla 默认 close handler 已自动回执 —— 见 [Close 内部步骤](#close-内部步骤精确顺序)）
- Handle panic → 内置 recover，转 `CloseInfo{Kind: CloseKindHandlerPanic}` 走 `Close()`
- watchdog 判死 → 转 `CloseInfo{Kind: CloseKindPongMiss / CloseKindInboundIdle}` 走 `Close()`；不抢发业务 payload（InboundIdle 已经 best-effort 发 1001 close frame，PongMiss 对端大概率不可达）
- ctx cancel → ctx watcher goroutine 主动 `Close(CloseKindAbort, "ctx_done")`，让阻塞 read 解阻塞；read path 走分类 Close，CAS 失败
- 同一 ManagedConn 同时只允许一个 Pump.Run 在跑 —— 由 [read state 5 态](#read-state-5-态) 强制（非法状态转换即 panic）

#### Handle 非阻塞契约

gorilla 的设计是 control frame（close、ping、pong）必须由 ReadMessage 路径触发处理 —— 应用必须**持续**调用 read 才能让 gorilla 处理 ping/pong/close handler。这意味着 Pump 的 read 循环不能被 Handle 阻塞，否则：

- pong 收不到 → PongMissTimeout 误触发
- peer close 收不到 → 业务感知不到客户端断开
- read deadline 推不进去 → InboundActivityTimeout 误触发

契约：

> **`Pump.Handle` 必须非阻塞**：禁止外部 IO、阻塞 channel send、持有任何锁等待。业务处理通过**非阻塞** select post 或独立 worker queue 完成。
>
> **1ms 是 slow-handle observation 阈值，不是硬契约**。Pump 内置 slow observation：每次 Handle 调用前后取 timestamp（用 `Config.Clock`），耗时超过阈值（默认 1ms）调用 `slowHandleRecorder.Observe(duration)` 计数。CI 测试通过注入 fake recorder 断言 slow observation 被触发，**不**依赖真实时间精度，也**不**对生产 Handle 设硬时间断言。

正确写法（非阻塞 select + 明确的 backpressure 策略）：

```go
func (a *Actor) handleClientFrame(ctx context.Context, mt wsconn.MessageType, payload []byte) {
    // 复制 payload（Pump 复用 buffer），select 投递不阻塞
    cloned := append([]byte(nil), payload...)

    select {
    case a.events <- ClientFrameEvent{MT: mt, Payload: cloned}:
        // 投递成功
    case <-ctx.Done():
        // Pump 退出中
    default:
        // Actor channel 满 = 业务处理跟不上 = backpressure
        // 用 CloseKindBackpressure（best-effort 发 1013 Try Again Later）
        a.conn.Close(wsconn.CloseInfo{
            Kind:   wsconn.CloseKindBackpressure,
            Code:   wsconn.CloseTryAgainLater,
            Reason: "actor_backpressure",
        })
    }
}
```

错误写法：

```go
func (a *Actor) handleClientFrame(ctx context.Context, mt wsconn.MessageType, payload []byte) {
    a.events <- ClientFrameEvent{...}            // ← 阻塞 send，channel 满时卡死 Pump
}

func (a *Actor) handleClientFrame(ctx context.Context, mt wsconn.MessageType, payload []byte) {
    var event ClientFrame
    json.Unmarshal(payload, &event)              // ← 同步 JSON 解析
    if err := a.checkRPM(event); err != nil {    // ← 同步 quota 检查
        a.conn.Close(wsconn.CloseInfo{...})
    }
    a.handleTurn(event)                          // ← 同步业务处理
}

func (a *Actor) handleClientFrame(ctx context.Context, mt wsconn.MessageType, payload []byte) {
    go a.processFrame(payload)                   // ← 每帧起 goroutine：乱序 + 高并发膨胀
}
```

需要 reliable delivery（不允许丢帧、不允许 Abort）的场景：用独立的 bounded handoff worker，Pump.Handle 投递到 worker 的输入 channel（仍然非阻塞 select），worker 自己用更大的 buffer 或外部存储承接。**不要**在 Handle 里直接同步阻塞。

`go func()` 不是禁止，但是少数特殊场景：例如一次性触发的、明确生命期短的、与帧顺序无关的任务。日常业务事件分发应走 Actor channel。

#### ErrReadLimit 处理决定

gorilla 的 `SetReadLimit` 实现里，超限时**已经在内部发出 close frame**（带 `CloseMessageTooBig` 1009）并返回 `ErrReadLimit`。等业务感知到时连接已处于 close-sent 状态，后续 `WriteMessage` 会拿到 `ErrCloseSent`。

本方案选择**接受 gorilla 行为**：

- Pump 看到 `errors.Is(err, websocket.ErrReadLimit)` → 转 `CloseInfo{Kind: CloseKindReadError, Code: CloseMessageTooBig, Err: err}` → 走 `Close()`（不发 close frame，gorilla 已发）
- 业务在 `Pump.OnClose` 中通过 `info.Code == CloseMessageTooBig` 感知

代价：当前 `relay/responses_ws.go:600-606` 那段自定义 "invalid_event / frame is too large" JSON payload 在新架构下消失。客户端通过 close code 1009 知道为什么被关。

如果将来确实需要在 frame too big 时给客户端一个自定义 JSON 错误，需要弃用 gorilla `SetReadLimit`、改用 `NextReader + io.LimitedReader` 在应用层做 —— 不在本方案范围内。

业务层（如 `ResponsesWSIOBridge`）原有的"`ErrReadLimit` → 发送 proxy-local error frame"路径**整体删除**。

#### 自定义 Ping / Pong Handler 实装

要让"任意 inbound control frame 刷新 activity"成立，wsconn 必须**安装自定义 Ping / Pong handler**，不能用 gorilla 默认：

- gorilla 默认 PingHandler：**自动回 pong，但不记录 activity**
- gorilla 默认 PongHandler：**什么都不做**

wsconn 在 `AcceptManaged` / `DialManaged` 包装时强制安装：

```go
func (c *ManagedConn) installControlHandlers(raw *websocket.Conn) {
    raw.SetPingHandler(func(appData string) error {
        c.markInboundActivity(c.clock.Now())
        // **不回退 gorilla 默认 PingHandler**：默认 handler 会调 raw.WriteControl(PongMessage,...)，
        // 绕过 wsconn 的统一 write deadline / 错误归类 / bounded queue。改走 wsconn 内部 control writer。
        return c.controlWriter.EnqueuePong([]byte(appData))
    })
    raw.SetPongHandler(func(appData string) error {
        c.markInboundActivity(c.clock.Now())
        c.observePongGeneration(appData)
        return nil
    })
    // SetCloseHandler 保留 gorilla 默认（自动回 close frame），
    // 业务感知 peer close 通过 ReadMessage 返回的 *CloseError 走 Pump 转换。
    // close frame 是连接终结事件，回完 close 立刻退出，不存在 deadline / 错误归类的持续语义。
}
```

为什么 PingHandler 不能 `return defaultPing(appData)`：

gorilla `Conn.WriteControl`（`conn.go:413`）通过内部 `c.mu` channel 串行化写，本身线程安全 —— 但**线程安全不等于"应该这么写"**。问题：

1. **write deadline 不统一**：gorilla 默认 ping handler 用硬编码 `writeWait`（60s），而 wsconn 配置的是 `config.RealtimeWebsocketWriteTimeout()`。混用导致同一连接两种写超时
2. **错误归类丢失**：默认 handler 的写错误返回给 read loop，read loop 把它当 read error 处理 —— 这是写错误，应该归 `CloseKindWriteError`
3. **绕过 bounded queue**：wsconn 的 `controlWriter` 在 pong/close 帧写阻塞时有 bounded queue + drop policy；默认 handler 直接调 `WriteControl`，没有 bounded 语义，慢 peer 场景下 read goroutine 会被 control 写阻塞

当前生产代码 `providers/openai/realtime_session.go:560-565` 已经是这个模式（`writer.EnqueuePong(appData)`），新方案把这个模式做到 wsconn 内部，所有 ManagedConn 默认享受。

`markInboundActivity` 刷新 `lastInboundActivity` 给 InboundActivityTimeout watchdog 用。`observePongGeneration` 解析 payload 前 8 字节匹配 outstanding generation，给 PongMiss watchdog 用。

data message 的 activity 刷新只能在 `ReadMessage` 返回完整 message 之后做（message 级粒度），不是 frame 级。这是 ReadMessage API 的本质限制 —— 文档不再宣称"任意 inbound frame"，而是**"任意 inbound control frame 或完整 data message"**。如果业务需要 frame 级粒度，需要切换 `NextReader` 实现（不在本方案范围）。

### CloseInfo（first-write-wins）

```go
type CloseKind string

const (
    CloseKindUnknown          CloseKind = ""
    CloseKindNormal           CloseKind = "normal"            // 业务主动有序关闭；可携带 1000，也可携带 protocol reject / unsupported data 等非 1000 合法 code
    CloseKindGracefulShutdown CloseKind = "graceful_shutdown" // 业务优雅关闭（detach / server graceful），发送 close frame
    CloseKindAbort            CloseKind = "abort"             // 业务强制关闭，跳过 close frame 立即释放
    CloseKindBackpressure     CloseKind = "backpressure"      // backpressure (actor channel 满)；best-effort 发 1013 Try Again Later
    CloseKindPeerClose        CloseKind = "peer_close"        // 对端发了 close frame（gorilla 默认 handler 已自动回执）
    CloseKindPongMiss         CloseKind = "pong_miss"         // PongMissTimeout 触发
    CloseKindInboundIdle      CloseKind = "inbound_idle"      // InboundActivityTimeout 触发
    CloseKindReadError        CloseKind = "read_error"        // 读 IO 失败（含 ErrReadLimit、deadline、网络异常）
    CloseKindWriteError       CloseKind = "write_error"       // 写 IO 失败
    CloseKindHandlerPanic     CloseKind = "handler_panic"     // MessagePump 的 Handle 函数 panic
    CloseKindDialFailed       CloseKind = "dial_failed"       // 仅出现在 DialError.CloseInfo 中，不会附加到 ManagedConn
)

type CloseInfo struct {
    Kind   CloseKind
    Code   CloseCode  // WS 关闭状态码（详见 CloseCode 列表），可能为 0
    Reason string
    Err    error
    At     time.Time
}
```

#### Close 内部步骤（精确顺序）

业务唯一关闭入口是 `Close(info)`。Close **快速返回**，资源 cleanup 在后台 goroutine 完成；并发 Close 调用的失败者**立即 return 不阻塞**，不被 close-frame 的 ~2s deadline 影响。业务通过 `<-Done()` 等待 cleanup 完成。

```go
// 业务唯一入口；CAS first-write-wins + 异步 cleanup
func (c *ManagedConn) Close(info CloseInfo) {
    if !c.closeStarted.CompareAndSwap(false, true) {
        return  // 后续调用立即返回，不阻塞；不在此打 warning（避免 loser 噪声）
    }
    // CloseKindUnknown 防御：放在 CAS 成功之后，只对真正赢得 first-write
    // 的那个调用点打一次 warning，避免被 loser 调用 Close(CloseInfo{}) 制造的噪声淹没。
    if info.Kind == CloseKindUnknown {
        log.Warn("wsconn: first Close called with CloseKindUnknown; treating as Abort")
        info.Kind = CloseKindAbort
    }
    info.At = c.clock.Now()
    c.closeInfo.Store(info)
    go c.cleanup(info)
}

func (c *ManagedConn) cleanup(info CloseInfo) {
    // 1. 按 Kind 决定是否发送 close control frame（决策表见下）
    //    code 必须走 wireCloseCodeFor(info)：先按 Kind 填默认值再 sanitize；
    //    不能直接传 info.Code（Code==0 时会被 sanitize 替换为 1011 而丢失 Kind 默认值）
    //    Go 端 cleanup 等待窗口走 Clock；socket WriteDeadline 用真实 time.Now()
    if c.shouldWriteCloseFrame(info.Kind) {
        code := wireCloseCodeFor(info)
        reason := safeCloseReason(info.Reason) // UTF-8 边界截断到 ≤123 字节
        c.writeCloseFrameBestEffort(code, reason)
    }
    // 2. 停止 ping loop / watchdog / Clock timer
    c.stopBackgroundLoops()
    // 3. 关闭底层 *websocket.Conn
    _ = c.rawConn.Close()
    // 4. close(c.done) —— 标志 cleanup 完全完成
    close(c.done)
    // 5. Pump.Run 监听 <-Done() 后由 Pump 自己派发 OnClose（唯一回调入口）
}
```

关键设计点：

- **单阶段 CAS**：`closeStarted` 一旦置 true，所有后续 Close 直接返回。没有 sync.Once.Do 那种"第一个 Do 把所有人阻塞到 cleanup 完成"的问题
- **cleanup 独立 goroutine**：调用 Close 的 goroutine 立即解放，不被 close-frame deadline 拖住
- **无 pre-close hook**：watchdog 判死时不抢发业务 payload。客户端从 CloseInfo.Kind 和 close code 推断原因；需要更多诊断信息走 metrics 和 log
- **`CloseKindUnknown` 不允许进入决策表**：Close 入口做 fallback Abort + log，方便定位"忘记设 Kind"的调用点
- **PongMiss 不发 close frame，InboundIdle / Backpressure best-effort 发 close frame**：见下方决策表

#### Close 决策表

写死，业务不可干预：

| CloseKind | 是否发 close frame | Code 来源 / 替换 | 原因 |
|---|---|---|---|
| `CloseKindNormal` | ✓ | `info.Code`（白名单校验，默认 1000） | 业务主动有序关闭；可携带 protocol reject / unsupported data 等非 1000 合法 code |
| `CloseKindGracefulShutdown` | ✓ | `info.Code`（默认 1001 GoingAway） | 优雅关闭，对端可能需要 close code 决定重连策略 |
| `CloseKindAbort` | ✗ | —— | 强制释放，不等 flush |
| `CloseKindBackpressure` | ✓ best-effort | `info.Code`（默认 1013 TryAgainLater），失败吞掉 | actor channel 满；告知客户端"我现在忙，重连"，但短 deadline 内发不出就放弃 |
| `CloseKindPeerClose` | ✗ | —— | **gorilla 默认 close handler 已自动回执**（详见下方"PeerClose 与 gorilla 默认 handler"） |
| `CloseKindPongMiss` | ✗ | —— | 对端可能 TCP 死亡，发 close frame 只浪费 deadline |
| `CloseKindInboundIdle` | ✓ best-effort | `info.Code`（默认 1001 GoingAway），失败吞掉 | inbound idle 不一定代表 TCP 死，给客户端一个明确 close code；若短 deadline 内发不出就放弃 |
| `CloseKindReadError` 普通 | ✗ | —— | 多数 read error 路径 gorilla 已自己处理 |
| `CloseKindReadError` + `Code == CloseMessageTooBig` | ✗ | —— | **gorilla 已发 1009，wsconn 不重复发**（这是 ErrReadLimit 的特化分支） |
| `CloseKindWriteError` | ✗ | —— | 写已失败，再发 close frame 也是失败 |
| `CloseKindHandlerPanic` | ✗ | —— | 业务自身坏了，不该继续向 peer 发数据 |

`Backpressure` 和 `InboundIdle` 共用同一 best-effort 模板：短 deadline（默认 ~500ms-1s）、socket deadline 用真实 `time.Now()`、失败吞掉、cleanup 必须继续完成。

#### Wire close code 白名单

RFC 6455 / IANA 定义了部分 code 是**保留/观测专用**，**不允许**作为 wire 上的 close frame status code 发送。明确允许 vs 禁止：

| 范围 | 是否可上 wire |
|---|---|
| `1000-1003` | ✓ |
| `1004` | ✗（Reserved） |
| `1005` | ✗（"No status received"，观测码） |
| `1006` | ✗（"Abnormal closure"，观测码） |
| `1007-1014` | ✓ |
| `1015` | ✗（"TLS handshake"，观测码） |
| `<1000` 或 `>4999` | ✗ |
| `3000-4999` | ✓（应用自定义码范围，包括 provider 自定义码如 4408 / 4499） |

`cleanup` 内部分两步决定 wire 上的 close code：先按 Kind 填默认值，再 sanitize。**顺序写死**避免实现分歧（`info.Code == 0` 时不会被 sanitize 直接替换为 1011）：

```go
func wireCloseCodeFor(info CloseInfo) CloseCode {
    code := info.Code
    if code == 0 {
        // 1) 按 Kind 填默认值
        switch info.Kind {
        case CloseKindNormal:
            code = CloseNormalClosure         // 1000
        case CloseKindGracefulShutdown, CloseKindInboundIdle:
            code = CloseGoingAway              // 1001
        case CloseKindBackpressure:
            code = CloseTryAgainLater          // 1013
        default:
            code = CloseInternalServerErr      // 1011
        }
    }
    // 2) sanitize：保护 provider 传入的任意 int 不上 wire 时变成保留/观测码
    return SanitizeWireCloseCode(int(code))
}

// SanitizeWireCloseCode 入参是 int 而非 CloseCode：
// 业务（尤其 provider close code 转发）拿到的是 *int*；强迫 caller 先转 CloseCode
// 等于把"非法值"塞进强类型，反而绕过校验。入口直接接 int，出口返 CloseCode 才是
// 类型安全的边界。
func SanitizeWireCloseCode(code int) CloseCode
```

- 在允许范围内 → 转 `CloseCode` 原样返回（含 `3000-4999`）
- 不在允许范围内 → 替换为 `1011 InternalServerErr`
- 替换时记 log，便于诊断业务为何传错 code

测试必须覆盖：

```go
SanitizeWireCloseCode(3000) == 3000  // 边界
SanitizeWireCloseCode(4408) == 4408  // provider 自定义码
SanitizeWireCloseCode(4999) == 4999  // 边界

SanitizeWireCloseCode(999)  == 1011  // 下限
SanitizeWireCloseCode(1004) == 1011
SanitizeWireCloseCode(1005) == 1011
SanitizeWireCloseCode(1006) == 1011
SanitizeWireCloseCode(1015) == 1011
SanitizeWireCloseCode(5000) == 1011  // 上限
```

为什么不让业务在 `CloseCode` 枚举里看不到 1005/1006：保留这些常量给业务**观测** `CloseInfo.Code` 时使用（例如 `info.Code == CloseNoStatusReceived` 表示 peer 没发 status code）。是否能上 wire 是 cleanup 内部的决策，业务感知不到。

#### PeerClose 与 gorilla 默认 handler

`github.com/gorilla/websocket@v1.5.3/conn.go` 中的默认 close handler 在收到 peer 的 close message 时**会自动给 peer 回 close message**。本方案选择**保留 gorilla 默认 handler 不替换**：

- 收到 peer close 时，gorilla 已经回执 close frame
- 随后 ReadMessage 返回 `*websocket.CloseError`
- wsconn Pump 把它转成 `CloseInfo{Kind: CloseKindPeerClose, Code, Reason}` 走 Close（业务侧拿到的是 `*wsconn.CloseError`，gorilla 类型不泄漏）
- Close 的 cleanup 路径**不再**发 close frame（避免重复发送）

这条决策必须有专门测试覆盖：模拟 peer 发 close → 抓 wire bytes，断言一共只发了一次 close frame（gorilla 回的那一次）。

#### 单源契约

所有 goroutine 通过 `Done()` 等待、通过 `CloseInfo()` 读取。`closeStarted` CAS 保证 first-write-wins：并发场景下多个 goroutine 调用 `Close(differentInfo)` 时只有首个被记录，**其余调用 CAS 失败立即返回**，不被 cleanup 的 ~2s close-frame deadline 阻塞。

这条契约由 first-write-wins 测试覆盖：多 goroutine 并发触发不同 Kind 的 Close，断言：
- `CloseInfo().Kind` 等于首个 CAS 成功者
- 失败者的 Close 调用在 1ms 内返回（不被 cleanup 阻塞）

#### WriteMessage 与 Close 的写锁顺序

WriteMessage 持有 write mutex 时检测到底层写 I/O 错误，**不能在持锁状态下同步调用 Close** —— Close 的 cleanup 路径会尝试 acquire 同一把 mutex 发送 close frame，造成死锁。

正确序列：

```go
func (c *ManagedConn) WriteMessage(mt MessageType, payload []byte) error {
    c.writeMu.Lock()
    err := c.rawConn.WriteMessage(int(mt), payload)
    c.writeMu.Unlock()                          // 必须先 release
    if err != nil {
        c.Close(CloseInfo{                      // 再触发 Close（CAS 抢闸 + go cleanup）
            Kind: CloseKindWriteError,
            Err:  err,
        })
        return err
    }
    return nil
}
```

注意 `Close` 内部已经是 CAS + go cleanup 的非阻塞结构（见上方"Close 内部步骤"），所以 release-then-close 序列不会再造成竞态。专门测试覆盖：write 失败时不出现 deadlock，CloseInfo 正确归类。

### MessageType（窄表面）

```go
type MessageType int

const (
    TextMessage   MessageType = 1
    BinaryMessage MessageType = 2
)
```

故意**不导出** Ping / Pong / Close。

- 业务永远不应该手写 ping/pong — 那是 ManagedConn 的职责。
- 业务永远不应该把 close frame 当 message 透传 — 用 `Close(CloseInfo{...})` 主动关闭，或通过 `OnClose`/`CloseInfo()` 感知 peer close。
- `WriteMessage` 入参约束为 Text/Binary；传其他值返回 `ErrInvalidMessageType`。

### CloseCode 与 CloseError

```go
type CloseCode int

const (
    CloseNormalClosure          CloseCode = 1000
    CloseGoingAway              CloseCode = 1001
    CloseProtocolError          CloseCode = 1002
    CloseUnsupportedData        CloseCode = 1003
    CloseNoStatusReceived       CloseCode = 1005
    CloseAbnormalClosure        CloseCode = 1006
    CloseInvalidFramePayloadData CloseCode = 1007
    ClosePolicyViolation        CloseCode = 1008
    CloseMessageTooBig          CloseCode = 1009  // ErrReadLimit 路径
    CloseInternalServerErr      CloseCode = 1011
    CloseServiceRestart         CloseCode = 1012
    CloseTryAgainLater          CloseCode = 1013
    // ... 按 RFC 6455 / one-hub 业务需要补充
)

type CloseError struct {
    Code   CloseCode
    Reason string
}

func (e *CloseError) Error() string

// 便于业务做 errors.As(err, &CloseError{})
```

业务**不引用** `websocket.CloseError`、`websocket.CloseNormalClosure` 等 gorilla 常量。

#### Close reason 截断

`Close(info)` 中的 `info.Reason` 在内部发送 close frame 之前会按 RFC 6455 约束截断 —— close 控制帧 payload 上限 125 字节，前 2 字节是 status code，剩 123 字节供 reason 使用。截断必须按 UTF-8 边界进行，不能切到多字节字符中间。

复用现有 `SafeWSCloseReason` 实现（`common/requester/ws_close.go`，将迁入 `common/wsconn`）。验收线必须覆盖：
- reason > 123 字节时正确截断且 UTF-8 合法
- 含多字节字符时不在 rune 中间切断
- 输入含无效 UTF-8 字节时丢弃，保持输出合法

### Subprotocols / IsUpgrade

```go
func Subprotocols(r *http.Request) []string
func IsUpgrade(r *http.Request) bool
```

是 `websocket.Subprotocols` 和 `websocket.IsWebSocketUpgrade` 的 trivial 透传，仅为了 `common/authutil/credential.go` 这类只做 HTTP 请求检视、不做 WS IO 的场景能在 import 禁令下继续工作。10 行代码换一条干净的规则（"任何包都不允许 import gorilla"），不开特例。

### DialSecurityPolicy 与错误脱敏

`DialManaged` 成为唯一出站 WS 入口后，安全默认必须随之集中。否则架构整合只解决了边界，却给项目引入新的单点安全风险（任何 provider 都可以靠 `WithProxyURL` 把流量导走，或在 DialError 里把 API key 写进日志）。

**默认契约**（即使调用方不传 `WithDialSecurityPolicy`）：

1. **TLS verify 默认开启**；想关闭必须显式 `WithTLSConfig(&tls.Config{InsecureSkipVerify: true})`，方便 grep 审计
2. **默认拒绝 `ws://`**：scheme 不在 `{wss}` 时 `DialManaged` 返回 `ErrInsecureScheme`，本地/测试需 `WithDialSecurityPolicy(DialSecurityPolicy{AllowInsecureWS: true})` 显式打开
3. **默认拒绝私网/loopback/metadata 地址**：解析 host 后通过 wsconn 内置规则判定；命中 RFC 1918 / loopback / link-local / metadata IP 返回 `ErrPrivateAddrBlocked`。部署可在 policy 里 `AllowPrivateIP: true` 放过 self-hosted 常用的 loopback / RFC 1918 / link-local，**metadata IP 即便 AllowPrivateIP=true 或 HostFilter 自定义同意也仍拒**（防 SSRF 抓 cloud metadata）
4. **proxy fail-closed**：`WithProxyURL("")` 表示"不走 proxy"；非空但解析失败、scheme 不支持时 `DialManaged` 返回 `ErrInvalidProxyURL`，**绝不静默退化为直连**。`socks5h` 属于历史兼容的 SOCKS remote-DNS proxy scheme，必须和 `socks5` 一起保留在白名单。当前 `common/requester/ws_client.go:30-34` 是 fail-open 路径，配置 proxy 但解析失败时返回裸 dialer 直连目标，违反"通过 proxy 出网"的部署约束。新方案必须 fail-closed 才能让该约束真正生效
5. **`DialError.Error()` / `DialError.URL` 输出脱敏**：永远不输出 Authorization / Cookie / Sec-WebSocket-Protocol 值，也不输出 URL userinfo / query / fragment；这些 header 在写入 `DialError.Header` 前被 redact 为 `"[REDACTED]"`，URL 字段也只保留脱敏后的结构化诊断值
6. **`BodySnippet` 限长**：默认 4 KiB（与现有 `wsDialBodySnippetLimit` 对齐）；超过即截断并设 `BodyTruncated = true`
7. **DialOption 全部纯 Go 类型**：禁止 `WithDialer(*websocket.Dialer)`、`WithGorillaHeader(http.Header)` 等暴露 gorilla 的选项；允许 `WithProxyURL(string)`、`WithSubprotocols(...string)`、`WithHandshakeTimeout(time.Duration)`、`WithTLSConfig(*tls.Config)`、`WithNetDialContext(...)`
8. **出站握手超时默认有界**：未传 `WithHandshakeTimeout` 或传入非正值时，`DialManaged` 使用 wsconn 内置 5s 默认值；生产 upstream 调用方显式传 `config.ConnectTimeout()`，恢复旧 websocket requester 受 `connect_timeout` 控制的行为

#### HostFilter 注入模型

`HostFilter func(host string, ips []net.IP) bool` 是 host/ip 准入的**补充决策点**。AllowPrivateIP 放宽 self-hosted 常用地址；metadata IP 是硬拦截，调用方不能通过 HostFilter 覆盖。这样保留了自建部署可用性，同时避免一个恒真 filter 把最危险的云 metadata SSRF 防线打穿：

```go
// 示例：self-hosted 部署允许特定 VPN 段
policy := wsconn.DialSecurityPolicy{
    HostFilter: func(host string, ips []net.IP) bool {
        for _, ip := range ips {
            if vpnCIDR.Contains(ip) {
                return true  // 允许企业 VPN 段
            }
            if isPublicIP(ip) {
                return true  // 允许公网
            }
        }
        return false
    },
}
```

wsconn **不感知**任何 provider / channel 语义。除 metadata IP 硬拦截外，特定准入策略通过这个 hook 由 provider 或部署配置注入。这条边界保证 wsconn 不会因为接入新 provider 而长出业务知识。

policy 字段在 wsconn 内被两次使用：

- dial 之前：scheme 检查、地址解析 + HostFilter 准入判定
- dial 失败构造 DialError 时：header redact + body 截断

provider endpoint allowlist、企业代理 DNS 策略都**不在 wsconn 范围** —— 通过 HostFilter 注入或在 provider/配置层处理。wsconn 只提供 metadata 硬底线、安全默认、脱敏 hook 和 filter 注入点。

## 架构契约

本节列出 wsconn 边界的契约，分**硬门禁**（CI blocking）与**软门禁**（CI advisory / review checklist）两类：

- **硬门禁**：违反必定让 PR 拿不到 CI 绿灯，对应实际可拦下的 lint / build 失败
- **软门禁**：CI 报 warning 或写进 review checklist，主要靠 review 兜底，不阻塞 PR

### 约束 1（硬门禁）：gorilla import 限制（depguard）

当前 `.golangci.yml` 使用 `disable-all: true`，必须**同时**把 depguard 加入 `linters.enable` 才会生效；仅写 `linters-settings` 块是死的。修改两处：

```yaml
# .golangci.yml
linters:
  disable-all: true
  enable:
    - goimports
    - gofmt
    - govet
    - misspell
    - ineffassign
    - typecheck
    - whitespace
    - gocyclo
    - revive
    - unused
    - depguard   # ← 新增

linters-settings:
  depguard:
    rules:
      # 规则 1：gorilla/websocket 只能在 wsconn 和 wstest 内出现
      gorilla-websocket-boundary:
        list-mode: lax
        files:
          - "!**/common/wsconn/**"
          - "!**/common/wsconn/wstest/**"
        deny:
          - pkg: github.com/gorilla/websocket
            desc: "WebSocket must go through common/wsconn"

      # 规则 2：wstest 是 test-only 包，禁止生产代码 import
      # （wstest 路径在 common/wsconn/wstest/，前缀匹配 common/wsconn/**，
      #  规则 1 不能顺带禁止它被生产代码 import，必须单独写）
      wstest-only-in-tests:
        list-mode: lax
        files:
          - "!**/*_test.go"
        deny:
          - pkg: one-api/common/wsconn/wstest
            desc: "wsconn/wstest is test-only; production code must not import it"
```

落地时必须做：

1. 启用 depguard 后在本地跑一次 `make lint` 或等价命令，**实测**两条规则各拦下一个故意写错的违规 import；不要假设配置生效。
2. 按当前 golangci-lint 版本校验 depguard v2 配置格式（v2 与 v1 字段名有差异，例如 `files` 在 v1 是 `ignore-file-rules`）；若版本不匹配则按版本调整。
3. **生产代码和测试代码同时覆盖 gorilla 禁令**：测试不能直接 import gorilla，必须走 `common/wsconn/wstest`。
4. **生产代码禁止 import wstest**：测试 helper 不应该泄漏到生产构建。

`grep -rE 'websocket\.' --include='*.go'` 在 `common/wsconn/**` 外不出现 —— 这是 lint failure 后的快速定位辅助，不是主防线。

### 约束 2（软门禁）：wsconn 业务关键词禁令 + close code 强转 advisory

`common/wsconn/` 包内禁止出现以下关键词。**这是 advisory / review checklist，不阻塞 CI**——`session|turn|quota|billing|provider|model` 这种纯文本 grep 误伤率高（"connection lifecycle 中的 session 概念"等无关词命中），适合作为 review 提示，不适合作为 blocking。真正的硬约束在约束 3（import 边界）。

- `session` / `turn` / `quota` / `billing` / `provider` / `model`
- `response.create` / `response.completed` / `response.failed` / 其它协议事件类型字符串

**provider close code 强转 advisory**：

```text
relay/ 和 providers/ 中出现 wsconn.CloseCode( 时触发 warning。
除 SanitizeWireCloseCode 实现和测试外，不允许 wsconn.CloseCode(任意 int) 强转——
provider close code 必须通过 SanitizeWireCloseCode(int) 进入 wire。
```

CI 配置：`grep` 命中输出 warning 但不退出 1。

### 约束 3（硬门禁）：wsconn import 边界

`common/wsconn/` 不允许 import：

- `one-api/relay`
- `one-api/providers`
- `one-api/runtime/session`
- `one-api/model`

通过 depguard 规则 3 实现：

```yaml
      wsconn-no-business-import:
        list-mode: lax
        files:
          - "**/common/wsconn/**"
        deny:
          - pkg: one-api/relay
          - pkg: one-api/providers
          - pkg: one-api/runtime/session
          - pkg: one-api/model
```

### 约束 4（硬门禁）：close 路径单源

```go
// 任何业务关闭路径：
conn.Close(wsconn.CloseInfo{
    Kind:   wsconn.CloseKindGracefulShutdown,
    Code:   wsconn.CloseGoingAway,
    Reason: "graceful shutdown",
})

// 任何业务感知关闭：
<-conn.Done()
info := conn.CloseInfo()  // 此时 info 已定型
```

业务**不允许**：
- 自己拼接 close frame 字节然后调用低层 API（API 已不暴露）
- 在 CloseInfo 已定型后试图修改/覆盖 reason
- 通过其他渠道（额外字段、共享变量）传递 close reason

CloseInfo first-write-wins 必须有专门测试覆盖：多 goroutine 并发触发不同 Kind 的 close，断言 `CloseInfo()` 是首个写入的。

### 约束 5（硬门禁）：单次活跃 Pump

每个 ManagedConn 同一时刻只允许一个 Pump.Run 在跑。**由 [read state 5 态机](#read-state-5-态) 强制**，非法状态转换 panic。registerPreCloseHook 机制已随 OnBeforeWatchdogClose 一并删除（没有需要预先注册的 hook）。

## runtime/session typed Frame

为让"协议表面收紧（只 Text/Binary）"目标贯穿到 session 层，`runtime/session.RealtimeSession` 接口从裸 `mt int` 改为不透明 `Frame` 类型。仅靠 named int type 无法在编译期阻止 `FrameKind(8)` 这种合法 type conversion，所以 `Frame` 采用 **unexported field + 构造器 + runtime 校验** 三件套，把非法值堵在构造点。

```go
package session

// FrameKind 是观测维度的标签；调用方拿不到也无法自己构造非法值，
// 因为 Frame 的 kind 字段不导出，且只有 NewTextFrame / NewBinaryFrame
// 两个构造器入口。
type FrameKind uint8

const (
    FrameKindText FrameKind = iota + 1
    FrameKindBinary
)

// Frame 是不透明类型：payload 可以读，kind 通过 getter 暴露，
// 但调用方无法构造 Frame{kind: 99, ...} 字面量（kind 不导出）。
type Frame struct {
    kind    FrameKind
    payload []byte
}

func NewTextFrame(payload []byte) Frame   { return Frame{kind: FrameKindText, payload: payload} }
func NewBinaryFrame(payload []byte) Frame { return Frame{kind: FrameKindBinary, payload: payload} }

func (f Frame) Kind() FrameKind        { return f.kind }
func (f Frame) Payload() []byte        { return f.payload } // 只读语义；调用方不得修改
func (f Frame) ClonePayload() []byte   { return append([]byte(nil), f.payload...) }
func (f Frame) IsZero() bool           { return f.kind == 0 }  // 零值守卫

// 上游 provider 主动发了 close frame 的语义事件
type ProviderClose struct {
    Code   int     // 上游 close code，可能是 4xxx 业务自定义码
    Reason string
    Err    error
}

// Recv 返回的统一事件
type RecvEvent struct {
    Frame         *Frame          // upstream 发来的 data frame
    ProviderClose *ProviderClose  // upstream 主动 close 时填，Frame 为 nil
    Usage         *types.UsageEvent
    Origin        RealtimePayloadOrigin
    Err           error           // 真正的传输/解析错误；ProviderClose 不再走 Err
}

type RealtimeSession interface {
    // SendClient 入口做 runtime 校验：frame.IsZero() 或未识别 kind 返回
    // ErrInvalidFrame；这是 FrameKind(99) 这种 type conversion 攻击的兜底。
    SendClient(ctx context.Context, frame Frame) error
    Recv(ctx context.Context) (RecvEvent, error)

    // Detach / Abort 签名保持原样（void 返回）。
    // 本轮只重塑 frame/event 表达，不顺手扩大 session 业务能力边界变更。
    Detach(reason string)
    Abort(reason string)
    SetTurnObserverFactory(factory TurnObserverFactory)
}

var ErrInvalidFrame = errors.New("session: invalid frame (zero value or unknown kind)")
```

设计要点：

- **不导出 `kind` 字段** = 调用方写不出 `session.Frame{kind: 99}` 字面量
- **只有两个构造器** = 调用方只能拿 `NewTextFrame` / `NewBinaryFrame` 这两个出口
- **`FrameKind(99)` 仍然合法**，但它只能被赋值给独立变量；构造 `Frame` 必须经构造器，而构造器内部硬编码 `kind: FrameKindText` / `kind: FrameKindBinary`
- **runtime 校验在 `SendClient` 入口**：`frame.IsZero() || frame.kind > FrameKindBinary` 时返回 `ErrInvalidFrame`。这一关把"调用方用零值 Frame{} 或构造器之外的途径"全堵住
- **`runtime/session` 包不 import `wsconn`**：relay 层负责把 `session.FrameKind` 映射到 `wsconn.MessageType`（两个值对一）

#### types.UsageEvent 扩展（Source / BillingBasis）

`RecvEvent.Usage` 是 `*types.UsageEvent`。OpenAI Realtime 实际能拿到的 usage 有两条来源：

| Source | 触发事件 | 计费维度 |
|---|---|---|
| `realtime_response` | `response.done.response.usage` | tokens（input/output 分桶） |
| `input_audio_transcription` | `conversation.item.input_audio_transcription.completed.usage` | duration（音频秒数）或 tokens（取决于模型） |

只用 `*types.UsageEvent` 本体不带 source 标记会让 actor 不知道这条 usage 该按 token 还是 duration 计费、是否已纳入定价表。在 `types` 包补两个不透明的小枚举：

```go
// types/realtime_usage.go（types 包扩展，session 包不直接依赖）

type UsageSource string

const (
    UsageSourceRealtimeResponse        UsageSource = "realtime_response"
    UsageSourceInputAudioTranscription UsageSource = "input_audio_transcription"
)

type UsageBillingBasis string

const (
    UsageBillingBasisTokens   UsageBillingBasis = "tokens"
    UsageBillingBasisDuration UsageBillingBasis = "duration"
)

// types.UsageEvent 增补字段：
//   Source          UsageSource
//   BillingBasis    UsageBillingBasis
//   ProviderEventID string  // optional；上游 event_id，用于幂等/诊断，不作为 settlement identity
//   ResponseID      string  // optional；response.done 对应 response.id
//   ItemID          string  // optional；input_audio_transcription 对应 item_id
//   DurationSeconds float64 // BillingBasisDuration 时填写；tokens basis 必须保持 0
//   provider 在解析 OpenAI event 时必须填好 Source / BillingBasis，并按来源尽量填关联 ID
```

字段语义：

- `Source=realtime_response, BillingBasis=tokens`：来自 `response.done.response.usage`，`ResponseID` 取 `response.id`，`ProviderEventID` 取上游 event id（如果有）。
- `Source=input_audio_transcription, BillingBasis=tokens`：来自 `conversation.item.input_audio_transcription.completed.usage` 的 token usage variant，`ItemID` 取上游 item id。
- `Source=input_audio_transcription, BillingBasis=duration`：来自 `conversation.item.input_audio_transcription.completed.usage` 的 duration usage variant，`DurationSeconds` 填音频时长秒数。

这些字段只表达**来源、计费维度和关联关系**。`ProviderEventID` / `ResponseID` / `ItemID` 不能替代 ResponsesWS turn settlement identity，也不能让 provider 直接触发 truth 写入；它们用于 actor 归属、幂等辅助、日志和诊断。provider-originated usage 只有在存在 pending 或 active turn 时才有 quota 语义；若没有可归属 turn，actor 只能记录 protocol/diagnostic metric，不能凭关联 ID 自行创建结算事实。最终扣费仍必须由 actor 在 turn finalize 时把累积 usage 合并进 quota，并通过 `Quota -> SettlementEnvelope -> ApplySettlement` 这条统一链路落 truth（见 [Billing / Usage 结算架构](./billing-settlement-architecture.md)）。

为什么不抽更复杂的 schema：actor 只需要"按什么维度结算 + 该模型是否支持该维度的定价 + 这条 usage 归属于哪个 provider 业务对象"这三个语义。其余 usage 细节（tokens 桶、cached/audio/text breakdown）仍走 `types.UsageEvent` 既有字段，不重新发明账务结构。

#### Frame payload ownership 契约

```text
- NewTextFrame / NewBinaryFrame 调用时，调用方移交 payload 所有权
- Frame 构造后 payload 视为 immutable
- Payload() 返回只读语义 []byte，调用方不得修改
- 跨 goroutine / channel 投递前，由投递方负责 copy
- 如确需防御性复制，调用 ClonePayload()；Payload() 不隐式 clone
```

不默认每次 `Payload()` 都 clone：realtime audio frame 体积可能很大（音频 chunk 数十 KB 起步），隐式复制成本不可接受。所有权契约的工程成本只是注释 + review，比 per-call clone 成本低一个量级。

#### RecvEvent 字段优先级（调用方处理顺序）

**顶层 error 与 RecvEvent 字段的互斥契约**：

> `Recv(ctx) (RecvEvent, error)` 的**顶层 error 只表示"没有 event 可消费"**（典型值：`ErrSessionClosed`、`context.Canceled`、`context.DeadlineExceeded`）。
>
> 互斥规则：
>
> - 顶层 error **非 nil** 时，RecvEvent **必须全零**（所有字段为 nil/零值）
> - RecvEvent **任一字段非空**时，顶层 error **必须 nil**
> - 业务错误（如 provider parse failure、transport write error 但已读到 payload）走 `RecvEvent.Err`，不走顶层 error
>
> 调用方先检顶层 error；非 nil 直接处理（通常退出 Recv 循环）；nil 再按下方字段优先级处理 RecvEvent。

字段优先级（顶层 error == nil 时）：

1. **`Frame` 非 nil** → 先转发到下游（与现有 `ClientPayloadError` 的"payload 是 authoritative wire frame"语义一致）
2. **`Usage` 非 nil** → 交给 actor settlement；与 `Frame` 并存时在 Frame 转发之后处理，单独出现时投递 `ProviderUsageObserved`（**不写 downstream**）；与 `ProviderClose` / `Err` 互斥，不会与下两条同时出现
3. **`ProviderClose` 非 nil** → 把上游 close code 映射到下游 close。**必须通过 wire code 白名单校验**（见下方 [Provider close 转发](#provider-close-转发)），不能让任意 int 直上 wire
4. **`Err` 非 nil** → 业务错误处理（log、metrics、向下游 Abort）

允许的组合：

| 组合 | 含义 | 处理方式 |
|---|---|---|
| 只 `Frame` 非 nil | 正常 upstream data | 转发 Frame |
| `Frame` + `Usage` | usage-bearing upstream data（如 `response.done.response.usage`） | 先转发 Frame，再把 Usage 交给 actor settlement |
| 只 `Usage` 非 nil | usage-only accounting event（如 transcription completed 不下发原 event 时） | 投递 `ProviderUsageObserved` 给 actor；**不写 downstream** |
| 只 `ProviderClose` 非 nil | upstream 主动 close（peer close 转译） | realtime pass-through；ResponsesWS 走 actor settlement |
| 只 `Err` 非 nil | 传输/解析失败，无 authoritative payload 可转发 | 投递 provider business error |
| `Frame` + `Err` | 已有 authoritative frame，但 provider 同时发现 transport/parse 错误 | 先转发 Frame，再处理 Err；**不得同时携带 Usage** |

不允许的组合：

- `ProviderClose` 与 `Frame` / `Usage` / `Err` 任一同时非 nil（provider close 是终态事件）
- **`Usage` 与 `Err` 同时非 nil**（usage 是已确认账务事实，与异常语义混在同一 event 会让 actor 路径分叉）
- **`Frame` + `Usage` + `Err` 三者同时非 nil**（同上）
- 顶层 error 非 nil 且 RecvEvent 任一字段（含 `Usage`）非零

**业务结果状态 vs 传输错误的边界**（关键）：

> `response.done.status` 为 `failed` / `incomplete` / `cancelled` 时，仍作为 `Frame + Usage` 投递，**不**转成 `RecvEvent.Err`。错误细节留在 OpenAI payload 的 `response.status` / `status_details` 内。

理由：`response.done` 是业务 data event，actor 既要看到原始 wire event（下发给客户端），也要看到 usage（参与 settlement）。如果 provider 提前把业务失败转成 `Err`，会让 usage 字段被禁止携带（按 Usage+Err 互斥），导致漏帐。**只有传输错误、解析失败、provider 业务异常**（拿不到 authoritative payload）才走 `Err`；业务结果状态留 Frame payload。

**Usage 字段规则**：

- `Usage` 与 `Frame` 可并存（payload-with-usage 场景）或单独出现（usage-only event）
- `Usage` 与 `ProviderClose` 互斥（close 是终态）
- `Usage` 与 `Err` 互斥（usage 是已确认账务，Err 是异常路径；详见上方"业务结果状态 vs 传输错误"边界说明）
- 顶层 error 非 nil 时 `Usage` 必须 nil
- 一次 Recv 最多携带一个 `Usage`；provider 若需要 emit 多个 usage，分多次 Recv 投递（**不**引入 `Usages []` 字段，保持 RecvEvent 单 event 形态）
- actor 处理顺序：`Frame → Usage → ProviderClose / Err`（`Usage` 与后两者互斥，实际不会三者并存）

关键点：

- **`FrameKind` 只有 Text/Binary**，没有 Close/Ping/Pong。Close 走 ProviderClose 事件，Ping/Pong 由 wsconn 内部处理业务感知不到
- **`runtime/session` 包不 import `wsconn`**，避免 session 层绑定具体传输实现。relay 层负责把 `session.FrameKind` 映射到 `wsconn.MessageType`（两个值对一）
- **`runtime/session` 包不 import gorilla**。`mt int` 时代里 mt 值是 gorilla 常量这件事直接消失
- `responsesws.SendPreflightCapable`、`GracefulDetachCapable` 这类 optional capability 扩展模式继续保留

迁移工作量：3 个 provider（openai/codex/xunfei）+ 2 个 relay handler + 所有 RealtimeSession 相关测试都要同步改 `SendClient`/`Recv` 调用形态。粗估 SendClient 调用点 ~40 处、Recv 调用点 ~30 处、Frame/RecvEvent 字段访问 ~50 处、测试 mock 重写 ~80 处，合计 **~200+ 行调用点更新**；折合 **3-5 人日**（一次性切换无双轨期）。

## Provider close 转发

provider 内部用 `wsconn.ManagedConn` 管理上游连接；当 wsconn 探测到 upstream peer close（`CloseInfo.Kind == CloseKindPeerClose`）时，**provider 实现把它转成 `session.RecvEvent{ProviderClose: ...}`**，而不是直接等同于 provider-side ManagedConn 关闭。

```go
// providers/openai/realtime_session.go
func (s *Session) onProviderClosed(info wsconn.CloseInfo) {
    if info.Kind == wsconn.CloseKindPeerClose {
        s.emit(session.RecvEvent{
            ProviderClose: &session.ProviderClose{
                Code:   int(info.Code),
                Reason: info.Reason,
                Err:    info.Err,
            },
            Origin: session.RealtimePayloadOriginProvider,
        })
        return
    }
    // 非 peer close（read error / write error / handler panic / pong miss 等）
    // 是 transport 失败，包装成业务错误
    s.emit(session.RecvEvent{
        Err: mapTransportCloseToProviderError(info),
    })
}
```

#### realtime（pass-through）：Recv 调用点直接 forward

`/v1/realtime` 是 pass-through 形态，没有 turn / quota / lease，可以在 Recv 调用点直接把 ProviderClose 映射为 downstream close：

```go
// relay/realtime.go
ev, err := session.Recv(ctx)
if err != nil {
    // 顶层 error：没有 event 可消费（详见 RecvEvent 互斥契约）
    return
}
if ev.ProviderClose != nil {
    // upstream 用某个 close code 通知下游：转发前必须通过 wire code 白名单校验
    code := wsconn.SanitizeWireCloseCode(ev.ProviderClose.Code)
    // SanitizeWireCloseCode（详见"Wire close code 白名单"小节）：
    //   - 允许：1000-1003、1007-1014、3000-4999 → 原样返回
    //   - 禁止：<1000、1004、1005、1006、1015、>4999 → 替换为 1011 InternalServerErr
    //   - 入参是 int（兼容 provider 自定义码 4408/4499），出参是 CloseCode
    //   - 任意 int 不能直接 wsconn.CloseCode(...) 强转上 wire

    client.Close(wsconn.CloseInfo{
        Kind:   wsconn.CloseKindGracefulShutdown,
        Code:   code,
        Reason: ev.ProviderClose.Reason,
        Err:    ev.ProviderClose.Err,
    })
    return
}
```

#### ResponsesWS：ProviderClose 必须进 actor

ResponsesWS **不能**像 realtime 那样在 Recv 调用点直接 `client.Close`。理由：当前 `relay/responses_ws.go:1989-2010` 显示 provider close 需要：

- 区分 `pendingAttempt` vs `activeAttempt` 状态（pending 期间收到 close 要缓冲）
- 触发 `finalizeActiveAttempt` 完成 settlement / quota finalize
- 调 `clearActiveTurn` 清理 turn 上下文
- 调 `markDownstreamCloseSent` 防止后续重复发 close
- 释放 active / pending lease

这一整套状态机锁边界都在 actor 内。**外层在 Recv 调用点直接 client.Close 会绕过 settlement，造成 quota 漏帐**。

正确形态：ProviderClose 投递为 actor event，由 actor 单线程决策何时 forward：

```go
// providers/{openai,codex}/responses_ws_session.go —— provider 侧不变，仍 emit RecvEvent

// relay/responses_ws.go —— actor recv worker
func (a *ResponsesWSActor) recvLoop(ctx context.Context) {
    for {
        ev, err := a.session.Recv(ctx)
        if err != nil {
            a.PostReliable(ProviderRecvFailed{Err: err})  // 顶层 error → actor 处理 session 退出
            return
        }
        if ev.Frame != nil {
            // frame 携带可能的 usage（如 response.done.response.usage）：一次性投递，actor 串行处理 frame -> usage 序
            a.PostReliable(ProviderDownstreamFrame{Frame: ev.Frame, Usage: ev.Usage, ReceivedAt: time.Now()})
        } else if ev.Usage != nil {
            // usage-only event（如 input_audio_transcription.completed.usage 单独到达）：
            // 不能挂在 ProviderDownstreamFrame 上，否则会被 Frame==nil 分支吞掉。投递独立 actor event。
            a.PostReliable(ProviderUsageObserved{Usage: ev.Usage, ReceivedAt: time.Now()})
        }
        if ev.ProviderClose != nil {
            // 不在这里 client.Close —— 投递为 actor event，actor 结合 attempt/lease 状态决定何时 forward
            a.PostReliable(ProviderClosed{Code: ev.ProviderClose.Code, Reason: ev.ProviderClose.Reason, Err: ev.ProviderClose.Err})
            return  // provider session 已 close，recvLoop 终止
        }
        if ev.Err != nil {
            a.PostReliable(ProviderBusinessError{Err: ev.Err})
        }
    }
}

// actor 在 handle 路径里做映射（伪代码）
func (a *ResponsesWSActor) handleProviderClosed(ev ProviderClosed) {
    code := wsconn.SanitizeWireCloseCode(ev.Code)
    if a.pendingAttempt != nil {
        a.bufferPendingProviderEvent(...)  // 同当前代码 line 1989
        return
    }
    if a.activeAttempt != nil {
        a.activeAttempt.MarkCompleted(time.Now())
    }
    a.finalizeActiveAttempt()       // settlement / quota finalize
    a.clearActiveTurn()              // turn 上下文清理
    a.markDownstreamCloseSent()      // 防重发
    a.conn.Close(wsconn.CloseInfo{
        Kind:   wsconn.CloseKindGracefulShutdown,
        Code:   code,
        Reason: ev.Reason,
        Err:    ev.Err,
    })
}

// usage-only event 的 actor handler（伪代码）
func (a *ResponsesWSActor) handleProviderUsageObserved(ev ProviderUsageObserved) {
    // 规则（与 ProviderDownstreamFrame 区分）：
    //   - 不写 downstream（usage-only 事件本身不下发；对应的 wire event 已在之前的 Frame 中下发或者 provider 选择不下发）
    //   - 不改变 lifecycle（不 MarkCompleted / 不 finalize / 不 clearActiveTurn）
    //   - 只更新 settlement 累积态（attempt 级 / turn 级 usage 累加）
    //   - 若 ev.Usage.Source == UsageSourceInputAudioTranscription 且当前模型未配置 transcription 计费，
    //     记录 metric usage_observed_unbilled（详见 metrics 节）并直接丢弃，不阻塞 actor
    a.recordObservedUsage(ev.Usage, ev.ReceivedAt)
}
```

**`session.ProviderClose.Code` 是任意 int**（provider 可以发任何业务自定义码，如 4408、4499），但 `wsconn.CloseCode` 出现在 wire 上必须合法。relay 层映射时**强制走 `SanitizeWireCloseCode`** 这个工具函数，不能裸 `wsconn.CloseCode(ev.ProviderClose.Code)`。CI grep 应该兜底拦下任何 `wsconn.CloseCode(...)` 的直接强转出现在 relay 层（除非通过 SanitizeWireCloseCode 走）。

这把两个维度分开：

- `wsconn.CloseInfo`：**某条具体 WS 连接**为什么关闭（transport 事实）
- `session.ProviderClose`：**provider 业务关闭语义**要如何传递给下游（业务事件）

之前用 `Recv` 返回 error 或把 close 假装成 data frame 来表达，是把这两件事硬塞进同一个返回通道。拆开后每一侧职责清晰，relay 层做的"把上游 close code 映射成下游 close code"这件事变成 realtime 简单字段拷贝、ResponsesWS 走 actor 单线程合成（与 settlement 同生命周期）。

## ResponsesWS 入口形态

ResponsesWS 的真实需求被 ReadInitial + actor 单线程模型直接满足，**不需要单独抽 Ingress 适配器**（之前设计的 `ResponsesWSIngress` 已删）。

```go
mc, _ := wsconn.AcceptManaged(...)

// 阶段 1：同步读首帧 + 轻量校验
firstCtx, cancel := context.WithTimeout(r.Context(), config.ResponsesWSFirstFrameTimeout())
defer cancel()
mt, payload, err := mc.ReadInitial(firstCtx)
if err != nil {
    // ReadInitial 内部用 LimitedReader；超限可发自定义 JSON error
    writeJSONError(mc, "first_frame_error", err)
    mc.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort, Reason: "first_frame_error"})
    return
}

// 轻量首帧校验：拒非 Text 帧 + 拒 JSON parse 失败。**不在这里做 quota / lease / RPM**
if mt != wsconn.TextMessage {
    mc.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindNormal, Code: wsconn.CloseUnsupportedData, Reason: "text_only"})
    return
}
parsed, err := responsesws.ParseRawResponsesCreateFrame(payload)
if err != nil {
    writeJSONError(mc, "invalid_response_create", err)
    mc.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindNormal, Code: wsconn.ClosePolicyViolation, Reason: "invalid_first_frame"})
    return
}

// 阶段 2：起 actor + Pump，首帧作为事件投递；heavy admission 在 actor 串行处理
actor := newResponsesWSActor(c, mc, pendingLease)
actor.Start()
pump := &wsconn.Pump{
    Conn:    mc,
    Handle:  actor.onClientFrame,       // 非阻塞 select post 到 actor channel
    OnClose: actor.onClientClose,       // close 后 turn ledger / quota finalize / lease release
}
go pump.Run(ctx)

if !actor.PostReliable(FirstTurnSetup{Frame: parsed, ReceivedAt: time.Now()}) {
    pendingLease.Release()
    actor.requestCloseIntent("first_turn_setup_not_queued")
}
```

#### 为什么删 Ingress

之前的 `ResponsesWSIngress` 设计承担两件事：

1. 首帧 gate（要求 admission 通过后才接收第 2 帧）
2. 后续帧的可靠投递（`BoundedReliableQueue` + 四种 outcome）

事实是：

- **第 1 件由 ReadInitial 直接覆盖** —— Pump 启动前没有读循环，物理上不会读到第 2 帧
- **第 2 件由 actor 自己的 `PostReliable` 覆盖** —— 当前 actor 已实现"事件投递不下/channel 满"的语义，actor 自己 close。再加一层 `BoundedReliableQueue` 等于双层 channel 队列，没有收益只有 ordering bug 风险

所以 Pump.Handle **直接绑** `actor.onClientFrame`，不需要中间 adapter。`actor.onClientFrame` 自己负责：

- payload 复制（Pump 复用 buffer）
- 非阻塞 select post 到 actor channel
- channel 满时 `Close(CloseKindBackpressure, 1013)`；actor closed 时丢弃事件，conn 由 `OnClose` 路径处理（两种语义分开，不合并 Kind）

这把"首帧 + 后续帧"两条路径统一到 actor 单一所有权，避免之前 Ingress / Actor 双状态机的并发态。

#### actor.onClientFrame 形态

```go
func (a *ResponsesWSActor) onClientFrame(ctx context.Context, mt wsconn.MessageType, payload []byte) {
    cloned := append([]byte(nil), payload...)
    event := ClientFrameEvent{MT: mt, Payload: cloned, ReceivedAt: time.Now()}

    // 非阻塞 select：Handle 必须立即返回（见"Handle 非阻塞契约"）
    select {
    case a.events <- event:
    case <-a.done:
        // actor 已退出；丢弃即可，conn 由 OnClose 路径处理
    default:
        // channel 满 = 客户端发得比 actor 处理快，按 backpressure 处理
        a.requestCloseIntent("client_frame_backpressure")
        a.conn.Close(wsconn.CloseInfo{
            Kind:   wsconn.CloseKindBackpressure,
            Code:   wsconn.CloseTryAgainLater,
            Reason: "client_frame_backpressure",
        })
    }
}
```

注意这段代码**生活在 `relay/responses_ws` 包里，不进 `common/wsconn`**。wsconn 永远不知道 actor / event / backpressure 这些业务概念。

`a.events` channel buffer 沿用当前 `responsesWSEventQueueSize = 128`，本方案不调整事件队列大小。是否额外加 bytes cap（例如按总 payload bytes 限制）待压测后再决定，不在首版范围。

## 业务层迁移形态

### providers/openai · providers/codex

```go
// 实现内部：
type Session struct {
    conn   *wsconn.ManagedConn
    pump   *wsconn.Pump
    // ... 业务状态
}

func (s *Session) start(ctx context.Context) error {
    conn, err := wsconn.DialManaged(ctx, url, header, wsconn.Config{
        Label:                  "openai-realtime",
        PingInterval:           config.ProviderPingInterval(),
        PongMissTimeout:        config.ProviderPongMiss(),
        InboundActivityTimeout: s.currentInboundActivityTimeout,  // 闭包：可根据业务状态动态返回
        ReadLimit:              config.ProviderReadLimit(),
        WriteTimeout:           config.ProviderWriteTimeout,
    })
    if err != nil {
        // err 是 *wsconn.DialError，业务可按 StatusCode 分类
        return err
    }
    s.conn = conn
    s.pump = &wsconn.Pump{
        Conn:    conn,
        Handle:  s.handleProviderFrame,             // < 1ms，仅向 channel post
        OnClose: s.onProviderClosed,                // 唯一 close 回调：metrics/log + actor 状态机迁移 + ProviderClose emit
    }
    go s.pump.Run(ctx)
    return nil
}
```

**RealtimeSession 业务能力边界不变；接口签名随 typed Frame / RecvEvent 一次性切换**（`SendClient`/`Recv`/`Detach`/`Abort`/`SetTurnObserverFactory`）。relay 层和 runtime/session 包看不到 ManagedConn —— 这是 provider 实现的内部细节。

`Detach` 和 `Abort` 通过不同 CloseKind 表达：

```go
// Detach: 优雅关闭，wsconn 会发 close frame
s.conn.Close(wsconn.CloseInfo{
    Kind:   wsconn.CloseKindGracefulShutdown,
    Code:   wsconn.CloseNormalClosure,
    Reason: "detach: " + reason,
})

// Abort: 异常路径，wsconn 跳过 close frame 立即释放
s.conn.Close(wsconn.CloseInfo{
    Kind:   wsconn.CloseKindAbort,
    Reason: "abort: " + reason,
})
```

握手失败时 `errors.As(err, &dialErr)` 拿到 `*wsconn.DialError`，按 `dialErr.StatusCode` 分类（404/426 → unsupported channel，401/403 → auth error，429 → rate limit，5xx → request failed），与当前 codex provider 错误分类逻辑保持一致。`dialErr.CloseInfo` 提供 dial-failed 场景的 CloseInfo 承载（Kind 固定为 `CloseKindDialFailed`），便于业务统一 metrics/log 入口。

### providers/xunfei

讯飞 chat 是 request/stream/close 一次性模式（拨号 → 发请求 → 流式收响应 → 关闭），不需要 ping/pong/idle watchdog；也不是 realtime 那种长连双工。**用同一个 ManagedConn**，Config 字段全填零值；流式响应在 Handle 内累积到 response builder，业务在另一 goroutine 监听完成信号触发主动 Close：

```go
type xunfeiResponseBuilder struct {
    mu       sync.Mutex
    chunks   []json.RawMessage
    completed chan struct{}    // 业务接收完成信号
}

conn, err := wsconn.DialManaged(ctx, url, header, wsconn.Config{
    Label: "xunfei-chat",
    // PingInterval / PongMissTimeout 全为 0 = 关闭
    // InboundActivityTimeout 为 nil = 关闭
    ReadLimit: config.XunfeiReadLimit(),
})
if err != nil { return err }

builder := &xunfeiResponseBuilder{completed: make(chan struct{})}

// 发请求
payload, err := json.Marshal(req)
if err != nil { return err }
if err := conn.WriteMessage(wsconn.TextMessage, payload); err != nil { return err }

// Pump.Handle 内累积响应到 builder；遇到结束标记 close(builder.completed)
pump := &wsconn.Pump{
    Conn:   conn,
    Handle: func(ctx context.Context, mt wsconn.MessageType, p []byte) {
        // Handle 仍 < 1ms：解析当前 chunk、append、检查 isLast
        if last := builder.appendAndCheckLast(p); last {
            close(builder.completed)
        }
    },
    OnClose: func(info wsconn.CloseInfo) {
        // 收到 peer close 或异常 close 时通知 builder
        builder.notifyClose(info)
    },
}
go pump.Run(ctx)

// 主协程等业务完成信号
select {
case <-builder.completed:
    conn.Close(wsconn.CloseInfo{
        Kind:   wsconn.CloseKindNormal,
        Code:   wsconn.CloseNormalClosure,
        Reason: "stream completed",
    })
case <-ctx.Done():
    conn.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort, Reason: "ctx cancelled"})
}
```

关键点：

- **Handle 必须非阻塞**：只做 append + isLast 检查，json.Unmarshal 重活在主协程消费 builder 时做
- **完成判定在业务层**：wsconn 不知道"什么是 last frame"，这是讯飞协议语义；builder 拿到 isLast=true 后业务主动 Close
- **会发 close frame 给 provider**：`CloseKindNormal` 走"发 close frame"路径，讯飞协议希望客户端主动 close 通知会话结束
- **错误路径**：Pump.OnClose 通过 CloseInfo.Kind != Normal 通知 builder 异常退出

讯飞场景验证了一件事：ManagedConn 的 **Config 字段 zero-value 必须无副作用**。这是设计期就要保证的属性，不是事后补丁。

### relay/realtime

```go
// 入口（原 relay/realtime.go 中的 package-level var upgrader 删除，
// CheckOrigin 以纯函数 realtimeWebSocketOriginAllowed 作为闭包传入）：
mc, err := wsconn.AcceptManaged(w, r, wsconn.Config{
    Label:                  "client-realtime",
    PingInterval:           config.ClientPingInterval(),
    PongMissTimeout:        config.ClientPongMiss(),
    InboundActivityTimeout: config.ClientInboundActivity,         // func() time.Duration
    ReadLimit:              config.ClientReadLimit(),
}, wsconn.AcceptOptions{
    CheckOrigin:       realtimeWebSocketOriginAllowed,
    ResponseHeader:    websocketUpgradeResponseHeader(r),
    EnableCompression: false,
    Subprotocols:      allowedClientWebSocketSubprotocols(r),
})
if err != nil { /* 错误响应已发，直接 return */ }

// 业务编排
actor := newRealtimeActor(c, mc, session, ...)
pump := &wsconn.Pump{
    Conn:    mc,
    Handle:  actor.handleClientFrame,   // < 1ms，仅向 actor channel post
    OnClose: actor.onClientClose,       // 唯一 close 回调：metrics + 状态机迁移
}
go pump.Run(ctx)
```

Actor 持有 client-side `*wsconn.ManagedConn` + provider-side `RealtimeSession`。Actor 自己编排 detach/abort、错误 payload、quota observer。**不**引入 Bridge 抽象。

### relay/responses_ws

同 realtime 形态，但 Actor 状态机更复杂（turn admission、quota 预扣、ambiguous admission、fallback、settlement、`responsesws.SendPreflightCapable` 调用）。**入口分两段：先用 `ReadInitial` 同步读首帧 + 轻量 parse，然后立刻启动 actor + Pump，首帧作为 `FirstTurnSetup` 事件交给 actor**；所有 heavy admission（quota / RPM / model / lease 升级）走 actor 单线程处理（见 [ReadInitial 的特殊性](#readinitial-的特殊性)、[ResponsesWS 入口形态](#responsesws-入口形态)）。

```go
mc, err := wsconn.AcceptManaged(w, r, wsconn.Config{
	    Label:                  "client-responses-ws",
	    PingInterval:           config.ResponsesWebsocketClientPingInterval(),
	    PongMissTimeout:        config.ResponsesWebsocketClientPongMissTimeout(),
	    InboundActivityTimeout: func() time.Duration {
	        return config.ResponsesWebsocketClientInboundActivityTimeout()
	    },
}, wsconn.AcceptOptions{
    CheckOrigin:       responsesWSOriginAllowed,
    Subprotocols:      responsesWSAllowedSubprotocols(r),
    EnableCompression: false,
})
if err != nil { return }

// 阶段 1：同步读首帧 + 轻量校验
firstCtx, cancel := context.WithTimeout(r.Context(), config.ResponsesWSFirstFrameTimeout())
defer cancel()
mt, payload, err := mc.ReadInitial(firstCtx)
if err != nil {
    writeResponsesWSFirstFrameError(mc, err)
    mc.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindNormal, Code: wsconn.CloseInvalidFramePayloadData, Reason: "bad first frame"})
    return
}
if mt != wsconn.TextMessage {
    mc.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindNormal, Code: wsconn.CloseUnsupportedData, Reason: "text_only"})
    return
}
parsed, err := responsesws.ParseRawResponsesCreateFrame(payload)
if err != nil {
    writeJSONError(mc, "invalid_response_create", err)
    mc.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindNormal, Code: wsconn.ClosePolicyViolation, Reason: "invalid_first_frame"})
    return
}

// 阶段 2：起 actor + Pump，首帧投递为 FirstTurnSetup 事件；
// quota / RPM / model 校验 / lease 升级全部在 actor 串行处理
actor := newResponsesWSActor(c, mc, pendingLease)
actor.Start()
pump := &wsconn.Pump{
    Conn:    mc,
    Handle:  actor.onClientFrame,       // 非阻塞 select post 到 actor channel
    OnClose: actor.onClientClose,       // close 后 turn ledger / quota finalize / lease release
}
go pump.Run(ctx)

if !actor.PostReliable(FirstTurnSetup{Frame: parsed, ReceivedAt: time.Now()}) {
    pendingLease.Release()
    actor.requestCloseIntent("first_turn_setup_not_queued")
}
```

`ResponsesWSIOBridge` 中负责 read pump / ping loop / send worker 的部分**整体替换**为 ManagedConn + ReadInitial + Pump 调用；send worker 中真正属于业务调度（attemptID/upstreamSessionGeneration 校验、send outcome 投递）的部分保留并搬到 Actor。Pump.Handle 直接绑 `actor.onClientFrame`，**不再有中间 Ingress 适配器**。

`relay/responses_ws.go` 的总行数应从 ~3000 行降到 ~2300-2400 行，下降的 ~600-700 行全部是传输层重复发明的代码；剩余的业务复杂度（admission/quota/settlement）保持不动。**无新增 `ingress.go`**（之前估计的 ~150-200 行因 Ingress 设计撤销而清零）。

### common/requester/ws_*.go 的命运

现有 `common/requester/ws_*.go`（`ws_client.go`、`ws_writer.go`、`ws_close.go`、`ws_activity.go`、`ws_read.go`、`ws_control_writer.go`、`ws_error.go`、`ws_requester.go`、`ws_reader.go`、`ws_active_guard.go`）及同包的 `realtime_session_proxy.go`：

- 内容被吸收进 `common/wsconn/`（拆分为 `managed.go`、`pump.go`、`accept.go`、`dial.go`、`close.go`、`message.go`、`http.go`、`control.go`、`watchdog.go`）
- 原 `common/requester/ws_*.go` 文件**删除**，不留兼容 wrapper
- `realtime_session_proxy.go` 的 IO 部分迁入 wsconn，业务部分（provider session 编排）回到 `providers/openai`、`providers/codex` 对应实现

### common/requester/SendWSJsonRequest 的命运

当前 `ws_requester.go:119` 的 `conn.WriteJSON(data)` 在新边界下没有对应路径。**`SendWSJsonRequest` 改写为 caller 显式 marshal**：

```go
// before
func SendWSJsonRequest(conn *websocket.Conn, data any, ...) {
    err := conn.WriteJSON(data)
    ...
}

// after — caller 端
payload, err := json.Marshal(data)
if err != nil { return err }
if err := conn.WriteMessage(wsconn.TextMessage, payload); err != nil { return err }
```

`ManagedConn` **不**提供 `WriteJSON`。理由：ManagedConn 停在字节边界。WriteJSON 是 payload 格式，加了它今天就会有人问要不要 WriteProtobuf、WriteMsgPack。架构契约越纯越好守，三行明确说出"序列化 + 发文本帧"两件事，比一个混合返回值更可调试。

### common/authutil/credential.go

只用 `websocket.Subprotocols(r)` 和 `websocket.IsWebSocketUpgrade(r)`，没有 WS IO。改用 `wsconn.Subprotocols(r)` / `wsconn.IsUpgrade(r)` 即可，不开 import 特例。

### 进程级 Graceful Shutdown

收到 SIGTERM / 主动 shutdown 时遍历所有活跃 ManagedConn 走 `CloseKindGracefulShutdown` 路径，wsconn 会发 1001 GoingAway close frame 给客户端：

```go
func (s *Server) Shutdown(ctx context.Context) error {
    for _, conn := range s.activeConns() {
        conn.Close(wsconn.CloseInfo{
            Kind:   wsconn.CloseKindGracefulShutdown,
            Code:   wsconn.CloseGoingAway,
            Reason: "server_shutdown",
        })
    }
    // 等所有 conn.Done() 或 ctx 超时
    return s.waitDrain(ctx)
}
```

CloseKind 不为 SIGTERM 引入新枚举值（`CloseKindServerShutdown` 不重新引入）；`CloseKindGracefulShutdown` + `Reason: "server_shutdown"` 已足够区分场景。Kind 越少越好，业务靠 Reason / log / metrics 进一步细分。

## Liveness 语义拆分

落地前的旧配置 `client_pong_timeout_ms` 实际行为是 inbound activity-based —— 任何来自客户端的消息都会刷新 `lastClientInboundActivity`，触发条件是 `time.Since(lastActivity) >= pongTimeout`。这是 inbound activity timeout 的语义，但名字像 pong miss。当前实现已拆分为 ResponsesWS 专用的 `responses_websocket_client_ping_interval_ms`、`responses_websocket_client_pong_miss_timeout_ms` 和 `responses_websocket_client_inbound_activity_timeout_ms`，代码入口分别是 `config.ResponsesWebsocketClientPingInterval()`、`config.ResponsesWebsocketClientPongMissTimeout()` 和 `config.ResponsesWebsocketClientInboundActivityTimeout()`。

新架构里**强制拆分为两个独立配置**：

| 配置 | 类型 | 语义 | 实现要点 |
|---|---|---|---|
| `PongMissTimeout` | `time.Duration` | 发出 ping 后未收到**对应** pong 的最大等待 | 必须追踪 ping generation / last pong；伪 inbound 流量不能让它误判活着。详见下方"Pong generation 算法" |
| `InboundActivityTimeout` | `func() time.Duration` | 任意 inbound control frame 或完整 data message 多久未到达判死 | 闭包形式支持业务态动态调整（例如 `config.ResponsesWSInboundActivity()`）；任何 inbound control frame 或完整 data message 都刷新；**outbound ping/data 不计入**。nil 或返回 0 = 关闭 |

旧名字 `client_pong_timeout_ms` **不沿用**。新配置：

```text
realtime_websocket_client_ping_interval_ms
realtime_websocket_client_pong_miss_timeout_ms
realtime_websocket_client_inbound_activity_timeout_ms

responses_websocket_client_ping_interval_ms
responses_websocket_client_pong_miss_timeout_ms
responses_websocket_client_inbound_activity_timeout_ms
```

由于本方案前提是"不考虑兼容性"，旧名字直接消失。如果旧配置在生产环境有值，迁移时需要部署文档明确说明。

#### Pong generation 算法（精确实现）

"对应 pong" 不能是模糊概念，否则伪 pong（peer 路由器/网卡的偶发 pong）会让 PongMissTimeout 形同虚设。算法用**每次 ping 独立 arm 的 PongMiss timer + generation 校验**，PingInterval 与 PongMissTimeout 完全正交：

```go
type pongState struct {
    mu                sync.Mutex   // 串行化 ticker / PongHandler / timer callback 三个入口
    gen               uint64       // 单调递增 generation counter
    awaiting          bool         // 当前是否有 outstanding ping
    outstandingGen    uint64       // 当前 outstanding 的 generation
    outstandingTimer  Timer        // 当前 ping 的 PongMiss timer（匹配 pong 后 Stop）
    lastMatchedPongAt time.Time    // 上一次匹配 outstanding 的 pong 到达时刻（仅 metrics）
}
```

**`PongMissTimeout == 0`（ping-only 分支）**：

`gen` 仍由 `ps.mu` 保护，因为它是 `pongState` 字段；写失败归 `CloseKindWriteError`，与标准分支一致。

```text
ticker fires:
    ps.mu.Lock()
    ps.gen += 1
    gen := ps.gen
    ps.mu.Unlock()

    if err := controlWriter.EnqueuePing(bigEndianUint64(gen)); err != nil {
        conn.Close(CloseInfo{Kind: CloseKindWriteError, Err: err})
        return
    }
    // 不设置 awaiting，不 arm PongMiss timer，
    // 不参与 PongMiss 判断；generation 只用于 metrics
```

此分支允许"发 ping 探测网络中间设备状态、但不监控 pong 回执"的场景。

**`PongMissTimeout > 0`（标准分支）**：

所有访问 `pongState` 字段必须在 `ps.mu` 保护下进行；`conn.Close` **在锁外**调用，避免锁交叉。**`controlWriter.EnqueuePing` 失败时必须 Stop 已 arm 的 timer 并清理 outstanding 状态**，否则 timer 会在 PongMissTimeout 后误触发 `CloseKindPongMiss`——但实际原因是 write error。

PingInterval 到达：

```text
ticker fires:
    ps.mu.Lock()
    if ps.awaiting {
        ps.mu.Unlock()
        return  // skip — 不发新 ping，等当前 outstanding 的 PongMiss timer 决定
    }
    ps.gen += 1
    gen := ps.gen
    ps.awaiting        = true
    ps.outstandingGen  = gen
    timer := clock.AfterFunc(PongMissTimeout, func() {
        // timer callback：短临界区检查 generation 一致性，确认后**锁外** Close
        shouldClose := false
        ps.mu.Lock()
        if ps.awaiting && ps.outstandingGen == gen {
            ps.awaiting        = false
            ps.outstandingTimer = nil
            shouldClose        = true
        }
        ps.mu.Unlock()
        if shouldClose {
            conn.Close(CloseInfo{Kind: CloseKindPongMiss, Reason: "pong_miss"})
        }
    })
    ps.outstandingTimer = timer
    ps.mu.Unlock()

    if err := controlWriter.EnqueuePing(bigEndianUint64(gen)); err != nil {
        // ping 写失败不能被误归为 PongMiss：先 Stop timer 防止误触发，再清理 outstanding 状态
        timer.Stop()
        ps.mu.Lock()
        if ps.awaiting && ps.outstandingGen == gen {
            ps.awaiting        = false
            ps.outstandingTimer = nil
        }
        ps.mu.Unlock()
        // 归类为 WriteError；CAS first-write-wins 保证不覆盖更早的 close 原因
        conn.Close(CloseInfo{Kind: CloseKindWriteError, Err: err})
    }
```

**控制 writer 的写失败归类边界**：

- `EnqueuePing` 同步返回 error（入队失败、queue 已关闭）→ ticker 路径归 `CloseKindWriteError`（如上）
- `EnqueuePing` 入队成功，真实写在 control writer goroutine 内失败 → 由 control writer 路径归 `CloseKindWriteError`
- **任何情况下，ping 写失败都不会表现为 `CloseKindPongMiss`**

PongHandler 收到 pong：

```text
pongHandler(payload):
    markInboundActivity(clock.Now())              // 所有 pong 都算 inbound activity（走独立锁路径）
    if len(payload) != 8 { return }
    pongGen := parseUint64(payload)

    ps.mu.Lock()
    if ps.awaiting && pongGen == ps.outstandingGen {
        ps.awaiting        = false
        if ps.outstandingTimer != nil {
            ps.outstandingTimer.Stop()             // 资源 hygiene；防止 callback 积压
            ps.outstandingTimer = nil
        }
        ps.lastMatchedPongAt = clock.Now()
    }
    ps.mu.Unlock()
    // 不匹配的 pong 静默忽略，不报错也不刷新 lastMatchedPongAt
```

这保证：

- **所有 pong** 都计入 inbound activity（PongHandler 第一行）
- **只有匹配 generation 的 pong** 才解除 PongMiss outstanding 状态
- 同一时刻最多一个 outstanding ping（`if awaiting: skip` 分支），不叠加
- **PingInterval 与 PongMissTimeout 完全正交**：per-ping timer 在 ping 发出时 arm，到期才 fire；不依赖 ticker 检测
- 伪 pong（peer 路由器、网卡缓存）payload 不会匹配 generation，不影响 PongMiss 判定
- 匹配 pong 到达后立即 `Stop()` outstanding timer，不留 callback 积压

#### pongState 并发保护

ticker（liveness goroutine）、PongHandler（gorilla read 路径的 control-handler 回调）、per-ping timer callback（`Clock.AfterFunc` 派发的独立 goroutine）三个入口都会访问 `pongState`，必须串行化：

- **`pongState.mu` 保护 `awaiting / outstandingGen / outstandingTimer / gen`**
- ticker / PongHandler / timer callback 都在持锁下读写这些字段
- timer callback 的临界区只做"generation 是否仍匹配 + awaiting 是否仍 true"短判断；**确认要触发 PongMiss 时在锁外调用 `conn.Close`**，避免持锁触发 close 链路造成锁交叉
- ticker 路径 `EnqueuePing` 在锁外执行；**入队失败时必须 `timer.Stop()` 并清理 outstanding 状态**，再锁外 `conn.Close(CloseKindWriteError)`，避免 timer 在 PongMissTimeout 后误触发 PongMiss
- `markInboundActivity` 走独立的 inbound activity 锁路径，不进 `pongState.mu`

不采用"所有事件进入同一个 liveness goroutine"方案的原因：PongHandler 是 gorilla read 路径触发的，强制 goroutine 切换会让 read loop 等 channel；用短临界区互斥锁更简单。

验收线必须覆盖：
- `PongMissTimeout == 0` 分支：持续发 ping、不因缺 pong 关闭、不会被 awaiting 卡住后续 ping
- 发出 generation=1 的 ping，注入 payload 不匹配的伪 pong；断言 PongMissTimeout 仍按时触发
- 匹配 pong 到达后 outstanding timer 被 `Stop()`，callback 不再触发
- 匹配 pong 到达后 `awaiting=false`；下一个 ticker 发新 ping（gen=2）
- markInboundActivity 在所有 pong（含不匹配的伪 pong）路径都被调用
- 并发场景：ticker / PongHandler / timer callback 高频交错触发，`-race` 下无数据竞争；任意时刻 `outstandingTimer` 与 `outstandingGen` 一致
- **`EnqueuePing` 失败归类测试**：mock control writer 让 `EnqueuePing` 返回 error，断言 (1) outstanding timer 已被 Stop，fake clock 推进到 `PongMissTimeout` 也不触发 callback；(2) `CloseInfo.Kind == CloseKindWriteError`，**不是** `CloseKindPongMiss`

#### turn-level deadline 不属于 wsconn

`providers/openai/realtime_session.go:628` 当前的 `currentReadTimeout()` 根据 `s.responsesWS` 与是否处于 active turn 返回不同值；同文件 `turnReadTimer`（684 行）用 `time.AfterFunc(openAIRealtimeReadTimeout, ...)` 在 turn 开始时单独武装一个 turn-level deadline。

这两类**业务态超时不放进 `Config.InboundActivityTimeout`**。`InboundActivityTimeout` 只承接"transport liveness 兜底"（任意 inbound 活动）。"turn 级 deadline"（一个 turn 多久没完成）继续留在 Actor / RealtimeSession 内部，通过：

```go
turnTimer := time.AfterFunc(openAIRealtimeReadTimeout, func() {
    s.conn.Close(wsconn.CloseInfo{
        Kind:   wsconn.CloseKindAbort,
        Reason: "turn_read_timeout",
    })
})
```

实现。wsconn 包内**永远不出现** "turn" 概念，这是架构契约 2 的硬约束。

## wsconn/wstest 测试支持

测试代码同样禁止 import gorilla，必须走 `common/wsconn/wstest`。**公共表面写死：`Option` 类型 + `Pair` / `Server` / `WithClock` 三个函数，共四个导出符号**：

```go
package wstest

// Option 同时适用于 Pair 和 Server；Server 也会构造 ManagedConn，
// 所以 fake clock 注入既要覆盖 Pair 也要覆盖 Server 入口的 dial/liveness 测试。
type Option interface {
    applyPair(*pairConfig)
    applyServer(*serverConfig)
}

// Pair 返回一对在内存中连通的 ManagedConn；
// 内部用 net.Pipe + httptest.Server + gorilla Upgrader 构造，
// 但对外只暴露 *wsconn.ManagedConn，gorilla 类型不泄漏。
func Pair(t testing.TB, opts ...Option) (client, server *wsconn.ManagedConn)

// Server 暴露一个 httptest.Server 让被测代码用 wsconn.DialManaged 真握手过来；
// 用于 DialError / handshake / TLS 路径覆盖。Server 内部 Upgrader 仅 wstest 可访问。
func Server(t testing.TB, handler func(*wsconn.ManagedConn), opts ...Option) (url string, cleanup func())

// 时间控制；同时适用于 Pair 和 Server 入口
func WithClock(clock wsconn.Clock) Option
```

**明确不公开**（落地需删除验收线、不出现在 wstest 包公开 API）：

- `DialHTTP`：DialError / TLS / handshake 测试直接用 `Server + wsconn.DialManaged` 组合
- `ForcePeerClose / ForceReadError / ForceWriteError / SimulatePongMiss / SimulateInboundIdle`：业务测试不该模拟任意网络故障；模拟 peer 行为通过 `Pair` 的另一端真发帧实现
- `CloseSpy / WireFrames / DeadlineRecorder`：close 决策表 / wire 抓帧 / deadline 调用次数都是 wsconn 内部实现验证，归到 `common/wsconn` 包内测试或 `internal/wsconntest`，不导出

**范围克制原则**：wstest 是测试支持，**不是网络仿真器**。下列能力**明确不做**：丢包/延迟注入、partial frame、TCP RST、可配置 latency profile、任意故障注入。

#### Clock 注入边界

`Config.Clock` 在 wstest 中可注入 fake clock，**只控制 Go runtime timer**：

- ping timer / ticker
- pong miss timer（每次 ping 独立 arm）
- inbound activity timer
- CloseInfo.At
- cleanup goroutine 等待窗口

`Config.Clock` **不控制** `gorilla WriteControl` 的 deadline 参数、`net.Conn.SetWriteDeadline` 和 `net.Conn.SetReadDeadline`——这些 deadline 进 kernel，fake clock 影响不到。涉及 socket deadline 的测试用 mock `net.Conn` 或极短真实 timeout，文档明确这条边界。

实现层面：`wstest` 内部用 `net.Pipe()` + `httptest.Server` + 真 gorilla 构造连通对，但**对外只暴露 `*wsconn.ManagedConn`**。

## 验收线（CI 强制）

落地完成的判定标准：

1. **depguard 同时覆盖生产和测试**，除 `common/wsconn/**` 和 `common/wsconn/wstest/**` 外无 `github.com/gorilla/websocket` import。启用 depguard 后**必须实测**至少一处违规 import 能被 lint 拦下，不能假设配置生效。
2. `grep -rE 'websocket\.' --include='*.go'` 在 `common/wsconn/**` 外**不出现**（兜底 depguard 的常量泄漏）。
3. **CloseInfo first-write-wins 并发测试**：多 goroutine 并发触发不同 Kind 的 Close。断言：
   - `CloseInfo().Kind` 等于首个 CAS 成功者
   - **失败者 Close 调用在 1ms 内返回**，不被 cleanup 的 close-frame deadline 阻塞
4. **Close 决策表测试**（按 Kind 分别断言）：
   - `CloseKindNormal` / `CloseKindGracefulShutdown`：peer 端收到 exactly 1 个 close frame，code 在 wire 白名单内（`Normal` 默认 1000，`GracefulShutdown` 默认 1001）
   - `CloseKindInboundIdle` / `CloseKindBackpressure`：best-effort 发 close frame，共用短 deadline 模板（~500ms-1s）
     - `InboundIdle` 默认 1001；`Backpressure` 默认 1013
     - socket 已坏发不出去时**不重试**，吞掉错误，cleanup 仍按时完成
     - 短 deadline 内能发出时 peer 收到 exactly 1 个对应 code 的 close frame
   - `CloseKindAbort` / `CloseKindPongMiss` / `CloseKindWriteError` / `CloseKindHandlerPanic`：peer **收不到** close frame
   - `CloseKindReadError` 普通：peer **收不到** close frame
   - `CloseKindReadError` + `Code == CloseMessageTooBig`：peer 收到 **exactly 1** 个 close frame，code=1009；该 frame 来自 gorilla 内部，wsconn **不**重复发
4b. **Wire close code 白名单**：传入 `CloseInfo{Code: CloseNoStatusReceived}` 或 `CloseInfo{Code: CloseAbnormalClosure}` 调用 `Close` 走需发 frame 路径时，wire 上的 close code 被替换为合法值（1000/1001/1011），并记 log
5. **PeerClose 不重复发 close frame**：模拟 peer 主动发 close → 抓 wire bytes，断言全过程一共只发一次 close frame（gorilla 默认 handler 自动回的那一次），`CloseKindPeerClose` 路径不再发
6. **PongMissTimeout per-ping timer + generation 测试**：
   - **`PongMissTimeout == 0` ping-only 分支**：持续发 ping、不因缺 pong 关闭、不被 awaiting 卡住
   - 发出 generation=1 的 ping，注入 payload 不匹配的伪 pong；断言 PongMissTimeout per-ping timer 仍按时触发
   - `PongMissTimeout > PingInterval`：同一时刻只一个 outstanding ping，`awaiting=true` 期间 ticker 不发新 ping（也不叠加新 timer）
   - 收到匹配 generation 的 pong 后：`outstandingTimer.Stop()` 被调用，watchdog callback 不再触发；`awaiting=false`，下一个 ticker 发 gen=2 的新 ping
   - `markInboundActivity` 在所有 pong（含不匹配的伪 pong）路径都被调用
7. **InboundActivityTimeout 测试**：
   - "全静默"：断言触发
   - "持续有任意 inbound control frame 或完整 data message"：断言不触发
   - `InboundActivityTimeout` 闭包返回 0 或 nil：断言不启动 watchdog
   - 验证 outbound ping 不刷新计时器
8. **xunfei zero-value 配置测试**：Config 全零（PingInterval=0、PongMissTimeout=0、InboundActivityTimeout=nil）的 ManagedConn 不发 ping、不触发 watchdog、读完 EOF 自然结束
9. **Pump.Run read state 5 态 + ctx watcher + 分类 Close**：
    - ctx cancel 时 ctx watcher 主动 Close（`CloseKindAbort`, reason 包含 `ctx_done`），Pump.Run 退出后 defer `pump_exit_without_close` 走 CAS 失败分支（正常路径不应赢）
    - read path 错误返回前必须先 `Close(classifiedInfo)` 完成分类（PeerClose / ReadError / ErrReadLimit / WriteError / HandlerPanic）
    - conn 已被其他路径关闭时 Pump.Run 退出，defer Close 走 CAS 失败分支立即返回，CloseInfo 保持先前定型值
    - 同一 ManagedConn 同时启动两个 Pump.Run：**第二个 Pump.Run 必须 panic**（read state 5 态机非法转换），断言 panic 消息含 `Pump.Run` / `invalid read state`
    - `ReadInitial` 期间起 Pump.Run：panic（同 read state 5 态机）
    - Pump.Run 退出后 read state 进入 `readTerminal`，再次 Pump.Run / ReadInitial **必须 panic**（不允许 reset 回 readIdle）
    - ctx watcher goroutine 在 Pump.Run 正常退出时一并退出（通过 `runtime.NumGoroutine()` 或 leaktest 断言无泄漏）
10. **Pump 不解释业务错误**：Handle 函数签名无 error 返回值；ErrReadLimit 不透传到 Handle，而是经 `CloseInfo{Kind: CloseKindReadError, Code: CloseMessageTooBig}` 在 `OnClose` 中感知
11. **Pump.Handle 非阻塞 observation 测试**：注入 fake `slowHandleRecorder` + fake clock；Handle 内部通过 fake clock 的 `Advance(5ms)`（测试 fake clock 实现专属方法，不进生产 `Clock` interface）推进时钟 5ms；**断言 recorder 收到一次 `Observe(5ms)`**；不依赖真实时间精度，不做 p99 硬断言
12. **`OnClose` 回调契约**：
    - `Pump.OnClose` 是唯一 close 回调（`Config.OnClosed` / `OnPongMiss` / `OnInboundIdle` / `OnBeforeWatchdogClose` 全部不存在）
    - 回调内调用 `conn.Close(...)` 立即返回（CAS 失败），不阻塞；回调内调用 `conn.WriteMessage(...)` 返回 `net.ErrClosed`
    - 回调在 close 锁内同步调用是 bug：通过 wsconn 内部 invariant assert 检测（仅 -race / debug build 启用）
13. **WriteMessage 出错后归类**：模拟底层 write error → 断言 `CloseInfo.Kind == CloseKindWriteError`；验证 release-then-close 序列下不出现 deadlock
14. **AcceptManaged 错误路径**：握手失败资源不泄漏；`AcceptOptions` 各字段均能透传到内部构造的 gorilla Upgrader
15. **DialManaged 错误路径**：握手失败时返回 `*wsconn.DialError`，`errors.As(err, &dialErr)` 能拿到脱敏 URL、StatusCode、Header、BodySnippet、CloseInfo 字段；`dialErr.CloseInfo.Kind == CloseKindDialFailed`
    - `DialError.URL` 与 `DialError.Error()` 均不得输出 URL userinfo / query / fragment
16. **Close reason 截断**：reason > 123 字节时按 UTF-8 边界截断；含多字节字符时不切到 rune 中间；含无效 UTF-8 字节时丢弃。close frame 仍然合法
17. **DialOption 无 gorilla 类型暴露**：`go vet` / 文档检查所有导出 DialOption 函数签名，禁止出现 `*websocket.Dialer` 等 gorilla 类型
18. **Config 校验测试**：
    - `PingInterval=0` + `PongMissTimeout>0`：`AcceptManaged` / `DialManaged` 返回 `ErrInvalidConfig`
    - `PingInterval < 0` 或 `PongMissTimeout < 0`：返回 `ErrInvalidConfig`
    - `ReadLimit < 0`：返回 `ErrInvalidConfig`
    - `WriteTimeout != nil && WriteTimeout() < 0`（首次调用）：返回 `ErrInvalidConfig`
    - `InboundActivityTimeout != nil && InboundActivityTimeout() < 0`（首次调用）：返回 `ErrInvalidConfig`
    - **`PingInterval >= PongMissTimeout`（两者均 >0）：成功**（per-ping timer 算法保证正交）
    - 运行期 `WriteTimeout()` 返回负数：fallback `defaultWriteTimeout` + warning
    - 运行期 `InboundActivityTimeout()` 返回负数：视为 0（禁用 watchdog）+ warning
    - 全零 Config：成功返回 ManagedConn
19. **Clock 注入测试**：fake clock 控制下，PongMissTimeout、InboundActivityTimeout watchdog 在推进时钟到 deadline 时立即触发；close frame 的 net.Conn 写 deadline **不受** fake clock 影响（用 mock net.Conn 单独覆盖该场景，文档明确边界）
20. **自定义 Ping/Pong handler 实装测试**：
    - wsconn 包装后 raw 连接的 PingHandler / PongHandler **不再是 gorilla 默认**（通过 wstest 内部访问或 reflect 验证）
    - inbound ping 触发 `markInboundActivity` + pong 走 wsconn 内部 `controlWriter.EnqueuePong`（断言 EnqueuePong 调用计数 +1）
    - PingHandler **不**调用 `raw.WriteControl(PongMessage,...)` 路径（mock raw conn 或 reflect 拦截）
    - PingHandler 中 `controlWriter.EnqueuePong` 返回 error 时，error 归类为 `CloseKindWriteError`（不是 `CloseKindReadError`）
    - inbound pong 同时触发 `markInboundActivity` 和 `observePongGeneration`
21. **depguard wstest 规则测试**：在 `relay/foo.go`（非测试文件）写入 `import "one-api/common/wsconn/wstest"`，断言 lint 拦下；在 `relay/foo_test.go` 写同样 import，断言 lint 通过
22. **Frame 构造器 + runtime 校验**：
    - `session.Frame{kind: 99}` 字面量编译失败（`kind` 字段不导出）
    - `session.NewTextFrame(payload)` / `NewBinaryFrame(payload)` 是唯一构造入口
    - `SendClient` 入参为零值 `Frame{}` 或 `frame.Kind() > FrameKindBinary` 时返回 `ErrInvalidFrame`（runtime 校验测试）
    - 调用方传 `FrameKind(99)` 转 int 作为局部变量合法，但不能进 `Frame` —— 验证 `Frame.Kind()` 永远只返回 `FrameKindText` / `FrameKindBinary` 之一
23. **RecvEvent 字段互斥与组合契约**：
    - 顶层 error 非 nil 时 RecvEvent 全零（含 `Usage`）（fuzz test 注入业务错误，断言此约束在所有 provider 实现中成立）
    - RecvEvent 任一字段非空时顶层 error 必须 nil
    - 业务错误经 `RecvEvent.Err` 投递，不污染顶层 error（调用方先判顶层 error 后再处理字段优先级）
    - **`Usage` 组合**：单独出现合法（usage-only event）；与 `Frame` 并存合法（payload-with-usage）；**与 `Err` 并存视为 provider bug**（usage 是已确认账务事实，与异常路径互斥；fuzz 断言不出现 Usage+Err / Frame+Usage+Err 组合）
    - **业务结果状态走 Frame**：`response.done.status == failed / incomplete / cancelled` 必须作为 `Frame + Usage` 投递（错误细节在 payload 内的 `response.status` / `status_details`），**不**转成 `RecvEvent.Err`；fuzz 注入 status=failed 的 response.done payload，断言到达 actor 的事件是 `ProviderDownstreamFrame`（带 Usage），不是 `ProviderBusinessError`
    - **`ProviderClose` 终态**：与 `Frame` / `Usage` / `Err` 任一并存即视为 provider 实现 bug（fuzz 断言不出现）
23b. **Usage-only 事件 actor 路径**：
    - provider 模拟单独发 `input_audio_transcription.completed.usage`（无 Frame）→ actor 收到 `ProviderUsageObserved`，**不**收到 `ProviderDownstreamFrame`（验证 recvLoop usage-only 分支不被 Frame==nil 吞掉）
    - actor handle `ProviderUsageObserved` 时：不下发 downstream、不调 `MarkCompleted` / `finalizeActiveAttempt` / `clearActiveTurn`，只累加 settlement 状态
    - `Usage.Source == UsageSourceInputAudioTranscription` 且当前模型未配置 transcription 定价：`usage_observed_unbilled{source="input_audio_transcription",model=X}` metric +1，actor 不阻塞、不报错，usage 被丢弃（不进入 settlement 累加）
    - `Usage.Source == UsageSourceRealtimeResponse`：按 `BillingBasis=tokens` 进入 settlement，定价表必须命中（命中失败按现有 ResponsesWS 漏帐告警走，不在本节新增）
24. **ProviderClose 转发**：模拟 provider 端发 close（code 例如 4408）→ provider 实现把 wsconn.CloseInfo 翻译为 `session.RecvEvent{ProviderClose: {Code: 4408}}`；对 `/v1/realtime`（pass-through）在 Recv 调用点直接 `client.Close(CloseInfo{Code: SanitizeWireCloseCode(4408)})`；对 ResponsesWS 投递为 actor event，由 actor 结合 attempt/lease 状态决定是否 forward close 并完成 settlement。客户端 wire 上看到 close code 4408
25. **ResponsesWS 入口形态审查**：
    - **不存在** `ResponsesWSIngress` / `BoundedReliableQueue` / `OfferOutcome` 类型（grep 兜底）
    - `Pump.Handle` 直接绑 `actor.onClientFrame`，无中间 adapter
    - 首帧路径：`ReadInitial → 帧类型校验 → JSON parse → actor.Start → Pump.Run → PostReliable(FirstTurnSetup)` 全在主 goroutine 顺序执行
    - heavy admission（quota / RPM / model / lease 升级）只在 actor handle FirstTurnSetup 时发生，外层不重复
26. **DialSecurityPolicy 默认与脱敏**：
    - 默认 policy 下 `ws://example` 返回 `ErrInsecureScheme`；带 `WithDialSecurityPolicy(AllowInsecureWS: true)` 后允许
    - 默认 policy 下私网地址（127.0.0.1 / 10.x / 192.168.x）返回 `ErrPrivateAddrBlocked`；带 `AllowPrivateIP: true` 后允许
    - 默认 policy 下 169.254.169.254（云厂商 metadata IP）返回 `ErrPrivateAddrBlocked`，即使开了 `AllowPrivateIP` 或 `HostFilter` 自定义同意也仍拒绝
    - DialError.Error() 输出不含 `Authorization` / `Cookie` / `Sec-WebSocket-Protocol` 的值，也不含 URL userinfo / query / fragment
    - dial 失败 body 大小超过 `MaxBodySnippet`：`BodySnippet` 长度被截至限值且 `BodyTruncated = true`
27. **Proxy fail-closed**：
    - `WithProxyURL("")` 不使用 proxy（直连），与现状一致
    - `WithProxyURL("not a url")` 触发 `DialManaged` 返回 `ErrInvalidProxyURL`，**不**退化为直连（与 `common/requester/ws_client.go:30-34` fail-open 行为对比）
    - `WithProxyURL("ftp://x")`（不支持的 scheme）同样返回 `ErrInvalidProxyURL`
    - 配置了 proxy 但解析失败时，wire 上没有任何到目标 host 的直连 TCP 尝试（用 mock NetDialContext 验证）
28. **ReadInitial 行为**：
    - 正常路径：peer 发一帧 → `ReadInitial(ctx)` 返回 `(TextMessage, payload, nil)`；后续 `Pump.Run` 启动后从第 2 帧开始走 Handle
    - ctx deadline 已到：返回 `context.DeadlineExceeded`；wire 上未发任何 close frame，由调用方决定是否 `Close`
    - oversized 首帧（payload > 应用层 LimitedReader 上限）：返回 `ErrFirstFrameTooLarge` 之类的 sentinel，**调用方仍可 `WriteMessage` 写自定义 JSON error 再 `Close`**；wire 上**不出现** 1009 close frame（与 gorilla SetReadLimit 路径区分）
    - peer 在握手后直接发 close：返回 `*wsconn.CloseError`（**不**返回 `*websocket.CloseError`），调用方据此 Close
    - 重复调用：同一 ManagedConn 第二次 `ReadInitial` 直接 panic（read state 5 态机非法转换）
    - `Pump.Run` 已启动后再 `ReadInitial`：panic（同 read state 5 态机）
    - ReadInitial 失败后 readState 进入 `readTerminal`，再调用 Pump.Run / ReadInitial 一律 panic
    - ReadInitial 期间 PongMissTimeout / InboundActivityTimeout watchdog **未武装**：fake clock 推进到任意时刻不会触发自动 Close
    - **ctx-cancel watcher 不泄漏 + 不串扰**：ReadInitial 成功返回后立刻启动 Pump.Run，然后再 cancel 原 ctx；断言 Pump 的 read 循环**未被打断**（旧 watcher 必须已在 ReadInitial defer 中 close stopWatcher 退出，不会再调 `SetReadDeadline`）；用 leaktest 或 `runtime.NumGoroutine()` 断言无 goroutine 泄漏
    - **ReadInitial 期间不触发 `Config.OnActivity`**：mock OnActivity 计数为 0；Pump.Run 启动后再发 inbound frame，OnActivity 才开始累计

## 范围

### 在范围内

- `common/wsconn` 和 `common/wsconn/wstest` 新建
- 现有 `common/requester/ws_*.go` 及相邻 `realtime_session_proxy.go` 全部迁移并删除
- `common/requester/realtime_session_proxy.go` 迁移
- `common/requester/SendWSJsonRequest` 改写为 caller-marshal 模式
- `providers/openai`、`providers/codex`、`providers/xunfei` 的 WS IO 重写（**实现层**接口跟随 RealtimeSession typed Frame 改造）
- **`runtime/session.RealtimeSession` 接口从裸 `mt int` 改为 typed `Frame` / `RecvEvent`**；新增 `session.ProviderClose` 用于 upstream close 到 downstream close 的显式映射
- `relay/realtime.go` 改用 ManagedConn + Pump
- `relay/responses_ws.go` 改用 ManagedConn + ReadInitial + Pump（`Pump.Handle` 直接绑 `actor.onClientFrame`，**无中间 Ingress 适配器**）
- `common/authutil/credential.go` 改用 `wsconn.Subprotocols`/`wsconn.IsUpgrade`
- 全部 13 个直接 import gorilla 的测试文件改用 `wsconn/wstest`
- depguard 双规则 + CI 集成
- 配置项 `client_pong_timeout_ms` 等改名 + 语义拆分
- **`DialManaged` 安全默认与错误脱敏**（默认拒 ws:// / 私网；DialError.Error() 脱敏；BodySnippet 限长）

### 不在范围内

- 业务状态机（Actor、TurnAttempt、quota/settlement、fallback、affinity）的**业务规则**变更（只调整传输/会话接口形态）
- `runtime/session` 的**业务能力边界**调整（能做什么不变；只重塑 frame / close 的接口表达）
- 通用 Bridge 公共包
- watchdog pre-close hook（明确不做，删除原 OnBeforeWatchdogClose 设计）
- 任何新传输协议（HTTP/2、WebTransport）
- metrics 维度的扩展（CloseInfo 提供了基础，但本方案不规定新 metrics）
- provider endpoint allowlist（由 provider / 配置层负责，wsconn 只提供安全默认 hook）

**明确不做的工程选项**（review checklist，防止后续实现回退到这些方向）：

- `lastValid` runtime duration fallback：运行期 closure 负数直接 fallback default + warning，不维护 per-connection 历史值
- 公开 `wstest` 故障注入 API（`ForceReadError` / `ForceWriteError` / `SimulatePongMiss` / `SimulateInboundIdle` / `ForcePeerClose` / `CloseSpy` / `DeadlineRecorder` / `DialHTTP`）：超出"测试支持但不做网络仿真器"边界
- 双 close 回调（`Config.OnClosed` + `Pump.OnClose`）"两种顺序都出现过"的概率验收测试：测试 race 不稳定，已经只保留单回调
- backpressure bytes cap：actor channel size 沿用 128，是否额外按 payload bytes 限制待压测决定
- 超过 5 态的 read lifecycle：5 态够用，再细化收益低于复杂度
- recoverable `ReadInitial` lifecycle 分支：`ReadInitial` 业务失败返回 error，调用方 `Close`；非法状态转换才 panic
- 关键词 grep 作 CI blocking：误伤率高，降为 advisory
- 为 invalid first frame 新增 `CloseKindProtocolReject`：`CloseKindNormal` 已扩义覆盖 protocol reject 场景
- DialSecurityPolicy 4+ profile：3 个内置 profile（`StrictPublicEgressPolicy / ConfiguredEgressPolicy / LocalTestEgressPolicy`）作为 follow-up，更多 profile 无收益

## 改动规模估计

| 项 | 估计 |
|---|---|
| 直接 import gorilla 的生产文件 | 14 个 |
| 直接 import gorilla 的测试文件 | 13 个 |
| 新增 `common/wsconn/` 文件 | ~11 个（含 managed.go / pump.go / accept.go / dial.go / close.go / message.go / control.go / watchdog.go / pong.go / clock.go / http.go） |
| 新增 `common/wsconn/wstest/` 文件 | ~4 个 |
| 新增 `relay/responses_ws/ingress.go` | **0**（Ingress 设计撤销，由 ReadInitial + `actor.onClientFrame` 直接覆盖） |
| `runtime/session/types.go` 接口改造（typed Frame / RecvEvent / ProviderClose） | ~80 行新增类型定义 + **~200+ 行**调用点更新（3 个 provider 的 SendClient/Recv 实现、2 个 relay handler、所有 RealtimeSession 测试与 mock） |
| 迁移并删除的 `common/requester/ws_*.go` 及邻居 | 现有 `ws_*.go` 全部 + `realtime_session_proxy.go` |
| `relay/responses_ws.go` 行数变化 | 约从 3003 → 2300-2400（IOBridge 传输部分搬出去，业务逻辑不动） |
| 全量集中工作量 | **15-25 人日**；单人 4-5 个工作周；wire-level 测试 + provider 边界 + wstest spike + depguard 收口约占近半 |

主要风险点：

- **typed Frame 接口迁移**：`RealtimeSession.SendClient/Recv` 签名变更牵涉 3 个 provider + 2 个 relay handler + 所有相关测试；需要一次性切换避免双轨期。
- **ProviderClose 转发边界**：什么样的 wsconn.CloseInfo 应该翻译为 `session.ProviderClose`（业务事件）vs `Err`（传输失败）需要 provider 实现里有清晰判定。错位会导致下游看到的 close code 与上游不一致。
- **Pump.Handle 直接绑 actor**：取消 Ingress 后，`actor.onClientFrame` 直接作为 Handle；非阻塞契约和 backpressure 由 actor 自己实现。grep 兜底防止 `Ingress` / `BoundedReliableQueue` 再被引入。
- **DialSecurityPolicy 默认值与现状差异**：开启默认拒 ws:// / 私网后，本地开发和 staging 配置需要显式 opt-in。落地时要梳理一遍部署配置避免回归。
- **provider WS IO 重写**：3 个 provider，每个都有边界条件（断线重连、session 复用、protocol-specific 错误处理）。重写时容易引入 detach/abort 时序差异。
- **CloseInfo 分类一致性**：多个 goroutine 同时可能触发 close 的场景下，确保 first-write-wins 不被绕过。
- **PongMissTimeout 与 ping generation 状态机**：测试要能区分"伪活跃" vs 真正回 pong；fake clock 必须能驱动状态机推进。
- **wstest 实现**：`Pair` 内部用 `net.Pipe + httptest.Server` 真握手构造连接对；Clock 注入需要从 ManagedConn 内部贯通到所有 watchdog / timer 使用点；故障注入留在 wsconn 包内 `internal_test`，不出 wstest 公共表面。
- **gorilla 默认 close handler 的行为**：PeerClose 不重复发 close frame 这条契约需要 wire-level 测试验证，否则容易在某个 provider 路径上意外触发双 close。
- **`relay/responses_ws.go` 业务部分的剥离**：3000 行里要准确切出"传输层重复发明"部分（~600-700 行），不能误伤 attemptID 校验、send outcome 投递等业务逻辑。

## 与现有 websocket-transport-architecture.md 的关系

[WebSocket Transport 复用方案](./websocket-transport-architecture.md) 描述的是**旧实现路径**：共享 safety primitives，但业务层各自组装、各自持有 `*websocket.Conn`。该路线已经达到 primitives-only 路径的能力上限，并已被本方案取代。

本方案是**当前方案**，替代而非叠加：

| 维度 | 旧实现（primitives-only） | 当前方案（wsconn 硬边界） |
|---|---|---|
| 业务持有 `*websocket.Conn` | 是 | 否 |
| 业务 import gorilla | 是（14 个生产文件） | 否（除 wsconn 外被 depguard 禁止） |
| read pump / ping loop / watchdog 实现 | 每个业务点各自实现 | wsconn 统一 |
| close reason | 各 goroutine 自分类 | CloseInfo first-write-wins |
| 死连接检测语义 | 模糊（idle 被命名为 pong timeout） | PongMissTimeout 与 InboundActivityTimeout 拆分 |
| 抽象通用 Bridge | 未抽 | 不抽（明确决定） |
| 抽象通用 MessagePump | 未抽 | 抽 |

落地按 4 个 commit 推进（部署仍是一次切换，commit 拆分只为 code review 可读性，每个 commit 必须独立通过 CI）：

1. **骨架 commit**：新建 `common/wsconn` 包结构（managed/pump/accept/dial/close/message/writer/control/watchdog/pong/clock/http）+ `common/wsconn/wstest` 最小表面（`Option` 类型 + `Pair / Server / WithClock` 三个函数）。本 commit 只新增 + 编译通过，不动业务。`.golangci.yml` 可加入 depguard 配置草案但 **不启用 blocking**（项目里仍有大量 gorilla import，blocking 会让 CI 红）。
2. **provider 迁移 commit**：openai / codex / xunfei 改用 `ManagedConn` + `Pump`；`runtime/session` typed Frame / RecvEvent / ProviderClose 同步切换；3 个 provider 的 `SendClient/Recv` 实现和测试 mock 一并改完。
3. **relay 迁移 commit**：realtime / responses_ws 改用 `ManagedConn` + `ReadInitial` + `Pump`；`common/authutil/credential.go` 改用 `wsconn.Subprotocols / IsUpgrade` 透传；`SendWSJsonRequest` 改写为 caller-marshal。
4. **收口 commit**：删除 `common/requester/ws_*.go` 全部文件 + `realtime_session_proxy.go`；**正式启用 depguard blocking**，实测两条违规 import（生产代码 import gorilla、生产代码 import wstest）各被 lint 拦下；旧 `client_pong_timeout_ms` 等配置项重命名为 `inbound_activity_timeout_ms` / `pong_miss_timeout_ms`。

## 与其它架构文档的关系

- [ResponsesWS 架构说明](./responses-ws-architecture.md)：Actor / TurnAttempt / quota / settlement 的业务边界不变，本方案只替换 IO 层。
- [Channel Affinity 架构](./channel-affinity-architecture.md)：affinity 选择和 channel 路由不变。
- [Execution Session Revocation](./execution-session-revocation-refactor.md)：runtime/session 锁边界和业务能力边界不变；RealtimeSession 接口签名随 typed Frame / RecvEvent 一次性切换。
- [Billing / Usage 结算架构](./billing-settlement-architecture.md)：结算链路不变。

本方案**只**是传输层的边界重整，不修改任何业务语义。
