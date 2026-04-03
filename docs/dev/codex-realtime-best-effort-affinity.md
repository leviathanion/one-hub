---
title: "Codex Realtime Best-Effort Affinity 方案"
layout: doc
outline: deep
lastUpdated: true
---

# Codex Realtime Best-Effort Affinity 方案

## 文档状态

- 状态：提案
- 范围：`Codex /v1/realtime` 的 execution session 续接、fresh 重建、binding 存储语义
- 目标：在不引入分布式 owner lease 的前提下，给出一版以可用性优先、实现复杂度受控的 `best-effort affinity` 方案

## 背景

当前 `Codex realtime` 的核心矛盾不是“能不能做成严格分布式 owner 协调”，而是：

1. 用户请求是否应该因为 Redis 抖动而直接不可用
2. stale 状态是否会反向污染新的 binding

如果产品真实承诺是：

- 会话保持只是优化
- 不追求多节点下无缝 handoff
- 一旦共享状态不确定，宁可 fresh reconnect
- 更看重当前请求可继续，而不是严格证明旧 key 已失效

那么相比 `lease + fencing` 或 strict reset，更匹配的是一版更轻的方案：

- binding 只是共享 resume hint
- Redis 不确定时，不续旧，直接 fresh
- fresh 不一定进入 shared binding
- 只要当前请求能继续，就接受一段时间的共享状态不一致

## 核心承诺

本方案真正保证的只有两件事：

1. stale 状态不能 blind delete / blind overwrite 新 binding
2. old `session_key` 一旦成功被废弃，就不能再被 resume 复用

本方案明确不保证：

1. binding 是分布式 owner 锁
2. 多节点下始终只有一个 active owner
3. `ForceFresh` 平滑切换
4. stale transport 会被立刻杀掉
5. Redis 不确定时还能严格证明旧 key 已经全局失效

## 设计边界

当前推荐实现只保留“收益明显且复杂度可控”的部分：

1. Redis-first 的共享 hint 读取
2. Redis CAS 写保护
3. `Resolve` / revocation 三态
4. 普通 open 的合法 CAS replace
5. publish 失败时退化为 local-only

当前不建议引入：

1. owner lease
2. fencing token
3. heartbeat 协调
4. strict reset 专用公开模式
5. 为了语义纯度而重做整条调用链

## 范式定位

这版设计不是经典 `cache-aside`。

更准确的表述是：

- Redis 是 `shared resume-hint store`
- 本地 `bindings/index` 是 `local near-cache`
- `ExecutionSession` 是本机 runtime 对象
- `best-effort fresh` 是一种 `availability-first rebuild` 语义

也就是说：

- Redis binding 只提供“下次尽量续哪个 session”的 hint
- 本地 runtime 决定当前节点是否还能直接 attach
- 当共享状态不确定时，系统允许起一个新的 local session 先服务当前请求
- 这个 fresh session 可能只是 local-only，而不是新的共享事实来源

## 状态模型

### Redis 共享状态

仅保留两类 Redis key：

1. `binding:<binding_key>`
   - 当前共享 `Binding`
   - 为下一次 open / re-attach 提供 resume hint

2. `revoked:<session_key>`
   - tombstone
   - 表示某个旧 key 已成功被标记为不可 resume

### 本地状态

1. `ExecutionSession`
   - 管理 transport、attachment、cleanup、capacity

2. `bindings/index`
   - 本地 near-cache
   - 只用于 attach 与 cleanup 加速

3. `local-only fresh session`
   - 当前节点上可服务请求的 fresh local session
   - 没有成功发布到 Redis
   - 不应被宣传为“别的节点也能 resume”

## 关键原则

### 原则 1：Redis 可读时，resume 以 Redis hint 为准

- `Resolve` 优先看 Redis
- 本地 near-cache 不得越权复活一个 Redis 已未知的 binding

### 原则 2：Redis 不确定时，不续旧，直接 fresh

如果 Redis 读异常：

- 不把错误当成 miss
- 不回退到旧本地 binding 继续 resume
- 直接创建 fresh local session
- 默认把它视为 local-only，除非后续共享写入明确成功

### 原则 3：所有 Redis binding 变更都必须是条件写

任何 stale local session 都不允许 blind 修改共享 hint：

- stale cleanup 不能 blind delete
- stale touch 不能 blind refresh
- stale open 不能 blind publish

### 原则 4：`ForceFresh` 在当前方案里按 best-effort fresh 理解

它的真实语义是：

- 尽量给当前请求一个 fresh session
- 尽量把新 session 发布为新的 shared hint
- 尽量把旧 key 标记为 revoked

但只要其中某一步无法确认：

- 当前请求仍可继续
- 语义立即降级为“新的 local session 正在服务”
- 不再宣称旧 key 已经被严格废弃

这不是在定义新的公开 API 模式，而是对当前 `ForceFresh` 实际收益边界的约束。

## Redis 原子操作

在 `runtime/session/redis.go` 中建议提供以下 CAS / Lua helper：

### 写路径结果模型

除 `Resolve` / revocation lookup 之外，所有 Redis 写路径 helper 也建议返回显式结果，而不是仅返回 `bool` 或吞掉错误：

```go
type BindingWriteStatus string

const (
    BindingWriteApplied           BindingWriteStatus = "applied"
    BindingWriteConditionMismatch BindingWriteStatus = "condition_mismatch"
    BindingWriteBackendError      BindingWriteStatus = "backend_error"
)
```

语义要求：

- `applied`
  - 共享状态已被确认写成目标状态
  - 或当前状态已经等价于目标状态，可视为幂等成功
- `condition_mismatch`
  - 共享状态可读，但不满足当前 CAS 前提
  - 调用方可以据此确认自己不是当前 winner
- `backend_error`
  - Redis 超时、连接失败、脚本执行失败、结果不确定
  - 调用方不得据此推断共享状态已经变更或未变更

这一步的目的不是追求严格全局一致，而是把“条件冲突”和“后端不确定”分开处理，避免把网络抖动误判成状态事实。

### `CreateBindingIfAbsent(binding, ttl)`

- 仅当 `binding_key` 不存在时创建
- 用于 `miss` 后的首次 publish
- 如果当前 binding 已经等价于 `binding`，可视为 `applied`

### `ReplaceBindingIfSessionMatches(bindingKey, expectedSessionKey, replacement, ttl)`

- 仅当当前 binding 的 `SessionKey == expectedSessionKey` 时替换
- 用于 compatibility 漂移后的合法迁移
- 如果当前 binding 已经等价于 `replacement`，可视为 `applied`

### `DeleteBindingIfSessionMatches(bindingKey, expectedSessionKey)`

- 仅当当前 binding 仍指向 `expectedSessionKey` 时删除
- 用于 stale cleanup
- 如果 binding 已经不存在，也可视为幂等 `applied`

### `TouchBindingIfSessionMatches(bindingKey, expectedSessionKey, ttl)`

- 仅当当前 binding 仍指向 `expectedSessionKey` 时 touch
- 防止 stale session blind refresh
- 不得把 Redis 写异常压扁成 `condition_mismatch`

### `DeleteBindingAndRevokeIfSessionMatches(bindingKey, expectedSessionKey, revokeTTL)`

- 仅当当前 binding 仍指向 `expectedSessionKey` 时：
  - 删除 binding
  - 写入 `revoked:<expectedSessionKey>`

注意：这一步在本方案中只是 best-effort 操作成功时的强化语义，不是当前请求继续的前置条件。

## Manager 语义

### `Resolve`

`Resolve` 建议使用三态：

```go
type ResolveStatus string

const (
    ResolveHit          ResolveStatus = "hit"
    ResolveMiss         ResolveStatus = "miss"
    ResolveBackendError ResolveStatus = "backend_error"
)
```

要求：

- `hit`：使用 Redis binding 作为共享 hint
- `miss`：允许 fresh，并尝试首次 publish
- `backend_error`：允许 fresh，但默认只作为 local-only session

### `RevocationStatus`

revocation 查询也建议使用三态：

```go
type RevocationStatus string

const (
    RevocationRevoked    RevocationStatus = "revoked"
    RevocationNotRevoked RevocationStatus = "not_revoked"
    RevocationUnknown    RevocationStatus = "unknown"
)
```

要求：

- `revoked`：旧 key 不得继续进入 resume 路径
- `not_revoked`：可继续按正常 resume 逻辑处理
- `unknown`：不继续 resume 旧 key，直接 fresh

### `TouchBinding`

- 本地校验通过后执行 `TouchBindingIfSessionMatches`
- 返回 `condition_mismatch` 时：
  - 清理本地 near-cache 中这条 binding
  - 认为本地 shared hint 已失效
- 返回 `backend_error` 时：
  - 不清理本地 live session
  - 不宣称共享 TTL 已成功刷新
  - 将该 session 保持为可服务状态，并标记为 `shared state uncertain`

这符合分布式系统中的常见最佳实践：客户端在超时或后端异常后，通常只能得到“结果未知”，而不能把未知直接收缩成“前置条件不成立”。

### `deleteBindingsForSessionLocked`

- 锁内只收集 `(bindingKey, sessionKey)`
- 锁外执行 `DeleteBindingIfSessionMatches`

### `upsertBindingLocked`

- 只更新本地 near-cache
- 共享 binding 的写入由 provider 显式调用 CAS helper 完成

## 普通 Open 流程

### 1. 解析 shared hint

provider open 时：

1. 读取 `bindingKey`
2. 查询 Redis binding
3. 按结果分支：

- `hit + compatible + not_revoked`
  - 把它视为 candidate `session_key`
- `hit + compatible + unknown_revocation`
  - 不继续 resume 旧 key
  - 按 fresh 处理
- `hit + incompatible`
  - 记录 `observedOldSessionKey`
  - 后续 fresh 成功后走 CAS replace
- `miss`
  - 按 fresh 处理
- `backend_error`
  - 按 fresh 处理
  - 不继续 resume 旧 key

### 2. attach 或 fresh

如果命中 candidate `session_key`：

- 只有本机确实持有对应 local session 时，才允许 attach 本机 existing session
- 或者上游协议明确支持跨节点 attach 同一个 `session_id`

如果不能安全 attach：

- 创建 fresh local session

### 3. publish 或 local-only 降级

fresh local session 打开成功后：

- 来源是 `miss`
  - 调用 `CreateBindingIfAbsent`
- 来源是 `hit + incompatible`
  - 调用 `ReplaceBindingIfSessionMatches(bindingKey, observedOldSessionKey, newBinding, ttl)`
- 来源是 `backend_error`
  - 不对 Redis 做基于猜测的 publish
  - 当前 session 直接作为 local-only
- 来源是 `hit + compatible but not attachable`
  - 不 blind replace 现有 binding
  - 当前 session 直接作为 local-only

如果 publish / replace 成功：

- 当前 fresh session 成为新的 shared hint

如果 publish / replace 失败：

- 默认保留当前 fresh local session
- 退化为 local-only
- 不再宣称它是新的共享 binding

## `ForceFresh` 语义

当前方案直接把现有 `ForceFresh` 字段解释为：

- 尽量 fresh
- 尽量丢掉旧 binding
- 但不把“旧 key 必然已失效”当作当前请求继续的前提

### 流程

1. 查询 shared hint store
2. 如果 `hit`：
   - 记录 `oldSessionKey`
   - best-effort 尝试 `DeleteBindingAndRevokeIfSessionMatches`
3. 如果 `miss`：
   - 说明当前没有共享 binding
   - 直接 fresh
4. 如果 `backend_error`：
   - 不尝试 resume 旧 key
   - 也不要求 reset 成功证明
   - 直接 fresh
5. 如果本机持有 `oldSessionKey` 对应 local session：
   - best-effort 清理本地 session
   - 释放本地 capacity
6. 创建 fresh local session
7. 尝试发布新 binding：
   - 旧 binding 已成功 delete+revoke：`CreateBindingIfAbsent(newBinding, ttl)`
   - 原本就是 `miss`：`CreateBindingIfAbsent(newBinding, ttl)`
   - 其他不确定情况：不发布，直接 local-only
8. 如果 publish 失败：
   - 保留当前 fresh local session
   - 退化为 local-only

### 结果语义

只要 fresh local session 创建成功，就允许当前请求继续。

如果以下条件全部成立：

- old binding 成功 delete
- old key 成功 revoke
- new binding 成功 publish

则可以进一步认为：

- 旧 key 已被成功废弃
- 新 key 已成为 shared hint

如果其中任一条件不成立，则本次 `ForceFresh` 只应被理解为：

- 当前节点已经切到一个 fresh local session
- shared binding 可能仍指向旧 session
- 旧 key 可能仍可被别的节点 resume

这就是本方案显式接受的 tradeoff。

## Revocation 语义

`revoked:<session_key>` 在本方案中只承担一件核心职责：

- 阻止“已成功废弃”的旧 key 再次进入 resume 路径

它不承担：

- 全局实时停掉 stale transport
- 在 Redis 不确定时替代 owner 协调

### 建议检查点

建议最少检查：

1. open attach 前
2. re-attach 前
3. 任何显式使用旧 `session_key` 恢复会话的入口

本方案不要求每个 send / recv 前都做 revocation 热路径检查。

原因很简单：

- 这版方案承认 dual-active window
- 它只阻止 future resume，不强求立刻停掉已经活着的 stale handle

## Redis 异常策略

### 读异常

Redis read backend error 时：

- 不按 miss 处理旧共享状态
- 不继续 resume 旧 key
- 直接创建 fresh local session
- 默认降级为 local-only

### 写异常

Redis write backend error 时：

- 当前 fresh local session 仍可继续服务
- 但不应被宣传为共享可 resumed binding
- 后续请求可能仍被旧 binding 或 miss 结果引导到其他 fresh 路径

## local-only 收敛策略

`local-only` 不应只是一个“临时说法”，而应是显式状态。

建议每个 fresh local session 在 runtime 中额外携带：

1. `Visibility`
   - `shared`
   - `local_only`
2. `PublishIntent`
   - `create_if_absent`
   - `replace_if_matches`
   - `none`
3. `ExpectedOldSessionKey`
   - 仅在合法 CAS replace 尚未完成时保留

### 为什么要补这一层

如果只有“当前请求先继续”，却没有后续收敛策略，那么一次短暂 Redis 写故障就可能把整个 session 生命周期永久降级成不可共享。

从分布式架构的最佳实践看，更稳妥的做法不是引入新的 owner 协调，而是：

- 明确把 `local-only` 视作一种降级后的可恢复状态
- 只在后续自然检查点 piggyback 做有限次补发布
- 一旦发现共享 binding 已被别的 session 合法占有，就停止争抢

### 建议的补发布检查点

不建议引入独立 heartbeat 或高频后台协调线程。

建议只在以下自然路径尝试收敛：

1. fresh session 创建成功后
2. 后续同节点 reopen / re-attach 成功后
3. turn finalize 后、session 仍保持 live 时
4. detach 前后、session 仍可继续复用时

### 补发布规则

当 session 当前为 `local_only` 且 Redis 已恢复可访问时：

1. 如果当前共享 binding 已经指向本 session
   - 直接把 `Visibility` 提升为 `shared`
2. 如果 `PublishIntent == create_if_absent` 且当前 binding 不存在
   - 调用 `CreateBindingIfAbsent`
   - 成功后提升为 `shared`
3. 如果 `PublishIntent == replace_if_matches` 且当前 binding 仍指向 `ExpectedOldSessionKey`
   - 调用 `ReplaceBindingIfSessionMatches`
   - 成功后提升为 `shared`
4. 如果当前 binding 指向其他 session
   - 停止当前 session 的补发布尝试
   - 保持 `local_only`，直到 session 自然结束
5. 如果补发布返回 `backend_error`
   - 保持 `local_only`
   - 等待下一个自然检查点重试

### 明确边界

即使存在补发布，`local-only` 也只保证：

- 当前请求在本节点可继续

它仍然不保证：

- 下一个请求一定被路由回本节点
- 在多节点下恢复成严格单 owner
- 任何时刻都能重新成为 shared hint

如果未来需要更强的跨节点续接保证，就不能只靠 shared hint store，还需要引入：

- node-aware routing / 更强的入口粘性
- owner lease
- fencing token
- 或者更严格的 handoff 协调

## TTL 与陈旧窗口

本方案不做 lease，也不做 heartbeat owner 续约。

需要接受的结果是：

1. binding TTL 到期后，resume 能力自然下降
2. winner 节点死掉后，shared binding 可能陈旧到 TTL 结束
3. compatible 但不可 attach 的旧 binding 也可能存活到 TTL 或下一次合法 replace
4. `best-effort fresh` 产生的 local-only session 不会自动成为新的 shared hint

这不是 bug，而是 availability-first 方案的代价。

## Trade-off 与最佳实践映射

这版方案接受的 trade-off，需要用分布式系统的常见实践来理解：

1. session affinity 本来就是 best-effort 路由优化，不是状态一致性协议
   - 云负载均衡和托管运行时普遍只承诺“尽量把后续请求送回相近实例”
   - 一旦扩缩容、健康状态、容量或网络条件变化，亲和关系就可能被打破

2. CAS 可以防止 blind overwrite，但不能把系统提升成严格 fencing
   - CAS 能保护共享 hint 不被 stale session 无条件覆盖
   - 但只要没有 fencing token，就不能严格阻止 dual-active window

3. 超时和 backend error 的本质是“结果未知”，不是“结果失败”
   - 因此方案需要区分 `condition_mismatch` 和 `backend_error`
   - 前者可据此修正本地认知，后者只能保守降级

4. TTL 永远是 safety 与 liveness 的折中
   - TTL 太短，容易把慢节点误判成失效 owner
   - TTL 太长，stale shared hint 会存活更久

5. `local-only` 是 availability-first 的代价，不是异常实现
   - 它提升的是“当前请求继续”的概率
   - 它不能单独提供“未来请求仍可跨节点续接”的保证

因此，本方案的正确定位应当是：

- 分布式 shared resume hint
- CAS-protected binding mutation
- uncertain 时 fresh 并降级为 local-only
- 通过有限补发布尽量收敛

而不是：

- 严格分布式 owner lock
- 可证明的全局单 active session
- 完整跨节点 seamless handoff

## Capacity 语义

本方案不做跨节点 capacity transfer。

这意味着：

- 本地 capacity 仍由本机 `ExecutionSession` 生命周期决定
- best-effort 清理本机旧 session 仍然有意义，因为它能释放当前节点资源
- 即使跨节点共享状态未收敛，本机也可以先为 fresh session 腾出容量

## 测试矩阵

至少补齐以下测试：

1. stale local cleanup 不会删除 replacement binding
2. stale `TouchBinding` 不会覆盖 winner binding
3. Redis read backend error 时不会回退到本地旧 binding resume
4. compatibility 漂移时普通 open 可以通过 CAS replace 合法迁移 binding
5. `backend_error` 下创建的 fresh session 会退化为 local-only
6. best-effort fresh 在 revoke / publish 失败时，当前请求仍可继续
7. revocation 成功后，旧 `session_key` 不能再进入 resume 路径
8. revocation lookup 返回 `unknown` 时，open 不会 fail-open
9. `TouchBinding` 返回 `backend_error` 时不会把本地 live session 误判成 `condition_mismatch`
10. `local-only` session 可以在后续自然检查点通过合法补发布提升为 `shared`
11. 共享 binding 已被其他 session 合法占有时，`local-only` session 会停止补发布尝试

## 实施顺序

建议分 5 步实施：

1. 先补 Redis CAS helper、写路径结果模型与 resolve / revocation 三态
2. 再改 manager 的 CAS delete / CAS touch 与 near-cache 语义
3. 再给 runtime session 补 `Visibility` / `PublishIntent` / `ExpectedOldSessionKey`
4. 再改普通 open 与 `ForceFresh`，区分 shared publish、local-only 降级和后续补发布
5. 最后补完整测试矩阵与观测指标

## 方案总结

这版方案的关键不是“证明 reset 一定成立”，而是：

- 不再在 Redis 不确定时续旧
- 尽快给当前请求一个 fresh session
- 用 CAS 防止 stale 状态写坏共享 binding
- 接受 fresh session 只是 local-only
- 只在 revoke 明确成功时才宣称旧 key 已真正废弃

这正是当前收益最高、同时又不过度设计的落点。

可以用一句话概括为：

`CAS-protected shared hint store + local near-cache + when uncertain, reconnect fresh and degrade to local-only`

## 参考资料

以下资料用于约束本方案的 trade-off 边界，而不是论证必须把系统升级成严格 owner 协调：

1. Google Cloud Load Balancing, Backend services overview
   - session affinity 明确是 `best-effort`
   - https://cloud.google.com/load-balancing/docs/backend-service
2. Google Cloud Run, Set session affinity for services
   - 即使启用 affinity，也不能假设客户端总会回到同一实例
   - https://cloud.google.com/run/docs/configuring/session-affinity
3. Google App Engine flexible, WebSockets and session affinity
   - 不应依赖 session affinity 来构建 stateful applications
   - https://cloud.google.com/appengine/docs/flexible/using-websockets-and-session-affinity
4. etcd API guarantees
   - KV 事务提供严格串行化；但客户端在超时/中断时可能处于结果未知状态
   - https://etcd.io/docs/v3.5/learning/api_guarantees/
5. etcd API
   - 事务支持 compare-and-swap 风格的条件更新
   - https://etcd.io/docs/v3.7/learning/api/
6. Redis distributed locks
   - 条件删除是必要的；如果关心 correctness，需要关注 fencing token
   - https://redis.io/docs/latest/develop/clients/patterns/distributed-locks/
7. Kubernetes client-go leader election
   - 官方文档明确说明不保证只有一个 leader 在 act，也就是不提供 fencing
   - https://pkg.go.dev/k8s.io/client-go/tools/leaderelection
8. Hazelcast FencedLock
   - 在异步网络中，锁服务本身无法绝对保证互斥，因此需要 fencing token
   - https://docs.hazelcast.com/hazelcast/5.0/data-structures/fencedlock
9. Hazelcast CP session configuration
   - session TTL 是 safety 与 liveness 的直接 trade-off
   - https://docs.hazelcast.com/hazelcast/5.0/cp-subsystem/configuration
