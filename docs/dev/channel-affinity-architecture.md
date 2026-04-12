---
title: "Channel Affinity 架构设计方案"
layout: doc
outline: deep
lastUpdated: true
---

# Channel Affinity 架构设计方案

## 文档状态

- 状态：正式方案，已选型并已按当前代码落地
- 目标：统一描述 one-hub 在 `responses`、Codex `realtime`、路由分组、fallback 与 continuation miss 上的 affinity 架构
- 文档口径：既保留选型说明，也明确当前实现 contract；不再把任何中间过渡形态视为目标方案
- 范围：`common/groupctx`、`common/config/channel_affinity.go`、`relay/channel_affinity.go`、`runtime/channelaffinity`、Codex routing hint 与 realtime affinity
- 非范围：strict distributed owner、跨节点 fencing token、全局 cooldown 协议、无 replay 前提下的 continuation 自动恢复

## 真实问题与设计目标

从第一性原理看，这里要解决的不是“做一个形式上最强的一致性系统”，而是三个更具体的问题：

1. 让跨请求路由尽可能命中同一渠道，从而提高 `responses` continuation 命中率、Codex prompt cache 命中率和 realtime session 复用率。
2. 在 fallback、Redis 抖动、provider 差异和渠道瞬时失败存在时，系统仍然要保留可服务的降级路径。
3. 在缺少 replay、lease、fencing、node-aware ingress 等基础设施时，不伪装成具备更强保证。

因此当前方案明确选择 `availability-first` 的 affinity 模型：把必须成为路由真相的状态前移并做实，把只能作为共享提示的状态保持为 best-effort hint，把没有语义等价恢复前提的错误直接 fail-close。

## 已选型结论

### 1. 路由作用域采用显式 `routing_group`，不复用声明态 `token_group`

已选方案：

- `token_group` 只表示令牌声明的主分组。
- `routing_group` 表示当前请求真实参与选路、affinity、quota、日志和元数据的分组。
- 初始化阶段由 `middleware/distributor.go` 建立首个 `routing_group`。
- fallback 到备用分组时只更新 `routing_group`，不回写 `token_group`。

选择原因：

- fallback 之后如果 affinity 仍按旧 `token_group` 计算，系统内部会同时存在两套“当前分组”定义，后续的 quota、日志、亲和键都可能错位。
- 路由作用域是运行时事实，不是声明配置；必须单独建模。

### 2. Affinity manager 的 janitor 生命周期在构造期确定

已选方案：

- `runtime/channelaffinity.ConfigureDefault(...)` 首次调用时用完整 `ManagerOptions` 创建默认 manager。
- `UpdateOptions(...)` 只更新 `DefaultTTL`、`MaxEntries`、Redis backend/prefix 这类运行期可调参数。
- `JanitorInterval` 明确是构造期配置，后续热更新不会启动或停止 janitor goroutine。

选择原因：

- 后台清理 worker 的存在与否属于生命周期问题，不应该隐藏在运行期配置更新里。
- 这样可以把 manager 的并发语义收敛到“构造期确定 worker，运行期只调参数”，避免隐式副作用。

### 3. 会影响 affinity 命中的稳定 hint 必须在 provider 选择前可见

已选方案：

- `responses` 请求在 `relay/channel_affinity.go` 的 `prepareResponsesChannelAffinity(...)` 阶段先调用 `requesthints.ResolveResponses(...)`。
- Codex routing hint 通过 `providers/codex/routing_hint.go` 生成 `responses.prompt_cache_key`。
- affinity lookup 和 provider 最终请求都消费同一个 hint；provider 不再在选路之后独占生成 `prompt_cache_key`。

选择原因：

- 如果稳定 hint 只在 provider 内部事后生成，记录 affinity 和命中 affinity 发生在两个时间层，路由阶段无法看到真正的 sticky identity。
- 对 prompt cache 这类会直接影响渠道选择的键，必须在选路之前固定下来。

### 4. Codex realtime 采用“共享 binding hint + 本地 runtime owner”，不做 strict distributed owner

已选方案：

- Redis binding 表示共享 resume hint。
- 本地 `runtime/session` 中的 execution session 才是真实执行体和本地 owner。
- revocation key 用来阻止已废弃 session key 再次进入 resume 路径。
- `ResolveBinding` / `CheckRevocation` / CAS 写入共同决定 resume、fresh open、replace 和 local-only promotion。

选择原因：

- 如果把 Redis binding 直接当成 strict owner，就必须进一步解决 lease、heartbeat、fencing、node-aware handoff 等问题；当前系统没有也不需要为此付出复杂度。
- 当前业务真正需要的是“尽量复用，遇到不确定时保守降级”，而不是跨节点强一致 owner 协议。

### 5. Preferred channel 失败后的影响范围限定在当前请求

已选方案：

- affinity 命中后先尝试同渠道打开。
- 若同渠道打开失败且当前不是 strict affinity，则只把该渠道写入当前请求的 `skip_channel_ids`。
- 后续 fresh reroute 复用已有选路逻辑，但不写入 Redis，不做全局 quarantine。

选择原因：

- 单次打开失败只能说明该次尝试不可用，不能推出“这个渠道对所有后续请求都不可用”。
- 失败隔离保持在 request scope，可以保住可用性，同时避免污染共享事实。

### 6. `previous_response_id` continuation miss 按 correctness 边界处理，不做隐式自动恢复

已选方案：

- 当上游返回 `previous_response_not_found` 时，外层统一清理本次请求涉及的 affinity binding。
- 记录“可恢复候选”元数据，但直接返回显式 `409 conflict` 错误给客户端。
- 不自动清空 `previous_response_id`，不在无 replay 能力前提下做静默重放。

选择原因：

- continuation 不是普通 hint，而是有语义锚点的状态续接。
- 没有完整 replay 能力时，自动恢复会导致上下文漂移和语义错误；宁可显式失败，也不能伪造正确性。

### 7. 显式 pin 是 request-local override，不属于 shared affinity

已选方案：

- 存在 `specific_channel_id` 这类显式 pin 时，跳过 shared affinity lookup。
- 请求成功后也不回写 shared affinity。
- 日志和元数据可以记录本次 pin 到的渠道，但不能把 pin 结果提升为跨请求共享事实。

选择原因：

- pin 表达的是“这次请求就走这里”，不是“后续未 pin 的请求也应走这里”。
- 如果 pin 成功会回写共享状态，调试、回放、手工续接会污染正常请求的路由结果。

## 架构总览

当前架构可以概括为：

`availability-first channel affinity = 显式路由作用域 + 构造期确定的 manager 生命周期 + 选路可见的稳定 hint + shared binding hint + 请求级失败隔离 + correctness-first continuation 边界`

这套架构由四层组成：

1. 作用域层：`groupctx.CurrentRoutingGroup(...)` 定义请求当前真实选路作用域。
2. 规则层：`ChannelAffinitySettings` 定义哪些请求、哪些键、哪些上下文维度参与 affinity。
3. 存储层：`runtime/channelaffinity.Manager` 提供本地内存 + 可选 Redis 的 hybrid affinity record 存储。
4. 协议层：`responses`、Codex routing hint、Codex realtime、fallback 与 continuation miss 共同消费同一套 affinity contract。

## 配置与数据模型

### 默认规则

`common/config/channel_affinity.go` 当前内置三条默认规则：

1. `responses-continuation`
   - kind: `responses`
   - key source: `request_field.previous_response_id`
   - strict: `true`
   - `record_on_success: true`
2. `responses-prompt-cache-key`
   - kind: `responses`
   - key source: `request_field.prompt_cache_key`
   - key source: `request_hint.responses.prompt_cache_key`
   - `record_on_success: true`
3. `realtime-session`
   - kind: `realtime`
   - key source: `header.x-session-id`
   - key source: `header.session_id`
   - `record_on_success: true`

规则支持：

- `ModelRegex`
- `PathRegex`
- `UserAgentRegex`
- `IncludeGroup`
- `IncludeModel`
- `IncludePath`
- `IncludeRuleName`
- `Strict`
- `SkipRetryOnFailure`
- `IgnorePreferredCooldown`
- `RecordOnSuccess`
- `TTLSeconds`

### Affinity key 模板

单个 affinity key 由 `channelAffinityTemplate` 生成，输入由以下部分拼接：

- `scope`
- `kind`
- `alias`
- 可选 `rule:<name>`
- 可选 `group:<routing_group>`
- 可选 `model:<model>`
- 可选 `path:<path>`
- `sha256(value)[:16]`

设计含义：

- 原始 value 不直接落键，避免明文暴露 `session_id` / `prompt_cache_key` / `response_id`。
- 同一 value 在不同 routing scope、不同规则、不同模型下天然隔离。
- `ResumeFingerprint` 进一步补充“值相同但恢复语义不同”的情况；当前 `responses` 用模型名作为 fingerprint。

### 存储模型

`runtime/channelaffinity.Record` 目前包含：

- `ChannelID`
- `ResumeFingerprint`
- `UpdatedAt`

manager 支持：

- 本地内存读写
- 可选 Redis 持久化
- TTL 过期
- 本地 `MaxEntries` 容量控制
- Redis 过期清理与超量裁剪

## 核心流程

### 1. 请求初始化与路由作用域建立

`middleware/distributor.go` 在请求进入主链路时：

1. 读取 `token_group`、`token_backup_group` 和用户组。
2. 根据优先级建立首个 `routing_group`。
3. 设置 `routing_group_source`。
4. 按当前 `routing_group` 计算 group ratio。

之后所有 affinity 相关逻辑都通过 `groupctx.CurrentRoutingGroup(...)` 读取有效作用域。

### 2. Responses affinity

请求进入 `responses` 路径时：

1. `prepareResponsesChannelAffinity(...)` 先解析 request hints。
2. 遍历规则并提取 `previous_response_id`、`prompt_cache_key` 或派生 hint。
3. 基于规则模板生成 request bindings。
4. 若命中历史 affinity record，则设置 `preferred_channel_id`、`strict`、`skip_retry_on_failure` 等上下文状态。
5. 请求成功后：
   - `recordCurrentChannelAffinity(...)` 记录当前请求命中的 lookup binding。
   - `recordResponsesChannelAffinity(...)` 进一步把响应里的 `response.id` 与 `prompt_cache_key` 回写为后续请求可复用的 binding。

关键约束：

- `request_hint` 只解决“选路前可见的稳定 identity”问题，不改变 provider 的最终请求语义。
- 如果请求已显式 pin 渠道，则只记录元数据，不写共享 affinity。

### 3. Codex prompt-cache pre-routing

Codex 的 `responses.prompt_cache_key` 派生由 `providers/codex/routing_hint.go` 提供：

1. 仅当客户端未显式提供 `prompt_cache_key` 时才派生。
2. 按 `prompt_cache_key_strategy` 生成稳定 key。
3. 将结果写入 `request_hints`。
4. affinity 和最终 provider 请求都读取同一个结果。

当前选型不是“provider 内部晚生成”，而是“路由阶段先派生，provider 阶段复用”。

### 4. Codex realtime affinity

Codex realtime 的共享亲和逻辑由 `providers/codex/realtime_session.go` 和 `runtime/session` 共同实现。

主流程如下：

1. `buildExecutionSessionMetadata(...)` 基于 caller namespace、client session id、channel id、兼容性哈希构建：
   - `BindingKey`
   - `ExecutionSession.Key`
   - `CompatibilityHash`
2. `planRealtimeOpen(...)` 读取 shared binding：
   - `ResolveMiss`：计划 `create_if_absent`
   - `ResolveHit + compatible + not_revoked`：计划 resume
   - `ResolveHit + revoked`：计划 `replace_if_matches`
   - `ResolveHit + incompatible`：计划 `replace_if_matches`
   - `ResolveBackendError` 或 `RevocationUnknown`：不 resume，也不宣称自己有新的 shared owner
3. 打开新 session 后，如果是 local-only，则通过 `codexMaybePromoteExecutionSession(...)` 尝试 piggyback promotion。

一致性边界：

- Redis binding 是 shared hint，不是 owner lease。
- `VisibilityLocalOnly` 表示本地可继续服务，但不宣称自己已成为新的共享 binding。
- publish/write 失败时优先保住本地可用性，而不是为了追求共享状态强一致而放弃服务。

### 5. Preferred channel fallback

当 affinity 命中某个 preferred channel 时：

1. 请求优先尝试该 channel。
2. 若打开失败且规则不是 strict affinity：
   - 将该 channel 写入当前请求的 `skip_channel_ids`
   - 清除“本次已选 preferred”的选择状态
   - 重新走正常 fresh reroute
3. 若规则是 strict affinity：
   - 不 reroute
   - 按原错误返回

这保证了：

- strict affinity 明确承担正确性优先；
- 非 strict affinity 明确承担可用性优先；
- 失败影响只局限在当前请求。

### 6. Continuation miss 处理

`relay/main.go` 在外层统一处理 `responses` continuation miss：

1. 判断上游错误是否等价于 `previous_response_not_found`。
2. 清理当前请求的所有 affinity binding。
3. 重新准备请求级 affinity 状态，避免 stale binding 残留。
4. 记录恢复候选元数据。
5. 返回本地构造的显式 `409 conflict` 错误。

当前明确不做：

- 自动去掉 `previous_response_id` 并重试。
- 静默回放。
- 把 continuation miss 视为普通渠道错误并走通用 retry/cooldown。

## 一致性模型

当前 Channel Affinity 采用以下一致性边界：

- `routing_group` 是请求内的 routing truth。
- affinity record 是跨请求共享 hint，而不是分布式 owner 协议。
- Redis/backend 抖动允许 false miss，不允许把 request-local override 升级成 shared fact。
- continuation miss 缺少 replay 能力时直接 fail-close。

换句话说：

- 对必须成为“当前请求真实作用域”的状态，选择强语义。
- 对跨请求共享但不值得引入分布式协议的状态，选择 best-effort。
- 对缺少语义等价恢复基础的错误，选择 correctness-first。

## Trade-off

### 当前方案获得了什么

- fallback 后的 routing、quota、日志和 affinity 作用域一致。
- prompt cache key 等稳定 hint 在选路前可见，命中率更高。
- realtime 在 Redis 抖动或共享状态不确定时仍可 local-only 继续服务。
- preferred channel 失败不会被放大成全局污染。
- continuation miss 不会因为错误的自动恢复导致上下文漂移。

### 当前方案牺牲了什么

- 不提供 strict distributed owner，也不承诺跨节点单 owner handoff。
- Redis/backend 异常时可能出现 false miss，导致 fresh route 或 local-only open 增加。
- 显式 pin 不会“顺便刷新”共享 affinity；这降低了操作便利性，但换来共享状态不被污染。
- continuation miss 需要客户端显式 replay，不能透明恢复。

### 为什么这是当前最佳点位

- 再往强一致方向走，必须引入 lease、fencing、跨节点 owner 协调和 replay 协议，复杂度明显上升。
- 当前系统的真实收益主要来自“更高命中率 + 更稳降级路径”，而不是全链路强一致。
- 因此当前点位是：在 routing truth、request scope 和 correctness boundary 上做强，在 shared hint 上保持克制。

## 当前实现落点

关键代码入口：

- 路由作用域：`common/groupctx/routing_group.go`
- 请求初始化：`middleware/distributor.go`
- affinity 配置：`common/config/channel_affinity.go`
- affinity 主链路：`relay/channel_affinity.go`
- affinity 存储：`runtime/channelaffinity/manager.go`
- request hints：`internal/requesthints/request_hints.go`
- Codex routing hint：`providers/codex/routing_hint.go`
- Codex realtime affinity：`providers/codex/realtime_session.go`
- continuation miss：`relay/responses.go`、`relay/main.go`

## 明确不采用的方案

- 不把 `token_group` 当作运行时真实路由作用域。
- 不把 janitor 生命周期做成热更新副作用。
- 不把 `prompt_cache_key` 的最终生成延后到 provider 选择之后。
- 不把 Redis binding 解释成 strict distributed owner。
- 不把单次 preferred channel 失败升级为全局 cooldown 或 quarantine。
- 不在无 replay 能力前提下对 `previous_response_id` 做自动恢复。

## 后续扩展边界

如果未来业务目标升级，可以在不破坏当前主线的前提下继续扩展：

- 为 shared binding 增加更强的 owner 协调协议。
- 为 continuation 提供显式 replay 协议后，再讨论自动恢复。
- 为 pin 增加单独的“重置 shared affinity”显式开关，而不是隐式副作用。
- 按业务域拆分更多 request hint resolver，但仍必须满足“选路前可见”的约束。
