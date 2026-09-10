---
title: "Responses WS 透明转发与计费设计"
layout: doc
outline: deep
lastUpdated: true
---

# Responses WS 透明转发与计费设计

## 文档状态

- 状态：已按本方案实施；本地契约回归已覆盖主要边界，真实上游交错与下述协议假设仍待验证。
- 目标：尽可能透明地转发 Native Responses WS，在必要的授权、资源归属和计费边界读取最少事实。
- 当前约束：固定上游渠道、串行 Response 支持面、已有 create FIFO、Stored Responses 公共入口。不扩展为客户端会话编排。
- 计费沿用 [ADR-0030](../adr/0030-use-provider-usage-confirmed-tcc-billing.md) 的 TCC、[ADR-0033](../adr/0033-settle-atomic-price-components.md) 的用量组件和 [ADR-0034](../adr/0034-read-configurable-policy-at-each-use.md) 的按阶段读取策略。本方案没有把“简单计费”解释成取消预扣或事后补建执行权。
- 与[主架构](./responses-ws-architecture.md)、[transport 边界](./responses-ws-transport-boundary.md)一起阅读。主架构记录共享边界，本文仅详细定义 steering 的专项契约。

## 1. 职责决定状态

| 本项目职责 | 必须知道的事实 | 可以据此作出的决定 |
| --- | --- | --- |
| 透明交付 | 原始帧、上下游连接、交付结果 | 按序转发、必要脱敏、处理断线与背压 |
| 访问授权 | 本地用户/token、渠道凭据、引用资源的归属 | 是否允许向该上游提交、是否允许访问该资源 |
| 计费 | 已准入执行、provider Response/用量归属、价格、预扣及最终动作 | 绑定用量、Confirm 或 Cancel，避免重复余额动作 |
| 有界资源生命周期 | 在途 I/O、排队字节、仍被使用的归属证明、关闭事实 | 拒绝新增占用、停止新工作、释放连接/任务/预扣 |

代理不拥有对话历史、工具执行和“是否继续思考”的决策，不合并 steering 输入、不解释或补齐 `required_input`，不替客户端构造 create、恢复历史或重放请求。`x-codex-turn-state` 按现有安全 header 规则传递；代理不猜测客户端逻辑 turn 并自行重建其生命周期。

本地 Codex `94697375cb9d` 的 `turn/steer` 是客户端 pending input，随后由模型调用循环消费；其 Responses WS 请求枚举只有 create。因此只参考认证 owner 隔离、短锁关闭准入、取消/释放和一次结果交接技术，不搬入其逻辑 turn、历史增量基线、整段流独占 socket、有限事件替代原帧或自动 fallback。

## 2. 转发路径与本地观察分开

公共 wire 的事实源始终是原始帧。Provider adapter 只做明确的同协议处理或必要转换，并附带代理需要的证据；relay 负责授权、资源归属和计费，transport 负责 I/O。保持现有单 actor 和发送 worker，不新增另一组 actor 或通用 effects 引擎。

```text
原始 provider 帧
    → 验证连接来源和必要安全边界
    → 提取本地决定需要的少量事实
    → 按原始协议交付客户端

本地观察
    → 已知停止信号先禁止新工作，验证账务关联
    → 一次归并合法 evidence，更新预扣或 Response 用量归属
    → 必要的资源 owner 交付屏障
    → 依现有规则执行一次结算
```

这两个职责分支共用现有处理循环，不建立第二条异步总线。验证来源、Response 与已准入 attempt 的账务关联后，合法 provider evidence 必须先且只归并一次；正常 terminal 交付后再执行余额动作。新 Stored Response 的 durable owner 屏障只限制对客可见性，不限制这些已归属证据进入结算。Owner SQL 写入失败时禁止交付并停止新工作，closure cut 内该执行的后续合法用量仍须归并；关闭排空不能再次让已失败的持久化屏障过滤 evidence 或反复重试写入。这不放宽已有 Stored 资源访问的 SQL 授权校验。客户端写入失败同样不抹掉证据。

### 控制回执不是转发准入条件

`response.steer.accepted/pending/failed` 来自已验证的本连接时，经过必要脱敏后原样交付。下列情形本身不构成拒绝该帧或关闭连接的理由：

- 没有当前自动续接预扣，或者它已经绑定/取消；
- 父 ID 不在最近 16 项完成历史中；
- 代理未保留该 `steer.id`，或回执包含未知 reason/字段。

原样交付不要求完整重建服务端 steering 状态机。只有该回执会影响一笔尚未绑定的预扣时，才读取它的关联字段。缺失或矛盾的关联不授权提前退款、错误绑账或再次执行；若无法安全推进仍可能发生的工作，先保留可安全交付的上游帧，再按现有有界失败路径关闭。纯诊断未知值不能升级为这一失败条件。

不为控制回执新增独立的严格序号验收器。序号或事件 ID 只在本地确实需要防止重复使用证据时参与观察；不改变原始字段。既有已知 Response terminal 的支持面检查仍保留，本方案不扩展其范围到所有未来事件。

Codex adapter 在 relay 之前更新自身序号和 usage accumulator，因而作用域保护必须发生在首次状态修改处：旧父控制回执不能推进当前 child 的 `lastSequence`、覆盖 `lastResponse` 或改变 child 用量。Adapter 只保护自己的协议观察状态，不决定账务 owner。

## 3. 最小保留状态及删除条件

| 状态 | 所有者与容量 | 生命周期及必要性 |
| --- | --- | --- |
| 连接、在途发送 | 现有 actor、send command/result、bounded queue；复用 generation、channel 和本地发送关联 | 决定事件来源、能否发送及清理。结果消费后释放，不保留永久发送日志；不能用最近发送 ID 代替 Response 归属 |
| Response 计费 attempt | 现有 pending/active attempt 和一次结算 guard | 实际 Response 的授权、用量、价格和余额动作；不引入客户端逻辑 turn，也不按帧建账 |
| 尚未绑定的自动续接预扣 | 当前串行支持面最多一笔；直接关联父 Response 与既有 attempt | 从首条 steer 的事前准入开始，到绑定 child 或成功 Cancel 为止。只有此阶段保留计费必需的回执计数/ID |
| 被续接使用的资源证明 | 现有连接内归属边界持有父 ID 引用，受连接总量限制 | 只证明此父 Response 确实属于本连接用户，不表示输入已经接管或提交；不保存工具结果、输入内容和完整父计费对象 |

待绑定预扣只需未结的初始回执义务数量、能够关联的已接管 ID，以及是否已有明确的取消/绑定依据。ID 集合服务“全部失败，能否取消预扣”的判断，不是客户端回执路由表。初始义务按同父候选聚合，不建立第 N 条发送到第 N 个回执的映射：

- 发布发送 command 前登记义务；该 command 的一次性 `NotAttempted` 只减少原候选的聚合初始义务，不匹配某条 accepted。若原候选仍未绑定、义务已为零，却又收到尚未消费的 `NotAttempted`，按证据矛盾停止新工作，不回退其他 ID 或修改新候选。Provider 回执可以先于 SendResult，不能重复消解已经使用过的同一事实；已绑定或取消的候选不因迟到结果重开观察。
- 同连接、channel、generation 和父匹配，唯一未绑定候选尚有初始义务时，首次消费的无 ID `failed` 可以消解一个初始义务；正常初始拒绝不因此等待 timeout。
- `accepted` 消解一个初始义务并记录 ID；该已接管 ID 的后续 `failed` 只移除相应可能性，不再减少初始义务。未知非空 ID 的失败不能冒充初始失败。
- 原 command 必须有独立的一次 completion/token，不能只用多条发送共享的候选 ID 判断是否消费。该关联在结果消费后结束，不建立永久发送表。

活动父已有的 provider 事件消费水位不随候选 Cancel 清零，用于防止同一入站事实在本地缓冲/重放中再次改变计数；可识别的上游重复同样不再消费，原帧仍可交付。该最小边界随活动父结束，不保留全连接回执历史，也不把本地接收顺序当成服务端事件身份。已识别的矛盾或无法区分的事实不能推进退款。

预扣一旦绑定或成功取消，立即释放该候选的提交明细和 ID 集合；不重置仍活动父的上述防重边界，不继续追踪每条输入的 committed/failed 状态，也不等待所有旧发送结果才释放候选观察。仍在途的发送由原 command 自己收尾，候选指针的存在与否不决定其有效性。

### 资源证明与计费观察分开结束

父为 `store:false` 时，首条 steer 准入前从已观察父响应取得连接内归属引用。全部 steering 明确失败，或自动 child 绑定且已收齐初始回执、足以确认旧父不再等待续接时，可以释放该引用。因等待客户端输入而取消预扣，或绑定时初始回执尚不完整，则保守保留父引用至匹配显式 create 创建新 Response，或连接结束；不为更早回收而继续保存整组回执状态。普通无关 create 不消费该引用；显式准入失败或上游拒绝也不消费。

这里保留的是授权事实。取消预扣后即使收到迟到 failed，也只转发，不为提前回收引用而重新维护整组 steering ID。引用可能保守地多保留到匹配续接或断线，这一取舍受容量上限约束；它不声称上游仍保存 steering 输入。归属入口直接查询这一事实，不依赖普通历史 LRU 或一小时临时缓存，也不复制一份 pin cache。

引用只对原 principal、channel、generation 有效，不能从客户端给出的任意父 ID 创建。`store:true` 继续遵循 SQL owner 校验；连接内临时证明不绕过持久权限。

### 容量只约束代理实际负责的占用

- 尚未绑定预扣最多接纳 64 条待观察提交，保留的 `steer.id` 每个最多 1024 字节；限制实际回执元数据和未结义务。候选观察结束后释放其预算。不再累计已发送且未保留的原始 payload：顺序完成发送的多个大帧只占用现有队列预算，不因累计流量达到 4 MiB 被拒绝。
- 留存父资源引用最多 64 个，父 ID 总字节不超过 4 MiB；均为明确的内存边界。首条 steer 在 provider work 前预留所需引用容量，防止收到 pending 后才发现无法保存必要授权事实。
- 原始帧仅占用现有发送/事件/FIFO 字节预算，不因观察再保存长期 payload。引用或队列超限时在新增 provider work 前拒绝；不能淘汰仍被引用的证明。
- 代理不承诺限制上游保存的全部 steering 输入；上游自己的队列限制由上游错误表达。

## 4. Steering 的计费映射与协议依据

本节区分服务端协议事实和本地收费选择。核对日期为 2026-09-10，来源为 [steering 指南](https://developers.openai.com/api/docs/guides/steering)及[事件参考](https://developers.openai.com/api/reference/cli/resources/beta/subresources/responses)。参考页面中的其他 beta 能力不自动进入本项目支持面。

| 已核对的上游事实 | 本地可使用的决定 | 不能推出的结论 |
| --- | --- | --- |
| accepted 只表示输入接管，后继 created 是提交点 | 继续持有尚未绑定的预扣；用已知关联处理后续失败 | accepted 是收费证据、每个 accepted 都有独立 Response |
| 自动后继继承父设置，响应各自适用 token/工具限制 | 为潜在后继事前准入，实际用量归后继 | 预扣等于最终费用、父子可以混合用量 |
| 同父多条待处理输入不需要分别显式续接；匹配 create 使用自身设置 | 在当前串行契约下为共同后继保留一笔预扣；显式 create 独立准入 | 任意多命令都能共享执行权，或者代理应合并输入 |
| 已知 waiting_for_required_input 表示须客户端提供输入；failed 表示该输入不会自动应用 | 在关联明确时取消不再等待自动执行的预扣 | 任何未知 pending reason 都能触发退款，或一条 failed 取消全组 |

据此采用以下本地映射：首条 steer 在可能触发新 Response 前完成现有权限、渠道、资源、RPM、Try 和一次 Claim；同一尚未绑定的后继只认领一次执行权。同父后续帧仍经过关闭许可、支持面、资源授权和容量检查，原样发送，不增加每帧账单。新 child 再接收 steer 时重新准入。

这是一项仅限 steering 的 Work Action/Application Submission 映射，定义同步记录在 `CONTEXT.md`；不能把追加工作归为无需授权的 Observation/Control Call。金额算法、当前策略读取和一次余额动作保持现行 ADR。

准入直接使用已授权父请求的必要事实与本次原始输入的资源引用，复用共享 policy；不先构造伪 `response.create` 再让 create 的所有参数校验器解释 steer。不需要复制上游消息、工具或 `required_input` schema。内部 admission projection 不进入 wire，不充当真实用量。

| 收到的事实 | 仅对尚未绑定预扣的处理 |
| --- | --- |
| 首条 steer / 同父追加 | 当前非 terminal 父的支持窗口内准入或加入现有预扣；记录必要的初始回执义务 |
| 可关联 accepted / failed | 按第 3 节归并初始义务与已接管 ID；只有全部提交都明确失败或未发送才取消，重复或无法归属的事实不重复扣减计数 |
| 父 terminal | 独立结算父；保留尚未消解的后继预扣，不保留已结算父对象 |
| 已知 waiting_for_required_input | 父已完成、通知属于该候选、全部初始义务已消解且尚无 child 绑定时 Cancel；随后只保留必要资源证明 |
| 匹配自动 child.created | 连接/channel/generation 正确、父已 terminal、无竞争的显式 create 在途，且存在唯一未取消、已 Claim 的预扣时绑定；父字段存在时必须匹配，缺省时须由已验证串行契约消歧。不要求完整 accepted 列表，也不做事后 Try |
| 匹配显式 child.created | 绑定已经按该显式请求准入的 attempt；不复用已取消预扣；释放相应父资源引用 |
| 预扣已消解后的控制回执 | 原样交付，不再改变账务或恢复旧计费观察 |

旧预扣的 Cancel 成功后才能解除关联、允许新的候选或释放相应调度屏障。取消失败或余额提交未知时停止新工作，沿用一次结算 guard；不重试余额动作、不把旧预扣转给新执行。同父重新准入还要求父仍活动、旧候选的初始义务和已接管输入已经全部明确失败或未发送，不允许旧初始义务与新候选重叠。旧发送结果仍只关联原 command。

Pending 的初始义务屏障还保护发送授权：只凭父等待工具结果就先 Cancel，可能让已准入但仍排队的追加命令在预扣终结后才进入 Send。保留“初始义务全部消解”这一保守条件，不另设提前释放路径；回执已经证明发送发生时，无需继续等迟到的 SendResult。

协议未能消除归属歧义时，不凭最近发送 ID、父终结状态或本地估算猜测绑定。已经发生的工作按可归属 evidence 收尾，没有可计价组件时依 ADR-0030 Cancel；这不证明上游未执行，也不授权重放。

## 5. 串行交付、停止与清理

### 保留必要顺序，不增加客户端编排

现有普通 create FIFO 和串行 Response 支持面继续有效。仍可能自动续接时，显式 create 在原 FIFO 等待必要的计费归属消歧；收到已知 pending、全部失败或自动 child 结束后，严格从队首推进，不跳取后方匹配请求，不检验工具结果是否齐全。客户端可以提前提交，代理不要求重发。这个等待不因仅有旧资源证明或无账务作用的回执而延长。

未知 pending reason 原样交付；若仍有未消解预扣，维持已有有界等待，不猜测新的续接方式。未来支持面如果允许多个执行候选同时在上游活动，必须先获得可区分它们的协议关联；不能因“更透明”而发送后再补执行权。

### 同一关闭许可边界

复用 `postMu` 的短锁边界发布停止事实、捕获 cut 或允许紧邻的一次操作，锁外执行 I/O/SQL。不增加 permit 注册表。每个后续 Try、Claim、open、worker Send 阶段重新检查；已知客户端断开不能等到队列消费时才生效。

关闭先撤销新工作许可。原 command 的真实发送歧义、已验证连接的 fatal/workflow stop 仍作用于连接；已消费结果或旧 generation 不影响当前执行。错误归到原操作，不因原计费候选已释放而解释为新候选失败。

停止信号不丢弃触发帧：已知 owner 的合法 usage 仍归并，可安全交付的上游错误仍保留，然后处理 closure cut。当前触发事件和截止内事实只消费一次；后到 usage 不追加第二次余额动作。关闭排空共用事实归并，不能再次进入新工作入口。

仅保留现有 pending create 最多 100ms 的发送完成收尾，不为每条 steer 增加 grace。晚到 open 结果沿用 `Adopted` 交接，worker/actor 中只有一方承担 session/lease 清理。交接决定和责任登记必须先于 actor 使用、清理资源或发布完成关闭；不能在关闭后的 defer 才宣布接手。Worker 看到 `done` 不自动取得清理权，仍以已确定的交接结果为准；actor 拒绝接手或投递失败才由 worker 清理。投递入队不等于交接完成，清理不受新工作禁止影响。

### 计时对象是占用，不是对话

复用一个现有 watchdog：当前 Response → 父释放后的自动候选 → 实际 child，按真实占用切换目标并更新 timer generation。取消候选且没有执行时撤销该计时；仅等待客户端的资源证明不占 active turn。旧父回执不刷新无关执行的期限，过期 timeout 不关闭新目标。继续尊重 timeout=0，容量仍有界，不另外承诺等待时长上限。

## 6. 替换范围与验收

本次替换以下错误共享边界，不以逐个 handler 加条件或扩大历史缓存完成：

- 删除“steering 回执必须被当前候选或完成历史认领才能交付”的门禁；计费观察失效不等于原始帧失效。
- 将计费回执观察限制在未绑定预扣期间，取消完整的跨 Response steering 输入生命周期镜像；资源证明和发送结果各按自身用途结束。
- 让 adapter 的协议观察、relay 的账务归属都避开“最近发送就是当前 Response”的假设；不增加 provider 类型分支。
- 共用新工作准入与关闭边界，调整 watchdog 和晚到资源交接；保留 TCC、Stored owner 屏障与既有传输预算。

变化集中于 `relay/responses_ws*`、必要的 `common/responsesws` 投影及 OpenAI/Codex adapter 接线。在同一完整变更中删除被替代路径，不保留 feature flag、legacy 双实现或新的恢复存储。

必须验证的行为：

1. 无当前候选、无父历史、未知 ID/reason 的本连接控制回执原样交付；未知字段、大整数、错误协议和流式顺序保留，凭据脱敏生效。
2. 首条 steer 在 provider work 前完成 Try；同父多条只绑定一个实际后继，父子 usage 分离。正常无 ID 初始失败能消解义务并释放；accepted 后 failed 不重复减初始计数，未知非空 ID 不误退款；同父重新准入后本地重放旧回执不影响新候选。Pending 时仍有排队追加，不提前 Cancel；回执先于 SendResult 不重复消解义务。
3. Parent terminal 后的 child.created 不依赖本地完整 accepted 队列；预扣不存在、候选不唯一或关联矛盾时不补建执行权，不换渠道或重放。
4. Pending 取消预扣后清理回执观察；超过 16 次无关响应且临时缓存失效，持有父证明的合法显式续接仍通过；伪造父 ID、跨用户及 Stored 权限绕过仍被拒绝。
5. 提前排队的匹配/无关 create 保持 FIFO；客户端及上游自行合入输入，代理没有额外 create、工具结果重建或输入重发。
6. 在准入、SQL 返回、Claim、worker Send、created/usage 入队及 open 交接处关闭；不取得新工作许可，不重复释放或结算。Stored child.created 已绑定而 owner SQL 失败，cut 内 child.completed 的合法 usage 仍结算且不对客暴露资源；安全 terminal 同样保留用量。交接确认与 done 同时可见时仍只有一个资源清理方。
7. Child 活动时旧父回执进入 Codex adapter，随后私有 child terminal 需要补序号；child 序号与 usage 不被旧回执污染。候选/child 目标切换后旧 timeout 无效。
8. 回执观察、资源证明、队列分别耗尽容量；拒绝在新增 provider work 前发生。多个顺序发完、累计超过 4 MiB 的帧不因历史 payload 流量被拒绝。Cancel 失败/提交未知禁止新候选；未发送帧不遗留回执义务。

复用现有 fake upstream、actor fixture 和余额操作计数器，用 channel/barrier 控制交错。验收衡量 wire 是否相同、是否出现额外上游请求、额度和资源是否只处理一次，不要求保留旧结构的字段值。实际实现运行相关包测试与定向 race 检查，不为文档方案建立通用消融测试框架。

### 外部影响与未验证边界

保留已有 native-only、固定渠道、精确 model、普通 create FIFO 与 Stored Responses 公共契约。晚到回执不再因代理无记录而变为本地协议错误；不完整计费观察不再阻止有明确唯一预扣的 child 绑定。steering 的容量限制已调整为实际观察和资源引用预算，并同步使用文档。

没有数据库、公共请求字段或配置格式迁移；内存状态只服务本连接，发布/回滚不迁移或重放输入。额外 provider 请求、跨用户归属、重复余额动作或 wire 改写是停止发布条件。回滚不能撤销已发生工作或费用。

实现已运行本地 fake upstream 和 actor 契约回归，但尚未完成真实上游事件交错验证；尤其“同父共同后继”、首次失败关联和取消条件必须由协议契约测试确认，不能用 Codex 本地 turn 或本地构造测试相互证明。若证据不足，应收窄该操作的已承诺支持面并在 provider work 前明确失败，不扩建隐式编排或恢复机制。

需要单列的支持假设是事件序号作用域和外部重复表达：一个新接收位置或更大序号不独立证明发生了新的提交拒绝。若上游用全新事件身份重复同一次无 ID 失败，现有字段可能无法把它与新提交的拒绝区分；本地消费水位或完整历史 registry 都不能消除这种歧义。可识别的重复/矛盾只交付、不用作退款证据；正常、关联明确的首次失败仍按上述聚合规则处理，不因这一待核假设一律等待 timeout。该协议假设应在实际 provider 契约验证中单独确认。
