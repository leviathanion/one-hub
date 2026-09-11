---
title: "Responses 透明转发与计费边界设计"
layout: doc
outline: deep
lastUpdated: true
---

# Responses 透明转发与计费边界设计

## 文档状态

- 状态：**当前工作区实现**，更新日期为 2026-09-11。审计基线 `4b4c398f` 中的生命周期门禁由本次变更替代；本地验证范围见第 9 节，不代表已发布或完成真实上游验证。
- 范围：原生 HTTP Responses JSON/SSE、Native Responses WS、Stored Response 交付屏障，以及 OpenAI/Codex adapter 的观察边界。本文替代此前仅覆盖 steering 的方案，记录这些路径共同的职责契约。
- 目标：尽可能保留客户端与上游的原始协议，代理只拥有认证、准入、选路、资源授权、计费和有界 I/O 生命周期。
- 约束：沿用固定 WS 渠道、现有 create FIFO、明确的支持面及 [ADR-0030](../adr/0030-use-provider-usage-confirmed-tcc-billing.md)、[ADR-0033](../adr/0033-settle-atomic-price-components.md)、[ADR-0034](../adr/0034-read-configurable-policy-at-each-use.md)。不增加多 lane、background、conversation、客户端工具执行或自动恢复能力。
- [WS 主架构](./responses-ws-architecture.md)、[transport 边界](./responses-ws-transport-boundary.md)与 [ADR-0014](../adr/0014-preserve-native-responses-websocket-turn-semantics.md)已同步本次实现。

## 1. 问题与职责

基线实现把“本地能够接受这一生命周期事件”作为原帧交付和用量观察的共同前提。本次改动修正以下问题：

| 基线行为 | 根因与证据范围 | 实现边界 |
| --- | --- | --- |
| 完成后的 inject 不发上游，由代理校验工具调用并合成 `response.inject.failed` 和序号 | 代理拥有了注入结果与客户端恢复逻辑 | 代理验证资源授权后发送原帧，由上游决定结果 |
| 没有 active attempt 的迟到 inject 回执会丢失或关闭连接 | 计费对象生命周期决定回执能否交付 | 回执来源与安全检查独立于计费观察 |
| 未知 WS `response.*` 事件也可能被序号检查拒绝 | 把观察器当成上游协议校验器 | 投影失败只影响依赖该字段的本地决定 |
| HTTP SSE 在本地 terminal 后截断剩余事件；字段投影失败也阻止计费回调 | 本地生命周期控制原始流和所有 evidence | 按传输结束交付，按独立证据完成本地账务 |
| 通用 EventStream fixture 中，owner 失败会截断同 chunk 的后续事件 | visitor 提前返回会丢弃 chunk 后缀；这不是当前逐行 native 链路的跨事件交付形态 | 撤销交付许可后处理消费者已取得的内容，不扩建接收队列 |

六个初始 overlay 是透传契约探测。inject 合成与迟到回执门禁已确认；未知字段形态和 terminal 后扩展事件不代表真实上游一定发送。同 chunk 多个完整事件仅在通用 fixture 中复现，不能据此认定真实 native 链路已发生同样的用量丢失。

真实 OpenAI/Codex SSE 经 `http_requester.go` 的无缓冲 `DataChan` 逐行交付，handler 没有额外接收队列。reader 的 `bufio` 预读、socket 已到达的数据和清理阶段的接收，都不等于业务消费者已取得的证据。第 5 节以此事实限定收尾范围，删除先前方案假定的 HTTP 队列截止协议。

每项状态保留前必须回答：它服务哪个代理拥有的决定，缺少它会损害什么真实契约。工具是否完成、注入是否成功、输入如何恢复、对话是否继续，均由上游或客户端决定。

| 层级 | 唯一职责 |
| --- | --- |
| ingress / relay policy | 认证、权限、支持面、资源授权、渠道选择与工作前准入 |
| provider / adapter | 构造上游协议、必要的方言转换、提取独立证据与安全脱敏 |
| transport | 原始字节或消息交付、背压、取消、连接来源与真实发送结果 |
| relay / billing owner | 关联已准入执行、持有资源证明、消费证据、执行一次结算 |

## 2. 共用读取循环，分开交付与观察

沿用 HTTP 请求读取循环、WS actor 与 send worker。统一的是职责与证据契约，不是把 SSE 和 WS 合成一个通用会话引擎。

```text
原始 HTTP chunk / WS frame + 已验证的连接来源
    │
    ├─ 安全、资源授权与有界 I/O → 原始交付
    │
    └─ 最小事实投影 → 已准入 owner → 用量 / 终结 / 必要控制观察
                                             │
                                      一次 Confirm / Cancel
```

同一次处理中的顺序为：确认来源和必要安全边界；对已知停止信号先撤销新工作许可；独立关联并归并合法证据；执行资源 owner 交付屏障；交付安全原帧。正常已归属 terminal 的交付尝试先于其余额动作，写入或 owner 持久化失败不抹掉已接收的合法用量。

禁止使用 `AcceptRawEvent` 一类“完整生命周期验证成功后，才调用计费并允许交付”的总门禁。语义投影不能重新序列化原帧，也不能把观察结果传给 framer 作为“停止读取”的布尔值。必要的小型投影直接返回所需事实或局部诊断，不建立所有上游事件的封闭 union、通用 effects 列表或第二条事件总线。

### 2.1 不同失败的作用范围

| 事实或失败 | 交付 | 本地处理 |
| --- | --- | --- |
| 未知事件、字段、reason、status，或纯观察字段无法解析 | 保留原始协议 | 不猜测其终结、执行或计费语义 |
| 缺失、重复或非递增的上游序号 | 不作为通用原帧拒绝条件 | 只限制实际依赖它的去重或取消判断；不补造序号 |
| 某个用量组件缺证据或冲突 | 安全帧仍交付 | 按 ADR-0033 隔离该组件，其他独立可计价组件仍进入结算 |
| 已知 Response 无法关联到已准入执行，或资源身份存在冲突 | 只交付已能确认安全且不越过 owner 屏障的数据 | 停止新工作并收尾；不串账、不事后 Try、不隐式换渠道 |
| 身份失效、必要脱敏失败、本地 envelope 不合法、实际保留资源超限 | 遵循本地安全或资源失败路径 | 允许中止连接；错误不能伪装成上游 lifecycle 回执 |
| 旧计费对象已结束，但到达同连接的控制回执或诊断 | 原样交付 | 不重开账务、不修改新 Response、不刷新无关执行期限 |

这里不取消 WS 文本/object/type 等本地 envelope、渠道可表示性或资源权限检查。新 Stored Response 的归属屏障也继续存在；不能用“投影可选”绕过已知资源的授权要求。

### 2.2 生命周期只提供事实

- `response.created`：在协议关联明确且已有 Claim 时绑定 Response；为已识别的 Stored Response 建立 durable owner。
- `response.queued`、`response.in_progress`：转发并观察必要活动事实，不自行轮询或推进上游状态。
- `response.completed`、`response.failed`、`response.incomplete`：在已有 Response 关联内观察终结和独立用量，不重写 `status`，不把 failed/incomplete 自动视为免费或可重试。
- `response.output_item.*`、工具和图片事件：只在实际计价依赖时观察有界身份和数量；工具结果属于客户端，上游内部 agent 状态不建立本地镜像。
- `error`：先交付安全的上游错误。只有明确关联当前 create 的拒绝才结束该准入；明确的 connection fatal / workflow stop 先禁止新工作。不能仅凭事件名把任意辅助请求错误当成当前 Response terminal 并启动 FIFO。
- 重复或迟到 terminal 仍可交付，不重复结算。出现新的不同用量但 owner 已结算时，只记录诊断，不追加第二次余额动作。

原生 JSON/SSE/WS 的最小投影区分字段缺省与字段无法解析。`DecodeOptionalRawField` 返回投影是否成功；`ApplyUsageAttribution` 通过既有 `MergeProviderAttribution` 合并非空 model/tier，并把前后矛盾或无法解释的维度传入冲突标志和诊断，使依赖它的 token 组件不能借请求值、默认值或旧值收费。真正缺省沿用原契约，独立计价的工具组件和有效 Response ID 仍可观察；无法解释的 item 身份不降级成匿名调用收费。未知事件不自动建立新 owner 或执行权。

官方 inject 契约中，不符合 schema 的请求会得到 generic `error`、status 400，随后连接关闭。代理不复制该 schema 做前置校验；能依据明确协议识别的 fatal 先停止新工作，缺乏关联的泛化错误则等待连接事实或既有超时，不把它当作当前 create 的结束信号。HTTP 400 或事件名 `error` 本身不是通用 fatal 判据。

## 3. WS：删除 inject 的客户端生命周期镜像

### 3.1 客户端 inject

根据[官方 Multi-agent 契约](https://developers.openai.com/api/docs/guides/responses-multi-agent#websocket)，上游确认输入是否注入；客户端读取回执，并在需要时构造下一次 create。代理不校验工具 `call_id` 是否在终结输出中、不重建 input、不合成 `response.inject.failed` 或 `response_already_completed`。

发送前只检查代理拥有的边界：连接许可、已声明的 native 操作支持面、当前 principal/channel/generation 下目标 `response_id` 的资源授权、本次输入的受管资源引用以及现有队列容量。是否启用正确的上游 multi-agent 模式、输入是否符合工具 schema，由真实服务端校验。

目标父已 terminal 不构成代理合成拒绝的理由。已证明同连接归属的目标可原样发往上游；无法取得所需资源证明时返回本地资源授权错误，不冒充上游已完成或未找到的业务回执。使用现有连接内证明或 Stored SQL owner 查询，不为覆盖任意久远目标扩建完整响应历史。

对尚在执行的已准入 Response，inject 是其输入追加，费用仍归该 Response；不按帧 Try/Claim 或建账。已完成目标的合法 inject 按当前上游契约不会创建后继；它不能恢复旧 Billing Attempt，也不能把费用转给另一个活动 Response。`CONTEXT.md` 的 Application Submission 定义明确了该多帧边界，不赋予任意命令共享执行权。

### 3.2 回执、FIFO 与发送结果

`response.inject.created/failed` 与 `response.steer.accepted/pending/failed` 使用同一原始交付规则：来源经过验证并完成必要脱敏后即可交付，不依赖 active attempt、pending 数量或最近 16 项历史。只有确实涉及未结束的本地决定时才观察关联字段。

删除 inject 的 `pendingTargets`、初始回执计数与 terminal-ack barrier。inject 不创建独立后继，回执计数不决定父费用，也不需要替客户端等待下一次输入。父 terminal 可结束父计费观察并推进原 create FIFO；已经发布到同一 send worker 的命令保持顺序。仍可能触发独立后继的 steering 预扣继续阻挡 create，规则见第 6 节。

普通 create 之间保持既有 FIFO，活动响应的合法输入追加沿用既有辅助发送入口；不从 FIFO 中挑选“更合适”的续接。调用者如需收齐 inject 回执再构造下一轮，应按上游协议自行等待。

每条辅助发送复用既有的一次完成关联：正常结果只收尾原命令，首次真实 ambiguous 或 transport contract violation 仍停止连接，已消费的重复结果无效。已发布发送的 context 和结果消费必须脱离父账务的 reset；父 terminal 不能取消尚在途的合法命令，或使其真实错误被当作 stale 而丢弃。资源仍受连接关闭控制，不建立持久发送日志。

发送结果与接收归属必须在 transport 首次写状态的地方分开：

| 关联 | 用途与更新条件 |
| --- | --- |
| 单个 command 的 ID / completion | 只标识该次发送及其一次完成，不能成为接收流的当前执行 ID |
| 当前 create 的接收关联 | 沿用现有串行 create 关联；在 create 进入发送时建立，以承接可能先于 SendResult 到达的事件，辅助发送不得覆盖它 |
| 上游 Response ID | 由已准入 owner 绑定，优先用于可明确关联的业务证据；无 ID 的普通流事件使用当前 create 关联 |
| channel / generation | 证明物理连接来源；控制回执与连接错误不因当前业务 attempt 不匹配而改绑或失效 |

`NativeSession.sendClient` 只在 `response.create` 发送时更新接收关联，inject/steer 使用各自的完成关联。真实 native WebSocket 测试覆盖 `A terminal → create(B) → inject/steer(A) → B 的无 response_id delta`，确认输出仍归 B。

### 3.3 已删除的状态

删除 `responsesWSCompletedResponseRecovery`、`parseResponsesWSCompletedInject`、`responsesWSCompletedResponseAcceptsInject`、`responsesWSCompletedInjectFailedPayload` 和 `handleCompletedResponseInject`。`lastFinal` 已无生产消费者，因此连同字段和相关代码一起删除，不替换成新的 `lastFinalID`。资源证明及其他仍有实际消费者的 ID 集合继续保留。

移除 `responsesWSInjectState` 的回执驱动生命周期；有需要的 deferred 原始帧、字节预算与取消清理由现有发送路径承接。不能只加一个“迟到回执例外”，同时留下模拟恢复和 ack barrier。

## 4. HTTP SSE：传输结束与账务结束分别处理

### 4.1 原始交付

保持 SSE 原始事件名、data、多行规则、注释、空行、未知字段和次序。TCP/HTTP chunk 的物理切分不是对外契约；framer 只提供有界观察和必要的 owner 暂存，不决定上游是否完成。

SSE reader、事件分帧、`SSEDataPayload` 和安全重建必须使用一致的 [WHATWG SSE 行语义](https://html.spec.whatwg.org/multipage/server-sent-events.html#parsing-an-event-stream)：识别 CR、LF、CRLF，正确处理跨 chunk 的 CRLF 与多行 data。行遍历规则在 SSE 共享边界实现一次，原始交付只因必要脱敏改写相应 payload。仅修正 reader/framer 会留下 CR-only 脱敏遗漏；完整事件交付后由 Responses 入口显式 Flush 并检查错误，使小事件及时可见，不依赖通用 writer 的 LF 分隔符猜测，也不扩建其协议识别规则。

已识别 terminal 记录该 Response 的执行终结，不等于停止读取或已经完成余额动作。HTTP 消费者继续交付后续字节，直到真实 EOF、取消、传输错误或已有资源/时间边界。`[DONE]` 同样按原始协议转发，不作为本地截断剩余 chunk 的命令。不得因控制回执或未知事件出现在 terminal 后而新建计费 owner。

EOF 前未识别到 terminal，只表示本地没有该终结事实；按已收齐的可归属 Price Components 结算，不能补造 `response.completed/failed`、推导新的 provider sequence，或仅为满足本地 terminal 白名单而追加 SSE error。

传输结束必须保留以下差别：

| 结束事实 | 下游处理 |
| --- | --- |
| 没有本地提前停止的真实正常 EOF | 原样完成 HTTP 响应，不额外要求已知业务 terminal |
| 非 EOF 读取错误、超限或代理在响应未结束时提前停止 | 已提交响应必须异常 abort/reset，不能用正常结束掩盖截断；尚未提交时返回明确本地错误 |
| 客户端已经断开 | 停止发送与上游读取，完成本地证据及资源收尾 |

取消读取后产生的 EOF 不自动变成“上游正常完成”。原始错误保留为传输诊断，不重写已提交状态码或追加伪造业务帧。复用 `relay/common.go` 的 `http.ErrAbortHandler` 通路和现有 recovery 中间件，使 HTTP/1.1 不发送正常 chunk terminator、HTTP/2 不发送正常 END_STREAM；先收尾已取得证据并保证一次结算，再结束 handler。此行为与 [Go ReverseProxy](https://go.dev/src/net/http/httputil/reverseproxy.go) 的响应复制失败处理一致，不新增传输错误框架。

EOF 留下的未完成 SSE 事件不能成为收费证据，即使 data 已经是完整 JSON。尾部仍经过必要脱敏和 owner 屏障，取得交付许可后保留原始分隔字节，payload 只作必要安全改写，不补空行、补事件或推断 terminal；JSON 语法完整性是现有结构化脱敏器的前提；无法解析的 data（包括 EOF 截断 JSON）在交付前返回本地失败，已提交响应走异常 abort/reset。注释、空 data 和 `[DONE]` 保留原协议，不检查上游字段 schema。现有单行、事件、缓冲和传输超时限制保留。

同次读取返回有效字节和非 EOF 错误时，先交接仍在容量和交付许可内的字节，再处理原始错误；不能只报告错误而丢掉已读尾部。这不扩大第 5 节的证据截止范围，也不使未完成 SSE 事件成为计费证据。

### 4.2 独立用量与结算时机

`StreamObserver` 只记录它实际识别的事实；provider 的 token、工具和图片证据分别提取。总生命周期投影失败不能阻止无依赖关系的组件观察；有依赖关系的 model/tier/cache 分区仍按 ADR-0033 作为完整原子组件验证。

HTTP 请求在 EOF 或本地停止收尾后执行一次 Confirm/Cancel；在此之前仍可接收同一 owner 的合法独立证据，并按现有规则去重，不能重复累加 terminal 快照。多个独立执行、background 和流式恢复不因这一调整而获得支持。未知事件不形成事后执行 owner。

这一时机直接复用 `RelayHandler` 在 `relay.send()` 返回后的结算和异常退出 guard，不新建“等待 EOF”的账务状态机。它选择了现有同步结构的简单性，并非协议证明必须等到 EOF 才可计价；代价是预扣释放与最终价格读取也可能延后，按 ADR-0034 处理，并在真实上游验证 terminal 到 EOF 的时长。

### 4.3 下游写入也必须可停止

`HTTPProfileLongStream` 的现有时限只控制上游；关闭上游 Body 不能解除下游同步 `Write/Flush` 的阻塞。Responses HTTP I/O 入口统一拥有上下游停止与写 deadline：下游每次可能阻塞的写入、刷新及 `Close` 中的刷新复用 2 分钟 body idle 取值，并受同次请求开始时的 1 小时 max lifetime 限制。进展不能延长绝对寿命；明确的上游异常/超时、客户端取消或本地停止须同时关闭上游并解除正在阻塞的下游写入。

使用真实 ResponseWriter 的原生 deadline 能力，例如 `http.ResponseController`；Gin 及中间件包装必须能透出该能力和刷新错误，并在 provider work 前确认可用。不能静默忽略不支持的 deadline，也不能让写 goroutine 留在后台后直接返回。正常 EOF 后的 Body 清理可能取消内部 context，不能将它误判为异常停止而丢弃已获许可的尾部；停止回调与 deadline 设置须协调，停止后不再延后期限，回调在请求收尾时解除。

写入解除阻塞后才进入第 5 节的有界证据收尾和一次结算。异常停止后不再通过 defer `Close` 重试刷新待交付内容；正常结束的最终刷新仍受同一写入边界约束，刷新失败按异常传输处理。这里控制 I/O，不根据业务 terminal 决定停止，不新增配置层、发送队列或每帧等待机制。

## 5. owner 失败与有界证据收尾

### 5.1 屏障只拥有交付许可

新 Stored Response 的已识别 ID 对客可见前必须成功写入 durable owner。合法证据先归属并进入已准入 attempt，再做该交付检查。owner 写入失败后停止新工作与相关交付；该失败只记录一次，收尾过程中不重试 owner SQL，不放行被拒绝的资源。

普通 Stored GET/DELETE 的 SQL 权限与 owner tombstone 继续表示代理自己的访问控制，不复制上游 queued/in_progress/completed 状态。原生非流式 JSON 和必要协议转换中的独立用量也遵循同一规则。

### 5.2 按真实交付边界收尾

| 传输 | 纳入本次最后结算的接收范围 | 截止后处理 |
| --- | --- | --- |
| WS | 触发关闭的当前事件，以及现有 `postMu` 发布关闭时已入队的 closure cut | 停止新工作；原命令资源继续清理，后到用量不产生第二次余额动作 |
| HTTP SSE | 停止前业务消费者最后一次成功接收的整个当前 chunk，以及 framer 已有前缀；此前观察过的证据已经在 owner 中 | 按第 4.3 节停止上下游 I/O；其后的清理接收均不交付、不观察、不入账 |

HTTP 沿用真实 native reader 的无缓冲交接。业务消费者只要停止下一次正常接收，证据范围便确定；不添加队列截止锁、接收队列或新的 journal。`bufio`、kernel/socket 中的预读，以及已阻塞但未交接的下一行，都不纳入本次证据承诺。若未来确有缓冲传输需求，须另行说明交接契约，不能以假想兼容性扩建当前实现。

当前 chunk 的剩余完整事件用现有 framer 顺序观察。发生 owner/write 失败时，visitor 只记录第一个失败并撤销后续交付，返回继续扫描，而不是让 `PushChunk` 提前 Reset。每个事件仍只观察一次，不重试 SQL，不复制 payload，不为此新增游标 API。安全或资源错误使继续解析不再可靠时仍停止；未完成事件不拼接之后清理收到的数据。真实逐行 reader 的当前 chunk 通常没有多个事件；保留这一处理只需局部变量，也覆盖通用 EventStream 的完整 chunk 合同。

`Close` 发布 done 后，若清理接收与 done 同时 ready，`SendData` 的 select 仍可能选择发送；所以不能把 drain 到的数据倒推为停止前的证据。清理不得再次调用观察器或拼接 framer。实际 transport 的关闭交错实验已验证这种交接可能性。

事实收尾由当前 chunk、已有前缀及既有行/事件上限限定；必要的生命周期操作复用 `boundedResponsesLifecycleContext(context.WithoutCancel(...))` 的现有 5 秒边界，并在扫描间隙检查停止期限，避免客户端取消跳过已取得证据。该期限只约束收尾操作，不能替代下游写 deadline。Responses 收尾入口使用 `CloseAndDrainStreamContext`，将同一期限传给实际清理循环；native emitter 由 done 解除阻塞，legacy producer 的 drain 等待也受该期限约束。超时返回错误，不伪装为全部清理成功。期限耗尽记录诊断，按已取得的合法组件执行一次结算，不增加每帧 grace。

### 5.3 关闭和错误后的唯一责任

WS 沿用 `postMu` 的短锁新工作许可，每个 Try、Claim、open、worker Send 阶段重新检查，外部客户端关闭立即发布。许可不跨 SQL/I/O 持有；已获许可的操作返回后只在仍允许时进入下一阶段。

已知 fatal/workflow stop 先发布停止事实，再归并触发帧的合法用量并交付安全上游错误，最后处理截止范围。未知控制或诊断值不触发这一分支。原命令结果与错误属于原操作，不能因候选已释放而改绑新执行。

删除 `capturePendingSendResultOnClose`、100ms 等待常量、pending 的 `sendCompletion` 及 command 中仅供该支路使用的 completion channel 和重复投递。该捕获只在没有 provider evidence 时补写 `TransportResult`，而 ADR-0030 下这些发送状态均不产生 Charge；无需延迟关闭以取得这一结果。正常运行的 SendResult、辅助命令一次完成 Receipt 和仍有消费者的 `TransportResult` 保留；关闭后仅用于等待结果的诊断机会随支路删除。

closure cut 继续在 `postMu` 下关闭 ingress、取得已有事件数量并消费固定数量；删除没有生产消费者的 `postedSequence`、`closureCutSequence` 及递增/赋值。open 结果的 `Adopted` 交接必须在 actor 使用、清理资源或发布 done 前登记唯一清理责任；worker 看到 done 不重新取得清理权。Abort、发送字节和 lease 的唯一清理责任独立保留，取消或余额提交失败沿用一次结算 guard。

## 6. Steering：保留最小的未绑定预扣观察

Steering 可以自动产生新的独立 Response，因此它保留计费必需的关联。这里沿用已实现的方案，并把原始回执交付提升到第 2 节共享规则。

### 6.1 工作前准入与容量

首条 steer 在活动父的支持窗口内，使用已授权父请求的必要事实和本次原始输入的资源引用，完成权限、渠道、精确 model、资源、RPM、Try 和一次 Claim。同父尚未绑定的共同后继只预扣一次；后续 steer 仍经过连接许可、支持面、资源与容量检查，不按帧建账。准入不构造伪 create，不合并输入或解释 `required_input`。

| 保留状态 | 容量、作用域与删除条件 |
| --- | --- |
| 一个尚未绑定的自动后继 attempt | 当前固定连接串行支持面最多一笔；绑定 child 或成功 Cancel 后释放候选观察 |
| 初始回执义务与已接管 ID | 最多 64 条待观察提交，ID 每个最多 1024 字节；只服务当前候选的取消判断 |
| 临时父资源证明 | 最多 64 个，父 ID 总量最多 4 MiB；只对原 principal/channel/generation 有效 |
| 原始帧与发送关联 | 复用现有事件、发送和 create FIFO 的帧数/字节限制；发送完成后不累计历史 payload 流量 |

父计费对象在其 terminal 后独立结算并释放。一个现有 watchdog 随当前 Response、未绑定候选、实际 child 切换目标与 generation；候选取消且没有执行时停用。仅持有父证明时按 idle 管理，未知或旧父回执不刷新无关执行期限，继续尊重 timeout=0。

### 6.2 只有这些事实可以消解候选

| 事实 | 对唯一未绑定候选的处理 |
| --- | --- |
| 发布 steer command | 先登记一个初始回执义务，再进入发送队列 |
| 原 command 首次 `NotAttempted` | 只减少原候选的聚合初始义务；不匹配第 N 条 accepted，不影响新候选 |
| 新的、可关联 accepted | 消解一个初始义务，记录上游 steer ID |
| 已知 accepted ID 的 failed | 只移除对应接管可能性，不再次减少初始义务 |
| 无 ID 的首次 failed | 同连接/渠道/generation/父匹配、尚有初始义务且事实未被消费时，减少一次义务 |
| 未知非空 ID 的 failed、未知 pending reason、缺失或无效序号 | 原样交付，不作为提前退款依据 |
| 全部义务和接管输入均明确失败或未发送 | 成功 Cancel 后才允许同一仍活动父重新准入 |
| 已知 `waiting_for_required_input` | 父已完成、关联明确且初始义务全部消解、没有 child 绑定时 Cancel；留下所需父证明 |
| 自动 child.created | 父已 terminal、没有竞争的显式 create、有唯一已 Claim 且未取消候选时绑定；父字段若存在必须匹配，缺省须由串行契约消歧 |

自动 child 的绑定不要求完整 accepted 列表，也不事后 Try。候选绑定或成功 Cancel 后立即清除计数和 ID，不继续追踪每条输入的 committed/failed，不等待已被上游事实证明发生的发送结果。

重复事实的消费水位按父的观察作用域保留，不随同父候选 Cancel/重新准入清零；它不参与普通原帧的序号门禁。只有实际用于本地决定的关联事实才推进水位。每个发送 command 的完成只消费一次；若原候选仍未绑定、初始义务已为零，却又首次报告 `NotAttempted`，按证据矛盾停止新工作，不回退其他 ID。

保留 pending 的初始义务屏障：否则已排队但未发送的追加可能在预扣 Cancel 后才触发工作。这个屏障只服务 steering 的潜在新执行，不恢复 inject 的回执屏障。

### 6.3 资源证明独立结束

`store:false` 父证明在首条 steer 工作前，从已经观察到的同连接父资源取得并预留容量；不能从客户端任意父 ID 创建。全部输入明确失败，或自动 child 绑定且初始回执已完整时，可以释放。

因等待客户端输入而取消预扣，或绑定 child 时初始回执尚不完整，保守保留父证明至匹配显式 create 的 child.created，或连接关闭。无关 create、准入失败和上游拒绝均不消费证明。迟到 failed 只转发，不为提前回收而恢复整组回执状态。归属查询直接使用这一事实，不依赖最近 16 项历史或临时缓存 TTL；Stored 资源仍走 SQL 授权。

仍可能自动产生后继时，显式 create 留在现有 FIFO；解除候选后从队首推进，不跳取匹配续接。仅有旧父证明不阻塞队列。

## 7. 方言转换和支持面的边界

原生 OpenAI JSON/SSE/WS 的未知字段和原始状态保持 wire 语义。Codex 的已证明私有 terminal 别名，以及 Chat→Responses 的明确协议转换，仍由 adapter 负责；跨协议确实无法表示时按既有可表示性边界失败。

为满足 relay 严格 classifier 而给已经采用公共事件名的帧补 `sequence_number`，应随门禁删除而取消。私有方言确实缺少目标协议字段时，只能在有明确供应商契约、确定关联和映射规则时转换；必要的合成字段由 adapter 唯一拥有，不通过猜测“已完成”修复不完整上游执行。

Adapter 必须在首次修改状态前隔离控制/诊断观察。旧父的 steer 或 inject 回执不能推进当前 child 的序号、覆盖 Response ID 或改变 usage accumulator。未知事件无需进入已知状态机才可交付，不在通用 relay 中增加 provider 类型分支。

当前不支持的 background、conversation、stream_id lanes、流式 Stored retrieval 等仍在工作前按能力拒绝。扩展这些操作必须先明确工作准入、资源授权与用量关联，不以运行时试错获得支持。SQL owner、计费组件去重、传输取消和资源预算不属于要删除的上游生命周期镜像。

## 8. 实施、删除与外部影响

### 8.1 实现范围

变更按共享边界落地：

1. `common/responses/lifecycle.go`、`stream_observer.go` 与 `common/responsesws/terminal_classifier.go`：拆开最小事实投影、局部诊断和交付许可，删除通用 sequence/status 门禁及无生产消费者的 `ResponsePresent` / `ResponseFieldError`，容错解码只投影一次；账务关联校验保持独立。
2. `types/responses.go`、`common/responses/stream_usage.go`、`providers/openai/responses*`、`providers/codex/responses*` 及计费组件入口：把协议转换和证据提取限定在 adapter，将 model/tier 投影失败送达依赖组件，消除观察失败阻断无关原帧/独立用量的路径。不能仅拆除 gate 后继续复用丢失失败事实的投影。
3. `common/responsesws/native.go`、`upstream.go` 与 `relay/responses_ws*`：先分开单命令完成和 create 接收关联，再解除旧目标 inject 门禁；这两部分必须作为同一完整变更验收。控制回执进入共享交付入口，删除 inject 恢复、ack barrier 和无消费者的 `lastFinal`，按第 5.3 节删除关闭等待支路及闲置计数器；保留已有准入、FIFO 和 steering 候选。同帧分类只在一次 handler 调用内复用，不跨事件或 journal replay 保存。
4. `relay/responses_stream_owner.go`、`common/responses/accepted_stream.go`、`relay/common.go` 的 SSE 脱敏及必要的 requester 接线：保持无缓冲移交，统一 SSE 行遍历、分帧、data 提取与安全重建；交付入口显式刷新完整事件，处理有字节伴随读取错误的尾部。失败时用局部交付状态扫完已取得 chunk，不增加 HTTP 队列截止锁、游标 API 或证据 journal。
5. Responses HTTP I/O 入口及实际 writer 包装：按第 4.3 节接通写 deadline、异常停止与错误可见性，保证阻塞写可解除，复用现有 abort 与结算通路。不能只在调用 Write 前检查 context。
6. 更新旧测试中对“模拟 inject 失败、terminal 后弃读、上游序号门禁、关闭等待结果及闲置计数器”的断言，并同步当前架构、使用文档和词汇。新路径成为默认路径时，在同一完整变更中删除被替代实现，不留 feature flag 或双实现。

[ADR-0014](../adr/0014-preserve-native-responses-websocket-turn-semantics.md) 已更新：删除 pending-inject 计数和 public terminal 校验对原帧交付的控制；native-only、精确 model、FIFO 和明确方言映射继续有效。本次直接替换旧路径，没有迁移开关或双实现。

### 8.2 可观察变化与回滚

- 客户端从上游获得 inject 成败和原始回执顺序。依赖代理模拟恢复或等待全部 inject 回执后才启动下一轮的客户端，应改为自行处理官方回执；正常按官方流程等待的客户端无需新字段。
- 新 create 可在父 terminal 后、旧 inject 回执尚未全部到达时进入上游，保留已发布发送命令的顺序；未消解 steering 后继继续阻挡该行为。
- 原生 SSE 不因本地 terminal 提前关闭；连接占用、预扣释放和最终价格读取可能延后至上游 EOF 或本地收尾。上下游 I/O 均受第 4.3 节时限控制，停止读取的客户端会触发写超时并进入收尾；不能把原有上游时限当作下游已有保证。部署前验证正常 EOF、最终刷新和异常 abort/reset 的可观察结果。
- owner 或客户端交付失败时，已接收的合法用量仍可能产生 Charge；无法计价的组件按现行规则免费，不改变价格算法。
- 无数据库或公开请求格式迁移。发布与回滚只影响新接收逻辑；不能重放旧输入、撤销已发生费用或追加第二次余额动作。出现原帧丢失、跨用户资源暴露、额外执行、重复余额动作或不受既有边界控制的资源占用时停止发布。

## 9. 验收与未验证边界

复用现有 fake upstream、actor、真实 native reader 与余额/资源操作计数器，通过 channel/barrier 控制交错。通用 EventStream fixture 的任意 chunk 测试单列，不能替代真实无缓冲移交测试。HTTP 结束语义使用真实 HTTP/1.1、HTTP/2 客户端验证，不能只检查 ResponseRecorder 的字符串。不把保留旧字段值或旧 helper 调用次数作为成功标准。

| 验收组 | 必须验证的外部结果 |
| --- | --- |
| 原始交付 | 同连接迟到 inject/steer 回执、无 active attempt 的安全诊断、未知字段与大整数原样交付；必要脱敏仍生效 |
| 投影局部失败 | 未知序号/status/reason 不触发通用原帧拒绝；JSON/SSE/WS 的 model/tier 为无法解析的形态时，原帧仍交付，依赖组件不可借请求值、默认值或旧值收费；独立组件及真正缺省行为分别验证 |
| inject | 活动及已完成的已授权目标均发送原始帧；没有本地工具 schema 验证、模拟失败或序号；未知/跨用户目标在工作前按资源授权失败 |
| 调度 | create FIFO 不跳队；旧 inject 回执不占用已结算父或阻挡新回合；发送歧义仍作用于原命令和连接 |
| WS 接收关联 | A terminal、B create、inject(A) 后，B 的无 ID delta 仍归 B；旧 inject 的完成/错误只消费原命令关联，接收错误保持正确连接作用域 |
| HTTP SSE | terminal、`[DONE]` 后仍交付剩余及后续字节；CR-only、混合行结束、跨 chunk CRLF、多行 data 的敏感字段被正确脱敏，小完整事件在 EOF 前及时可见；完整 JSON 但未完成 SSE 事件的尾部脱敏后可交付且不计费 |
| HTTP 结束语义 | 正常 EOF 不因 Body 清理取消而误 abort；有字节伴随读取错误时先处理许可内字节；非 EOF 错误、超限、本地提前停止及最终刷新失败在已提交后 abort/reset；缺业务 terminal 不合成业务事件；不重复结算 |
| owner/write 失败 | 当前已取得 chunk 内的合法证据仅观察/结算一次，不暴露资源、不重试 SQL；真实 reader 关闭后即使清理收到新行也不补账，通用多事件 chunk fixture 独立验证 |
| 关闭与背压 | 在准入、Try 返回、Claim、Send、消费者接收、open Adopted 处停止；无新许可、无重复释放。真实 H1/H2 客户端连接保持但不读取时，超时和显式异常停止均能解除 Write/Flush，handler 有界退出并一次结算；owner SQL 阻塞时缓冲有界，native emitter 退出，legacy 清理不能无限等待 |
| WS 关闭消融 | 在途 create 的 NotAttempted、Attempted、Ambiguous 及未知结果下，无需等待关闭专用 completion；无用量仍 Cancel，截止内用量仍结算，Abort、lease、发送字节及 open Adopted 均只清理一次；运行中非法结果仍停止连接 |
| 错误作用域 | 由明确上游契约识别的 schema-invalid inject 400/Close 不推进新 create；不能把其他无关联 generic error 或任意 400 当作当前 Response terminal/fatal |
| Steering 回归 | 无 ID 初始失败、accepted→failed、同父重新准入时去重、pending 初始义务屏障、缺少完整 accepted 仍绑定、Cancel 失败关闭均保留 |
| 资源与时限 | 父证明超过 16 次无关响应且缓存失效仍有效；64 条观察、64 个父引用/4 MiB、现有队列预算有效；历史 payload 不累计；watchdog 目标切换安全 |
| 方言与账务 | Codex 旧控制回执不污染 child；必要转换有真实证据；Stored 权限、一次结算、无本地估算和歧义执行不重放均保留 |

本次实现的本地回归覆盖：

- 真实 NativeSession 辅助发送后的接收 ID；actor 的已完成 inject、迟到控制帧、FIFO、发送歧义、closure cut、steering 预扣及一次资源释放。
- OpenAI/Codex 的独立字段投影、工具去重和组件冲突，无法解释或前后冲突的 model/tier 不进入 token 计价，独立服务组件仍可结算；相同累计终态和迟到工具事件不重复计费。
- 真实 HTTP/1.1、HTTP/2 的正常 EOF、异常读取、客户端停止读取时的 source error / 取消 / deadline；验证客户端能区分正常结束与截断，handler 有界退出，SQL 余额和日志只结算一次。
- SSE 的 CR、LF、CRLF 分帧和安全重建、大整数保留、完整事件及时刷新，以及完整 JSON 但未完成事件的尾部不计费。截断 JSON 的账户字段不会交付，真实 H1/H2 客户端观察到异常结束，已取得工具证据只结算一次。owner/write 失败后的当前 chunk 和 legacy 有界清理另有回归。

早期消融实验确认 steering 的初始回执义务、水位和父资源证明仍有实际消费者，删除它们会造成提前退款或拒绝合法续接；这三项保留。关闭专用 100ms 等待、`postedSequence` / `closureCutSequence`、inject ack barrier 和完整 `lastFinal` 已从生产代码及旧断言中删除。

本地契约回归不能证明上游会提供所有测试形态。后续真实验证应覆盖 OpenAI/Codex 的 inject 完成竞态、HTTP terminal 到 EOF 行为、自动 steering 的共同后继与初始失败关联。

已核对的协议依据为 [WebSocket Mode](https://developers.openai.com/api/docs/guides/websocket-mode)、[Multi-agent](https://developers.openai.com/api/docs/guides/responses-multi-agent)、[Steering](https://developers.openai.com/api/docs/guides/steering)和[事件参考](https://developers.openai.com/api/reference/cli/resources/beta/subresources/responses)。参考页中其他能力不自动进入本项目支持面。

Steering 仍有不能由本地状态消除的假设：更大的序号或新的接收位置不独立证明新的拒绝发生；上游若用全新身份重复同一次无 ID failed，现有字段可能无法与新提交拒绝区分。完整 registry 同样无法补足该信息。可识别重复不重复消费，正常可关联首次失败仍处理；不能把这一不确定性变成全面等待 timeout、隐式重放或重建客户端编排。
