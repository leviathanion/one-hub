---
title: "基于下游 Usage 的 TCC 计费回退方案"
layout: doc
outline: deep
lastUpdated: true
---

# 基于下游 Usage 的 TCC 计费回退方案

## 文档状态

- 状态：当前实现。
- 范围：消费侧预扣、下游 usage evidence、Confirm/Cancel、短请求与异步任务的结算 owner。
- 本文是消费结算主规格。
- 异步 Task、ResponsesWS 与 Realtime 的协议生命周期仍由各自专项文档负责；本文只拥有共同计费语义。
- 决策记录：[ADR-0030](../adr/0030-use-provider-usage-confirmed-tcc-billing.md)。

## 一、结论

one-hub 回退到旧方案的金额模型：请求前做小额预扣，结束时按实际费用与预扣的差额补扣或退款。旧方案只保留价格计算和预扣公式，不保留“本地估算也能收费”“没有 usage 时保留 floor”或 Redis 拥有结算事实的语义。

计费生命周期采用业务级 TCC：

```text
Try      -> 建立一次 Billing Attempt，并预留小额 quota R
Confirm  -> 仅在收到可归属、可完整计价的下游 usage 时，最终收费 C
Cancel   -> 没有合格下游 usage 时，释放全部预留，最终收费 0
```

最终不变量：

1. Reservation 不是 Charge；只有 Confirm 才形成最终消费事实。
2. provider work 可能已经发生、HTTP 状态成功、收到部分正文或本地估算出 token，都不能替代下游 usage。
3. 没有合格下游 usage 一律 Cancel，不保留预扣 floor，不建立 unresolved settlement intent。
4. 一个 Billing Attempt 只有一次 Application Submission 和一次 Confirm/Cancel 执行权。
5. SQL 是 user/token 余额事实源；Redis 只允许作为缓存，不能授予或恢复结算执行权。
6. 最终费用可以高于预扣，允许正向补扣；并发请求可能造成少量透支，这是为简化架构接受的代价。

这是一套围绕本地额度资源实现的 TCC，不声称下游 provider 是事务参与者，也不提供跨系统原子提交。

## 二、领域模型

### Billing Attempt

一个 Billing Attempt 对应一个能够独立产生下游 work 和 usage 的 Work Action，例如一次 unary 请求、一个 ResponsesWS turn、一个 Realtime turn 或一个异步任务提交。

短请求和 turn 由当前 request/actor 持有 Attempt；异步任务由 Task 行持久拥有 Attempt。客户端重复请求会创建新的 Attempt，不提供客户端请求幂等。

Attempt 至少持有：

- `user_id`、`token_id`、`channel_id`、operation 与 model；
- 预扣额度 `R`；
- 已确定的执行归属；
- Application Submission 是否已经 claim；
- Confirm/Cancel 是否已经 claim；
- 已归属的权威下游 usage evidence。

短 Attempt 使用进程内 guard，异步 Task 使用现有 Task 行和 SQL CAS。当前不为所有同步请求新增通用 billing ledger、outbox 或 workflow engine；进程在 Try 后、Confirm/Cancel 前崩溃时可能留下小额预扣，这是明确接受的 best-effort TCC 边界。

### Quota Reservation

Quota Reservation 是 Try 阶段暂时占用的客户可用额度 `R`，用于低余额准入和降低并发透支。它不是价格上界，也不是下游成本估算，更不是最终账单。

沿用旧预扣公式：

- token price：本地 prompt token 估算乘输入价格，再加配置的 `PreConsumedQuota`；
- times price：沿用一次请求的旧预扣额度；
- 免费价格：`R = 0`；
- 普通高余额用户允许沿用旧 fast path，把 `R` 降为 `0`；
- 异步 Task、ResponsesWS/Realtime turn 是否强制建立非零 `R` 由其准入策略决定，但不能改变“无 usage 必须 Cancel”的规则。

`R = 0` 仍然存在 Attempt、submission claim 和 Confirm/Cancel，只跳过余额写入。

### Authoritative Provider Usage

Authoritative Provider Usage 是唯一的收费证据，必须同时满足：

- 数据确实来自选定下游 provider，而不是本地 token 估算；
- 能关联到当前 Attempt，不能只凭模型名、连接或用户做弱关联；
- usage 字段合法、非负、有限且覆盖当前 Price Policy 所需的计费维度；
- adapter 明确声明 usage 是增量值还是累计值，relay 只采用一种聚合语义；
- 重复 evidence 能以稳定 identity 或完整内容比较收敛，冲突 evidence 不收费。

不能再通过 `usage != nil` 判断证据存在。当前 relay 会预先填入本地 prompt tokens，并可能从响应文本估算 completion tokens；目标实现必须增加显式的 provider usage evidence 标记。

固定次数、图片、视频或其他 operation 若不返回下游 usage/billable-units evidence，则最终免费。provider success、accepted task ID 或业务 terminal 本身不能被合成为 usage；若该产品不能接受免费，应在 provider work 前把 operation 标记为不支持。

### 当前 Price Policy

Try 计算 Reservation 时读取当前完整价格发布与 group ratio；Confirm 对 provider usage 计价时重新读取当时的当前完整发布。两阶段可以观察不同配置，余额差额由 `C - R` 吸收；provider upstream cost 始终不是客户价格。

每个阶段的一次读取必须来自同一完整 publication，不能把两份发布的 Base Price、Rate Rules 或 Extra Ratios拼成一个 Price Component。Try 缺少当前价格时在 provider work 前失败；provider work 已发生后若 Confirm 无法取得可计价的当前策略，则该 component 计 0 并记录诊断，不重放 provider work。

## 三、TCC 状态与余额公式

### Try

Try 在 provider work 前完成：

```text
计算 R
校验 user/token 当前可用额度
R > 0 时预扣 user/token quota
记录 Attempt 已进入 tried
```

Try 必须幂等：同一个 Attempt 重入不得预扣两次。Try 失败时不允许 Application Submission。

旧 schema 会在预扣时同时改变 remain/used quota。实现时应把“暂时占用可用额度”和“最终 used quota”在语义上分开：消费日志、请求计数和最终 used quota 只能由 Confirm 投影；若暂不拆余额列，至少不能把 Try 记录成最终消费事件。

### Application Submission

Attempt 在调用 requester、SDK、adapter，或把 billable WebSocket frame 放入发送队列前 claim 一次 submission。claim 永不重置。

provider 可能已经观察请求后，不因 transport error、状态码、credential refresh、响应转换失败或下游断开执行应用层 retry、换渠道或第二次提交。provider 自身提供的幂等键只服从其协议，不授予 one-hub 重试权。

### Confirm

只有 Authoritative Provider Usage 存在时 Confirm：

```text
C = CurrentPricePolicy.Calculate(provider_usage)
delta = C - R
```

- `delta > 0`：补扣；允许余额因此变为负数。
- `delta = 0`：预扣正好等于最终费用。
- `delta < 0`：退回未使用预扣。
- `C = 0`：仍是合法 Confirm，最终收费为 0。

Confirm 必须只取得一次最终余额执行权。重复调用返回第一次结果，不重复应用 `delta`，也不重复生成消费日志或请求计数。

### Cancel

closure 时没有合格 Authoritative Provider Usage 就 Cancel：

```text
delta = -R
final_charge = 0
```

以下情况全部执行相同 Cancel 语义：

- provider 成功响应但没有 usage；
- provider error、EOF、timeout 或 malformed response 没有 usage；
- transport 结果 ambiguous，但代理没有收到合格 usage；
- usage 缺少价格所需维度、非法、溢出或相互冲突；
- 本地发送前失败或明确 not-attempted；
- 异步任务进入终态或过期，但仍没有 usage。

Cancel 只释放 Reservation，不改写下游响应，不恢复 submission 权，也不授权换渠道重放。

### 结算 reducer

共享 reducer 只保留两个结果：

```text
存在完整、无冲突的 Authoritative Provider Usage -> Confirm(C)
其他                                             -> Cancel
```

不再存在 `KeepReserved` 最终结果、NoChargeProof 特判、observed-or-floor、preconsume floor 或 unresolved intent。

## 四、不同调用形态

### Unary HTTP 与 HTTP Streaming

每个入站请求建立一个 Attempt。adapter 可以从非流式响应或流式事件提取 provider usage；最终 closure 使用最后一份合法累计 usage，或按 adapter 契约聚合增量 usage。

响应成功与否和是否收费相互独立：带合法 usage 的 provider error 可以 Confirm；没有 usage 的成功响应必须 Cancel。本地从 prompt 或输出文本估算的 token 仅用于日志诊断，不能进入 Confirm。

### Responses WebSocket 与 Realtime

每个能够独立触发 provider work 的 turn 建立独立 Attempt，连接本身不共享一个总预扣。actor 只接受能够关联当前 turn 的 provider usage。

turn terminal、provider close、client close、watchdog 或 ambiguous send 都只决定 closure 时机，不决定金额。closure 前观察到合法 usage则 Confirm；否则 Cancel。发送结果不明确仍禁止重放，但不再以预扣 floor 收费。

### Async Task

Task 是持久 owner，并保存提交时实际发生的 Reservation 和 channel binding；价格配置不随 Task 持久化，finalization 使用当时的当前完整发布。submit 只允许一次；polling/callback/fetch 都必须在同一 Task 行上竞争一次 finalization。

provider terminal 带合法 usage时，Task terminal 与 Confirm 在同一 SQL 事务提交；provider terminal、超时或 stale close 没有 usage时，Task terminal 与 Cancel 在同一事务提交。`charged_quota` 以 nullable 值区分“未 final”和“已 Confirm/Cancel 为 0”，禁止从业务状态猜测收费。

Midjourney 等没有持久 Task owner 的旧路径，要么接入相同 Task owner，要么只使用请求内 TCC；不得通过回调 success 重复收费。

## 五、为什么不采用完整上界 U

### U 的定义与收益

完整上界方案设真实最终客户费用为 `C`，在 provider work 前计算并强制：

```text
0 <= C <= U
reserved_quota = U
```

如果该约束成立，Confirm 只需退款 `U - C`，不需要正向补扣；并发请求不能消费未被 SQL 预留担保的额度，客户余额也不会因正常 Confirm 变成负数。在缺少 usage 时保留 `U`，还能把一次歧义多扣限制在预先声明的范围内。

### 本次不采用的原因

1. **与收费事实冲突**：当前产品规则要求没有下游 usage 就不收费，因此 ambiguous 或 usage 缺失时必须 Cancel；`U` 不能再充当 `KeepReserved` 的最终收费依据。
2. **破坏代理边界**：exact-wire 和未来扩展字段可能影响 provider work 或费用。为证明完整上界，代理必须封闭未知字段、注入最大值或复制上游参数语义，这与协议透传职责冲突。
3. **支持面显著收缩**：没有强制 `max_output_tokens`、时长、工具次数或 provider account hard cap 的 operation 无法证明 `U`，只能在 provider work 前拒绝。
4. **过度预留损害可用性**：理论最大输出、最长时长和工具上限通常远高于实际 usage，低余额用户会被大量合法请求错误拒绝。
5. **复杂度收益不匹配**：U 需要 operation capability、完整计费维度枚举、catalogue preflight、上界 calculator、capability violation quarantine 和更多测试状态；这些复杂度主要服务于“绝不正向补扣”和“歧义仍收费”，而当前明确接受少量透支并放弃无 usage 收费。
6. **旧方案已有成熟路径**：小额 `R`、最终 `C - R` 与高余额 fast path 已存在，回退只需收紧 usage 证据和 finalization owner，无需建立第二套上界体系。

### 重新考虑 U 的触发条件

只有出现以下硬要求之一时才重新评估完整上界：

- user/token 余额绝不允许为负；
- 并发透支形成不可接受的实际坏账；
- provider/account 对所有收费维度提供稳定、可强制的 hard cap；
- 产品愿意缩小 exact-wire 支持面并要求客户端显式提交所有最大费用参数。

即使未来采用 U，没有下游 usage 是否收费仍是独立产品决策，不能由 Reservation 自动推导。

## 六、事实源、幂等与故障边界

- user/token SQL 余额是唯一 quota truth；Redis cache failure 不能改变 Confirm/Cancel 决策。
- 短 Attempt 的进程内 guard 只防当前 owner 重入；进程 crash 或 SQL commit unknown 可能造成小额错算，不建设通用 ledger 修复。
- Async Task 必须用 Task 行的 nullable `charged_quota` 和状态 CAS 持久防重，不依赖 Redis gate。
- Confirm 在同一事务内更新 user/token 余额、用户 `used_quota += C` 并重算用户组；Cancel 退回预扣后也重算组。`used_quota` 不再由批量投影写入，`C=R` 时仍须记录最终消费。
- projection 在余额事务之后执行；日志、请求计数、dashboard aggregate 和 metrics 失败不重放余额 action。
- 预扣期间接受暂时的分组偏差，正常结算后收敛；异常时管理员用零额度增减手动重算。发布前须排空旧实例的已消费批量统计，避免与新事务 writer 重复累计。
- usage evidence 必须先完成 owner correlation 和安全脱敏，再进入 reducer；provider response security 与 billing reducer 分属不同边界。
- Confirm/Cancel SQL 失败可以重试同一个余额 action，但绝不能因此重试 Application Submission。

## 七、落地结果

本次未保留 feature flag、dual writer 或兼容分支，已一次性完成：

1. 删除完整上界 `U`、Hard Reservation、`KeepReserved` 和 operation upper-bound preflight 的未完成实现。
2. 恢复旧 `Quota` 的小额预扣与 `final - preConsumed` 差额结算作为唯一金额路径。
3. 在 provider/adapter evidence 边界增加显式 Authoritative Provider Usage，禁止本地估算伪装成下游 usage。
4. 将 unary、stream、ResponsesWS、Realtime 和 Async Task 收敛为 `usage -> Confirm / otherwise -> Cancel`；当前无 usage 的 Async provider 终态免费。
5. 短 owner 使用一次 final guard；Async Task 使用 Task 行 SQL CAS，Redis 不再承担结算执行权。
6. 删除 ResponsesWS unresolved settlement intent、floor reducer、清理任务和对应 schema。
7. 删除被替代且会形成双重权威的三份方案文档，并更新 ADR、CONTEXT 与开发索引。

切换完成的回滚条件是：出现系统性重复扣费、Reservation 无法释放或 provider usage 被错误归属。回滚只恢复上一版本代码和 schema，不同时运行两套 settlement writer。

## 八、验证矩阵

至少覆盖：

| 场景 | 预期 |
| --- | --- |
| provider success + 完整 usage | Confirm，最终收费 `C` |
| provider error + 完整 usage | Confirm，最终收费 `C` |
| success/error/timeout/EOF 均无 usage | Cancel，最终收费 `0` |
| 本地估算 token 非零但 provider usage 缺失 | Cancel |
| `C > R` | 只补扣一次，允许负余额 |
| `C < R` | 只退款一次 |
| 重复 Confirm / Cancel | 余额、日志和请求计数不重复 |
| Confirm 与 Cancel 并发 | 只有一个终态获得余额执行权 |
| Redis 不可用 | SQL TCC 语义不变 |
| WS ambiguous send 无 usage | 不重放并 Cancel |
| Async callback、poller、sweeper 并发 | Task 只 final 一次 |
| fixed-price operation success 但无 usage | 免费或 provider work 前拒绝 |

协议边界还需验证：未知字段仍透传、不可表示输入在 provider work 前失败、上游状态/错误保持原样，以及 provider work 可能发生后没有隐式 retry 或换渠道。

## 九、接受的代价

- provider 实际产生费用但没有返回 usage 时，one-hub 会少收或不收；这是明确产品规则。
- 小额预扣不是完整费用担保，`C > R` 和并发请求可能使余额为负。
- 短 Attempt 在进程 crash 或 commit unknown 时仍可能留下少量错算；若该风险超过预算，升级方向是持久 Attempt receipt，而不是重新让 Redis 成为结算 owner。
- 每个支持收费的 operation 必须有明确的 provider usage evidence contract；没有该证据的 fixed-price operation 只能免费或不支持。

该取舍优先保证收费事实可解释、协议边界不被计费上界侵入，并以少量可接受错算换取实现和维护简洁性。
