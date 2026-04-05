---
title: "Channel Affinity 统一方案"
layout: doc
outline: deep
lastUpdated: true
---

# Channel Affinity 统一方案

## 文档状态

- 状态：已选型，按当前代码实现收敛
- 当前现状：本文描述的是当前线上代码采用的 affinity 组合范式，不是待定提案
- 范围：request 路由分组、responses affinity、Codex prompt-cache pre-routing、Codex realtime affinity、preferred channel fallback、responses continuation miss


## 当前代码采用的总方案

当前 one-hub 采用的是一套 availability-first 的 channel affinity 方案，核心决策如下：

1. 把声明分组和实际路由分组拆开，使用 request-scoped `routing_group`
2. channel affinity manager 在首次初始化时带完整 options 创建，`JanitorInterval` 只在构造期生效
3. responses 的稳定 routing hint 必须在 provider 选择前可见，Codex 的 `prompt_cache_key` 通过 `request_hint` 预派生
4. Codex realtime 采用 best-effort shared binding，而不是 strict distributed owner 协调
5. preferred channel same-channel open 失败后，只在当前请求内排除该 channel，再做 fresh reroute
6. `previous_response_id` 命中 continuation miss 时，外层清理 stale affinity 并返回显式错误，不做无 replay 能力的自动恢复

这六点不是互相独立的 patch，而是同一条 routing / affinity 主线的统一边界。

## 范式定位

当前代码采用的不是单一 pattern，而是一条明确的组合范式：

`availability-first channel affinity = explicit effective routing scope + construction-time lifecycle + routing-visible stable hint + CAS-protected shared hint store + request-scoped failure isolation + correctness-first recovery boundary`

对应到当前实现，就是六个局部最佳范式的拼接：

1. 用显式 `routing_group` 建模“当前真实生效的路由作用域”，而不是复用声明字段
2. 把 affinity manager janitor 视为构造期确定生命周期的后台 worker，而不是普通热更新配置
3. 把所有会影响 affinity 命中的稳定 key 提前暴露到 routing 阶段，而不是留到 provider 内部事后生成
4. 把 realtime affinity 建模成 `shared resume hint + local runtime ownership`，而不是 strict distributed owner lock
5. 把 preferred channel 失败后的影响范围限制在当前请求，而不是放大成跨请求全局事实
6. 把 continuation miss 当成 correctness 边界问题，而不是普通 retry / fallback 问题

这套范式的核心不是“每处都追求更强一致性”，而是：

- 哪些状态必须成为 routing truth，就显式前移
- 哪些状态只是共享 hint，就接受 best-effort 语义
- 哪些失败缺少语义等价恢复前提，就显式 fail-close

## 统一设计

### 1. 路由分组语义

当前设计明确区分两种分组：

- `token_group`
  - token 声明的原始主分组
- `routing_group`
  - 当前请求实际用于选路、affinity scope、quota / log / meta 的分组

初始化时：

- `middleware/distributor.go` 会建立第一版 `routing_group`
- token 有主分组时，`routing_group = token_group`
- 没有 token 主分组时，退到 `user_group`

fallback 到备用分组时：

- `relay/common.go` 只更新 `routing_group`
- `token_group` 保持声明值不变

因此当前语义是：

- affinity scope 读 `routing_group`
- quota / log / meta 也读 `routing_group`
- `token_group` 只保留“声明配置”语义

### 2. Affinity manager 生命周期

当前默认 manager 的生命周期边界已经确定：

- `ConfigureDefault(...)` 首次调用时，直接用完整 `ManagerOptions` 创建默认 manager
- 如果默认 manager 已存在，后续只更新 runtime-tunable 选项
- `JanitorInterval` 明确是 construction-time config

这意味着：

- janitor goroutine 的生命周期不通过 `UpdateOptions(...)` 热切换
- `UpdateOptions(...)` 只负责轻量参数
- `Stats().LocalEntries` 依赖首次构造时是否正确启动 janitor

### 3. Responses routing hint

当前方案要求所有会影响 channel affinity 的稳定 key，都必须在 provider 选择前可见。

对 Codex responses，当前实现是：

1. `relay/channel_affinity.go` 在 `prepareResponsesChannelAffinity(...)` 中先做 `requesthints.ResolveResponses(...)`
2. `providers/codex/routing_hint.go` 根据 `CodexRoutingHintSetting` 生成 `responses.prompt_cache_key`
3. affinity 引擎通过 generic `request_hint` source 参与 lookup
4. `providers/codex/promptcache.go` 在真正发上游请求时优先复用 relay 已派生的 key

因此当前采用的不是“provider 内部事后生成 prompt_cache_key”的方案，而是“routing-visible hint pre-derivation”。

### 4. Codex realtime affinity

当前 realtime 采用的是 best-effort affinity，而不是强 owner 协调。

共享状态：

- Redis binding 作为 shared resume hint
- revocation key 用于阻止已明确废弃的旧 session key 再次进入 resume 路径

本地状态：

- runtime session 是真实执行体
- 本地 binding/index 是 near-cache
- Redis 不确定时允许先创建 fresh local session

关键语义：

- Redis `hit + compatible + not_revoked` 时，优先尝试 resume
- Redis `miss` 时 fresh，并尝试 publish
- Redis `backend_error` 时不继续 resume 旧 key，直接 fresh，默认降级为 local-only
- publish / replace 失败时保留当前 local session，但不宣称其成为新的 shared binding

当前代码已经实现了：

- `ResolveStatus`
- `RevocationStatus`
- `BindingWriteStatus`
- CAS 风格的 binding create / replace / delete / touch / revoke
- local-only session 的 piggyback promotion

当前代码明确没有实现：

- owner lease
- fencing token
- strict global handoff

### 4.1 显式 pin / manual override 不是 shared affinity

本文前文讨论的 shared affinity，前提都是“这是可复用的稳定 routing hint”。

`specific_channel_id` 这类显式 pin 不属于这类 hint，它是 request-local routing override。

因此必须补一条硬约束：

- 显式 pin 存在时，要同时跳过 shared affinity lookup 和 shared affinity writeback
- `record_on_success` 不能因为一次 pinned request 成功，就把该 channel 回写成后续普通请求的共享 affinity
- 这条约束同时适用于 responses 的 `prompt_cache_key` / `previous_response_id` 派生 binding，以及 realtime session binding
- 可以记录 request log / meta，说明本次请求是被 pin 到哪个 channel，但不能把这个 override 提升为 shared fact

原因很简单：

- 显式 pin 只表达“这次请求就走这里”
- 它不表达“后续未 pin 的请求也应该优先走这里”
- 否则一个临时调试、回放、强制同渠道续接请求，就会污染跨请求共享路由结果

如果产品以后真的需要“pin success 顺便重置 shared affinity”，那必须是单独的显式开关或 API，而不能是普通 pin 的隐式副作用。

### 5. Preferred channel fallback

当前采用的不是全局 quarantine，而是请求级 exclusion。

当 realtime affinity 命中 preferred channel 后：

1. 先在同 channel 上做 open
2. same-channel open 失败且当前不是 strict affinity 时
3. 把失败的 `preferredChannelID` 写入当前请求的 `skip_channel_ids`
4. 后续 fresh reroute 复用现有选路逻辑跳过这个 channel

这样做的边界很清楚：

- 只影响当前请求
- 不写 Redis
- 不做 channel 级 cooldown
- 不把单次失败放大成全局共享状态判断

### 6. Responses continuation miss

`previous_response_id` 不是普通 affinity hint，而是 continuation anchor。

当前采用的处理方式是：

1. `relay/responses.go` 只负责识别 `previous_response_not_found`
2. `relay/main.go` 在外层统一处理 continuation miss
3. 清理 stale affinity
4. 记录 recovery-candidate meta
5. 返回显式错误给客户端

当前明确不做：

- 在 `send()` 内部 reroute
- 自动清空 `previous_response_id` 再重发
- 在无 replay 能力前提下做语义漂移的自动恢复

## 为什么当前代码采用这套组合

从第一性原理看，one-hub 在这条链路上要解决的真实问题，不是“做出一套形式上最强的一致性协议”，而是：

1. affinity 必须真正提升选路命中率、prompt cache 命中率与 session 复用率
2. fallback、provider 差异和 Redis 抖动出现时，系统仍要保留可继续服务的退化路径
3. 在缺少 replay、lease、fencing、node-aware ingress 等基础设施时，不能伪装成自己具备更强保证

因此当前代码选择的不是“把所有状态都做强”，而是“只把必须成为 truth 的部分做强，把其余部分限制在 hint 或 request scope 内”。

具体原因如下：

### 1. `routing_group` 必须绑定真实选路结果

如果 fallback 后请求已经落到备用组，但 affinity、quota、日志还继续读取旧 `token_group`，系统内部就会同时存在两套“当前组”定义。

这不是实现细节问题，而是 scope truth 没有被显式建模。

所以当前代码采用：

- `token_group` 保留声明语义
- `routing_group` 承担真实 routing / affinity / quota scope

这对应的最佳范式是：

- `explicit effective scope`

### 2. janitor 生命周期应在构造期确定

janitor goroutine 本质上是后台 worker，不是普通配置字段。把它藏进 `UpdateOptions(...)` 会让“是否启动清理器”变成一个隐式副作用。

所以当前代码采用：

- 首次构造默认 manager 时就带完整 options
- `JanitorInterval` 只在构造期生效
- runtime update 只更新轻量参数

这对应的最佳范式是：

- `construction-time lifecycle for singleton background worker`

### 3. 稳定 routing hint 必须在 provider 选择前可见

`prompt_cache_key` 会影响 prompt cache route 命中。如果它只在 provider 内部事后生成，那么“记录 affinity”与“命中 affinity”就会发生在不同时间层。

所以当前代码采用：

- request normalization 阶段先做 hint resolve
- relay 与 provider 复用同一派生结果
- provider 只负责消费最终 key，而不再独占决定 key

这对应的最佳范式是：

- `routing-visible stable identity`

### 4. realtime affinity 的真实角色是共享 hint，不是 owner 锁

如果把 Redis binding 当成 strict owner，系统就必须进一步解决 lease、fencing、heartbeat、node-aware handoff 等问题。当前代码和产品承诺都没有走到那一步。

而当前真正需要保证的只有：

- stale 状态不能 blind overwrite / blind delete 新 binding
- Redis 不确定时不要继续 resume 旧 key
- 当前请求尽量还能继续

所以当前代码采用：

- `shared resume hint store`
- `CAS-protected binding mutation`
- uncertain 时 fresh，并允许降级为 `local-only`

这对应的最佳范式是：

- `availability-first shared hint store + local near-cache`

### 5. preferred channel 失败不应立刻升级为全局判断

单次请求在 preferred channel 上 open 失败，只能说明“这次没成功”，不能推出“全局 binding 一定 stale”或“这个 channel 对所有请求都应 cooldown”。

所以当前代码采用：

- 在当前请求内排除失败的 preferred channel
- 后续 fresh reroute 走既有负载均衡
- 只在新的成功结果出现后再改写 affinity

这对应的最佳范式是：

- `bounded fallback with request-scoped failure isolation`

### 6. continuation miss 在无 replay 能力前提下必须保守处理

`previous_response_id` 是 continuation anchor，不是普通 retry hint。没有 canonical transcript / compaction replay 能力时，自动清空它再重发，本质上是在做语义漂移的自动恢复。

所以当前代码采用：

- 外层 orchestration 统一识别 continuation miss
- 清理 stale affinity
- 返回显式错误，而不是在 `send()` 内偷偷 reroute

这对应的最佳范式是：

- `correctness-first recovery boundary`

综合起来，当前组合成立的原因很简单：

- 真实生效 scope 显式化
- 影响路由的稳定 identity 前移
- 共享状态只承担 hint 职责
- 单次失败不升级成全局事实
- 缺少等价恢复前提时明确报错

这正是当前代码复杂度、收益和真实承诺之间最稳的平衡点。

## Trade-Off 与最佳范式映射

| 决策 | 对应最佳范式 | 收益 | 代价 | 当前为何接受 |
| --- | --- | --- | --- | --- |
| 拆分 `token_group` / `routing_group` | `explicit effective scope` | affinity、quota、日志与真实选路一致 | 多一个 request-scoped 字段与 helper | 这是修复 fallback 语义错位的最小正确建模 |
| `JanitorInterval` 只在构造期生效 | `construction-time lifecycle` | 生命周期清晰，不在热路径里隐式启停 goroutine | 热更间隔需要整实例替换 | janitor 是 worker，不值得为它引入复杂热切换状态机 |
| `prompt_cache_key` 预派生 | `routing-visible stable identity` | 第二次请求开始即可在 provider 选择前命中 affinity | relay 需要看见一小部分 provider-local strategy | 只上移稳定 key 派生，比 speculative provider selection 成本低得多 |
| 显式 pin 视为 request-local override | `explicit effective scope + non-promotable override` | 避免临时 pin 污染后续未 pin 请求的 shared affinity | pinned success 不能顺带“预热”共享 affinity | 单次显式 override 不能被提升成跨请求共享事实 |
| realtime 采用 best-effort shared binding | `CAS-protected shared hint store + local near-cache` | Redis 抖动时当前请求仍可继续，且 stale 状态不易写坏共享 binding | 接受 dual-active window、`local-only`、非严格 handoff | 产品真实承诺是 session reuse 优化，不是分布式 owner 协议 |
| preferred channel 失败后只做请求级 exclusion | `bounded fallback / failure isolation` | 避免同一请求重复打到同一失败 channel | 不能顺带提供跨请求 quarantine | 单次失败不足以推导全局健康结论 |
| continuation miss 显式报错 | `correctness-first recovery boundary` | 避免隐藏 reroute、usage/quota 漂移与语义错位 | 用户体验更保守，客户端可能要带完整上下文重发 | 在无 replay 能力前，保守失败比伪恢复更正确 |

## 当前实现落点

- `common/groupctx/routing_group.go`
- `middleware/distributor.go`
- `runtime/channelaffinity/default_manager.go`
- `runtime/channelaffinity/manager.go`
- `internal/requesthints/request_hints.go`
- `providers/codex/routing_hint.go`
- `providers/codex/promptcache.go`
- `runtime/session/types.go`
- `runtime/session/redis.go`
- `runtime/session/manager.go`
- `providers/codex/realtime_session.go`
- `relay/realtime.go`
- `relay/responses.go`
- `relay/main.go`

## 明确不采用的方案

为了避免后续文档再次分叉，下面这些方向明确不是当前方案：

1. 不再让 `token_group` 同时承担声明分组和当前生效分组
2. 不在 `UpdateOptions(...)` 里热切换 janitor 生命周期
3. 不把 Codex 自动 `prompt_cache_key` 继续留在 provider 内部事后生成
4. 不把 realtime affinity 升级成 strict distributed owner 协调
5. 不对 preferred channel 做跨请求全局 quarantine
6. 不在 `send()` 内部偷偷处理 `previous_response_id` 恢复

## 后续扩展边界

如果以后要继续增强，这几件事必须单独设计，而不是继续往当前方案里硬塞：

1. replay-capable 的 `previous_response_id` 自动恢复
2. 更强的 node-aware routing / handoff
3. lease / fencing token 级别的 realtime ownership
4. 跨请求的 channel quarantine / cooldown 语义

## 参考资料

这些资料用于约束当前方案的边界与 trade-off，不意味着 one-hub 需要把自己实现成其中任一系统的完整版。

### 外部资料

#### 路由分组与 sticky scope

- HAProxy Session persistence
  - https://www.haproxy.com/documentation/haproxy-configuration-tutorials/proxying-essentials/session-persistence/
- HAProxy Sticky Sessions
  - https://www.haproxy.com/blog/enable-sticky-sessions-in-haproxy

#### 生命周期与后台 worker

- ABP Background Workers
  - https://abp.io/docs/10.0/framework/infrastructure/background-workers
- `patrickmn/go-cache` package docs
  - https://pkg.go.dev/github.com/patrickmn/go-cache

#### Prompt cache 与 conversation state

- OpenAI Prompt Caching
  - https://developers.openai.com/api/docs/guides/prompt-caching
- OpenAI Conversation State
  - https://developers.openai.com/api/docs/guides/conversation-state
- OpenAI Conversation State (Responses)
  - https://platform.openai.com/docs/guides/conversation-state?api-mode=responses
- OpenAI Compaction guide
  - https://developers.openai.com/api/docs/guides/compaction

#### Realtime shared hint / distributed systems

- Google Cloud Load Balancing, Backend services overview
  - https://cloud.google.com/load-balancing/docs/backend-service
- Google Cloud Run, Session Affinity
  - https://cloud.google.com/run/docs/configuring/session-affinity
- Google App Engine flexible, WebSockets and session affinity
  - https://cloud.google.com/appengine/docs/flexible/using-websockets-and-session-affinity
- etcd API guarantees
  - https://etcd.io/docs/v3.5/learning/api_guarantees/
- etcd API
  - https://etcd.io/docs/v3.7/learning/api/
- Redis distributed locks
  - https://redis.io/docs/latest/develop/clients/patterns/distributed-locks/
- Kubernetes client-go leader election
  - https://pkg.go.dev/k8s.io/client-go/tools/leaderelection
- Hazelcast FencedLock
  - https://docs.hazelcast.com/hazelcast/5.0/data-structures/fencedlock
- Hazelcast CP session configuration
  - https://docs.hazelcast.com/hazelcast/5.0/cp-subsystem/configuration

#### Fallback / retry / recovery 语义

- AWS Builders Library: Avoiding fallback in distributed systems
  - https://aws.amazon.com/builders-library/avoiding-fallback-in-distributed-systems/
- AWS Builders Library: Making retries safe with idempotent APIs
  - https://aws.amazon.com/builders-library/making-retries-safe-with-idempotent-apis/
- AWS Builders Library: Timeouts, retries, and backoff with jitter
  - https://aws.amazon.com/builders-library/timeouts-retries-and-backoff-with-jitter/
- Kubernetes Session Affinity
  - https://kubernetes.io/docs/reference/networking/virtual-ips/#session-affinity
- YARP Session Affinity
  - https://learn.microsoft.com/aspnet/core/fundamentals/servers/yarp/session-affinity

### 相邻实现参考

- `../sub2api/backend/internal/repository/gateway_cache.go`
- `../sub2api/backend/internal/service/request_metadata.go`
- `../sub2api/backend/internal/service/openai_gateway_service.go`
- `../sub2api/backend/internal/service/openai_gateway_service_test.go`
- `../sub2api/backend/internal/service/openai_ws_forwarder.go`
- `../sub2api/backend/internal/service/openai_ws_protocol_forward_test.go`
- `../new-api/service/channel_affinity.go`
- `../new-api/setting/operation_setting/channel_affinity_setting.go`
- `../new-api/relay/common/override_test.go`
- `../CLIProxyAPI/sdk/cliproxy/usage/manager.go`
- `../CLIProxyAPI/sdk/api/handlers/openai/openai_responses_websocket.go`
- `../CLIProxyAPI/sdk/api/handlers/openai/openai_responses_websocket_test.go`
