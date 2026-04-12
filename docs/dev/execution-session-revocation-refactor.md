---
title: "Execution Session Revocation 架构设计方案"
layout: doc
outline: deep
lastUpdated: true
---

# Execution Session Revocation 架构设计方案

## 文档状态

- 状态：正式方案，已选型并已按当前代码落地
- 目标：描述 `runtime/session` 在 revocation、binding、Sweep、容量回收和 Codex realtime 复用上的并发与一致性架构
- 文档口径：保留选型说明，但所有描述都以当前终态 contract 为准，不再记录实现前待确认事项
- 范围：`runtime/session/manager.go`、`runtime/session/redis.go`、Codex execution session manager 接线与 realtime open planning
- 非范围：strict global owner、fencing token、跨节点 single-owner handoff、完整 request-scoped context 传播

## 真实问题与设计目标

从第一性原理看，本题真正要解决的不是“单次 Redis `EXISTS` 有多慢”，而是：

1. 本地真相由 `m.mu` 保护，远端 revocation/binding 查询不应放大这把锁的临界区。
2. `Sweep`、容量回收和 binding 相关路径不能把远端 I/O 放大成 `N x RTT` 的锁持有。
3. manager 必须在 Redis 不可达、超时或状态切换窗口下，宁可 false miss，也不能返回 stale session、stale binding 或由旧观察得出的错误复用决策。

因此当前架构明确选择：

- 本地状态 correctness 优先；
- 远端 revocation/binding 作为 best-effort remote fact；
- 锁外 I/O + 回锁 latest-truth 重验；
- false miss 优先于 false hit；
- manager 自己拥有 revocation timeout，而不是把等待时间交给 `context.Background()` 或上层偶然控制。

## 已选型结论

### 1. `m.mu` 只保护本地 truth，revocation I/O 全部移到锁外

已选方案：

- `sessions`、`bindings`、`index`、`capacityIndex` 的读取、判定和变更都在 `m.mu` 下完成。
- `CheckRevocation(...)` 和 bulk revocation 查询一律在锁外执行。
- 回锁后必须重验本地对象是否还是同一个对象、是否仍然 live、是否仍匹配当前 owner。

选择原因：

- 本地 map/index 的一致性属于进程内真相，远端 I/O 不应决定本地锁持有时长。
- 否则 Redis 抖动会把整个 manager 的写锁等待放大到所有 session/binding 操作。

### 2. remote config 采用 manager 级 snapshot，不在普通路径上做热切换协议

已选方案：

- `Manager` 维护独立的 `remoteConfig`，包含 backend 和 `revocationTimeout`。
- 普通路径通过 `remoteConfigSnapshot()` 获取本轮使用的 remote config。
- Codex 全局 execution session manager 在初始化阶段通过 `InitExecutionSessionManager()` 注入 Redis client、prefix 和 revocation timeout。
- `ConfigureRedis(...)` 仅保留兼容入口；生产路径不依赖请求热路径反复 reconfigure。

选择原因：

- 需要稳定的是“本轮判断使用哪份 remote config”，而不是为运行期动态切换付出额外 snapshot 发布协议。
- 先固定配置生命周期，才能把并发复杂度集中在 revocation 锁外化和 latest-truth 重验上。

### 3. revocation timeout 由 manager 自己拥有

已选方案：

- `CheckRevocation(...)` 和 bulk `RevocationStatuses(...)` 都通过 manager-owned timeout helper 执行。
- 默认超时为 `200ms`，可由 `codex.execution_session_revocation_timeout_ms` 配置。
- timeout/backend error 收敛为 `RevocationUnknown`。

选择原因：

- 当前公开 API 大多不接受 `ctx`，因此本轮目标不是“全链路 context 化”，而是先给 revocation I/O 一个明确的等待上界。
- 这解决了最危险的无上界等待问题，同时不扩大公开 API 改造面。

### 4. `collect` 路径采用两阶段处理，并优先使用批量 revocation

已选方案：

1. 第一阶段在 `m.mu` 下收集候选并清理本地已过期对象。
2. 第二阶段在锁外通过 bulk revocation 查询远端状态。
3. 第三阶段回锁重验最新本地 truth，再决定是否删除。

`bulkRevocationBackend` 的 contract 已固定为：

- 输入一个 `[]sessionKey`
- 返回等长、同序的 `[]RevocationStatus`
- 任何 error 或长度不符都视为整批 `RevocationUnknown`

选择原因：

- `Sweep` 和容量路径最怕的不是单次慢，而是批量扫描时的成倍放大。
- “等长同序切片”比 `map` 或“缺项隐含语义”更短、更确定，也更不容易在错误路径上推理失真。

### 5. 回锁后一律按 latest truth 重验，不信任 unlock 前观察

已选方案：

- `AcquireExisting(...)`、`GetOrCreate(...)`、`GetOrCreateBound(...)`、`ResolveLocal(...)` 都遵循：
  1. 锁内拿到候选对象；
  2. 锁外做 revocation probe；
  3. 回锁后验证对象身份、owner 和活性是否仍匹配；
  4. 不匹配则返回 false miss、restart 或重新观察。
- 复杂路径允许有限次数 restart；最终阶段再走很窄的 latest-truth 重观测预算。

选择原因：

- 一旦在 unlock 窗口里发生 session/binding replacement，把旧 revocation 结论挪用给新 owner 就会直接破坏 correctness。
- 所以可以接受 miss，可以接受重试，但不能接受旧结论误中到新对象。

### 6. 语义上接受 false miss，不接受 stale hit

已选方案：

- `RevocationUnknown` 不会把对象直接判死，但也不能作为“安全可复用”的强证明。
- 观测窗口内若对象已换代，simple path 返回 false miss；bound path 则回到 restart/latest-truth 流程。
- Codex realtime 在 `unknown` 下选择“不 resume”。

选择原因：

- false miss 只会损失命中率或多开一个新 session。
- stale hit 会把错误 owner、错误 binding 或已撤销 session 继续复用，属于 correctness 退化。

### 7. `backend == nil` 与 `unknown` 语义分离

已选方案：

- `m == nil` 或 `backend == nil` 时，`CheckRevocation(...)` 返回 `RevocationNotRevoked`。
- 只有“已配置 backend，但本次查询超时/出错/批量失败”才返回 `RevocationUnknown`。

选择原因：

- 没有 backend 代表当前运行在 local-only / memory-only 语义，不等于“查询失败”。
- 如果把二者混在一起，会错误放大 local-only 模式的不确定性。

## 架构总览

当前 execution session manager 可以概括为：

`本地 truth under m.mu + manager-owned remote config + lock-outside revocation I/O + latest-truth revalidation + bounded restart + best-effort remote fact`

这套架构分四层：

1. 本地状态层：`sessions`、`bindings`、`index`、`capacityIndex`
2. 远端配置层：`backend` + `revocationTimeout`
3. 远端事实层：Redis binding + revocation key + CAS 写脚本
4. 协议消费层：Codex realtime open planning、promotion、force-fresh 和 janitor cleanup

## 数据结构与接口

### Manager 本地状态

`runtime/session.Manager` 当前维护：

- `sessions`
- `bindings`
- `index`
- `capacityIndex`
- `remoteConfig`

本地状态的职责：

- 维护进程内 session/binding 真相
- 控制容量与 caller 容量
- 为本地复用、删除、绑定修正提供 latest truth

### Remote config

`managerRemoteConfig` 当前包含：

- `backend`
- `revocationTimeout`

该配置通过 `remoteMu` 独立保护，和 `m.mu` 分离，避免把本地对象锁与远端配置生命周期耦合在一起。

### Backend contract

`bindingBackend` 当前提供：

- `ResolveBinding`
- `RevocationStatus`
- `CreateBindingIfAbsent`
- `ReplaceBindingIfSessionMatches`
- `DeleteBindingIfSessionMatches`
- `TouchBindingIfSessionMatches`
- `DeleteBindingAndRevokeIfSessionMatches`
- `CountBindings`

可选 bulk 能力由 `bulkRevocationBackend` 提供：

- `RevocationStatuses(ctx, []string) ([]RevocationStatus, error)`

### Redis 远端事实

Redis backend 当前保存两类键：

1. binding key
   - 保存 `Binding`
   - 用 Lua 脚本实现 create-if-absent / replace-if-match / delete-if-match / touch-if-match
2. revocation key
   - 保存“该 session key 已撤销”的远端事实
   - 由 `DeleteBindingAndRevokeIfSessionMatches(...)` 原子写入

这代表当前远端事实模型是：

- binding：共享 resume/owner hint
- revocation：废弃旧 session key 的否决事实

不是：

- strict owner lease
- epoch/fencing token

## 核心流程

### 1. 单 session 复用路径

适用入口：

- `AcquireExisting(...)`
- `GetOrCreate(...)`
- `AcquireOrCreate(...)`

统一流程：

1. 锁内观察本地 session 是否 live。
2. 如需远端确认，则在锁外做 revocation probe。
3. 回锁后检查：
   - 还是不是同一个指针
   - 本地是否已过期
   - revocation 是否为 `revoked`
4. 只有全部满足才允许复用。
5. 若需要 lease，则在 `m.mu` 下完成 `reserveLease()`，避免 sweep 在 lease 可见前删除该对象。

### 2. bound 复用与 owner 冲突路径

适用入口：

- `GetOrCreateBound(...)`
- `AcquireOrCreateBound(...)`

相比 simple path，这类路径还必须证明：

- 当前 binding owner 是否仍是同一个 session key
- 当前 session 是否仍与 binding 一致

因此 bound path 采用：

- 更严格的 owner 重验
- 有界 restart
- restart exhausted 后的 latest-truth 复观察

最终原则是：

- 可以返回 conflict
- 可以返回 miss
- 可以新建 fresh session
- 但不能把 replacement binding 的 owner 当成旧观察的合法延续

### 3. local binding 观测路径

适用入口：

- `ResolveLocal(...)`
- memory-only 模式下的 `ResolveBinding(...)` 本地分支

流程与 simple path 类似，但观察对象从 session 变成 binding：

1. 锁内拿到 live local binding。
2. 锁外检查该 binding 对应 session 的 revocation。
3. 回锁重验 binding owner 和 session 指针是否仍匹配。
4. 一旦 replacement 发生于 probe 窗口，收敛为 false miss，而不是退回新 owner。

### 4. Sweep 与容量回收

`Sweep(now)` 和 collect 路径当前采用两阶段结构：

1. 锁内：
   - 删除本地已过期 session
   - 收集仍需远端确认的候选
2. 锁外：
   - 批量执行 revocation 查询
3. 回锁：
   - 验证候选是否还是原对象
   - 按调用方传入的 `now` 重新判断本地过期
   - 对 `revoked` 或本地已过期对象做最终删除

重要语义：

- `Sweep(now)` 的最终本地过期重验继续使用调用方传入的 `now`，不退化成新的 wall-clock。
- 这样显式时间语义在 phase 3 仍然成立。

### 5. destructive cleanup 路径

`InvalidateBinding(...)` 的职责不是复用 live binding，而是显式失效：

1. 锁内清理本地 session/binding/index。
2. 解锁后执行 `DeleteBindingAndRevokeIfSessionMatches(...)`。
3. 最后做 cleanup 回调。

这里没有把它硬并入 simple path 的 restart/reobserve 控制流，因为它的核心职责是“销毁当前 owner”，不是“证明当前 owner 还能安全复用”。

## latest-truth 重验规则

### 1. 回锁后先信本地 truth，不信 unlock 前快照

所有锁外 revocation probe 都只提供“一个远端观察结果”，不能直接得出最终复用结论。

最终结论必须以回锁后的本地 truth 为准：

- 当前对象是否还存在
- 当前指针是否还是原对象
- 当前 binding owner 是否还匹配
- 当前 session 是否仍本地存活

### 2. `RevocationRevoked` 优先级高于本地 `TryLock` miss

本地 `TryLock` miss 只能说明“现在拿不到对象锁”，不能推翻已经得到的 `revoked` 结果。

因此当前 contract 是：

- `revoked` 一票否决
- `TryLock` miss 不会把已知 revoked 对象重新视为 live

### 3. restart 是有界的，不会无限放大

当前实现采用两层控制：

- 第一层：复杂路径允许有限次数 restart
- 第二层：restart exhausted 后只给 latest-truth 重观察一个很小预算

这样做的目的不是追求“永远成功复用”，而是：

- 尽量避开瞬时 replacement 抖动
- 但把最坏复杂度明确压住

## Revocation 状态语义

当前三态含义如下：

- `RevocationRevoked`
  - 已确认该 session key 被撤销
  - 不允许复用
- `RevocationNotRevoked`
  - 当前未观察到 revocation key
  - 仍需结合本地 latest truth 决定是否复用
- `RevocationUnknown`
  - 已配置 backend，但本次查询失败或超时
  - 不提供安全复用证明，只能触发保守降级

对 Codex realtime 的影响是：

- `unknown` 不 resume 旧 session
- 可能退到 fresh open 或 local-only 路径

## Codex 消费方式

Codex realtime 当前直接消费这套 contract。

### open planning

`planRealtimeOpen(...)` 当前语义：

- `ResolveMiss`：计划 `create_if_absent`
- `ResolveHit + compatible + not_revoked`：resume
- `ResolveHit + revoked`：计划 `replace_if_matches`
- `ResolveHit + incompatible`：计划 `replace_if_matches`
- `ResolveBackendError` 或 `RevocationUnknown`：不 resume，不宣称新 shared owner

### local-only promotion

新开 session 在共享状态不确定时可以先以 `VisibilityLocalOnly` 服务请求，随后由 `codexMaybePromoteExecutionSession(...)` piggyback promotion：

- 若发现 shared binding 已是自己，转为 `shared`
- 若 create-if-absent / replace-if-match 成功，转为 `shared`
- 若条件不匹配，停止 republish
- 若 backend error，继续保持 local-only

这让 Codex 在共享状态不确定时优先保住可服务性，而不是强依赖 Redis 必须实时可用。

## Trade-off

### 当前方案获得了什么

- 锁临界区不再被 revocation I/O 放大。
- `Sweep` 和容量路径不再在锁内逐个查 Redis。
- replacement 窗口下不会把旧 revocation 结论误用于新 owner。
- Codex 在 Redis 不稳定时仍可 local-only 继续服务。
- revocation 等待时间有 manager 自己控制的上界。

### 当前方案牺牲了什么

- 不提供跨节点 strict single-owner 保证。
- 在 `unknown` 或 replacement 高频抖动下，会出现更多 false miss 和 fresh open。
- 公开 API 仍未全量 context 化，上层请求取消不会直接打断所有 manager 路径。
- 远端 binding delete/touch 等写路径仍是解锁后的独立副作用，并未被收敛成单个固定 wall-clock。

### 为什么这是当前最佳点位

- 如果继续向强一致推进，就必须引入 owner epoch、fencing token、lease 协议和更复杂的分布式恢复语义。
- 当前系统的主要收益来自“保证不返回 stale result，同时在不确定时还能保住可服务性”。
- 因此最合适的点位是：进程内 correctness 做强，远端状态维持 best-effort，分布式 owner 保持克制。

## 当前实现落点

关键代码入口：

- manager 主实现：`runtime/session/manager.go`
- backend 接口：`runtime/session/backend.go`
- Redis backend：`runtime/session/redis.go`
- 状态与类型：`runtime/session/types.go`
- Codex 接线：`providers/codex/realtime_session.go`
- 配置默认值：`common/config/config.go`

关键配置项：

- `codex.execution_session_revocation_timeout_ms`

## 明确不采用的方案

- 不把 Redis binding 升级为 strict distributed owner。
- 不在本轮为全部公开 API 引入 request-scoped `ctx`。
- 不依赖 `context.Background()` 无限等待 revocation。
- 不把 bulk revocation 设计成“缺项有语义”的 `map` 接口。
- 不为了减少 false miss 而容忍 stale hit。
- 不在 request hot path 里靠反复 `ConfigureRedis(...)` 改写 manager 远端配置。

## 后续扩展边界

如果未来目标升级，可以在当前主线之上继续扩展：

- 为公开 API 引入显式 `ctx`，让上层取消传播到 manager。
- 为 binding read/write 增加更完整的模块级 timeout 体系。
- 如果业务要求 strict owner，再单独设计 owner epoch / fencing token。
- 在保持 current contract 的前提下继续优化 bulk collect、删除批量化和观测指标。
