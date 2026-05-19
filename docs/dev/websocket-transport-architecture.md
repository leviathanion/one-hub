# WebSocket Transport 复用方案

## 文档状态

本文描述 one-hub 如何把 `/v1/realtime` 与 `GET /v1/responses` 中重复的 WebSocket I/O 能力收敛为共享 safety primitive。Phase 0（正确性修复）和 Phase 1–4（抽 primitives、ResponsesWS 复用、文档拆分、回看）均已完成，当前代码已按本文方案落地。

当前实现总览：
- `common/requester/ws_writer.go` — `WSClientWriter`、`WithWSWriteDeadline`
- `common/requester/ws_close.go` — `SafeWSCloseReason`、`SafeWSCloseMessage`
- `common/requester/ws_activity.go` — `InstallWSActivityHandlers`
- `common/requester/ws_read.go` — `ApplyWSReadLimit`
- `common/requester/ws_error.go` — `WriteWSLocalError`
- `common/requester/ws_active_guard.go` — `WSActiveCounterGuard`
- `common/requester/ws_control_writer.go` — `WSControlFrameWriter`（已有，继续复用）
- `/v1/realtime` 通过 `RealtimeSessionProxy` 使用上述 primitives
- `GET /v1/responses` 通过 `ResponsesWSIOBridge` 使用上述 primitives
- 暂未创建 `wsrelay` 子包
- read/recv/send pump 仍由各自 orchestrator 持有，未抽成 callback helper

Phase 5（回看 RealtimeSessionProxy 是否薄化）留待后续观察决定。

## 真实问题

`/v1/realtime` 和 `GET /v1/responses` 都需要 WebSocket 底层能力，但业务语义不同：

1. `/v1/realtime` 是 pass-through：客户端帧送上游，上游帧写回客户端，连接关闭时按 detach/abort 收尾。
2. `GET /v1/responses` 是 turn-based：每个 `response.create` 都有 RPM、quota、affinity、send 结果、terminal 分类和 rollback/finalize 边界。

所以目标不是让 ResponsesWS 直接复用 `RealtimeSessionProxy`，也不是一次性抽象一个通用 WebSocket 框架；目标是先消除两条链路里重复且容易出错、语义稳定的 I/O 代码，包括 single writer、write deadline、read limit、control frame、close reason、ping/pong activity、bounded error/control writer、active counter guard 和本地 OpenAI-compatible WS error writer。read/recv/send pump 虽然也有模式重复，但它们承载关闭、detach、abort、retry、quota 等业务决策，第一阶段不抽。

最终架构边界是：

- ResponsesWS 使用 `ResponsesWSSessionActor + ResponsesWSIOBridge`，actor 拥有 turn ledger、RPM、quota、affinity、send outcome 和 finalize。
- Realtime 继续使用 pass-through-oriented `RealtimeSessionProxy`，只解析必要的 session、usage 和安全信号。
- 共享层只提供 WebSocket safety primitives；共享 provider URL/header builder 规则，但不共享 turn accounting、retry/finalize 逻辑或协议 parser。

## 已选型结论

采用两层边界，但第一阶段不引入新包：

| 层级 | 初始落点 | 职责 | 不负责 |
| --- | --- | --- | --- |
| WebSocket safety primitives | `common/requester` 内 unexported helper / 小文件 | single writer、write deadline、read limit、ping/pong activity、close control、UTF-8 close reason、bounded control/error writer、active counter acquire/release guard、本地 WS error writer | quota、RPM、channel selection、affinity、Responses terminal、turn retry/rollback 语义、read/recv/send pump 控制流 |
| Protocol orchestrator | 现有 relay/provider 入口 | 入口协议状态机、业务账本、错误语义、retry/rollback/finalize、读写循环的业务收尾 | 重复实现 writer lock、deadline、close reason、control-frame 细节 |

关键边界：阶段 1 的底层 primitive 只封装机械事实，例如 `write returned nil`、`write returned error`、deadline 设置/清理结果、close control 写入结果、read limit 设置结果和 lease release 是否成对发生。`LocalWriteOK / NotSent / Ambiguous` 这类账本语义不属于底层；它们由具体 orchestrator 基于当前协议和事务边界合成。

## 第一性原理

WebSocket 底层复用要满足五个事实：

1. **gorilla 事实**：同一连接只能有一个 writer；read-side control handler、read deadline 和 message read 的归属必须明确。
2. **并发事实**：client read、provider recv、provider send、watchdog 和 handler return 会并发发生；第一阶段只抽不会改变控制流的机械 I/O helper，goroutine 生命周期仍由现有 owner struct 管。
3. **语义事实**：底层不知道 turn，也不知道上游是否“观察到”一次业务请求。它不能替 ResponsesWS 判断 rollback、retry 或 quota finalize。
4. **失败事实**：网络写失败只是原始错误。是否 ambiguous、是否可证明 not-sent、是否可恢复，只能由上层结合 send 时机、provider evidence 和 session 状态判断。
5. **复杂度事实**：当前只有两个 WebSocket endpoint。复用方案必须服务当前问题，不能为了未来未知协议预设一套大框架。

## 外部依据

本方案参考以下一手资料，并把它们落实为本仓库的约束：

| 来源 | 对本方案的约束 |
| --- | --- |
| [gorilla/websocket Concurrency 文档](https://pkg.go.dev/github.com/gorilla/websocket#hdr-Concurrency) | 同一连接只支持一个并发 reader 和一个并发 writer；`SetReadDeadline`、`SetPingHandler`、`SetPongHandler` 属于 read-side 操作，write 方法必须由应用保证不并发。因此 single writer 和 control handler 安装是阶段 1 复用点；read pump 归属是必须遵守的约束，但暂不抽成 callback helper。 |
| [RFC 6455 WebSocket Protocol](https://www.rfc-editor.org/rfc/rfc6455) | control frame payload 最多 125 bytes；close frame payload 中前 2 bytes 是状态码，后续 reason 是 UTF-8。因此 close reason 截断和 close control 组帧属于协议底层能力。 |
| [OpenAI Realtime GA / deprecations](https://developers.openai.com/api/docs/deprecations) | Realtime GA 不再把 `OpenAI-Beta: realtime=v1` 作为默认路径；legacy/beta header 只能由 channel 显式配置。 |
| [Azure Realtime WebSocket](https://learn.microsoft.com/en-us/azure/foundry/openai/how-to/realtime-audio-websockets) | Azure Realtime GA 与 preview URL 形态不同，GA 使用 `/openai/v1/realtime?model=<deployment>`，preview 使用 `/openai/realtime?api-version=...&deployment=<deployment>`。 |
| [Go Code Review Comments: Interfaces](https://go.googlesource.com/wiki/+/6a941f07bde0a8939058c1fc3610dc4cba782a06/CodeReviewComments.md#Interfaces) | interface 应在使用方定义，不应在没有真实用例前预先定义；实现方优先返回具体类型。因此第一阶段只抽 unexported helper，不建立公共 interface-heavy 包。 |
| [Go Blog: Package names](https://go.dev/blog/package-names) | 包名是设计的一部分，过泛的 `util/common/api/interfaces` 会降低可维护性。因此暂不新增含义不稳的 `wsrelay` 包；若未来抽包，应等复用点稳定后选择短且具体的包名。 |
| [Go Blog: Context](https://go.dev/blog/context) | `Context` 用于跨 goroutine 传播取消和 deadline，且可被多个 goroutine 安全使用。因此现有 pump 生命周期继续用 context/cancel/wait group 表达，暂不引入独立 `Lifecycle` 类型。 |

这些依据共同指向一个折中：**复用必须发生在 gorilla writer 约束、deadline、read limit、control frame、close framing、activity tracking 和 acquire/release guard 这类稳定事实上；业务语义、账本语义、retry 语义和 pump 生命周期留在 orchestrator。**

## 复用单元

第一阶段只抽小而明确的单元，优先函数和一个窄结构体，不抽 callback-based pump。因为 ResponsesWS 位于 `relay` 包而 shared primitive 位于 `common/requester`，跨包复用需要暴露最小 concrete API；但不暴露通用 `Conn` interface，不把测试 fake 变成公共抽象。

| 单元 | 形式 | 职责 |
| --- | --- | --- |
| `WSClientWriter` / `wsClientWriter` | concrete writer | 唯一写客户端的组件；内部持有 write mutex，统一 write deadline 和 close control；跨包使用时暴露 concrete API |
| `WithWSWriteDeadline` / `withWSWriteDeadline` | helper func | 在实际写点设置/清理 `config.RealtimeWebsocketWriteTimeout()`，只返回原始 error |
| `SafeWSCloseReason` / `safeWSCloseReason` | helper func | 按 123-byte 限制截断 close reason，并保持合法 UTF-8 |
| `InstallWSActivityHandlers` / `installWSActivityHandlers` | helper func | 包装 ping/pong handler，只刷新 activity 并调用原 handler |
| `ApplyWSReadLimit` / `applyWSReadLimit` | helper func | 按配置设置 read limit，返回原始错误或由调用方决定关闭策略 |
| `WriteWSLocalError` / `writeWSLocalError` | helper func | 写本地 OpenAI-compatible WS error frame；不触发 terminal sniff 或账本副作用 |
| `WSActiveCounterGuard` / `wsActiveCounterGuard` | narrow guard | 封装 acquire/release 成对执行和幂等 release；具体维度仍由调用方决定 |

`WSControlFrameWriter` 已经存在，继续作为上游 pong/control frame 的 bounded writer 复用，不在本文新增第二套控制帧队列。error writer 和 active counter guard 只能表达安全生命周期，不允许携带 quota、attempt、channel selection 或 terminal 分类。

暂不抽以下单元：

| 单元 | 暂不抽原因 |
| --- | --- |
| `wsConn` interface | 当前没有稳定公共使用方；需要测试时在具体测试或使用方定义窄接口，避免实现方预设 interface。 |
| `runClientReadPump` | callback pump 会把 read loop、close 语义和业务事件投递绑在一起，容易变成换名 proxy。 |
| `runSessionRecvPump` | `payload + err` 的保留规则值得复用，但 pump 生命周期和事件投递由 realtime / ResponsesWS 分别持有。 |
| `runSessionSend` | 价值太低，容易把 send outcome 分类边界拉进 helper；先让调用方直接调用 `SendClient`。 |
| `ActivityClock` / `Lifecycle` | context、ticker、wait group 和 timestamp 先留在 owner struct；等出现真实重复再抽。 |

Trade-off：这会保留一些 read/recv loop 形状重复；收益是没有 callback framework，没有协议 flag，也不会把 detach/abort/quota/finalize 语义拖进底层 helper。

## Send 语义归属

三态分类属于 orchestrator，不属于 transport helper。

| 原始结果 | `/v1/realtime` 处理 | ResponsesWS 处理 |
| --- | --- | --- |
| ctx done before call | 连接收尾 | 通常可归为 not-sent，允许 rollback，前提是没有 provider evidence |
| `SendClient` 返回 nil | 继续转发 | local-write-ok，只表示本地调用成功，不表示 provider accepted |
| `SendClient` 返回业务错误 | 写 recoverable 或关闭 | 根据错误类型、attempt phase、provider evidence 判断 not-sent / rejected / ambiguous |
| `SendClient` 返回网络/写 deadline 错误 | 关闭或 detach | 通常归为 ambiguous，不 retry、不 quota undo |
| send worker panic recovered | 关闭 | ambiguous 或 fail closed，由 actor 持有状态判断 |

Trade-off：把三态移出底层会让 ResponsesWS actor 保留更多分类代码；收益是底层保持协议无关，避免把 turn 账本概念泄漏给 pass-through realtime。

## 与现有 `RealtimeSessionProxy` 的关系

`RealtimeSessionProxy` 当前混合了两类职责：

- I/O 职责：client read pump、session recv pump、single writer、idle watchdog、panic recovery。
- pass-through policy：客户端关闭时 detach/abort、provider 关闭时关闭代理、错误 payload 如何写回。

迁移后不急着删除这个类型。第一步只是在 `RealtimeSessionProxy` 内部拆出 writer/deadline/close/activity helper，并保持外部 `Start/Wait/Close/UserClosed/SupplierClosed` 行为不变。read loop、recv loop、idle watchdog 和 pass-through 收尾策略暂时留在 `RealtimeSessionProxy` 内。等 ResponsesWS 也复用这些 helper 后，再回看：

1. 如果 `RealtimeSessionProxy` 仍表达清晰的 pass-through policy，就保留它。
2. 如果它只剩 50 行左右的薄壳，并且调用方直接组合 writer/close/activity helper 更清楚，再删除或改名。

这个决策必须发生在重构之后，而不是方案阶段提前决定。

## 与 ResponsesWS Actor 的关系

ResponsesWS 仍保留专用 actor。原因是 ResponsesWS 的复杂度来自 turn 账本，而不是 WebSocket I/O。

I/O helper 负责：

- client writer 的单写、deadline 和 close control。
- close reason 的 UTF-8 安全截断。
- ping/pong activity handler 包装。
- 上游 control frame writer 的 bounded queue 复用。

ResponsesWS actor 负责：

- 首帧和后续 `response.create` 状态机。
- channel selection、RequireWS、affinity owner 检查。
- RPM admission、quota preconsume、rollback、finalize。
- provider event buffering。
- terminal classifier、record/clear affinity。
- 维护 client read pump、provider recv pump 和 send worker 的事件投递语义。
- 把 raw send result 合成 not-sent / local-write-ok / ambiguous，并处理 proof conflict。

Trade-off：actor 不会被“复用底层能力”消除，代码仍然有业务状态机；收益是 I/O bug 和账本 bug 分层，未来维护者可以先判断问题属于连接层还是 turn 层。

## 迁移方案

### 阶段 0：先修正确性风险

在抽象前先修会影响账本或安全的风险，避免把 bug 固化进共享 helper：

1. OpenAI Realtime GA 默认不发送 `OpenAI-Beta: realtime=v1`；legacy/beta 只能由 channel 显式配置启用。
2. Azure Realtime URL 明确区分 GA `/openai/v1/realtime?model=<deployment>` 与 preview `/openai/realtime?api-version=...&deployment=<deployment>`，不能用单一拼接规则覆盖。
3. quota 预扣改为 DB 原子条件更新，用户额度和 token 剩余额度都要有非负守卫。
4. WebSocket subprotocol 不能回显 `openai-insecure-api-key.*`；该值只能作为鉴权输入，不能作为协商结果返回。使用该 credential subprotocol 时必须配置显式 Origin 白名单；空白名单/通配 Origin 只有在 `realtime.unsafe_allow_credential_subprotocol_any_origin=true` 时作为旧版兼容路径放开。
5. Codex realtime reader 的 panic recovery 不能在持有 `exec.Lock()` 时重入 cleanup；cleanup 要拆成不会自锁的路径。
6. close reason 截断改成 UTF-8 安全。

### 阶段 1：只抽稳定 safety primitive

不新建包，不改外部行为：

- 抽 `wsClientWriter` 或等价 unexported helper，统一 write deadline 和 close control。
- 抽 `withWSWriteDeadline`、`safeWSCloseReason`、`installWSActivityHandlers`、read limit helper、本地 WS error writer 和 active counter guard。
- 复用现有 `WSControlFrameWriter`，不新增第二套 control queue。
- 不抽 `runClientReadPump`、`runSessionRecvPump`、`runSessionSend`。
- 保持现有测试全部通过，再补 close reason、ping/pong activity、write deadline、read limit、local error writer 和 active counter release 的底层单测。

这个阶段的目标是降低重复和 race 风险，不追求公共 API 漂亮，也不改变两条链路的事件循环。

### 阶段 2：让 ResponsesWS 复用 safety primitive

进入阶段 2 前先做一次调用点清单：列出 `/v1/realtime` 已复用的 primitive、ResponsesWS 预计可替换的调用点、对应测试覆盖。如果 ResponsesWS 无法复用至少 3 个阶段 1 primitive，停在阶段 1，不为对称性推进改造。

只复用纯 I/O helper，不迁移业务状态：

- `ResponsesWSIOBridge` 使用同一个 writer helper。
- close control、deadline、read limit、ping/pong activity、本地 error writer 和 active counter guard 复用同一套 helper。
- provider 侧 pong/control frame 继续复用 `WSControlFrameWriter`。
- read/recv/send loop 仍留在 `ResponsesWSIOBridge` / actor 周边，三态分类仍在 actor。

如果某个 helper 为了 ResponsesWS 被迫引入 quota/attempt/channel 语义，说明边界错了，应退回 actor 内部。

### 阶段 3：观察与收益门槛

阶段 2 合并后观察 8-12 周或至少一个稳定发布周期。满足以下任一条件才继续抽 pump：

- 两条链路出现同一个 read/recv/send loop bug，需要重复修复。
- 两边存在 40 行以上只涉及 context、deadline、recover、wait group、raw payload preservation 的机械重复，并且抽取不需要新增协议 flag。
- 新 helper 至少被两个生产路径调用，且测试能覆盖两边行为。

出现以下任一情况应停止或回滚对应 helper：

- ResponsesWS 实际复用少于 3 个阶段 1 primitive。
- helper 参数开始出现 quota、attempt、channel、terminal、retry、detach policy 等业务字段。
- 为了兼容两边行为需要多个 boolean flag 或 callback 分支。
- 抽取后测试更多依赖 mock callback，而不是真实 WebSocket 行为。

若阶段 3 结束后没有达到继续抽象的门槛，方案停在 safety primitives，文档归档为当前最佳边界。

### 阶段 4：整理文件和命名

当 safety primitives 被两边稳定复用后，再把 helper 从 `realtime_session_proxy.go` 拆到同包文件，例如：

- `common/requester/ws_writer.go`
- `common/requester/ws_close.go`

不新增 `ws_pump.go`，除非阶段 3 的门槛已经满足。仍不新建 `wsrelay` 子包；只有当 `common/requester` 内部已经拥挤、且有第三处真实调用方时，再讨论独立包。

### 阶段 5：回看 `RealtimeSessionProxy`

重构完成后做一次明确评估：

- 如果保留：文档说明它是 `/v1/realtime` 的 pass-through orchestrator，不是通用 transport core。
- 如果删除：调用方直接组合 writer/close/activity helper，并确保行为测试覆盖 detach/abort、UserClosed/SupplierClosed 等旧语义。

## 测试要求

底层 helper 至少覆盖：

| 测试 | 风险 |
| --- | --- |
| single writer 并发写 | 防止 data frame / close frame 交错 |
| write deadline 返回原始错误 | 防止底层提前做业务分类 |
| close reason UTF-8 安全截断 | 防止非法 close frame |
| ping/pong 刷新 activity | 防止活跃连接误 idle |
| control writer 队列有界 | 防止 pong/control frame 写入阻塞 read loop |
| local WS error writer 不触发业务副作用 | 防止底层 helper 误做 terminal/rollback |
| active counter guard 幂等 release | 防止异常关闭路径泄漏连接额度 |
| writer close 多次幂等 | 防止 panic 或资源泄漏 |

集成测试至少覆盖：

- `/v1/realtime` 行为在重构前后不变：正常转发、客户端断开、provider 断开、recoverable provider error。
- ResponsesWS 首帧、后续 turn、busy、not-sent rollback、ambiguous no-retry、provider terminal 早于 send result。
- `go test -race` 覆盖 client close 与 provider terminal 并发、handler return 与 actor close 并发。

## 当前架构不做什么

- 不新建 `common/wsrelay` 或 `common/requester/wsrelay` 包。
- 不新建 `SharedWsRelayCore`、`WsProtocolAdapter`、generic retry engine 或 generic quota engine。
- 不把 `LocalWriteOK / NotSent / Ambiguous` 放进底层 transport。
- 不在阶段 1 抽 callback-based read/recv/send pump。
- 不把 `/v1/realtime` 改造成 ResponsesWS actor。
- 不把 ResponsesWS 的 quota/affinity/terminal classifier 下沉进 I/O helper。
- 不为未来未知协议预设通用 turn DSL、通用账本事务框架或统一 retry engine。
- 不在第一阶段引入 connection pool / prewarm。
- 不把 provider 私有 websocket 差异藏进底层；provider adapter 仍负责 URL、header、frame shape 和 bootstrap/control frame。

## 结论

共享底层能力是必要的，但当前复用点只应该是 WebSocket I/O primitive，而不是现有 `RealtimeSessionProxy` 的 pass-through policy，也不是一个预先设计完整的新 transport core 包。

当前最佳路径是：

修正确性 bug → 抽稳定 safety primitives → ResponsesWS 复用这些 primitives → 观察 8-12 周并按门槛决定是否抽 pump → 再评估是否保留 proxy 类型或升格独立包。

这样既避免每个 WebSocket endpoint 重写最容易出错的 gorilla writer/control 细节，也避免为了假想第三个 endpoint 提前建立过重抽象。
