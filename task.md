# wsconn 架构落地任务清单

来源文档：`docs/dev/wsconn-architecture.md`

完成定义：本文件所有代办项完成后，one-hub 的 WebSocket 传输层必须实现 `common/wsconn` 唯一传输边界方案；除 `common/wsconn/**` 和 `common/wsconn/wstest/**` 外，生产与测试代码都不得直接 import `github.com/gorilla/websocket`。

## 0. 全局原则与硬边界

- [x] 明确本次是一次性切换，不保留兼容 wrapper，不建立双轨期。
- [x] 明确 `common/wsconn` 只承接传输层职责：握手、读写、liveness、CloseInfo、DialError、安全默认、gorilla import 隔离。
- [x] 确认 `runtime/session` 只承接 provider 会话语义：typed Frame、ProviderClose、usage、provider error。
- [x] 确认 `relay/realtime` 与 `relay/responses_ws` 只承接业务编排：Actor、首帧 gate、lease、quota、settlement、fallback。
- [x] 明确不抽通用 Bridge 公共包。
- [x] 明确不新增 `ResponsesWSIngress`、`BoundedReliableQueue`、`OfferOutcome` 等中间 ingress 适配层。
- [x] 明确不引入 watchdog pre-close hook，不实现 `OnBeforeWatchdogClose`。
- [x] 明确 `common/wsconn` 内不得出现 relay/provider/session/turn/quota/billing/model 等业务概念。
- [x] 明确业务层不可持有或暴露 `*websocket.Conn`。
- [x] 明确业务层不可调用 `ReadMessage`、`WriteControl`、`WriteClose`、`SetReadDeadline`、`SetPongHandler`、`SetPingHandler`、`SetCloseHandler`、`WriteJSON` 等 gorilla API。
- [x] 明确业务写消息只能通过 `ManagedConn.WriteMessage(TextMessage|BinaryMessage, payload)`。
- [x] 明确业务关闭只能通过 `ManagedConn.Close(CloseInfo)`，是否发 close frame 由 wsconn 决定。
- [x] 明确 close reason 单源：first-write-wins，后续观察者只能通过 `Done()` + `CloseInfo()` 读取。
- [x] 明确 liveness 拆为 `PongMissTimeout` 与 `InboundActivityTimeout`，不得继续沿用旧 `client_pong_timeout_ms` 语义命名。
- [x] 把关键 trade-off 写入代码注释或开发文档：无 Bridge、无 Ingress、无 pre-close hook、无 WriteJSON、运行期负数 timeout fail-open、wstest 不做网络仿真器。

## 1. 基线盘点

- [x] 列出当前直接 import `github.com/gorilla/websocket` 的生产文件，预期约 14 个。
- [x] 列出当前直接 import `github.com/gorilla/websocket` 的测试文件，预期约 13 个。
- [x] 列出 `common/requester/ws_*.go` 全部文件和 `common/requester/realtime_session_proxy.go` 的职责归属。
- [x] 列出 `runtime/session.RealtimeSession` 的 `SendClient`、`Recv` 调用点和测试 mock。
- [x] 列出 `relay/realtime.go` 中握手、read pump、ping、watchdog、close 分类、send worker 的当前实现点。
- [x] 列出 `relay/responses_ws.go` 中首帧、IOBridge、read pump、ping loop、liveness watchdog、send worker 的当前实现点。
- [x] 列出 providers 中 openai、codex、xunfei 的 WS IO 与 RealtimeSession 实现点。
- [x] 列出 `common/authutil/credential.go` 使用 `websocket.Subprotocols` / `websocket.IsWebSocketUpgrade` 的位置。
- [x] 列出旧配置项：`client_pong_timeout_ms` 及 realtime/responses WS 相关 ping/timeout 配置。
- [x] 记录本次迁移不应改变的业务规则：Actor、TurnAttempt、quota/settlement、fallback、affinity、lease、RPM/model admission。

盘点记录（2026-05-24，迁移中状态）：

- 生产侧直接 import gorilla 的非 wsconn 文件：`common/authutil/credential.go`、`relay/realtime.go`、`relay/responses_ws.go`、`common/requester/realtime_session_proxy.go`、`common/requester/ws_reader.go`、`providers/xunfei/chat.go`、`common/requester/ws_writer.go`、`common/requester/ws_client.go`、`common/requester/ws_requester.go`、`common/requester/ws_error.go`、`common/requester/ws_control_writer.go`、`providers/openai/realtime_session.go`、`providers/codex/realtime_session.go`、`providers/codex/realtime.go`。
- 测试侧直接 import gorilla 的文件：`common/requester/realtime_session_proxy_more_test.go`、`common/requester/ws_writer_test.go`、`providers/openai/realtime_session_more_test.go`、`common/requester/ws_requester_test.go`、`providers/openai/realtime_session_test.go`、`common/requester/ws_control_writer_test.go`、`common/requester/ws_close_test.go`、`common/requester/realtime_session_proxy_test.go`、`providers/codex/realtime_session_more_test.go`、`providers/codex/realtime_test.go`、`relay/responses_ws_test.go`、`providers/codex/realtime_more_test.go`、`relay/realtime_test.go`。
- `common/requester/ws_*.go` 归属：`ws_client.go`/`ws_requester.go` 出站 dial 与握手诊断，迁入 `wsconn.DialManaged`；`ws_writer.go` 数据/控制写串行与 write deadline，迁入 `ManagedConn`/control writer；`ws_close.go` close reason 安全截断，迁入 `wsconn` close；`ws_activity.go` ping/pong activity hook，迁入 `wsconn` handler；`ws_read.go` read limit 默认值，迁入 `wsconn.Config.ReadLimit`；`ws_control_writer.go` control frame bounded queue，迁入 `wsconn` control writer；`ws_error.go` JSON error 写入能力应改为 caller marshal + `WriteMessage`；`ws_reader.go` reader helper 与 gorilla reader 耦合，后续随 caller 改造删除；`ws_active_guard.go` active counter guard 后续由 `ManagedConn` 生命周期/注册表替代。
- `common/requester/realtime_session_proxy.go` 同时包含客户端 WS IO、provider session 转发和轻量业务编排；迁移方向是 IO 进入 `wsconn`，业务编排回到 openai/codex provider 或 relay Actor，不保留兼容 wrapper。
- `runtime/session.RealtimeSession` 当前接口在 `runtime/session/types.go` 定义为 `SendClient(ctx, frame Frame)` 与 `Recv(ctx) (RecvEvent, error)`；调用点集中在 `common/requester/realtime_session_proxy.go`、`relay/responses_ws.go`、openai/codex provider 测试和实现；测试 mock 包括 `common/requester/realtime_session_proxy_test.go`、`relay/responses_ws_test.go`、`relay/realtime_test.go`。
- `common/authutil/credential.go` 原使用 gorilla `Subprotocols` / `IsWebSocketUpgrade` 的位置分别在 upstream subprotocol allowlist 与 websocket credential fallback；当前已改为 `wsconn.Subprotocols` / `wsconn.IsUpgrade`，仅保留 trivial HTTP 透传能力，不暴露 IO。
- `relay/realtime.go` 当前实现点：package-level `websocket.Upgrader` 完成入站握手；裸 `*websocket.Conn` 存在 `RelayModeChatRealtime.userConn`；读写/close 编排主要委托 `common/requester.NewRealtimeSessionProxy`，由 proxy 承担 user/provider 双向 read pump、send worker、close 分类；本文件仍在 `writeAbortPayload` 直接 `WriteMessage`/`Close`；本文件自身没有独立 ping/watchdog。
- `relay/responses_ws.go` 当前实现点：客户端入站握手已迁到 `wsconn.AcceptManaged`，首帧阶段使用 `ReadInitial(firstCtx)`，并启动 `wsconn.Pump` 将后续 client frame 非阻塞投递给 actor；`ResponsesWSIOBridge` 仍保留 provider recv pump、send worker 以及旧 gorilla fallback 路径，但 live ResponsesWS client-side read pump/ping loop 已由 managed conn/pump 接管；`ArmProviderRecvPump` 消费 typed `RecvEvent` 并投递 actor event；`startSendWorker`/`handleSendCommand` 负责 upstream `SendClient`；`WriteClientFrame`/`WriteCloseControl` 在 managed client 路径通过 `ManagedConn` 写 downstream，旧 bridge writer 路径仍保留给测试/遗留路径。
- provider 当前实现点：OpenAI 出站 WS 通过 requester dial helper 返回 `*websocket.Conn`，`openAIRealtimeSession` 内部直接持有 conn/control writer，`configureConn` 安装 read limit、ping/pong handler、read deadline，`readLoop` 直接 `ReadMessage` 并将 close 转为 `RecvEvent.ProviderClose`，`SendClient` 已接收 typed `session.Frame` 但仍映射回 gorilla message type 后直接 `WriteMessage`；Codex managed realtime 在 runtime state 中持有 `*websocket.Conn`、control writer、attachment queue，`startRealtimeWSReaderLocked` 直接 `ReadMessage`，close 转为 `RecvEvent.ProviderClose`，`writeCodexRealtimeWSMessage*` 仍直接写 gorilla；Xunfei chat 已迁到 `wsconn.DialManaged` + `Pump` + `WriteMessage`，不再直接 import gorilla。
- 旧配置项当前状态：`realtime.websocket_ping_interval_ms` 仍被 requester/proxy 旧路径复用；`realtime.websocket_read_limit` 与 `realtime.websocket_write_timeout_ms` 仍被 requester/provider/relay 复用；`responses_ws.client_pong_timeout_ms` 已停止读取，ResponsesWS client inbound activity timeout 改为 `responses_websocket_client_inbound_activity_timeout_ms`；`responses_ws.first_frame_timeout_ms`、`responses_ws.idle_timeout_ms`、`responses_ws.max_lifetime_ms` 仍由 ResponsesWS 首帧、provider idle、session lifetime 使用。
- 不应改变的业务规则：ResponsesWS Actor 单线程处理 TurnAttempt；quota preconsume/finalize/rollback、settlement identity、fallback/channel affinity、active/pending lease、RPM/model admission、stale continuation 检查、provider preflight、first turn setup 与 subsequent turn 语义必须保持；迁移只替换 transport 边界，不移动这些规则到 `common/wsconn`。

## 2. 建立 `common/wsconn` 公共表面

### 2.1 包结构

- [x] 新建 `common/wsconn/managed.go`。
- [x] 新建 `common/wsconn/pump.go`。
- [x] 新建 `common/wsconn/accept.go`。
- [x] 新建 `common/wsconn/dial.go`。
- [x] 新建 `common/wsconn/close.go`。
- [x] 新建 `common/wsconn/message.go`。
- [x] 新建 `common/wsconn/http.go`。
- [x] 新建 `common/wsconn/writer.go`。
- [x] 新建 `common/wsconn/control.go`。
- [x] 新建 `common/wsconn/watchdog.go`。
- [x] 新建 `common/wsconn/pong.go`。
- [x] 新建 `common/wsconn/clock.go`。
- [x] 确认 `common/wsconn` 导出 API 中不出现任何 gorilla 类型。
- [x] 确认 `common/wsconn` 导出 API 只包含架构文档与本任务清单列明的公共表面；任何新增导出类型、函数、方法、常量必须先更新 `docs/dev/wsconn-architecture.md` 并记录职责边界。
- [x] 确认 `common/wsconn` 不 import `one-api/relay`、`one-api/providers`、`one-api/runtime/session`、`one-api/model`。

### 2.2 MessageType 与消息写入

- [x] 定义 `type MessageType int`。
- [x] 只导出 `TextMessage MessageType = 1` 与 `BinaryMessage MessageType = 2`。
- [x] 不导出 Ping/Pong/Close message type。
- [x] 定义 `ErrInvalidMessageType`。
- [x] `WriteMessage` 对非 Text/Binary 返回 `ErrInvalidMessageType`。
- [x] `WriteMessage` 不提供 JSON 序列化能力。
- [x] `WriteMessage` 写失败时先释放 write mutex，再调用 `Close(CloseKindWriteError)`，避免 close cleanup 与 write mutex 死锁。
- [x] 添加 write 失败不死锁测试。

### 2.3 CloseCode 与 CloseError

- [x] 定义 `type CloseCode int`。
- [x] 定义 close code 常量：1000、1001、1002、1003、1005、1006、1007、1008、1009、1011、1012、1013，以及项目需要的其它合法 code。
- [x] 定义 `type CloseError struct { Code CloseCode; Reason string }`。
- [x] 实现 `(*CloseError).Error()`。
- [x] 业务侧错误分类使用 `errors.As(err, &wsconn.CloseError{})`，不再引用 `websocket.CloseError`。

### 2.4 CloseInfo 与 CloseKind

- [x] 定义 `CloseKindUnknown`。
- [x] 定义 `CloseKindNormal`。
- [x] 定义 `CloseKindGracefulShutdown`。
- [x] 定义 `CloseKindAbort`。
- [x] 定义 `CloseKindBackpressure`。
- [x] 定义 `CloseKindPeerClose`。
- [x] 定义 `CloseKindPongMiss`。
- [x] 定义 `CloseKindInboundIdle`。
- [x] 定义 `CloseKindReadError`。
- [x] 定义 `CloseKindWriteError`。
- [x] 定义 `CloseKindHandlerPanic`。
- [x] 定义 `CloseKindDialFailed`，仅用于 `DialError.CloseInfo`。
- [x] 定义 `CloseInfo{Kind, Code, Reason, Err, At}`。
- [x] `CloseKindUnknown` 只在 CAS 成功的第一关闭者处 warn，并 fallback 为 `CloseKindAbort`。
- [x] `CloseInfo.At` 使用 `Config.Clock.Now()`。
- [x] `CloseInfo()` 只在 `<-Done()` 后保证定型。

### 2.5 ManagedConn

- [x] 定义 `type ManagedConn struct`，字段全部未导出。
- [x] 实现 `WriteMessage(mt MessageType, payload []byte) error`。
- [x] 实现 `ReadInitial(ctx context.Context) (MessageType, []byte, error)`。
- [x] 实现 `Close(info CloseInfo)`。
- [x] 实现 `Done() <-chan struct{}`。
- [x] 实现 `CloseInfo() CloseInfo`。
- [x] `Close` 使用 `closeStarted.CompareAndSwap(false, true)` 实现 first-write-wins。
- [x] `Close` 快速返回，cleanup 在独立 goroutine 中执行。
- [x] CAS 失败的 `Close` 调用立即返回，不等待 close frame deadline。
- [x] cleanup 完成后 `close(c.done)`。
- [x] cleanup 内不得同步调用业务回调。

### 2.6 Config、Clock 与 Timer

- [x] 定义 `Config.Label`。
- [x] 定义 `Config.Clock`，nil 时使用真实时钟。
- [x] 定义 `Config.PingInterval`，0 表示不发 ping。
- [x] 定义 `Config.PongMissTimeout`，0 表示不监控 pong 回执。
- [x] 定义 `Config.InboundActivityTimeout func() time.Duration`，nil 或返回 0 表示禁用。
- [x] 定义 `Config.ReadLimit`，0 表示默认 16 MiB。
- [x] 定义 `Config.WriteTimeout func() time.Duration`，nil 表示默认 5s。
- [x] 定义 `Config.OnActivity func(time.Time)`。
- [x] 注释说明 `WriteTimeout` / `InboundActivityTimeout` closure 必须快速、无阻塞、无副作用。
- [x] 注释说明 `WriteTimeout` / `InboundActivityTimeout` closure 可能在入口 validation 与运行期被多次调用。
- [x] 注释说明 `WriteTimeout` / `InboundActivityTimeout` closure 内禁止查数据库、拿锁、做远程调用。
- [x] 注释说明 `Config.OnActivity` 只允许 metrics / refresh business idle，禁止写连接或阻塞调用。
- [x] 不定义 `Config.OnClosed`。
- [x] 不定义 `Config.OnPongMiss`。
- [x] 不定义 `Config.OnInboundIdle`。
- [x] 不定义 `Config.OnBeforeWatchdogClose`。
- [x] 定义 `Clock.Now()`。
- [x] 定义 `Clock.NewTimer(d)`。
- [x] 定义 `Clock.AfterFunc(d, f)`。
- [x] 定义 `Timer.Chan()`。
- [x] 定义 `Timer.Stop()`。
- [x] 定义 `Timer.Reset(d)`。
- [x] 走 Clock 的场景包括 ping timer/ticker、pong miss timer、inbound activity timer、CloseInfo.At、cleanup 等待窗口、slow-handle observation。
- [x] 不走 Clock 的场景包括 gorilla WriteControl deadline、net.Conn.SetWriteDeadline、net.Conn.SetReadDeadline。

### 2.7 Config 校验

- [x] 定义 `ErrInvalidConfig`。
- [x] `AcceptManaged` 与 `DialManaged` 入口统一校验 Config。
- [x] `PingInterval < 0` 返回 `ErrInvalidConfig`。
- [x] `PongMissTimeout < 0` 返回 `ErrInvalidConfig`。
- [x] `PingInterval <= 0 && PongMissTimeout > 0` 返回 `ErrInvalidConfig`。
- [x] `ReadLimit < 0` 返回 `ErrInvalidConfig`。
- [x] `WriteTimeout != nil && WriteTimeout() < 0` 首次调用返回 `ErrInvalidConfig`。
- [x] `InboundActivityTimeout != nil && InboundActivityTimeout() < 0` 首次调用返回 `ErrInvalidConfig`。
- [x] `PingInterval >= PongMissTimeout` 不作为非法组合。
- [x] 全零 Config 必须成功。
- [x] 运行期 `WriteTimeout()` 返回负数时 warn 并 fallback 到默认 5s。
- [x] 运行期 `InboundActivityTimeout()` 返回负数时 warn 并视为 0，禁用 watchdog。
- [x] 在代码注释中记录运行期 `InboundActivityTimeout` 负数 fail-open 的取舍：牺牲更快释放死连接，避免配置 bug 立即关闭所有活跃连接。

## 3. AcceptManaged 与 DialManaged

### 3.1 AcceptManaged

- [x] 定义 `AcceptOptions.CheckOrigin func(*http.Request) bool`。
- [x] 定义 `AcceptOptions.ResponseHeader http.Header`。
- [x] 定义 `AcceptOptions.ReadBufferSize int`。
- [x] 定义 `AcceptOptions.WriteBufferSize int`。
- [x] 定义 `AcceptOptions.EnableCompression bool`。
- [x] 定义 `AcceptOptions.Subprotocols []string`。
- [x] 定义 `AcceptOptions.Error func(w http.ResponseWriter, r *http.Request, status int, reason error)`。
- [x] `AcceptManaged(w, r, cfg, opts)` 内部构造 gorilla Upgrader。
- [x] 入站握手超时只通过 `r.Context()` deadline 表达，不在 `AcceptOptions` 单独配置。
- [x] upgrade 成功后立即包装为 `*ManagedConn` 并安装安全默认。
- [x] upgrade 失败时不得泄漏资源。
- [x] `AcceptOptions` 所有字段都必须透传到内部 Upgrader。

### 3.2 DialManaged 基础能力

- [x] 定义 `DialOption func(*dialConfig)`，不暴露 gorilla 类型。
- [x] 实现 `WithProxyURL(rawURL string)`。
- [x] 实现 `WithSubprotocols(protos ...string)`。
- [x] 实现 `WithHandshakeTimeout(d time.Duration)`。
- [x] 实现 `WithTLSConfig(cfg *tls.Config)`。
- [x] 实现 `WithNetDialContext(f func(ctx context.Context, network, addr string) (net.Conn, error))`。
- [x] 实现 `DialManaged(ctx, rawURL, header, cfg, opts...) (*ManagedConn, error)`。
- [x] 成功时返回已就绪可读写的 ManagedConn。
- [x] 失败时按场景返回普通 error 或 `*DialError`。
- [x] 禁止实现 `WithDialer(*websocket.Dialer)`。
- [x] 禁止任何 DialOption 导出签名出现 gorilla 类型。

### 3.3 DialError

- [x] 定义 `DialError.URL`。
- [x] 定义 `DialError.StatusCode`。
- [x] 定义 `DialError.Header`。
- [x] 定义 `DialError.BodySnippet`。
- [x] 定义 `DialError.BodyTruncated`。
- [x] 定义 `DialError.BodyReadErr`。
- [x] 定义 `DialError.Err`。
- [x] 定义 `DialError.CloseInfo`，Kind 固定为 `CloseKindDialFailed`。
- [x] 握手失败响应 body 读取失败时填充 `DialError.BodyReadErr`，不得丢失 URL、StatusCode、Header、CloseInfo 等其它诊断字段。
- [x] 实现 `(*DialError).Error()`。
- [x] 实现 `(*DialError).Unwrap()`。
- [x] 调用方可通过 `errors.As(err, &wsconn.DialError{})` 按 404/426/401/403/429/5xx 分类。

### 3.4 DialSecurityPolicy

- [x] 定义 `DialSecurityPolicy.AllowInsecureWS bool`，默认 false。
- [x] 定义 `DialSecurityPolicy.AllowPrivateIP bool`，默认 false。
- [x] 定义 `DialSecurityPolicy.MaxBodySnippet int64`，默认 4 KiB。
- [x] 定义 `DialSecurityPolicy.RedactHeaders []string`，默认含 Authorization、Cookie、Sec-WebSocket-Protocol。
- [x] 定义 `DialSecurityPolicy.HostFilter func(host string, ips []net.IP) bool`。
- [x] 定义导出 sentinel `ErrInsecureScheme`。
- [x] 定义导出 sentinel `ErrPrivateAddrBlocked`。
- [x] `ErrInsecureScheme` / `ErrPrivateAddrBlocked` 的返回错误必须支持 `errors.Is` 分类；可包装上下文，但不得丢失分类能力。
- [x] 实现 `WithDialSecurityPolicy(p DialSecurityPolicy)`。
- [x] TLS verify 默认开启；不传 `WithTLSConfig` 时不得跳过证书校验。
- [x] 关闭 TLS verify 只能显式传 `WithTLSConfig(&tls.Config{InsecureSkipVerify: true})`，便于 grep 审计。
- [x] 默认拒绝 `ws://`，返回 `ErrInsecureScheme`。
- [x] `AllowInsecureWS=true` 时允许本地/测试使用 `ws://`。
- [x] 默认拒绝 RFC1918、loopback、link-local、metadata IP `169.254.169.254`。
- [x] `AllowPrivateIP=true` 时放过 RFC1918，但 metadata IP 仍默认拒绝。
- [x] 自定义 `HostFilter` 是最终准入决策，可覆盖默认规则。
- [x] wsconn 不内置 provider endpoint allowlist。
- [x] `DialError.Error()` 不输出敏感 header 值。
- [x] 写入 `DialError.Header` 前敏感 header 被替换为 `[REDACTED]`。
- [x] `BodySnippet` 按 `MaxBodySnippet` 限长，超限设置 `BodyTruncated=true`。

### 3.5 Proxy fail-closed

- [x] 定义导出 sentinel `ErrInvalidProxyURL`。
- [x] `ErrInvalidProxyURL` 的返回错误必须支持 `errors.Is` 分类；可包装具体 parse/scheme 信息，但不得退化为不可分类字符串错误。
- [x] `WithProxyURL("")` 表示不使用 proxy。
- [x] 非空 proxy URL 解析失败返回 `ErrInvalidProxyURL`。
- [x] 不支持的 proxy scheme 返回 `ErrInvalidProxyURL`。
- [x] 支持 scheme 白名单至少覆盖 http、https、socks5。
- [x] 配置 proxy 但解析失败时不得退化为直连。
- [x] 使用 mock `NetDialContext` 验证 proxy 解析失败时没有到目标 host 的直连 TCP 尝试。

### 3.6 HTTP 透传工具

- [x] 实现 `wsconn.Subprotocols(r *http.Request) []string`。
- [x] 实现 `wsconn.IsUpgrade(r *http.Request) bool`。
- [x] 这两个函数只作为 gorilla trivial 透传，不暴露 IO 能力。

## 4. Close 路径实现

### 4.1 cleanup 精确顺序

- [x] `Close(info)` CAS 成功后写入 `CloseInfo`。
- [x] `Close(info)` CAS 成功后启动 cleanup goroutine。
- [x] cleanup 第一步根据 CloseKind 决定是否发 close control frame。
- [x] cleanup 发 close frame 前先通过 `wireCloseCodeFor(info)` 取得 code。
- [x] cleanup 发 close frame 前通过 `safeCloseReason(info.Reason)` 截断 reason。
- [x] cleanup 第二步停止 ping loop、watchdog、Clock timer。
- [x] cleanup 第三步关闭底层 gorilla conn。
- [x] cleanup 第四步关闭 `done` channel。
- [x] `Pump.Run` 在 `<-Done()` 后派发唯一 `OnClose`。

### 4.2 Close 决策表

- [x] `CloseKindNormal` 发 close frame，默认 code 1000。
- [x] `CloseKindNormal` 允许携带 protocol reject、unsupported data 等非 1000 合法 code。
- [x] `CloseKindGracefulShutdown` 发 close frame，默认 code 1001。
- [x] `CloseKindAbort` 不发 close frame。
- [x] `CloseKindBackpressure` best-effort 发 close frame，默认 code 1013。
- [x] `CloseKindPeerClose` 不发 close frame。
- [x] `CloseKindPongMiss` 不发 close frame。
- [x] `CloseKindInboundIdle` best-effort 发 close frame，默认 code 1001。
- [x] 普通 `CloseKindReadError` 不发 close frame。
- [x] `CloseKindReadError` 且 `Code==CloseMessageTooBig` 不重复发 close frame。
- [x] `CloseKindWriteError` 不发 close frame。
- [x] `CloseKindHandlerPanic` 不发 close frame。
- [x] Backpressure 与 InboundIdle 共用短 deadline best-effort 模板，失败吞掉并继续 cleanup。

### 4.3 Wire close code 白名单

- [x] 实现 `SanitizeWireCloseCode(code int) CloseCode`。
- [x] 允许 1000-1003。
- [x] 禁止 1004。
- [x] 禁止 1005。
- [x] 禁止 1006。
- [x] 允许 1007-1014。
- [x] 禁止 1015。
- [x] 禁止 `<1000`。
- [x] 禁止 `>4999`。
- [x] 允许 3000-4999，包括 provider 自定义码 4408、4499。
- [x] 非法值替换为 1011 并记录 log。
- [x] `wireCloseCodeFor(info)` 必须先按 Kind 填默认 code，再 sanitize。
- [x] 不允许在 relay/providers 中把任意 int 直接 `wsconn.CloseCode(...)` 后上 wire。

### 4.4 Close reason

- [x] 将现有 `common/requester/ws_close.go` 中的安全 close reason 截断逻辑迁入 `common/wsconn`。
- [x] close reason 最大 123 字节。
- [x] 截断必须保持 UTF-8 合法。
- [x] 输入含无效 UTF-8 字节时丢弃或修正，输出必须合法。
- [x] 多字节字符不得被截断在 rune 中间。

### 4.5 PeerClose

- [x] 保留 gorilla 默认 close handler。
- [x] 收到 peer close 后由 gorilla 默认 handler 自动回 close frame。
- [x] `ReadMessage` 返回的 `*websocket.CloseError` 在 wsconn 内转为 `*wsconn.CloseError`。
- [x] Pump 将 peer close 分类为 `CloseKindPeerClose`。
- [x] cleanup 不再重复发送 close frame。
- [x] 添加 wire-level 测试，断言 peer close 全过程只出现一次 close frame。

## 5. 读生命周期、ReadInitial 与 Pump

### 5.1 read state 5 态

- [x] 定义 `readIdle`。
- [x] 定义 `readInitialActive`。
- [x] 定义 `readInitialReady`。
- [x] 定义 `readPumpActive`。
- [x] 定义 `readTerminal`。
- [x] `ManagedConn` 使用 atomic readState 管理读生命周期。
- [x] 实现 `beginReadInitial()`。
- [x] 实现 `finishReadInitial(err)`。
- [x] 实现 `beginPump()`。
- [x] 实现 `finishPump()`。
- [x] `ReadInitial` 仅允许 `readIdle -> readInitialActive`。
- [x] `ReadInitial` 成功后进入 `readInitialReady`。
- [x] `ReadInitial` 失败后进入 `readTerminal`。
- [x] `Pump.Run` 允许 `readIdle -> readPumpActive`。
- [x] `Pump.Run` 允许 `readInitialReady -> readPumpActive`。
- [x] `Pump.Run` 结束后进入 `readTerminal`。
- [x] 非法状态转换一律 panic。
- [x] `beginPump()` 发现 `closeStarted` 已 true 时 panic，防止 ReadInitial 成功后业务轻量校验失败又误启 Pump。

### 5.2 ReadInitial

- [x] `ReadInitial` 是唯一同步读 API。
- [x] `ReadInitial` 必须在 `Pump.Run` 前调用。
- [x] 每个 ManagedConn 只能调用一次 `ReadInitial`。
- [x] `ReadInitial` 不启动 PongMissTimeout watchdog。
- [x] `ReadInitial` 不启动 InboundActivityTimeout watchdog。
- [x] `ReadInitial` 不触发 `Config.OnActivity`。
- [x] `ReadInitial` 期间仍安装自定义 PingHandler/PongHandler。
- [x] ReadInitial 期间 PingHandler 自动回 pong，走 wsconn 内部 control writer。
- [x] ReadInitial 期间 PongHandler 可更新内部观测值和 generation。
- [x] ReadInitial 使用应用层 `NextReader + io.LimitedReader`，不依赖 gorilla `SetReadLimit`。
- [x] 定义导出 sentinel `ErrFirstFrameTooLarge`。
- [x] oversized first frame 返回 sentinel error，例如 `ErrFirstFrameTooLarge`。
- [x] oversized first frame 返回的 error 必须支持 `errors.Is(err, ErrFirstFrameTooLarge)` 分类；可包装实际大小、limit 等诊断字段。
- [x] oversized first frame 后调用方仍可 `WriteMessage` 写 JSON error，再 `Close`。
- [x] oversized first frame 路径 wire 上不应出现 gorilla 1009 close frame。
- [x] peer 握手后直接发 close 时返回 `*wsconn.CloseError`。
- [x] 普通协议读取失败或底层 IO error 返回 error，不自动发送 close frame；调用方必须负责写业务错误（如可写）并调用 `Close`。
- [x] ctx deadline/cancel 返回 `context.DeadlineExceeded` 或 `context.Canceled`，不透传 net.Error。
- [x] ReadInitial 不自动发送 close frame，由调用方决定是否写业务错误与 Close。
- [x] ReadInitial 失败后不允许再启动 Pump。
- [x] ReadInitial 成功后如果业务轻量校验失败，调用方可写 JSON error，然后必须 Close，不允许再启动 Pump。

### 5.3 ReadInitial ctx deadline/cancel

- [x] ctx 有 deadline 时临时映射到 `net.Conn.SetReadDeadline(real time.Now()+remaining)`。
- [x] ctx cancel 无 deadline 时启动内部 watcher goroutine。
- [x] watcher 在 ctx done 时调用 `SetReadDeadline(time.Now())` 打断 read。
- [x] watcher 生命周期必须不超过 ReadInitial 调用生命周期。
- [x] ReadInitial 所有返回路径都先停止 watcher。
- [x] 停止 watcher 后清理临时 read deadline：`SetReadDeadline(time.Time{})`。
- [x] 清理 deadline 后再转移 read state。
- [x] defer 顺序固定为：close watcher -> clear read deadline -> finish read state。
- [x] ReadInitial 成功后启动 Pump，再 cancel 原 ctx，不得打断 Pump read loop。
- [x] 添加 goroutine leak 测试覆盖 watcher 不泄漏。

### 5.4 Pump API

- [x] 定义 `type Pump struct { Conn *ManagedConn; Handle func(context.Context, MessageType, []byte); OnClose func(CloseInfo) }`。
- [x] 实现 `Run(ctx context.Context)`。
- [x] `Pump.Run` 拥有 read lifecycle。
- [x] 调用方启动 Pump 后不需要额外绑定 `ctx.Done() -> Close`。
- [x] 主循环不 select ctx.Done。
- [x] ctx cancel 由 watcher goroutine 调用 `Close(CloseKindAbort, reason "ctx_done")` 打断 read。
- [x] `Pump.Run` 正常退出时关闭 watcher goroutine。
- [x] `Pump.Run` defer fallback `Close(CloseKindAbort, "pump_exit_without_close")` 仅作为 invariant fallback。
- [x] read path 错误返回前必须先完成 `Close(classifiedInfo)`。
- [x] `Pump.Run` defer 中先 `finishPump()`，再 `<-Done()`，最后调用 `OnClose`。
- [x] `OnClose` 只在 cleanup 完全完成后调用。
- [x] `OnClose` 是业务感知 close 的唯一回调。
- [x] 文档和代码注释说明 `Pump.OnClose` 只允许 metrics/log、actor post、状态迁移、资源释放，禁止写连接、阻塞调用、同步外部 IO。

### 5.5 read path 分类

- [x] 收到 peer close 转 `CloseKindPeerClose`，不调用 Handle。
- [x] 底层 read IO error 转 `CloseKindReadError`，不调用 Handle。
- [x] `websocket.ErrReadLimit` 转 `CloseKindReadError` 且 `Code=CloseMessageTooBig`，不调用 Handle。
- [x] control writer 写错误转 `CloseKindWriteError`。
- [x] Handle panic recover 后转 `CloseKindHandlerPanic`。
- [x] watchdog 判死转 `CloseKindPongMiss` 或 `CloseKindInboundIdle`。
- [x] ctx cancel 转 `CloseKindAbort`，reason 包含 `ctx_done`。
- [x] conn 已被其它路径关闭时，Pump fallback Close CAS 失败，CloseInfo 保持先前定型值。

### 5.6 Pump.Handle 非阻塞契约

- [x] 文档和代码注释说明 `Pump.Handle` 必须 <1ms 返回。
- [x] `Pump.Handle` 禁止阻塞 channel send。
- [x] `Pump.Handle` 禁止同步外部 IO。
- [x] `Pump.Handle` 禁止持锁等待。
- [x] `Pump.Handle` 禁止同步 JSON 重解析、quota 检查、数据库查询等重活。
- [x] `Pump.Handle` 必须通过非阻塞 select post 到 actor/worker。
- [x] `Pump.Handle` 不得按每帧启动 goroutine 分发常规业务事件；只有明确短生命周期、与帧顺序无关的一次性任务可例外，并需在代码注释中说明。
- [x] channel 满时由业务调用 `Close(CloseKindBackpressure, Code=1013)`。
- [x] payload 跨 goroutine/channel 投递前必须 copy。
- [x] 实现 slow-handle observation，阈值默认 1ms。
- [x] slow-handle observation 使用 `Config.Clock`。
- [x] slow-handle observation 只记录，不对生产 Handle 做硬时间断言。

## 6. Control writer 与 Ping/Pong handler

### 6.1 Control writer

- [x] 实现内部 control writer，串行发送 pong、ping、close control frame。
- [x] control writer 具有 bounded queue。
- [x] control writer 关闭后 enqueue 返回 error。
- [x] control frame 写失败归类为 `CloseKindWriteError`。
- [x] inbound ping 的 pong 回执必须走 control writer，不走 gorilla 默认 `WriteControl`。
- [x] control writer 的 write deadline 使用真实 `time.Now()`，不走 fake Clock。

### 6.2 自定义 Ping/Pong handler

- [x] `AcceptManaged` 和 `DialManaged` 包装时强制安装自定义 PingHandler。
- [x] `AcceptManaged` 和 `DialManaged` 包装时强制安装自定义 PongHandler。
- [x] PingHandler 调用 `markInboundActivity(c.clock.Now())`。
- [x] PingHandler 通过 `controlWriter.EnqueuePong([]byte(appData))` 自动回 pong。
- [x] PingHandler 的 `EnqueuePong` error 归类为 `CloseKindWriteError`，不是 `CloseKindReadError`。
- [x] PongHandler 调用 `markInboundActivity(c.clock.Now())`。
- [x] PongHandler 调用 `observePongGeneration(appData)`。
- [x] SetCloseHandler 保留 gorilla 默认。
- [x] data message activity 只在完整 message 读完后刷新。
- [x] Pump.Run 启动后 `markInboundActivity` 刷新 watchdog 的同时，必须按 `Config.OnActivity` 契约派发回调。
- [x] `Config.OnActivity` 派发不得在 `pongState.mu`、write mutex、close cleanup lock 等内部锁下同步执行。
- [x] 文档或注释说明 data activity 是 message 级，不是 frame 级。

## 7. Liveness 拆分与实现

### 7.1 配置语义

- [x] 新配置 `realtime_websocket_client_ping_interval_ms`。
- [x] 新配置 `realtime_websocket_client_pong_miss_timeout_ms`。
- [x] 新配置 `realtime_websocket_client_inbound_activity_timeout_ms`。
- [x] 新配置 `responses_websocket_client_ping_interval_ms`。
- [x] 新配置 `responses_websocket_client_pong_miss_timeout_ms`。
- [x] 新配置 `responses_websocket_client_inbound_activity_timeout_ms`。
- [x] 删除或停止使用旧 `client_pong_timeout_ms`。
- [x] 部署文档说明旧配置不兼容迁移。
- [x] `PongMissTimeout` 仅表示发出 ping 后未收到对应 pong。
- [x] `InboundActivityTimeout` 仅表示任意 inbound control frame 或完整 data message 多久未到达。
- [x] outbound ping/data 不刷新 InboundActivityTimeout。
- [x] turn-level deadline 不放入 wsconn。

### 7.2 Pong generation 算法

- [x] 定义 `pongState.mu`。
- [x] 定义 `pongState.gen`。
- [x] 定义 `pongState.awaiting`。
- [x] 定义 `pongState.outstandingGen`。
- [x] 定义 `pongState.outstandingTimer`。
- [x] 定义 `pongState.lastMatchedPongAt`。
- [x] ticker、PongHandler、timer callback 都必须在 `pongState.mu` 下访问 pongState 字段。
- [x] `PongMissTimeout == 0` 分支仍递增 generation 并发送 ping。
- [x] `PongMissTimeout == 0` 分支不设置 awaiting，不 arm timer。
- [x] `PongMissTimeout == 0` 分支写失败归 `CloseKindWriteError`。
- [x] `PongMissTimeout > 0` 分支若 awaiting=true，则本次 ticker skip，不发新 ping。
- [x] 发送 ping 前设置 awaiting=true、outstandingGen=gen，并 arm per-ping timer。
- [x] per-ping timer callback 在锁内校验 awaiting 与 generation。
- [x] timer callback 在锁外调用 `Close(CloseKindPongMiss)`。
- [x] `EnqueuePing` 在锁外执行。
- [x] `EnqueuePing` 同步失败时先 Stop timer 并清理 outstanding 状态。
- [x] `EnqueuePing` 同步失败归 `CloseKindWriteError`，不是 `CloseKindPongMiss`。
- [x] control writer goroutine 内真实写 ping 失败也归 `CloseKindWriteError`。
- [x] PongHandler 先 mark inbound activity。
- [x] PongHandler 只接受 8 字节 generation payload 参与匹配。
- [x] 不匹配 pong 静默忽略，不解除 outstanding。
- [x] 匹配 pong 停止 outstandingTimer，并置 awaiting=false。
- [x] 匹配 pong 后下一次 ticker 可发 gen+1。

### 7.3 InboundActivityTimeout

- [x] `InboundActivityTimeout == nil` 时不启动 watchdog。
- [x] `InboundActivityTimeout() == 0` 时不启动 watchdog。
- [x] 任意 inbound ping 刷新 activity。
- [x] 任意 inbound pong 刷新 activity。
- [x] 收到 close control frame 时刷新或记录 inbound activity，再走 close 分类。
- [x] 完整 data message 读完后刷新 activity。
- [x] outbound ping 不刷新 activity。
- [x] outbound data 不刷新 activity。
- [x] 触发后调用 `Close(CloseKindInboundIdle)`。
- [x] InboundIdle 默认 best-effort 发 1001 GoingAway。
- [x] InboundIdle 不抢发业务 payload。

### 7.4 turn-level deadline

- [x] 保留 openai/codex 等 provider 内部 turn-level deadline。
- [x] turn-level deadline 通过业务层 timer 调用 `conn.Close(CloseKindAbort, reason "turn_read_timeout")`。
- [x] `common/wsconn` 包内不得出现 turn 概念。

## 8. `common/wsconn/wstest`

### 8.1 公共表面

- [x] 新建 `common/wsconn/wstest` 包。
- [x] 定义 `Option`，同时适用于 Pair 与 Server。
- [x] 实现 `Pair(t testing.TB, opts ...Option) (client, server *wsconn.ManagedConn)`。
- [x] 实现 `Server(t testing.TB, handler func(*wsconn.ManagedConn), opts ...Option) (url string, cleanup func())`。
- [x] 实现 `WithClock(clock wsconn.Clock) Option`。
- [x] `Pair` 内部可使用 net.Pipe、httptest.Server、gorilla Upgrader，但对外只暴露 ManagedConn。
- [x] `Server` 用于真实握手、DialError、TLS 路径测试。
- [x] fake clock 注入必须同时覆盖 Pair 与 Server 构造出来的 ManagedConn。
- [x] `common/wsconn/wstest` 除 `Option`、`Pair`、`Server`、`WithClock` 外不得新增导出符号；确需扩展测试选项时必须先更新架构文档。
- [x] 统一文档口径：`wstest` 公共表面是 `Option` 类型 + `Pair` / `Server` / `WithClock` 三个函数，共四个导出符号；不得写成“只暴露三个 API”造成歧义。

### 8.2 明确不公开

- [x] 不公开 `DialHTTP`。
- [x] 不公开 `ForcePeerClose`。
- [x] 不公开 `ForceReadError`。
- [x] 不公开 `ForceWriteError`。
- [x] 不公开 `SimulatePongMiss`。
- [x] 不公开 `SimulateInboundIdle`。
- [x] 不公开 `CloseSpy`。
- [x] 不公开 `WireFrames`。
- [x] 不公开 `DeadlineRecorder`。
- [x] 不做丢包、延迟注入、partial frame、TCP RST、latency profile、任意故障注入。
- [x] wire 抓帧、deadline 计数、故障注入只允许放在 wsconn 包内部测试或 internal test helper。

### 8.3 Clock 注入边界

- [x] fake Clock 控制 ping timer/ticker。
- [x] fake Clock 控制 pong miss timer。
- [x] fake Clock 控制 inbound activity timer。
- [x] fake Clock 控制 CloseInfo.At。
- [x] fake Clock 控制 cleanup 等待窗口。
- [x] fake Clock 不控制 gorilla WriteControl deadline。
- [x] fake Clock 不控制 net.Conn.SetWriteDeadline。
- [x] fake Clock 不控制 net.Conn.SetReadDeadline。
- [x] 涉及 socket deadline 的测试使用 mock net.Conn 或极短真实 timeout。

## 9. `runtime/session` typed Frame 与 RecvEvent

### 9.1 Frame

- [x] 在 `runtime/session` 定义 `FrameKind`。
- [x] 定义 `FrameKindText`。
- [x] 定义 `FrameKindBinary`。
- [x] 定义不透明 `Frame`，字段 `kind` 与 `payload` 不导出。
- [x] 实现 `NewTextFrame(payload []byte) Frame`。
- [x] 实现 `NewBinaryFrame(payload []byte) Frame`。
- [x] 实现 `Frame.Kind() FrameKind`。
- [x] 实现 `Frame.Payload() []byte`，只读语义，不隐式 clone。
- [x] 实现 `Frame.ClonePayload() []byte`。
- [x] 实现 `Frame.IsZero() bool`。
- [x] 定义 `ErrInvalidFrame`。
- [x] `SendClient` 入口校验 zero Frame 或未知 kind，返回 `ErrInvalidFrame`。
- [x] `runtime/session` 不 import `common/wsconn`。
- [x] `runtime/session` 不 import gorilla。
- [x] relay 层负责 `session.FrameKind` 与 `wsconn.MessageType` 的双值映射。

### 9.2 Frame payload ownership

- [x] 注释说明 NewTextFrame/NewBinaryFrame 调用时调用方移交 payload 所有权。
- [x] 注释说明 Frame 构造后 payload 视为 immutable。
- [x] 注释说明 Payload() 返回只读语义，调用方不得修改。
- [x] 注释说明跨 goroutine/channel 投递前由投递方负责 copy。
- [x] 不在 Payload() 中隐式 clone，避免音频 chunk 复制成本。

### 9.3 ProviderClose 与 RecvEvent

- [x] 定义 `ProviderClose{Code int, Reason string, Err error}`。
- [x] 定义 `RecvEvent.Frame *Frame`。
- [x] 定义 `RecvEvent.ProviderClose *ProviderClose`。
- [x] 定义 `RecvEvent.Usage *types.UsageEvent`。
- [x] 定义 `RecvEvent.Origin RealtimePayloadOrigin`。
- [x] 定义 `RecvEvent.Err error`。
- [x] provider emit 的 `RecvEvent` 必须保持现有 `Origin` 语义；provider-originated Frame、Usage、ProviderClose、Err 不得因 typed Frame / RecvEvent 迁移丢失来源信息。
- [x] 更新 `RealtimeSession.SendClient(ctx, frame Frame) error`。
- [x] 更新 `RealtimeSession.Recv(ctx) (RecvEvent, error)`。
- [x] 保持 `Detach(reason string)` 签名。
- [x] 保持 `Abort(reason string)` 签名。
- [x] 保持 `SetTurnObserverFactory(factory TurnObserverFactory)` 签名。
- [x] 保留 `ResponsesWSSendPreflightCapable` optional capability 扩展模式，不因 typed Frame / RecvEvent 迁移删除。
- [x] 保留 `GracefulDetachCapable` optional capability 扩展模式，不因 typed Frame / RecvEvent 迁移删除。

### 9.4 RecvEvent 互斥与处理顺序

- [x] 顶层 error 非 nil 时，RecvEvent 必须全零。
- [x] RecvEvent 任一字段非空时，顶层 error 必须 nil。
- [x] 顶层 error 只表示没有 event 可消费。
- [x] provider 业务错误走 `RecvEvent.Err`，不走顶层 error。
- [x] 处理顺序为 Frame -> Usage -> ProviderClose -> Err。
- [x] 只 `Frame` 非 nil 合法。
- [x] `Frame + Usage` 合法，先转发 Frame，再处理 Usage。
- [x] 只 `Usage` 非 nil 合法，不写 downstream。
- [x] 只 `ProviderClose` 非 nil 合法。
- [x] 只 `Err` 非 nil 合法。
- [x] `Frame + Err` 合法，先转发 Frame，再处理 Err，不得同时带 Usage。
- [x] `ProviderClose` 不得与 Frame/Usage/Err 并存。
- [x] `Usage` 不得与 Err 并存。
- [x] `Frame + Usage + Err` 不允许。
- [x] `response.done.status == failed/incomplete/cancelled` 仍作为 `Frame + Usage` 投递，不转成 `RecvEvent.Err`。

### 9.5 types.UsageEvent 扩展

- [x] 在 `types` 包新增 `UsageSource`。
- [x] 定义 `UsageSourceRealtimeResponse = "realtime_response"`。
- [x] 定义 `UsageSourceInputAudioTranscription = "input_audio_transcription"`。
- [x] 新增 `UsageBillingBasis`。
- [x] 定义 `UsageBillingBasisTokens = "tokens"`。
- [x] 定义 `UsageBillingBasisDuration = "duration"`。
- [x] 为 `types.UsageEvent` 增补 `Source UsageSource`。
- [x] 为 `types.UsageEvent` 增补 `BillingBasis UsageBillingBasis`。
- [x] 为 `types.UsageEvent` 增补 `ProviderEventID string`。
- [x] 为 `types.UsageEvent` 增补 `ResponseID string`。
- [x] 为 `types.UsageEvent` 增补 `ItemID string`。
- [x] 为 `types.UsageEvent` 增补 `DurationSeconds float64`。
- [x] OpenAI `response.done.response.usage` 填 `Source=realtime_response`、`BillingBasis=tokens`、`ResponseID`。
- [x] OpenAI input audio transcription token usage 填 `Source=input_audio_transcription`、`BillingBasis=tokens`、`ItemID`。
- [x] OpenAI input audio transcription duration usage 填 `Source=input_audio_transcription`、`BillingBasis=duration`、`DurationSeconds`。
- [x] `ProviderEventID` / `ResponseID` / `ItemID` 只用于归属、幂等辅助、日志诊断，不替代 settlement identity。
- [x] provider-originated usage 只有存在 pending 或 active turn 时才有 quota 语义。
- [x] 最终扣费仍走 Actor turn finalize -> Quota -> SettlementEnvelope -> ApplySettlement。

## 10. Provider 迁移

### 10.1 openai / codex

- [x] provider 内部持有 `*wsconn.ManagedConn`。
- [x] provider 内部持有 `*wsconn.Pump`。
- [x] provider 出站连接改用 `wsconn.DialManaged`。
- [x] provider Config 设置 Label、PingInterval、PongMissTimeout、InboundActivityTimeout、ReadLimit、WriteTimeout。
- [x] provider `Pump.Handle` 只做非阻塞 post，不做重业务处理。
- [x] provider `Pump.OnClose` 是唯一 close 回调，负责 metrics/log、状态迁移、ProviderClose emit。
- [x] provider 收到 upstream peer close 时将 `CloseKindPeerClose` 转为 `session.RecvEvent{ProviderClose: ...}`。
- [x] provider 收到非 peer close，如 read error/write error/handler panic/pong miss，转为 `RecvEvent.Err`。
- [x] provider `SendClient` 接受 `session.Frame`。
- [x] provider `SendClient` 对 Frame zero/未知 kind 返回 `session.ErrInvalidFrame`。
- [x] provider 根据 FrameKind 写 `wsconn.TextMessage` 或 `wsconn.BinaryMessage`。
- [x] provider `Recv` 返回新的 `session.RecvEvent`。
- [x] provider `Detach` 调用 `CloseKindGracefulShutdown` 并显式设置 `Code=CloseNormalClosure(1000)`，不得改变 `CloseKindGracefulShutdown` 全局默认 code 1001。
  - 验证备注：`go test ./common/wsconn ./providers/openai ./providers/codex` 通过；OpenAI/Codex Detach 测试覆盖 upstream peer close 1000，`common/wsconn` 仍覆盖 `CloseKindGracefulShutdown` 默认 1001。
- [x] provider `Abort` 调用 `CloseKindAbort`，跳过 close frame。
- [x] provider 握手失败使用 `*wsconn.DialError` 做状态码分类。
- [x] 保持 existing unsupported channel、auth error、rate limit、5xx 分类语义。

### 10.2 xunfei

- [x] xunfei chat 改用 `wsconn.DialManaged`。
- [x] xunfei 使用全零 liveness Config，不发 ping、不启 watchdog。
- [x] xunfei 根据配置设置 ReadLimit。
- [x] caller 显式 `json.Marshal(req)` 后 `WriteMessage(wsconn.TextMessage, payload)`。
- [x] Pump.Handle 只做 append chunk 与 isLast 检查，保持 <1ms。
- [x] JSON 重解析或重业务处理不得放在 Pump.Handle。
- [x] builder 收到 isLast 后通过完成信号通知主协程。
- [x] 主协程收到 completed 后 `Close(CloseKindNormal, Code=1000, Reason="stream completed")`。
- [x] ctx cancel 路径 `Close(CloseKindAbort)`。
- [x] Pump.OnClose 通知 builder 异常退出。
- [x] 验证全零 Config 不发 ping、不触发 watchdog、EOF 自然结束。

## 11. Relay 迁移

### 11.1 relay/realtime

- [x] 删除 package-level gorilla upgrader。
- [x] 使用 `wsconn.AcceptManaged` 完成客户端入站握手。
- [x] 将 origin 检查作为 `AcceptOptions.CheckOrigin` 纯函数传入。
- [x] 将 response header 作为 `AcceptOptions.ResponseHeader` 传入。
- [x] 将 subprotocols 作为 `AcceptOptions.Subprotocols` 传入。
- [x] Actor 持有 client-side `*wsconn.ManagedConn`。
- [x] Actor 继续持有 provider-side `session.RealtimeSession`。
- [x] Pump.Handle 绑定 actor client frame handler，必须非阻塞。
- [x] Pump.OnClose 绑定 actor client close handler。
- [x] client frame handler copy payload 后非阻塞 post 到 actor。
- [x] client frame channel 满时 `CloseKindBackpressure` + 1013。
- [x] provider RecvEvent.Frame 映射为 downstream `WriteMessage`。
- [x] provider RecvEvent.ProviderClose 在 Recv 调用点直接 `client.Close(CloseKindGracefulShutdown, Code=SanitizeWireCloseCode(code))`。
- [x] provider close code 例如 4408 必须原样通过 sanitize 后上 wire。
- [x] 不引入 Bridge 抽象。

### 11.2 relay/responses_ws 入口

- [x] 使用 `wsconn.AcceptManaged` 完成客户端入站握手。
- [x] 设置 Label 为 client-responses-ws。
- [x] 设置 PingInterval、PongMissTimeout、InboundActivityTimeout。
- [x] `responses_websocket_client_inbound_activity_timeout_ms` 取代旧 `client_pong_timeout_ms` 行为。
- [x] 通过 `ReadInitial(firstCtx)` 同步读首帧。
- [x] firstCtx 使用 `config.ResponsesWSFirstFrameTimeout()`。
- [x] ReadInitial error 时可写 first-frame JSON error，再 Close。
- [x] 非 Text 首帧 Close：`CloseKindNormal` + `CloseUnsupportedData` + reason `text_only`。
- [x] JSON parse 失败可写 `invalid_response_create` JSON error，再 Close：`CloseKindNormal` + `ClosePolicyViolation`。
- [x] 首帧阶段只做帧类型校验与协议 JSON parse。
- [x] quota、lease 升级、RPM、model 准入、affinity owner 检查不得放在首帧外层。
- [x] parse 成功后立即 `actor.Start()`。
- [x] 启动 `wsconn.Pump`，Handle 直接绑定 `actor.onClientFrame`。
- [x] 启动 Pump 后将首帧作为 `FirstTurnSetup` 投递给 actor。
- [x] `FirstTurnSetup` 投递失败时释放 pending lease 并 request close intent。
- [x] heavy admission 只在 actor 单线程处理 `FirstTurnSetup` 时发生。

### 11.3 relay/responses_ws actor client frame

- [x] 实现或改造 `actor.onClientFrame(ctx, mt, payload)`。
- [x] 在 `actor.onClientFrame` 中 copy payload。
- [x] 构造 `ClientFrameEvent{MT, Payload, ReceivedAt}`。
- [x] 使用非阻塞 select 投递到 actor events。
- [x] actor 已退出时丢弃事件，连接由 OnClose 路径处理。
- [x] channel 满时 request close intent `client_frame_backpressure`。
- [x] channel 满时调用 `conn.Close(CloseKindBackpressure, Code=CloseTryAgainLater)`。
- [x] 沿用当前 `responsesWSEventQueueSize = 128`。
- [x] 首版不新增 payload bytes cap，压测后再判断。

### 11.4 relay/responses_ws provider recv

- [x] provider `RecvEvent.Frame` 投递为 `ProviderDownstreamFrame`。
- [x] `Frame + Usage` 在同一 actor event 中保持 Frame -> Usage 顺序。
- [x] usage-only event 投递为 `ProviderUsageObserved`。
- [x] usage-only event 不得被 Frame==nil 分支吞掉。
- [x] `ProviderClose` 投递为 `ProviderClosed` actor event，不在 recvLoop 直接 client.Close。
- [x] `ProviderClose` 后 recvLoop 终止。
- [x] `RecvEvent.Err` 投递为 `ProviderBusinessError`。
- [x] 顶层 Recv error 投递为 `ProviderRecvFailed` 并退出。

### 11.5 relay/responses_ws ProviderClose actor 处理

- [x] actor handle `ProviderClosed` 时通过 `SanitizeWireCloseCode(ev.Code)` 取得 downstream code。
- [x] pendingAttempt 存在时缓冲 provider close，不直接关闭下游。
- [x] activeAttempt 存在时 MarkCompleted。
- [x] 执行 `finalizeActiveAttempt()` 完成 settlement/quota finalize。
- [x] 执行 `clearActiveTurn()`。
- [x] 执行 `markDownstreamCloseSent()` 防重发。
- [x] 最后 `conn.Close(CloseKindGracefulShutdown, Code=sanitized, Reason=ev.Reason, Err=ev.Err)`。
- [x] 确认 active/pending lease 按现有业务语义释放。
- [x] 禁止外层直接 Close 绕过 settlement。

### 11.6 ProviderUsageObserved actor 处理

- [x] actor handle `ProviderUsageObserved` 不写 downstream。
- [x] actor handle `ProviderUsageObserved` 不 MarkCompleted。
- [x] actor handle `ProviderUsageObserved` 不 finalizeActiveAttempt。
- [x] actor handle `ProviderUsageObserved` 不 clearActiveTurn。
- [x] 只更新 settlement 累积态。
- [x] input audio transcription usage 但模型未配置 transcription 定价时记录 `usage_observed_unbilled{source="input_audio_transcription",model=X}` metric +1。
- [x] 未配置 transcription 定价时 actor 不阻塞、不报错，usage 不进入 settlement 累加。
- [x] realtime response usage 走 tokens settlement，定价表必须命中，命中失败按现有漏帐告警处理。

### 11.7 responses_ws 旧 IOBridge 清理

- [x] 删除或替换 `ResponsesWSIOBridge` 中 read pump。
- [x] 删除或替换 `ResponsesWSIOBridge` 中 ping loop。
- [x] 删除或替换 `ResponsesWSIOBridge` 中 liveness watchdog。
- [x] 删除或替换 `ResponsesWSIOBridge` 中传输层 send worker。
- [x] send worker 中属于业务调度的 attemptID/upstreamSessionGeneration 校验保留并搬到 Actor。
- [x] send outcome 投递等业务语义保留。
- [x] `relay/responses_ws.go` 行数下降目标约 2300-2400 行；不以行数为硬验收，但用于 review 检查是否真的移除了传输重复逻辑。

## 12. common/requester 与 authutil 收口

- [x] 将 `common/requester/ws_client.go` 的出站 dial 能力迁入 `common/wsconn/dial.go`。
- [x] 将 `common/requester/ws_writer.go` 的安全写能力迁入 wsconn。
- [x] 将 `common/requester/ws_close.go` 的 close reason 能力迁入 wsconn。
- [x] 将 `common/requester/ws_activity.go` 的 activity 逻辑迁入 wsconn。
- [x] 将 `common/requester/ws_read.go` 的 read limit 逻辑迁入 wsconn。
- [x] 将 `common/requester/ws_control_writer.go` 的 control writer 迁入 wsconn。
- [x] 将 `common/requester/ws_error.go` 的错误分类能力迁入 wsconn。
- [x] 将 `common/requester/ws_requester.go` 中仍需要的非 gorilla 逻辑迁移或删除。
- [x] 将 `common/requester/ws_reader.go` 的 reader 逻辑迁入 wsconn 或删除。
- [x] 将 `common/requester/ws_active_guard.go` 的 active counter guard 逻辑迁入 wsconn 或由 ManagedConn 生命周期替代。
- [x] 删除所有 `common/requester/ws_*.go` 原文件，不留兼容 wrapper。
- [x] 拆解 `common/requester/realtime_session_proxy.go`：IO 迁入 wsconn，业务编排回到 openai/codex provider。
- [x] 删除 `common/requester/realtime_session_proxy.go` 或移除其中 WS IO 职责。
- [x] `SendWSJsonRequest` 改写为 caller 显式 `json.Marshal` + `ManagedConn.WriteMessage(TextMessage, payload)`。
- [x] `ManagedConn` 不提供 `WriteJSON`。
- [x] `common/authutil/credential.go` 改用 `wsconn.Subprotocols(r)`。
- [x] `common/authutil/credential.go` 改用 `wsconn.IsUpgrade(r)`。

## 13. 进程级 Graceful Shutdown

- [x] 梳理服务内活跃 WebSocket 连接注册位置。
- [x] 将活跃连接注册为 `*wsconn.ManagedConn`。
- [x] Shutdown 遍历 active conns 调用 `Close(CloseKindGracefulShutdown, Code=CloseGoingAway, Reason="server_shutdown")`。
- [x] Shutdown 等待所有 `conn.Done()` 或 ctx 超时。
- [x] 不新增 `CloseKindServerShutdown`，使用 `CloseKindGracefulShutdown + Reason` 表达。

## 14. CI 与架构契约

### 14.1 depguard 硬门禁

- [x] 在 `.golangci.yml` 的 `linters.enable` 中启用 `depguard`。
- [x] 配置 depguard 规则：除 `common/wsconn/**` 与 `common/wsconn/wstest/**` 外禁止 import `github.com/gorilla/websocket`。
- [x] 配置 depguard 规则：非 `*_test.go` 禁止 import `one-api/common/wsconn/wstest`。
- [x] 配置 depguard 规则：`common/wsconn/**` 禁止 import `one-api/relay`。
- [x] 配置 depguard 规则：`common/wsconn/**` 禁止 import `one-api/providers`。
- [x] 配置 depguard 规则：`common/wsconn/**` 禁止 import `one-api/runtime/session`。
- [x] 配置 depguard 规则：`common/wsconn/**` 禁止 import `one-api/model`。
- [x] 按当前 golangci-lint 版本校验 depguard v2/v1 配置字段。
- [x] 本地运行 `make lint` 或等价命令，确认 depguard 实际生效。
- [x] 故意在非 wsconn 文件 import gorilla，确认 lint 拦下。
- [x] 故意在生产文件 import `common/wsconn/wstest`，确认 lint 拦下。
- [x] 确认测试文件 import `common/wsconn/wstest` 允许通过。

### 14.2 advisory 检查

- [x] 添加 advisory grep：`common/wsconn/` 中出现 `session|turn|quota|billing|provider|model` 时输出 warning，不阻塞 CI。
- [x] 添加 advisory grep：`common/wsconn/` 中出现 `response.create`、`response.completed`、`response.failed` 等协议事件字符串时输出 warning。
- [x] 添加 advisory grep：`relay/` 与 `providers/` 中出现 `wsconn.CloseCode(` 时输出 warning。
- [x] 明确只有 `SanitizeWireCloseCode` 实现和测试允许处理任意 int 到 CloseCode。

### 14.3 grep 兜底

- [x] `grep -rE 'websocket\.' --include='*.go'` 在 `common/wsconn/**` 外无命中。
- [x] grep 确认不存在 `ResponsesWSIngress`。
- [x] grep 确认不存在 `BoundedReliableQueue`。
- [x] grep 确认不存在 `OfferOutcome`。
- [x] grep 确认生产代码不 import `common/wsconn/wstest`。

## 15. 测试矩阵：wsconn 基础与关闭

- [x] first-write-wins 并发测试：多 goroutine 并发 Close，不同 Kind，最终 CloseInfo 为首个成功者。
- [x] first-write-wins 并发测试：CAS 失败者 Close 调用 1ms 内返回，不被 cleanup deadline 阻塞。
- [x] CloseKindNormal 默认发 exactly 1 个 close frame，code=1000。
- [x] CloseKindGracefulShutdown 默认发 exactly 1 个 close frame，code=1001。
- [x] CloseKindInboundIdle 短 deadline 内可发时 peer 收到 exactly 1 个 close frame，code=1001。
- [x] CloseKindBackpressure 短 deadline 内可发时 peer 收到 exactly 1 个 close frame，code=1013。
- [x] InboundIdle/Backpressure socket 已坏时不重试，吞掉错误，cleanup 按时完成。
- [x] CloseKindAbort peer 收不到 close frame。
- [x] CloseKindPongMiss peer 收不到 close frame。
- [x] CloseKindWriteError peer 收不到 close frame。
- [x] CloseKindHandlerPanic peer 收不到 close frame。
- [x] 普通 CloseKindReadError peer 收不到 close frame。
- [x] CloseKindReadError + Code=CloseMessageTooBig 时 peer 收到 exactly 1 个 1009 close frame，来源为 gorilla，wsconn 不重复发。
- [x] `SanitizeWireCloseCode(3000)==3000`。
- [x] `SanitizeWireCloseCode(4408)==4408`。
- [x] `SanitizeWireCloseCode(4999)==4999`。
- [x] `SanitizeWireCloseCode(999)==1011`。
- [x] `SanitizeWireCloseCode(1004)==1011`。
- [x] `SanitizeWireCloseCode(1005)==1011`。
- [x] `SanitizeWireCloseCode(1006)==1011`。
- [x] `SanitizeWireCloseCode(1015)==1011`。
- [x] `SanitizeWireCloseCode(5000)==1011`。
- [x] CloseInfo{Code: CloseNoStatusReceived} 走发 frame 路径时 wire code 被替换为合法值并记 log。
- [x] CloseInfo{Code: CloseAbnormalClosure} 走发 frame 路径时 wire code 被替换为合法值并记 log。
- [x] PeerClose 不重复发 close frame。
- [x] reason >123 字节时按 UTF-8 边界截断。
- [x] reason 含多字节字符时不切到 rune 中间。
- [x] reason 含无效 UTF-8 字节时输出合法 UTF-8。
- [x] WriteMessage 底层 write error 后 CloseInfo.Kind 为 CloseKindWriteError。
- [x] WriteMessage 写失败 release-then-close 序列无 deadlock。
- [x] OnClose 内调用 `conn.Close(...)` 立即返回。
- [x] OnClose 内调用 `conn.WriteMessage(...)` 返回关闭错误，例如 `net.ErrClosed`。
- [x] OnClose 不在 close 锁内同步调用；可在 -race/debug build 下加 invariant assert。

## 16. 测试矩阵：ReadInitial 与 Pump

- [x] ReadInitial 正常路径：peer 发一帧，返回 TextMessage 与 payload。
- [x] ReadInitial 成功后启动 Pump，Pump 从第 2 帧开始调用 Handle。
- [x] ReadInitial ctx deadline 已到，返回 `context.DeadlineExceeded`，wire 上不发 close frame。
- [x] ReadInitial ctx cancel，返回 `context.Canceled`，wire 上不发 close frame。
- [x] ReadInitial oversized 首帧返回 sentinel，调用方仍可 WriteMessage 写 JSON error，再 Close。
- [x] ReadInitial oversized 首帧 wire 上不出现 1009 close frame。
- [x] ReadInitial peer 握手后直接发 close，返回 `*wsconn.CloseError`。
- [x] ReadInitial 普通协议读取失败或底层 IO error 返回 error，wire 上不主动发 close frame，失败后再启动 Pump panic。
- [x] ReadInitial 重复调用 panic。
- [x] Pump.Run 已启动后再 ReadInitial panic。
- [x] ReadInitial 期间启动 Pump.Run panic。
- [x] ReadInitial 失败后再 Pump.Run panic。
- [x] Pump.Run 退出后再次 Pump.Run panic。
- [x] Pump.Run 退出后再次 ReadInitial panic。
- [x] ReadInitial 期间 fake clock 推进不会触发 PongMissTimeout。
- [x] ReadInitial 期间 fake clock 推进不会触发 InboundActivityTimeout。
- [x] ReadInitial 期间不触发 Config.OnActivity。
- [x] ReadInitial 成功返回后启动 Pump，再 cancel 原 ctx，不打断 Pump read loop。
- [x] ReadInitial watcher goroutine 无泄漏。
- [x] Pump ctx cancel 时 watcher 主动 Close，CloseInfo.Kind=CloseKindAbort，Reason 包含 ctx_done。
- [x] Pump ctx cancel 后 fallback `pump_exit_without_close` CAS 失败，不覆盖 CloseInfo。
- [x] read path PeerClose 返回前已分类 Close。
- [x] read path ReadError 返回前已分类 Close。
- [x] read path ErrReadLimit 返回前已分类 Close。
- [x] read path WriteError 返回前已分类 Close。
- [x] read path HandlerPanic 返回前已分类 Close。
- [x] 同一 ManagedConn 同时启动两个 Pump.Run，第二个 panic，panic 消息含 Pump.Run / invalid read state。
- [x] Pump watcher goroutine 在正常退出时无泄漏。
- [x] Handle 函数无 error 返回值。
- [x] ErrReadLimit 不透传到 Handle，在 OnClose 中以 CloseInfo 感知。
- [x] slow-handle observation 使用 fake recorder + fake clock，Handle 内推进 5ms 后 recorder 收到 Observe(5ms)。

## 17. 测试矩阵：liveness 与 control handler

- [x] `PongMissTimeout == 0` 分支持续发 ping。
- [x] `PongMissTimeout == 0` 分支缺 pong 不关闭。
- [x] `PongMissTimeout == 0` 分支不被 awaiting 卡住后续 ping。
- [x] generation=1 ping 后注入 payload 不匹配伪 pong，PongMissTimeout 仍按时触发。
- [x] `PongMissTimeout > PingInterval` 时同一时刻只有一个 outstanding ping。
- [x] awaiting=true 期间 ticker 不发新 ping，不叠加 timer。
- [x] 匹配 pong 到达后 outstandingTimer.Stop 被调用。
- [x] 匹配 pong 到达后 watchdog callback 不再触发。
- [x] 匹配 pong 到达后 awaiting=false，下一个 ticker 发送 gen=2。
- [x] 所有 pong，包括不匹配伪 pong，都调用 markInboundActivity。
- [x] ticker/PongHandler/timer callback 高频交错在 `go test -race` 下无数据竞争。
- [x] 任意时刻 outstandingTimer 与 outstandingGen 一致。
- [x] mock control writer 使 EnqueuePing 返回 error，断言 outstanding timer 已 Stop。
- [x] EnqueuePing 失败后 fake clock 推进到 PongMissTimeout 不触发 PongMiss。
- [x] EnqueuePing 失败 CloseInfo.Kind 为 CloseKindWriteError，不是 CloseKindPongMiss。
- [x] InboundActivityTimeout 全静默时触发。
- [x] 持续 inbound control frame 时 InboundActivityTimeout 不触发。
- [x] 持续完整 data message 时 InboundActivityTimeout 不触发。
- [x] `InboundActivityTimeout == nil` 不启动 watchdog。
- [x] `InboundActivityTimeout() == 0` 不启动 watchdog。
- [x] outbound ping 不刷新 InboundActivityTimeout。
- [x] 全零 Config 不发 ping、不触发 watchdog、EOF 自然结束。
- [x] fake clock 推进到 PongMissTimeout deadline 时立即触发。
- [x] fake clock 推进到 InboundActivityTimeout deadline 时立即触发。
- [x] close frame 的 net.Conn 写 deadline 不受 fake clock 影响，使用 mock net.Conn 单独覆盖。
- [x] wsconn 包装后 PingHandler/PongHandler 不再是 gorilla 默认。
- [x] inbound ping 触发 markInboundActivity。
- [x] Pump 阶段 inbound ping 触发 `Config.OnActivity` 一次，回调时间来自 `Config.Clock`。
- [x] inbound ping 的 pong 走 controlWriter.EnqueuePong，计数 +1。
- [x] PingHandler 不调用 raw.WriteControl(PongMessage) 路径。
- [x] PingHandler 中 EnqueuePong 返回 error 时归类 CloseKindWriteError。
- [x] inbound pong 同时触发 markInboundActivity 与 observePongGeneration。
- [x] Pump 阶段 inbound pong 触发 `Config.OnActivity` 一次，回调时间来自 `Config.Clock`。
- [x] Pump 阶段完整 data message 触发 `Config.OnActivity` 一次，且 ReadInitial 阶段同样消息不触发。
- [x] peer close control frame 进入 `CloseKindPeerClose` 分类前记录 inbound activity，并按 Pump 阶段语义触发 `Config.OnActivity`。

## 18. 测试矩阵：Accept/Dial/Security/Config

- [x] AcceptManaged 握手失败资源不泄漏。
- [x] AcceptOptions.CheckOrigin 透传。
- [x] AcceptOptions.ResponseHeader 透传。
- [x] AcceptOptions.ReadBufferSize 透传。
- [x] AcceptOptions.WriteBufferSize 透传。
- [x] AcceptOptions.EnableCompression 透传。
- [x] AcceptOptions.Subprotocols 透传。
- [x] AcceptOptions.Error 透传。
- [x] DialManaged 握手失败返回 `*wsconn.DialError`。
- [x] `errors.As(err, &dialErr)` 可取得 URL、StatusCode、Header、BodySnippet、CloseInfo。
- [x] 握手失败响应 body 读取失败时 `dialErr.BodyReadErr` 非 nil，且 StatusCode/Header/CloseInfo 仍可取得。
- [x] `dialErr.CloseInfo.Kind == CloseKindDialFailed`。
- [x] DialOption 导出签名无 gorilla 类型。
- [x] 默认 TLS 校验开启；自签名或 hostname mismatch 证书在默认配置下握手失败。
- [x] 仅显式 `WithTLSConfig(&tls.Config{InsecureSkipVerify: true})` 时允许跳过 TLS 校验。
- [x] 默认 policy 下 `ws://example` 返回支持 `errors.Is(err, wsconn.ErrInsecureScheme)` 的错误。
- [x] `AllowInsecureWS=true` 后允许 ws://。
- [x] 默认 policy 下 127.0.0.1 返回支持 `errors.Is(err, wsconn.ErrPrivateAddrBlocked)` 的错误。
- [x] 默认 policy 下 10.x 返回支持 `errors.Is(err, wsconn.ErrPrivateAddrBlocked)` 的错误。
- [x] 默认 policy 下 192.168.x 返回支持 `errors.Is(err, wsconn.ErrPrivateAddrBlocked)` 的错误。
- [x] `AllowPrivateIP=true` 后允许 RFC1918。
- [x] metadata IP 169.254.169.254 默认返回支持 `errors.Is(err, wsconn.ErrPrivateAddrBlocked)` 的错误。
- [x] metadata IP 即使 `AllowPrivateIP=true` 也默认拒绝。
- [x] 自定义 HostFilter 同意时才可放行特殊地址。
- [x] DialError.Error() 不含 Authorization 值。
- [x] DialError.Error() 不含 Cookie 值。
- [x] DialError.Error() 不含 Sec-WebSocket-Protocol 值。
- [x] dial 失败 body 超过 MaxBodySnippet 时 BodySnippet 长度被限制且 BodyTruncated=true。
- [x] WithProxyURL("") 不使用 proxy。
- [x] WithProxyURL("not a url") 返回支持 `errors.Is(err, wsconn.ErrInvalidProxyURL)` 的错误，不直连。
- [x] WithProxyURL("ftp://x") 返回支持 `errors.Is(err, wsconn.ErrInvalidProxyURL)` 的错误。
- [x] proxy 解析失败时 mock NetDialContext 未收到目标 host 直连尝试。
- [x] Config: `PingInterval=0` 且 `PongMissTimeout>0` 返回 ErrInvalidConfig。
- [x] Config: `PingInterval<0` 返回 ErrInvalidConfig。
- [x] Config: `PongMissTimeout<0` 返回 ErrInvalidConfig。
- [x] Config: `ReadLimit<0` 返回 ErrInvalidConfig。
- [x] Config: 首次 `WriteTimeout()<0` 返回 ErrInvalidConfig。
- [x] Config: 首次 `InboundActivityTimeout()<0` 返回 ErrInvalidConfig。
- [x] Config: `PingInterval>=PongMissTimeout` 且两者 >0 成功。
- [x] Config: 运行期 `WriteTimeout()<0` 使用 defaultWriteTimeout 并 warn。
- [x] Config: 运行期 `InboundActivityTimeout()<0` 禁用 watchdog 并 warn。
- [x] Config: 全零成功返回 ManagedConn。
- [x] ReadLimit 正向生效：Pump 路径设置小 `ReadLimit` 后 oversized data message 触发 `CloseKindReadError + Code=CloseMessageTooBig`，且不调用 Handle。
- [x] ReadLimit 正向生效：`ReadInitial` 路径设置小 `ReadLimit` 后 oversized first frame 返回支持 `errors.Is(err, wsconn.ErrFirstFrameTooLarge)` 的错误，wire 上不出现 1009，调用方仍可写 JSON error。
- [x] ReadLimit 默认值生效：`ReadLimit=0` 使用 wsconn 默认 16 MiB，而不是无上限。
- [x] WriteTimeout 正向生效：自定义正数 `WriteTimeout()` 在 `WriteMessage` data frame 写路径被调用并用于底层 write deadline。
- [x] WriteTimeout 正向生效：自定义正数 `WriteTimeout()` 在 ping/pong control frame 写路径被调用并用于底层 write deadline。
- [x] WriteTimeout 正向生效：自定义正数 `WriteTimeout()` 在 close frame 写路径被调用并用于底层 write deadline。

## 19. 测试矩阵：runtime/session 与 provider/relay

- [x] `session.Frame{kind: 99}` 在外部包字面量构造编译失败。
- [x] `NewTextFrame` 与 `NewBinaryFrame` 是唯一常规构造入口。
- [x] SendClient 传零值 Frame 返回 `ErrInvalidFrame`。
- [x] SendClient 传未知 kind Frame 返回 `ErrInvalidFrame`。
- [x] Frame.Kind() 在正常路径只返回 Text/Binary。
- [x] 顶层 Recv error 非 nil 时 RecvEvent 全零，含 Usage。
- [x] RecvEvent 任一字段非空时顶层 error 为 nil。
- [x] provider 业务错误经 RecvEvent.Err 投递。
- [x] Usage-only event 合法。
- [x] Frame+Usage event 合法。
- [x] Usage+Err 组合视为 provider bug，测试或 fuzz 不应出现。
- [x] Frame+Usage+Err 组合视为 provider bug，测试或 fuzz 不应出现。
- [x] ProviderClose 与 Frame/Usage/Err 并存视为 provider bug，测试或 fuzz 不应出现。
- [x] `response.done.status=failed` 作为 ProviderDownstreamFrame 带 Usage 到达 actor，不转成 ProviderBusinessError。
- [x] `response.done.status=incomplete` 作为 ProviderDownstreamFrame 带 Usage 到达 actor，不转成 ProviderBusinessError。
- [x] `response.done.status=cancelled` 作为 ProviderDownstreamFrame 带 Usage 到达 actor，不转成 ProviderBusinessError。
- [x] provider 模拟 input_audio_transcription usage-only，actor 收到 ProviderUsageObserved。
- [x] usage-only 路径 actor 不收到 ProviderDownstreamFrame。
- [x] provider emit 的 Frame、Usage、ProviderClose、Err 类型 `RecvEvent` 均保持现有 `Origin` 语义，不因接口迁移变成零值或错误来源。
- [x] `ResponsesWSSendPreflightCapable` 迁移后仍被 ResponsesWS preflight 路径识别并按现有语义调用。
- [x] `GracefulDetachCapable` 迁移后仍按现有语义保留 graceful detach 能力。
- [x] ProviderUsageObserved 不写 downstream。
- [x] ProviderUsageObserved 不触发生命周期完成/清理。
- [x] ProviderUsageObserved 只累加 settlement 状态。
- [x] input_audio_transcription 未配置定价时 metric `usage_observed_unbilled` +1，actor 不阻塞。
- [x] provider upstream close code 4408 转为 `session.RecvEvent{ProviderClose.Code=4408}`。
- [x] `/v1/realtime` 将 ProviderClose 直接 close 下游，wire code 4408。
- [x] ResponsesWS 将 ProviderClose 投递 actor，完成 settlement 后 close 下游，wire code 4408。
- [x] ResponsesWS pendingAttempt 下 provider close 被缓冲，不绕过 actor 状态机。
- [x] ResponsesWS activeAttempt 下 provider close 完成 finalize/clear/lease release。
- [x] 首帧路径顺序为 `ReadInitial -> 帧类型校验 -> JSON parse -> actor.Start -> Pump.Run -> PostReliable(FirstTurnSetup)`。
- [x] heavy admission 只在 actor handle FirstTurnSetup 时发生。
- [x] Pump.Handle 直接绑定 actor.onClientFrame，无中间 adapter。

## 20. 迁移收口与删除

- [x] 除 `common/wsconn/**` 外，所有生产业务代码不再 import gorilla。
- [x] 除 `common/wsconn/**` 与 `common/wsconn/wstest/**` 外，所有测试代码不再 import gorilla，改用 `common/wsconn/wstest` 或 wsconn 内部测试 helper。
- [x] 删除 `common/requester/ws_*.go`。
- [x] 删除或清空 `common/requester/realtime_session_proxy.go` 的 WS IO 旧实现。
- [x] 删除所有旧 read pump/ping loop/watchdog/send worker 传输重复实现。
- [x] 删除旧 `client_pong_timeout_ms` 配置读取路径。
- [x] 更新 `config.example.yaml`。
- [x] 更新实际配置默认值和配置解析。
- [x] 更新 README 或部署文档，说明新 timeout 配置不兼容旧名。
- [x] 更新相关开发文档索引。
- [x] 确认 `docs/dev/websocket-transport-architecture.md` 仍被描述为当前旧方案或被新文档替代，不让二者语义冲突。

## 21. 推荐 commit 拆分

说明：commit 拆分只为 review 可读性，部署仍是一次切换；最终收口前不得合并会让主分支长期双轨运行的状态。

- [x] Commit 1：新增 `common/wsconn` 骨架与 `common/wsconn/wstest` 最小表面，编译通过，不迁移业务。
- [x] Commit 1 中可加入 depguard 草案，但不启用 blocking，避免现有 gorilla import 导致 CI 红。
- [x] Commit 2：迁移 openai/codex/xunfei provider，完成 `runtime/session` typed Frame / RecvEvent / ProviderClose 切换，更新测试 mock。
- [x] Commit 3：迁移 relay/realtime 与 relay/responses_ws，完成 ReadInitial + Pump 入口，更新 authutil 与 SendWSJsonRequest 调用形态。
- [x] Commit 4：删除 `common/requester/ws_*.go` 与旧 proxy，启用 depguard blocking，完成旧配置重命名与文档收口。
- [x] 每个 commit 独立通过编译与相关测试。
- [x] 最终 commit 必须通过全量 `go test ./...`。
- [x] 最终 commit 必须通过 lint。

## 22. 最终验收命令与人工检查

- [x] 运行 `go test ./...` 并通过。
- [x] 运行 `go test -race ./common/wsconn/...` 并通过。
- [x] 运行 provider、relay 相关重点测试并通过。
- [x] 运行 `make lint` 或项目等价 lint 命令并通过。
- [x] 运行 gorilla import grep，确认只在 `common/wsconn/**` 与 `common/wsconn/wstest/**` 出现。
- [x] 运行 `websocket\.` grep，确认 wsconn 外无 gorilla 常量/API 泄漏。
- [x] 运行 wstest import grep，确认生产代码无 `common/wsconn/wstest`。
- [x] 人工 review `common/wsconn` 导出 API，确认无 gorilla 类型、无业务包 import、无业务关键词实质耦合。
- [x] 人工 review `common/wsconn` 导出 API，确认没有架构文档未列明的新公共能力；如确需新增，必须先更新架构文档并解释 trade-off。
- [x] 人工 review `common/wsconn/wstest` 导出 API，确认除 `Option`、`Pair`、`Server`、`WithClock` 外无其它导出符号。
- [x] 人工 review relay/provider close code 映射，确认 provider int close code 都经 `SanitizeWireCloseCode`。
- [x] 人工 review ResponsesWS provider close 路径，确认 settlement/lease/finalize 未被绕过。
- [x] 人工 review Pump.Handle 实现，确认所有 handler 都是非阻塞 post 或明确短路径。
- [x] 人工 review ReadInitial 首帧路径，确认没有 heavy admission 迁出 actor。
- [x] 人工 review xunfei 全零 Config，确认没有隐式 ping/watchdog 副作用。
- [x] 人工 review DialSecurityPolicy 默认值，确认本地/staging 需要 ws:// 或私网时已显式 opt-in。
- [x] 人工 review `common/wsconn` 未扩展 metrics 维度，CloseInfo 只提供基础字段，业务 metrics 扩展不进入本方案。
- [x] 人工 review `common/wsconn` 未内置 provider endpoint allowlist、metadata-source 段定义、企业代理 DNS 策略；这些策略只能通过 `HostFilter` 或 provider/配置层表达。
- [x] 人工 review read lifecycle 未超过 5 态，未增加 reset/recoverable reader 状态。
- [x] 人工 review 未新增 `CloseKindProtocolReject`、`CloseKindServerShutdown` 等越界 CloseKind，协议拒绝仍用 `CloseKindNormal + code`，服务关闭仍用 `CloseKindGracefulShutdown + Reason`。
- [x] 人工 review 未新增 DialSecurityPolicy profile/helper（如 `StrictPublicEgressPolicy`、`ConfiguredEgressPolicy`、`LocalTestEgressPolicy`）；如后续需要 profile，必须另起 follow-up 设计。
- [x] 实现完成后更新 `docs/dev/wsconn-architecture.md` 文档状态，删除或改写“目标方案，尚未落地”，明确该文档描述的是已落地方案。
