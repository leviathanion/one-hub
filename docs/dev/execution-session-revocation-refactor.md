---
title: "Execution Session Revocation 并发修复方案"
layout: doc
outline: deep
lastUpdated: true
---

# Execution Session Revocation 并发修复方案

## 文档状态

- 状态：已实现
- 当前现状：`runtime/session` revocation 检查已移出 `m.mu` 临界区，`Sweep` / 容量回收已走批量 revocation 检查；Codex execution session manager 也已在启动阶段注入 remote config，并接入 `codex.execution_session_revocation_timeout_ms`
- 设计形式：本文描述单次完整重构的终态 contract；编码时允许按依赖顺序推进，但文档不把任何中间形态视为独立方案或独立语义目标
- 范围：`runtime/session` 中 session 过期判定、revocation 检查、`Sweep`、容量驱逐、bound/binding 相关重验
- 非范围：owner lease、fencing token、strict global handoff、跨节点强一致 owner 协议
- 非范围：仅靠 revocation timeout 就收敛 realtime open 全路径 deadline；`ResolveBinding(...)` / binding CAS 写路径的 context 化属于相邻议题，不由本方案单独解决
- 非范围：仅通过本次改造就把 `Sweep` / 容量路径的全部远端副作用收敛到固定 wall-clock；`applyPendingBindingDeletes(...)` 仍然是解锁后的独立远端写路径，是否再做批量化属于相邻优化
- 非范围：caller-scoped collect。caller capacity 在本方案内继续复用同一套 global two-phase collect；后者已经满足正确性闭包，前者只影响扫描范围优化

## 已实现结果

- `Sweep(now)` 的 phase 3 重新使用调用方传入的 `now`，不再把显式时间语义退化成 wall-clock 相关行为
- plain `GetOrCreate(...)` / `AcquireOrCreate(...)` 在复用已有 session 时，把 lease 记账收回到 `m.mu` 保护下，关闭与本地过期 sweep 的 stale-pointer 窗口
- `ResolveLocal(...)` 与 memory-only `ResolveBinding(...)` 本地分支在 binding replacement 发生于 revocation probe 窗口时，收敛为 false miss，而不是回退到未重验的新 binding owner
- `GetOrCreateBound(...)` / `AcquireOrCreateBound(...)` 在 restart exhausted 后，会继续对 replacement binding / session 做 latest-truth revocation 重验，而不是直接把 fallback binding 当作 live conflict
- Codex 全局 execution session manager 现在显式读取 `codex.execution_session_revocation_timeout_ms`，避免生产环境被硬编码在默认 `200ms`

## 当前 trade-off

- 本地 manager 状态优先保证“不得返回 stale result”这一级的进程内 correctness：`m.mu` 保护的 `sessions` / `bindings` / index 不因旧观察、被动 cleanup 或锁外 revocation 重验返回 stale session / stale binding / stale 决策；它不承诺显式 `Delete(...)` / `DeleteIf(...)` 或跨节点 owner 切换的 fencing 级保证
- Redis revocation 继续是 best-effort remote fact。超时或 backend error 收敛为 `RevocationUnknown`
- 对 Codex realtime 而言，`unknown` 仍选择“不 resume”。这牺牲一部分高 RTT 场景下的命中率，换来不在不确定状态下继续复用旧 session
- simple path 明确接受 false miss，但不接受“把旧 revocation 结论转移给 replacement owner”这种 correctness 退化
- 本方案没有引入跨节点 owner fencing / strict global handoff；那会进一步提高分布式正确性，但会显著增加协议复杂度、存储写放大和恢复成本，不是当前最佳点位

## 问题定义

当前 `runtime/session/manager.go` 的 `sessionExpiredLocked(...)` 会在持有 `m.mu` 写锁时调用 `CheckRevocation(...)`，而 `CheckRevocation(...)` 最终会落到 Redis `EXISTS`。

这导致本地写锁保护的对象和远端 I/O 被混在同一个临界区内：

- `m.mu` 负责保护本地 `sessions`、`bindings`、`index`、`capacityIndex`
- Redis revocation 查询属于远端 best-effort 状态确认，不应该决定本地锁持有时长

从第一性原理看，真实问题不是“某次 `EXISTS` 平均只多花几百微秒”，而是：

1. 本地一致性锁被远端网络时延放大
2. Redis 抖动时，manager 的所有 session 操作都会被同一把写锁阻塞
3. `Sweep` / 容量驱逐路径会把单次网络延迟放大成 `N x RTT`
4. 当前 revocation 查询使用 `context.Background()`，manager 没有自己的操作级 deadline；即使把 I/O 移到锁外，当前 goroutine 的最坏等待时间仍然不由 manager 自己控制

同时要明确一个边界：

- 这次文档解决的是“revocation I/O 不应放大 `m.mu` 临界区”和“revocation 自己的等待时间要可控”
- 它不是“realtime open 全路径远端 I/O 都已经有 deadline”的同义表达
- 以 Codex realtime open planning 为例，`ResolveBinding(...)` 仍然是独立的远端读路径；只给 revocation 加 timeout，不会自动让整个 planning latency 受控

## 当前受影响路径

### 单 session / live binding 读路径

这些路径每次通常只触发 1 次 revocation 查询，但一旦 Redis 超时，仍会把整个 manager 卡住：

- `AcquireExisting(...)`
- `GetOrCreate(...)` / `AcquireOrCreate(...)`
- `GetOrCreateBound(...)` / `AcquireOrCreateBound(...)`
- `ResolveLocal(...)`
- memory-only 模式下的 `ResolveBinding(...)` 本地 binding 分支

### destructive cleanup 路径

`InvalidateBinding(...)` 当前会复用本地 binding 存活性检查，因此会间接受到锁内 revocation 的影响；但它自身不是“为了读取活性而额外做 1 次 revocation 查询”的路径。

重构后它应保持：

- 锁内完成本地 session / binding / index 清理，或清理 orphan residue
- 解锁后再执行远端 `DeleteBindingAndRevokeIfSessionMatches(...)`
- 不被机械并入 live binding 读路径的 unlock/relock 重验控制流

### 批量放大路径

这些路径会通过 `collectExpiredSessionsLocked(...)` 扫描全部 session；如果仍在锁内逐个查 Redis，会把锁持有时间放大成 `N x RTT`：

- `Sweep(...)`
- `ensureCapacityLocked(...)`
- `ensureCallerCapacityLocked(...)`
- janitor 后台回收

## 设计目标

本次修复必须同时满足以下目标：

1. `m.mu` 临界区内只做纯本地状态读取、判定和 map/index 变更
2. 所有 revocation I/O 都移到锁外
3. remote config 的发布边界必须明确；普通请求路径不在 unlock 窗口内承受运行期配置切换
4. 保持现有语义
5. 回锁后显式重建不变量，不能信任 unlock 前捕获的旧状态
6. `collect` 路径优先做批量化，避免 `N x RTT`
7. revocation 查询必须带 manager 自己控制的操作级超时，超时后收敛为 `RevocationUnknown`

这里“保持现有语义”具体指：

- `RevocationRevoked` 优先，视为应清理/不应复用
- `RevocationUnknown` 不驱逐本地 session
- 不引入 `not_revoked` 的负缓存

### 一致性模型与 CAP 边界

本轮目标不是把 execution session manager 升级成跨节点强一致 owner 协议，而是把一致性边界明确收缩为：

- 进程内 correctness 的 contract 是：manager 不得因旧观察、被动 cleanup 或锁外 revocation 重验而返回 stale result
- 这里的 stale result 包括 stale session、stale binding，以及由旧快照导出的 stale reuse / conflict / create / capacity 决策
- 这条 contract 依赖 `m.mu` 保护的本地 truth 与回锁重验成立；它不承诺对显式 `Delete(...)` / `DeleteIf(...)` 提供 epoch/fencing 级保证
- Redis revocation / binding 是带 manager-owned timeout 的 best-effort remote fact，只参与“当前是否还能安全复用”的判断；`unknown` 只允许触发保守降级，不允许把旧对象当成已验证 live owner
- 跨节点 ownership 继续只接受 eventual consistency，不承诺 strict single-owner handoff；如果未来业务目标升级为 strict owner，必须单独设计 owner epoch / fencing token
- CAP 取舍上，本方案优先 A + 分区下安全降级：Redis 抖动或超时时，允许 false miss、允许 `unknown => 不 resume`，但不允许 false hit，也不允许因为旧观察返回 stale result

## 方案借鉴与边界

本方案借鉴三类实现模式，但只借边界，不复制具体实现：

- remote config 生命周期：默认初始化后基本不可变，变更收敛到 manager 初始化或显式 reload 边界；只有明确需要运行期热切换时，才补 snapshot/原子发布
- sweep / cleanup 控制流：锁内只收集 candidate 或提交本地删除，锁外做远端探测或关闭，回锁后按最新本地 truth 重验
- timeout helper：第一阶段先由 manager 自己叠加模块级 timeout，不默认透传 parent `ctx`

同时明确不采用三类看起来“通用”、但不适合本问题的做法：

- 不把 revocation/backend 配置读取做成 TTL cache + singleflight；这里需要的是稳定生命周期边界，而不是允许短暂陈旧的设置缓存
- 不把 session capacity 清理改成增量限额扫描；容量路径需要的是正确性闭包，而不是“这轮先扫一点”
- 不把 timeout ownership 藏进全局 Redis helper 或 `context.Background()` wrapper；manager 必须显式拥有这一步的等待时间控制权

## 实现前需要补齐的 contract

这份方案的主方向已经成立，但在真正进入编码前，还必须先锁定几条 contract；否则实现阶段很容易出现“代码已经拆开了，但语义仍然不一致”的情况。

### 1. remote config 的生命周期必须先固定，默认不为运行期可变性付费

当前 `ConfigureRedis(...)` 允许在运行期替换 backend，而 Codex realtime 的请求路径和 stats 路径都会重复调用它。

从第一性原理看，本轮真正必须保证的是：

- request / stats hot path 不再以“读请求顺手 reconfigure manager”的方式影响并发语义
- 普通 manager 公开操作面对的是一份稳定的 remote config，而不是可在请求中途切换的全局输入
- 锁外 backend 路径不再依赖“谁最后一次调用了 `ConfigureRedis(...)`”这种隐式共享状态

当前 Codex realtime 已存在下面这些多调用链路：

- `ResolveBinding(...) -> CheckRevocation(...)`
- `ResolveBinding(...) -> CreateBindingIfAbsent(...) / ReplaceBindingIfSessionMatches(...)`
- `ResolveBinding(...) -> DeleteBindingAndRevokeIfSessionMatches(...)`

如果这些调用之间仍允许 request hot path 或 stats read path 触发 `ConfigureRedis(...)` / timeout 热更新，那么无论 manager 内部是否做 per-attempt snapshot，都还会在更上层留下“同一轮 open / publish 决策混用了两份配置”的可能。

所以文档必须先固定：

- `backend`、`redisPrefix`、revocation timeout，以及后续可能加入的远端拨盘，应被视为同一份 remote config
- 默认路径是把 `ConfigureRedis(...)` 从 Codex request / stats hot path 移走：remote config 在 manager 初始化时注入；如果确实需要更新，只允许发生在显式 reload 边界
- 对 Codex 生产路径，`InitExecutionSessionManager()` 必须一次性把 Redis client、prefix 与 revocation timeout 注入全局 execution session manager；当前可通过 `ConfigureRemote(...)` 完成
- `ConfigureRedis(...)` 仅作为兼容接口保留，不再作为生产路径的完整配置入口；生产路径也不应把 channel `Other` JSON 字段当成 revocation timeout 的配置来源
- 显式 reload 边界优先采用粗粒度发布方式，例如新建一个 manager 再在上层切换引用；本轮不把 provider/request-scoped snapshot 或运行中原子热切换当作默认交付成本
- 在这一前提下，普通 manager 公开操作只需假定“本轮观察期内配置不变”，而不必额外为运行期可变性引入 per-attempt snapshot 机制
- 回锁后允许重建本地 map/index 不变量，但不要求在同一轮 attempt 中途重新读取另一份 backend / timeout
- `m.mu` 继续只保护本地 `sessions` / `bindings` / `index` / `capacityIndex`
- remote config 的生命周期应与本地对象锁分离；如果 `redisClient`、`redisPrefix` 仍以字段形式保留，它们也只能在构造期或显式 reload 边界更新
- 这条约束不能只覆盖 revocation：manager 内所有锁外 backend 路径，都应读取同一份生命周期明确的 remote config，而不是在热路径上依赖可变全局状态
- 当前至少包括：`CheckRevocation(...)`、`ResolveBinding(...)` 的 remote backend 分支、binding write wrappers，以及 `Stats()` 这类锁外 backend 观测路径
- 只有当产品明确要求“运行期 backend / timeout 热更新要对 in-flight realtime open 立即可见”时，才需要额外补 snapshot/原子发布或 provider/request-scoped 组合 API；这属于后续独立设计，而不是本方案默认复杂度

这里的 trade-off 要提前写明：

- 获得：先收敛 `ConfigureRedis(...)` 生命周期，能直接消掉大量“为偶发现状付出的并发复杂度税”
- 获得：锁边界与配置边界分离，当前 patch 只需要解决 revocation 锁外化和回锁重验，不必同时实现一套运行期配置发布协议
- 获得：Codex 多段 manager 调用链天然落在同一份配置之下，不再要求每个 manager 方法都单独发布 snapshot
- 牺牲：运行中的 request / stats 调用不会立即看到 backend / timeout 变更；变更生效点被推迟到显式 reload 边界
- 牺牲：如果未来必须支持 in-flight request 观察热切换，仍要补 manager swap、atomic snapshot 或 provider/request-scoped snapshot 等额外复杂度
- 牺牲：运维层面要接受更粗粒度的 reload 语义，而不是继续依赖热路径随手改配置

只有先把“远端配置的并发可见性”固定下来，后面的 revocation 锁外化才不会把隐藏 race / TOCTOU 问题放大。

### 2. bulk revocation 的 contract 收缩为“按输入顺序返回切片”

`collect` 路径需要 batch revocation，但 manager 不能把“结果里缺一项”误解成 `not_revoked`。

因此 bulk 接口的 contract 应直接收缩成和输入一一对应的结果切片，例如：

```go
type bulkRevocationBackend interface {
    RevocationStatuses(ctx context.Context, sessionKeys []string) ([]RevocationStatus, error)
}
```

并且在文档里固定解释规则：

- 返回切片长度必须等于输入 `sessionKeys` 长度
- 返回顺序必须与输入顺序一致
- 重复 key 也按输入位置逐个返回，不做去重语义
- `error != nil` 或返回长度不符时，manager 必须忽略整批结果，把本次请求的全部 key 收敛为 `RevocationUnknown`
- manager 不再依赖“缺 key 表示什么”的隐式语义；因为 contract 本身不允许缺项
- 如果未来真的需要“部分成功、部分 authoritative”的能力，就必须升级成显式逐项 authoritative 结构，而不是继续在当前接口里塞 `map` 或局部结果

只有把“显式 `not_revoked`”和“后端没能给出结论”分开，`collect` 路径才能在 batch backend 部分失败时继续保持现有语义。

这里的 trade-off 也要写明：

- 获得：接口更短，调用方没有“缺 key / 重复 key / map 覆盖”这类歧义
- 获得：Redis pipeline 的实现和测试都更直接，只要保证输入输出位置对齐即可
- 牺牲：当前接口不支持“部分 authoritative、部分 unknown 但仍带局部结果”的表达；若未来真有此需求，需要升级接口
- 牺牲：manager 要把长度不符视为整批失败处理，这会放弃部分本可利用的局部结果，但换来更低的错误推理空间

### 3. revocation timeout 的所有权先收缩为 manager 自己拥有

当前 `CheckRevocation(...)` 不只被 manager 内部使用，也直接影响 `providers/codex/realtime_session.go` 的 open planning 路径。

从第一性原理看，本轮主问题是“锁内远端 I/O”和“最坏等待时间不受控”，而不是一次性把整个 manager 公共 API 全部 `context` 化。

因此本轮 contract 收缩为：

- `CheckRevocation(...)` 和 bulk `RevocationStatuses(...)` 都统一通过 manager-owned timeout helper 执行，而不是长期依赖 `context.Background()`
- revocation timeout 属于上面的 remote config 一部分，在 manager 初始化或显式 reload 边界配置
- revocation timeout 是 manager 级 remote config，不是协议常量，也不是 channel `Other` JSON 字段
- 如果当前阶段为了兼容保留 `CheckRevocation(sessionKey string)` 这样的无 `ctx` wrapper，它只能是兼容入口；内部仍应委托给 manager timeout helper
- timeout 或 backend error 继续统一收敛为 `RevocationUnknown`
- timeout helper 的所有权应留在 manager / backend helper 中；不要把它重新藏回全局 Redis helper 或新的 `context.Background()` wrapper
- 现有 `AcquireExisting(...)`、`GetOrCreate*`、`ResolveLocal(...)`、`InvalidateBinding(...)` 等 manager 公开操作本身并不接收 `ctx`
- 因此这一个阶段不能在文档里暗示“所有 manager 路径都会自动继承上层请求取消”；当前无 `ctx` 的公开操作继续只使用 manager 自己拥有的 revocation timeout
- 如果未来希望这些公开操作也继承请求级取消，那需要单独扩展公开 API；这是后续独立工作，而不是当前 patch 隐式承诺的能力
- 仍要显式写明：revocation timeout 只约束 revocation 这一步，不自动覆盖 `ResolveBinding(...)` 或 binding write 的等待时间
- `backend == nil` / `m == nil` 的兼容 wrapper 继续短路为 `RevocationNotRevoked`，不应因为“没有 backend”而退化成 `unknown`

同时还要把 timeout 的业务 trade-off 提前写死：

- timeout 越短，`RevocationUnknown` 的比例越高
- 在 Codex realtime 里，`hit + compatible + unknown` 当前语义是“避免 resume，也不把它当作可安全 publish 的依据”
- 因此激进短超时会降低 resume 命中率，并增加 fresh/local-only 路径概率、上游新 session 建立概率和成本
- 所以 timeout 不能只写死一个常量；至少应支持配置，并配套指标观测 `revoked/not_revoked/unknown` 比例、revocation 延迟分布与 resume hit rate 变化
- timeout 也应属于上面的 remote config 一部分，避免出现“backend 已更新，但 timeout 还是旧值”的混合配置

这条收缩版 contract 的 trade-off 是：

- 获得：补丁面更小，不需要在本轮同时改 public API 签名、provider 调用链和所有上层取消传播
- 获得：manager 仍然能显式约束 revocation 的最坏等待时间，解决当前 `context.Background()` 带来的无上界等待
- 牺牲：上层请求取消暂时不会更早打断 revocation；请求即使已无意义，也可能继续等到 manager timeout 才返回
- 牺牲：文档必须诚实承认“当前只完成 manager-owned timeout”，不能把它表述成“已经完成完整 context 化”

### 4. `backend == nil` 的兼容语义不能被误并入 `unknown`

当前 `CheckRevocation(...)` 在 `m == nil` 或 `backend == nil` 时返回的是 `RevocationNotRevoked`，而不是 `RevocationUnknown`。

这代表的是：

- 当前运行在 local-only / memory-only 语义下，没有远端 revocation backend
- 而不是“后端查询失败”

因此文档必须显式固定：

- `backend == nil` 或 `m == nil` 的兼容 wrapper，继续返回 `RevocationNotRevoked`
- 只有“已经配置了 backend，但本次查询超时 / 出错 / 部分失败”才收敛为 `RevocationUnknown`
- `collect` 路径在 backend 缺席时可以直接跳过远端 revocation phase，而不是制造一批 `unknown`

只有把“未启用该能力”和“能力调用失败”分开，memory-only 兼容语义才不会在这次重构里被悄悄改坏。

### 5. 回锁后 `RevocationRevoked` 的优先级必须高于 `TryLock` miss

当前代码的真实语义是：

1. 先看 revocation
2. 再看 `TryLock + IsExpired`

因此，被明确判定为 `RevocationRevoked` 的 session，不能因为对象此刻 `TryLock` 失败就继续存活。

重构后文档必须把这条规则写死：

- `TryLock` 失败只意味着“当前不能据此判定本地 idle expiration”
- `TryLock` 失败不能覆盖已经确认的 `RevocationRevoked`
- Phase 3 回锁后的最终删除条件应当是：`same object && (locally expired || revoked)`

否则会把当前“revoked 优先”的既有语义，悄悄退化成“只要忙就暂时不删 revoked session”。

### 6. bounded restart 的副作用归属必须先固定

复杂公开路径允许 bounded restart，但 restart 的对象只能是“决策流程”，不能把 abandon 掉的 attempt 副作用一并泄漏出去。

因此文档必须先固定：

- 每轮 attempt 都应使用 attempt-local 的中间状态，至少区分：
  - 已经提交到本地 manager state 的删除副作用
  - 还未提交、仅来自旧快照判断的意图和临时观察
- 对已经在锁内真实提交的本地删除，相关 `toCleanup` / `pendingDeletes` 必须精确保留，并在最终公开操作返回前执行一次；restart 不能把它们丢掉，也不能执行两次
- 对未提交的旧意图，例如旧 binding owner 判断、旧容量判断、旧 revocation 结果推导出的 create/reuse/conflict 分支，restart 后必须全部丢弃，不能继续污染新一轮判断
- lease reservation 只能属于最终胜出的 attempt；如果某个 attempt 在进入 restart 前已经 reserve 过 lease，就必须在 restart 前补偿释放，或更简单地把 reserve 延后到最终结果确认之后
- 凡是返回 `release func()` 的公开路径，`reserveLease()` 必须在最终回锁确认后、解锁返回前完成；不得先把对象交给调用方，再在锁外或返回后补记账
- 这条约束的目标不是让显式 `Delete(...)` / `DeleteIf(...)` 尊重 `reservations`，而是防 `Sweep(...)` / `sessionLocallyExpiredLocked(...)` 在 `reservations == 0` 的窗口里误删即将返回的对象
- cleanup callback 和远端 binding delete/revoke 这类副作用，必须与“本地状态已经真实发生的变更”绑定，而不是与“曾经走到某个中间分支”绑定

这里的 trade-off 也要提前写明：

- 好处：bounded restart 不会引入双重 cleanup、重复远端 delete，或泄漏 lease reservation
- 代价：实现上需要显式区分“已提交副作用”和“已失效意图”，不能再把 attempt 中间变量当作一个扁平过程顺手复用

只有先锁死这条 contract，后面的 `GetOrCreate*` / `*Bound` bounded restart 才不会在控制流上看起来正确、但在副作用层面悄悄出错。

## 最终方案

在进入下面这些步骤前，先固定 remote config 的生命周期边界：

- 先收敛 `ConfigureRedis(...)` 生命周期：Codex request / stats hot path 不再反复调用；backend 发布发生在 manager 初始化或显式 reload 边界
- 至少包含 `backend`、`redisPrefix` 与 revocation timeout
- Codex 初始化路径应通过 `InitExecutionSessionManager()` 一次性把 client、prefix、timeout 注入全局 manager；`ConfigureRedis(...)` 仅作为兼容接口保留
- 普通请求路径默认观察到的是一份稳定配置；本轮不把 provider/request-scoped snapshot 或运行中原子热切换作为默认交付
- 如果未来明确要求 in-flight request 感知热更新，再单独设计 manager swap 或 atomic snapshot 发布模型

### 1. 抽取纯本地 helper

先把当前混合逻辑拆成纯本地判定：

```go
func (m *Manager) sessionLocallyExpiredLocked(sess *ExecutionSession, now time.Time) bool
```

这个 helper 只负责：

- `nil` 判定
- `TryLock`
- `IsExpired`

它不做任何 Redis I/O，也不做隐式 `unlock/relock`。

这一步是整个重构的基础，因为只有把“本地过期”和“远端 revoke”拆开，调用方才能安全地把网络调用移到锁外。

### 2. revocation I/O 必须带操作级超时

当前 `CheckRevocation(...)` 直接使用 `context.Background()`，这意味着 manager 自己没有能力为 revocation 查询设置 deadline。

因此本次方案要求：

- `CheckRevocation(...)` 使用 manager-owned timeout helper
- bulk `RevocationStatuses(...)` 也使用同类 timeout helper
- 推荐默认超时使用激进短值，例如 `200ms`
- 超时或 backend error 一律收敛为 `RevocationUnknown`
- 现有无 `ctx` wrapper 仅作为兼容层保留，但内部仍必须委托给 timeout helper
- timeout helper 的所有权留在 manager / backend helper 中，不重新藏回全局 Redis helper 或新的 `context.Background()` wrapper
- 现有无 `ctx` 的 manager 公开操作如果本轮不改签名，就继续只使用 manager timeout；不要在当前阶段隐式承诺请求取消已经贯穿这些 API
- `backend == nil` / `m == nil` 的兼容 wrapper 继续短路为 `RevocationNotRevoked`，不应因为“没有 backend”而退化成 `unknown`

这条约束的价值是：

- 即使某条路径暂时还没有完成锁边界重构，也能先把最坏阻塞时间压到 manager 自己可控的范围
- 即使 revocation I/O 已移到锁外，也不会让当前 goroutine 无限期等待远端
- 不需要在本轮同时扩 public API、provider 调用链和请求取消传播
- 能把“没有 backend”与“backend 不可用”这两类完全不同的语义分开
- 能让 manager 级 backend 读写路径共享同一个明确的配置生命周期，而不是只有 revocation 单独特殊处理

但需要明确：

- 短超时是配套措施，不是替代方案
- 只加超时而不改锁边界，仍然没有解决 `collect` 路径的 `N x RTT`
- 单 session 路径即使只有 1 次查询，`200ms` 落在 `m.mu` 内仍然过高
- 只给 revocation 加 timeout，不代表 `ResolveBinding(...)` 等其他远端路径也已经具备相同的 deadline 约束
- 上层请求取消暂时不会更早打断 revocation；这是本轮明确接受的 scope cut
- timeout 值必须接受业务 trade-off：它不是“越短越先进”，而是“unknown 比例、resume 命中率、上游成本”之间的拨盘

### 3. `collect` 路径采用两阶段处理，并在第一阶段就做 batch/pipeline

`collectExpiredSessionsLocked(...)` 是本次修复的最高优先级路径。

最终形态采用两阶段：

1. 锁内：
   - 对全部 session 先跑 `sessionLocallyExpiredLocked(...)`
   - 对已经本地过期的 session，直接在当前锁内删除
   - 对本地未过期的 session，收集 revocation 候选 `(sessionKey, *ExecutionSession)`
2. 锁外：
   - 对候选 key 做批量 revocation 查询
   - Redis backend 使用 pipeline，把多次 `EXISTS` 收敛为 1 次网络往返
3. 回锁：
   - 逐个检查 `m.sessions[key]` 是否仍然是同一个 `*ExecutionSession`
   - 重新跑一次 `sessionLocallyExpiredLocked(...)`
   - 对确认 `RevocationRevoked` 的同一对象执行 `deleteSessionLocked(...)`
   - 对 `RevocationUnknown` 不驱逐

这里还要把本地 expiry 的时间锚点 contract 写死：

- `Sweep(now)` 若传入非零 `now`，它定义了本次 collect 的 `operationNow`，也就是本轮本地 expiry 的观察时间
- Phase 1 与 Phase 3 对 `sessionLocallyExpiredLocked(...)` 的调用都必须使用同一个 `operationNow`；不能在 Phase 3 重新取 `time.Now()`
- revocation probe 的耗时只能影响 wall-clock 完成时间，不能改变本次 sweep 的本地过期语义
- 允许 phase gap 造成“本轮没看到新 revoke / new replacement”，但不允许 `time.Now()` 漂移把 `Sweep(non-zero now)` 退化成 wall-clock 相关行为
- 因此 `collectExpiredSessions(...)` 的回锁删除条件应固定为：`same object && (revoked || locallyExpiredAt(operationNow))`
- 对 `Sweep(non-zero now)`，`operationNow` 就是调用方传入的 `now`；对容量回收路径，`operationNow` 可取该次 collect 的入口时刻

这里的核心不是“先正确再优化，所以 pipeline 可以最后再说”，而是：

- 两阶段重构解决“锁内做 I/O”的结构问题
- pipeline 解决 `collect` 路径的 `N x RTT` 放大

两者应该在同一阶段落地，而不是分成前后互不相干的 patch。

这里还要显式接受一个窗口期 trade-off：

- 在 Phase 1 收集候选、Phase 2 查 Redis、Phase 3 回锁删除之间，新创建且已被 revoke 的 session 可能不会被本轮 sweep 捕获
- 这不是遗漏，而是当前 best-effort session manager 在不引入 lease / fencing 前提下接受的非原子窗口
- 该对象会在下一轮 sweep、下一次容量回收或下一次显式访问时再被处理

同时要补一个容易误解的边界：

- 本次重构解决的是 revocation read 对 `m.mu` 的放大
- Phase 3 删除后产生的 `pendingDeletes`，仍会在解锁后触发远端 `DeleteBindingIfSessionMatches(...)`
- 所以这次可以显著缩短锁持有时间，但不承诺把 `Sweep(...)` 或容量清理的端到端 wall-clock 直接压成固定常数
- 如果后续发现端到端删除尾延迟仍然需要继续收敛，再单独讨论 binding delete / revoke 写路径是否也要批量化

### 4. caller capacity 路径不单独引入 scoped collect

当前 `ensureCallerCapacityLocked(...)` 在单个 caller 达到上限时，会回退到全量 `collectExpiredSessionsLocked(...)`。

从第一性原理看，caller capacity 的真实需求是：

1. 容量判定不能在锁内携带 revocation I/O
2. caller/global capacity 的返回语义必须在回锁后按最新本地 truth 重算

caller-scoped collect 只减少扫描范围，不增加正确性。它会额外引入一套“局部候选收集 + 作用域重验 + global fallback”的算法分支，却不改变本方案必须成立的并发 contract。

因此本方案明确选择：

- caller capacity 继续复用同一套 global two-phase collect
- `ensureCallerCapacityLocked(...)` 与 `ensureCapacityLocked(...)` 共享同一组 revocation 批量化与回锁重验规则
- 如果后续 profiling 证明“全量扫描成本”本身成为独立瓶颈，再单独讨论 caller-scoped collect；它不是这份完整设计的一部分

### 5. 所有单 session 路径都把 revocation 查询移到锁外

不能只修 `collect`。凡是“为了判断 session / binding 是否还能安全复用”而读取 revocation 的路径，即使每次只查 1 次 Redis，只要它发生在 `m.mu` 内，就是结构性风险。

这些路径统一遵循下面的边界：

1. 第一次加锁：
   - 只做本地 lookup / 本地过期判定
   - 捕获本次决策依赖的稳定标识
2. 锁外：
   - 调用 `CheckRevocation(...)`
3. 第二次加锁：
   - 重新确认 unlock 前依赖的不变量仍然成立
   - 重新跑本地过期判定
   - 只有在前提仍成立时才继续删除、复用或创建

这里“稳定标识”至少包括：

- session 路径：`sessionKey + *ExecutionSession`
- binding 路径：`bindingKey + expectedSessionKey`
- 容量路径：当前 session 是否已存在、当前 binding owner、当前容量是否仍足够

并且要补一条优先级约束：

- 如果锁外结果已经确认 `RevocationRevoked`，回锁后对“同 key、同指针”的对象应优先按 revoked 处理
- 不能因为回锁后 `TryLock` 失败，就把这个已确认 revoked 的对象重新视为“暂时还活着”

### 6. binding 路径拆成两类，不与最简单路径混写

`liveLocalBindingLocked(...)` 及其调用者同时涉及：

- binding lookup
- session 过期判定
- 本地 binding/index 清理
- owner 重验

因此 binding 相关路径虽然也属于“单对象决策”，但它的实现复杂度明显高于 `AcquireExisting(...)` 这类最简单路径。

在同一重构里，应把下面这些“live binding 读路径”作为同一类控制流统一处理：

- `liveLocalBindingLocked(...)`
- `ResolveLocal(...)`
- memory-only 模式下的 `ResolveBinding(...)` 本地分支

如果未来真的引入 `ResolveBinding(...)` 的本地 near-cache 读路径，它也应属于这一类；但当前文档不应把“尚未存在的 near-cache 分支”当成既有实现前提。

这样可以避免把“简单 session 路径单次重验”和“binding owner + 本地 map 清理”的复杂控制流揉进同一段实现分支里。

这类 simple binding live-read helper 的结构约束也要写死：

- 像 `observeLiveLocalBinding(...)` 这类 helper，只允许围绕 unlock 前捕获的 `bindingKey + expectedSessionKey + expectedSession` 做单次重验
- unlock 窗口内如果发现 binding replacement / session replacement，helper 只能返回 false miss / changed，并把控制权交回调用方；不得直接返回 fallback binding
- 不允许存在“旧 `expectedSessionKey` 已做 revocation 检查，但 fallback binding 的新 `SessionKey` 还未做自己的 revocation + 本地活性重验”的中间态
- 只有 complex path 才允许 bounded restart / latest-truth reobserve；simple binding live-read helper 不得自己长出 ad hoc fallback 命中分支
- 因此 `ResolveLocal(...)` 与 memory-only `ResolveBinding(...)` 本地分支在 replacement 发生于 revocation probe 窗口时，合法收敛只能是 false miss，而不是返回未验证 fallback owner

但 `InvalidateBinding(...)` 不应被机械并入这批“live binding 重验”：

- 它的真实需求是 destructive cleanup：删除本地 binding / session，并尝试远端 delete-and-revoke
- 它不需要为了做本地删除，再额外引入一次 revocation read
- 更合适的边界是：
  1. 锁内捕获 `bindingKey + expectedSessionKey + revocationTTL`
  2. 在本地同一临界区内完成 session / binding / index 清理，或清理 orphan residue
  3. 解锁后再调用 `DeleteBindingAndRevokeIfSessionMatches(...)`

这条拆分的 trade-off 也要写明：

- 好处：`InvalidateBinding(...)` 保持最短路径，不把 destructive cleanup 和 live-owner 判定混成同一套复杂重验流程
- 代价：同一重构里需要显式维护“live binding 读路径”和“destructive invalidation”两类控制流，而不是试图用一套分支强行统一

但从收益/复杂度比看，这是比“把所有 binding 相关路径揉成一套超大控制流”更稳妥的点位。

### 7. 回锁后的重验规则

回锁后不能只看“session 指针还在不在”，需要按路径重建真实不变量。

#### 7.1 所有路径都要重跑本地过期判断

unlock 窗口内，下面这些字段都可能变化：

- `closed`
- `reservations`
- `Attached`
- `Inflight`
- `LastUsedAt`

因此回锁后必须再跑一次 `sessionLocallyExpiredLocked(...)`，不能假设 unlock 前看到的“本地未过期”仍然成立。

#### 7.1.1 `RevocationRevoked` 高于本地 `TryLock` 失败

`sessionLocallyExpiredLocked(...)` 只负责纯本地判断，因此它的 `TryLock` miss 语义只能表示“当前不能判定本地过期”。

但如果锁外 revocation 查询已经明确返回 `RevocationRevoked`，那么回锁后的最终决策必须保持：

- 同 key、同指针对象：即使这次 `TryLock` 失败，仍应按 revoked 处理
- `TryLock` miss 只能阻止“本地 idle 过期”的判定，不能屏蔽“已确认 revoked”的判定

建议在实现注释里直接写明最终条件：

```go
shouldDelete := sameObject && (locallyExpired || revoked)
```

#### 7.2 binding 路径要重验 owner

凡是依赖 binding owner 的路径，回锁后都要确认：

- 该 binding 仍然存在
- `binding.SessionKey == expectedSessionKey`

否则旧的 revocation 检查结果可能会误删或误判新的 binding owner。

#### 7.3 容量路径要重算容量

`GetOrCreate*` 系列路径在 unlock 期间，其他 goroutine 可能已经：

- 删除了旧 session
- 创建了新 session
- 占用了 caller capacity
- 占用了 global capacity

因此回锁后不能沿用 unlock 前的容量判断，必须重新执行容量校验。

### 8. restart 策略按路径复杂度分级，并预先定义边界

本次方案不采用“所有公开操作统一通用 retry loop”的粗暴做法，也不允许在 `*Locked` helper 内部偷偷 `unlock/relock`。

采用的策略是：

- 简单路径：
  - 例如 `AcquireExisting(...)`，以及只做 live-owner 判断的 binding live-read path（`ResolveLocal(...)`、memory-only `ResolveBinding(...)` 本地分支、`observeLiveLocalBinding(...)` / 同类 helper）
  - unlock 后回锁只做单次 re-validate
  - 如果前提失效，直接返回 miss / nil / not found
  - 这意味着 unlock 窗口内若同 key 对象被删除后又重建，简单路径允许返回 false miss，而不是再为了追回新对象引入额外 restart
- 复杂路径：
  - 例如 `GetOrCreate(...)`、`AcquireOrCreate(...)`、`GetOrCreateBound(...)`、`AcquireOrCreateBound(...)`
  - 允许在公开入口层做 bounded restart
  - 当回锁后发现 session 被替换、binding owner 变化、容量状态变化时，从公开操作起点重新跑一次

对 bounded restart，本次方案提前定义边界：

- `bound = 1`
- 也就是最多执行 2 轮公开操作
- restart 必须从公开入口重新开始，不能从某个中间局部状态续跑
- 超过 bound 后，返回本轮最新观测到的真实结果
- 不能把“并发窗口内重试耗尽”伪装成 `ErrCapacityExceeded` 或 `ErrCallerCapacityExceeded`
- restart 只能重跑决策流程，不能让 abandon attempt 的 lease / cleanup / remote delete 副作用泄漏到最终结果之外；副作用归属规则以前面的 contract 为准

这里“本轮最新观测到的真实结果”不能只停留在口号，必须写成路径级决策规则：

- plain `GetOrCreate(...)` / `AcquireOrCreate(...)`：
  - 如果第二轮看到同 key live session，返回 reuse
  - 如果第二轮容量校验仍失败，才返回 capacity error
  - 否则创建新 session
- bound `GetOrCreateBound(...)` / `AcquireOrCreateBound(...)`：
  - 如果第二轮看到 live binding owner 且 `binding.SessionKey != meta.Key`，返回 conflict binding
  - 否则如果第二轮看到同 key live session，返回 reuse
  - 否则重新校验 caller/global capacity；只有最新校验仍失败时才返回对应 capacity error
  - 否则创建新 session 并 upsert 本地 binding
- 如果 restart exhausted 后最新看到的是 replacement binding，则必须把 replacement binding 指向的 `SessionKey` 当成新的候选 owner，再完成它自己的 revocation + 本地活性重验；未完成前，不允许把该 binding 直接作为 conflict 返回，也不允许把它当成 fallback reuse

这组规则的核心是：

- 重试耗尽后，要按“当前本地 truth”分支，而不是按第一次 unlock 前的旧意图返回
- `capacity exceeded` 只能来自最新一轮容量校验，不能来自已经失效的旧快照
- conflict / reuse / create 三类结果的优先级必须由当前重验后的事实决定，而不是由旧分支残留决定
- 因此 complex path 的最终合法出口只能是 `conflict(verified live replacement owner)` / `reuse(latest verified)` / `capacity error(after latest capacity recheck)` / `create`；不允许返回“未验证 replacement 的 fallback binding”

这样做的原因是：

- 简单路径上强行套通用 loop，收益很低，认知成本很高
- 复杂创建路径上，单次 ad hoc re-validate 往往会散落出大量分支，反而更容易错
- 对简单路径而言，“允许少量 false miss，但绝不返回旧指针、绝不误删新对象”比“为了追回命中而把控制流升级成半套 restart”更符合收益/复杂度比

这里要把 simple-path trade-off 明确写死：

- 获得：实现边界清晰，简单路径不需要引入 restart、副作用回滚或额外 lease 归属逻辑
- 牺牲：会丢失一小部分本可追回的 reuse 命中，表现为 false miss、fresh fallback 或后续 `AcquireOrCreate(...)` 概率上升
- 约束：false miss 只能影响命中率，不能影响正确性；实现绝不能返回旧指针，也不能因为旧 revocation 结果误删新对象
- 观测：需要单独统计 simple-path revalidate miss / same-key replacement miss，并与 resume hit rate、fresh path 比例一起看

因此当前推荐的是“简单路径单次重验，复杂路径有界重启”，而不是“一刀切 loop”或“一刀切不 loop”。

#### 8.1 复杂创建路径先收口一个“session 是否仍可复用”的小原语

这轮实现里真正容易失控的点，不是 `bound = 1` 太小，而是 `GetOrCreate*` / `*Bound` 同时要处理：

- 锁内 live session lookup
- 锁外 revocation 查询
- 回锁后的 same-key replacement
- 本地过期重验
- 后续 capacity / binding owner 决策

如果这些逻辑继续散落在每个公开路径自己的分支里，那么一旦第二轮又遇到 same-key replacement，就很容易出现：

- 返回 `nil, nil, nil` 这类“成功但无真实结果”的空态
- 直接复用新的 replacement session，却没有重新跑它自己的 revocation 判定
- 在 binding 路径里沿用已经失效的 `sess` 指针，最后又把 stale session re-upsert 回本地

因此复杂创建路径应先抽出一个单一的小原语，例如 `observeReusableSession(...)` 这种职责边界：

1. 锁内：
   - 只拿当前 `sessionKey` 对应的 live local session
2. 锁外：
   - 只查这一个候选对象的 revocation
3. 回锁：
   - 如果还是同 key、同指针对象：
     - `revoked || locallyExpired` => 删除并返回“不可复用”
     - 否则返回“可复用的同一对象”
   - 如果同 key 已换成新指针：
     - 不直接返回新对象
     - 只向调用方报告“观察期间对象发生替换”

如果这个原语返回的是“可复用对象”且对应公开路径最终还会返回 `release func()`，那么 lease 记账必须已经在这次回锁阶段、`m.mu` 保护下提交完成。

这个原语故意不负责 binding owner、capacity、create/reuse/conflict 的最终分支；它只负责回答一件事：当前这个 key 上，是否已经有一个经过 revocation + 本地状态双重重验后仍可复用的对象。

这样做的 trade-off 要明确写进文档：

- 好处：plain `GetOrCreate*` 和 bound `GetOrCreate*` 可以共享同一套 session 复用证明，不必各自散落 replacement / revocation 分支
- 好处：复杂度被限定在“单 key session 真相重建”这一层，不需要把 binding owner/capacity 也塞进同一个超大 helper
- 代价：实现上多出一个 tri-state 观测 helper，而不是继续用扁平 if/else 直接拼在公开路径里

但从第一性原理看，这是控制复杂度上升幅度最小、同时又能堵住 correctness 漏洞的点位。

#### 8.2 restart exhausted 后，不再继续扩大 public restart，而是给“复用观测”一个很窄的 latest-truth 重观测预算

复杂路径第一轮如果发现 same-key replacement，仍然应该从公开入口 restart；因为 binding owner、capacity、是否 create/reuse/conflict 这些判断都需要重新基于最新本地 truth 构建。

但第二轮如果再次遇到 same-key replacement，文档必须明确：

- 不能直接返回旧分支残留出来的空结果
- 不能跳过 replacement session 自己的 revocation 判定
- 不能为了追回最新对象，把整个公开路径继续升级成第三轮、第四轮 public restart

当前选择的折中是：

- public bounded restart 仍然保持 `bound = 1`
- 但在最后一轮 attempt 内，只对“session 是否可复用”这一个窄问题允许固定次数的 latest-truth 重观测
- 这个重观测预算只服务于同 key replacement churn 的收敛，不重跑整套 binding owner / capacity / create 决策树

也就是说：

- 第一轮 replacement => public restart
- 第二轮若再次 replacement => 只在“session 复用观测”层做极窄、固定次数的再次观测
- 如果 latest-truth 最终看到的是 replacement binding，则必须对 replacement binding 的最新 `SessionKey` 重新执行 revocation + 本地活性重验；未验证前不得把它当成 fallback conflict
- 观测结束后，仍按本节前面定义好的最新 truth 优先级落到 `conflict / reuse / latest capacity error / create`

这里要明确，这不是“偷偷引入第三轮公开操作”；它只是把 final attempt 的 same-key replacement churn 收敛在单一观测原语里，避免继续散落 ad hoc fallback 分支。

这条 trade-off 也要写清楚：

- 好处：不会为了一个很窄的 replacement 窗口，把所有复杂路径升级成更大的统一 retry loop
- 好处：可以修住“restart 耗尽后空成功”“replacement 未重验 revocation 就被复用”这类 correctness 问题
- 代价：在极少数 replacement churn 场景下，最后一轮 attempt 可能会多做 1 到 2 次额外 revocation 查询

相比“在每个路径各补一段 fallback 分支”，这个额外读放大更可控，代码也更容易审计。

#### 8.3 bound 路径在最终 owner 重验后，还必须再证明当前 `sess` 仍是 live owner

`GetOrCreateBound(...)` / `AcquireOrCreateBound(...)` 比 plain create path 更危险的一点是：

- `liveLocalBindingLocked(...)` 不只是读 binding
- 它可能顺带清理 orphan binding / stale session / binding index

因此在 bound 路径里，哪怕已经拿到了一个看起来可复用的 `sess`，只要后面又跑了一次 binding owner 重验，那么在真正：

- 更新 `sess.BindingKey`
- `upsertBindingLocked(sess)`
- 返回 reuse 结果

之前，都必须再次确认：

- `m.sessions[meta.Key]` 仍然是这个 `sess`

否则就可能出现：binding 重验的副作用已经把旧 `sess` 从本地删掉了，但调用方还沿用这个 stale 指针继续 upsert / return。

所以文档要把这条约束写死：

- bound 路径任何一次最终 owner 重验之后，只要还要继续复用当前 `sess`，都必须再做一次“当前 key 上仍是同一对象”的确认
- 如果确认失败：
  - 第一轮 attempt => restart
  - 最后一轮 attempt => 回到上一小节定义的窄化 latest-truth 重观测，而不是继续返回 stale session

这是避免 binding 路径 correctness 退化的最低必要条件，不属于可选优化。

### 9. batch revocation 能力采用可选接口，不把 manager 绑死到具体 backend

`collect` 路径需要批量 revocation 查询，但 manager 不应直接感知具体的 `redisBindingBackend` 类型。

推荐增加一个可选能力接口，例如：

```go
type bulkRevocationBackend interface {
    RevocationStatuses(ctx context.Context, sessionKeys []string) ([]RevocationStatus, error)
}
```

然后：

- Redis backend 实现该接口，内部用 pipeline 批量 `EXISTS`
- manager 优先走 bulk 能力
- 不支持 bulk 的 backend 退回到锁外逐个检查
- 返回切片长度必须等于输入长度，且顺序与输入一致
- 重复 key 也按输入位置逐个返回，不做去重语义
- bulk backend 如果返回 `error` 或长度不符，manager 必须忽略整批结果，并把本次批量请求中的全部 key 收敛到 `RevocationUnknown`

这条取舍的收益和代价如下：

- 收益：
  - manager 不需要对具体 backend 做 type assertion
  - Redis 获得 1 RTT 批量 revocation 查询
  - ordered slice contract 比 `map + Complete` 更短，也更不容易出现“缺 key 到底算什么”的歧义
  - 非 Redis backend 保持退化兼容
- 代价：
  - 增加一个可选接口和对应测试
  - `collect` 路径会出现 bulk/fallback 两条分支
  - 如果未来需要“部分 authoritative + 局部结果复用”，必须升级接口，而不是在当前 contract 上硬塞更多状态

这是当前复杂度和收益之间更合适的平衡点。

## 明确不采用的方案

| 方案 | 放弃原因 |
| --- | --- |
| 只修 `collect`，单 session 路径保留锁内 Redis I/O | 单 session 路径仍会在 Redis 抖动时阻塞整把 `m.mu`，只是把最坏路径缩小，不是修复结构问题 |
| 只给 `CheckRevocation` / `RevocationStatuses` 加短超时作为唯一修复 | 只是压缩最坏等待时间，不解决 `collect` 路径的 `N x RTT`，也不解决锁边界的结构问题 |
| 在 `*Locked` helper 内部偷偷 `unlock/relock` | 破坏 `*Locked` 的调用契约，调用方无法知道中间状态已经失去原子性 |
| 给 `CheckRevocation` 增加 `not_revoked` 短 TTL 负缓存 | 会延迟 revocation 生效，改变“旧 key 不应再次进入 resume 路径”的现有语义 |
| 用 TTL cache + singleflight 缓存 revocation/backend 配置读取 | 这里需要的是稳定生命周期边界，不是允许短暂陈旧的设置缓存；引入 TTL 只会把语义边界重新搞模糊 |
| 把 session sweep 改成增量限额扫描 | 适合辅助缓存，不适合 capacity 正确性路径；会把“这轮必须得出容量真相”退化成 best-effort |
| 复用全局 Redis helper 并在内部继续使用 `context.Background()` | 会把 timeout ownership 重新藏回 wrapper，和“manager 显式控制 revocation 等待时间”的目标冲突 |
| 把 pipeline 留到最后再做 | `collect` 是最高 ROI 路径，pipeline 应与两阶段改造一起落地 |
| 对所有公开操作统一套一个通用 retry loop | 简单路径收益很低，复杂路径又需要更细粒度的不变量重建，统一 loop 会放大代码复杂度 |
| 只在回锁后校验 session 指针 | 不足以覆盖 binding owner 变化、容量变化和本地过期状态变化 |

## 语义约束

本次重构必须保持以下行为不变：

### 1. manager 过期/清理语义

- 只有 `RevocationRevoked` 直接导致 session 被视为应清理
- `RevocationUnknown` 不驱逐本地 session
- backend 不可用时，不因为 revocation 查询失败而做 fail-close 清理
- `backend == nil` / `m == nil` 的兼容 wrapper 继续视为 `RevocationNotRevoked`，不应和“backend 查询失败”混为一谈

### 2. Codex realtime 复用语义

这次改动不改变 `providers/codex/realtime_session.go` 的业务语义：

- `hit + compatible + not_revoked` 才允许优先 resume
- `unknown` 仍然避免 resume
- `revoked` 仍然进入合法 replace / fresh 路径
- 因此 revocation timeout 的调参，会真实影响 `unknown` 比例、resume 命中率和 fresh/local-only 比例；这属于显式 trade-off，不是实现细节

### 3. 边界澄清

- 本次文档修的是 manager revocation 的锁边界与其自身 deadline
- 它不单独承诺 realtime open 全路径都已经摆脱 `context.Background()`
- 如果后续目标升级为“open planning 全链路 deadline 可控”，应单独追加 `ResolveBinding(...)` / binding write 的 context 化方案

本次文档关注的是 manager 的锁边界修复，而不是重新定义 affinity 协议。

## 运行拨盘与观测要求

这份设计不是把 `200ms` 或 `bound = 1` 写成协议常量，而是把它们定义为当前实现的默认拨盘，并要求配套观测。

- revocation timeout 是 manager 级 remote config，不是协议常量，也不是 channel `Other` JSON 字段
- Codex 当前使用全局 execution session manager，因此 timeout 应走全局配置键 `codex.execution_session_revocation_timeout_ms`
- revocation timeout：默认可取激进短值，例如 `200ms`，但它属于 manager 初始化/显式 reload 配置的一部分，不是写死的协议语义
- bounded restart：默认 `bound = 1`，目标是在复杂路径上限制控制流膨胀，而不是承诺永远不能调整
- `InitExecutionSessionManager()` 必须在初始化时一次性注入 client、prefix、timeout；request / stats hot path 不得顺手 reconfigure manager

至少应补齐以下指标：

- revocation 查询结果分布：`revoked` / `not_revoked` / `unknown`
- revocation 查询延迟分布，以及 timeout 次数
- batch revocation 的 `error` 次数、返回长度不符次数，以及整批按 `unknown` 回退的次数
- Codex realtime 的 resume hit rate、fresh 路径比例、local-only 路径比例
- simple path 的 revalidate miss / same-key replacement miss 次数，以及 miss 后落入 fresh / create 的比例
- bounded restart 次数、restart exhausted 次数

只有把这些观测补齐，timeout 和 restart bound 才是可调拨盘，而不是拍脑袋常量

## 测试要求

本次修复除了 happy path，至少要覆盖以下高风险边界和异常场景：

### 并发重验

- unlock 做 revocation 检查期间，同 key session 被删除后重建为新指针
- unlock 期间 binding owner 切换，旧检查结果不能误删新 owner
- `ResolveLocal(...)` 与 memory-only `ResolveBinding(...)` 本地分支在 replacement 发生于 revocation probe 窗口时，只能 false miss，不得返回未验证 fallback binding
- unlock 期间 caller capacity / global capacity 被其他 goroutine 占用
- unlock 期间 session 本地状态变化，回锁后必须重新跑 `sessionLocallyExpiredLocked(...)`
- unlock 期间如果对象仍是同一指针，已确认 `RevocationRevoked` 不能因为 `TryLock` miss 而幸存
- bounded restart 的最后一轮里，如果同 key replacement 再次发生，plain / bound 路径都必须重新对 replacement 做 revocation 判定，不能直接 reuse，也不能返回空成功结果
- `GetOrCreateBound(...)` / `AcquireOrCreateBound(...)` 在 restart exhausted 后如果最终看到 replacement binding，必须对 replacement binding 的 `SessionKey` 重新完成 revocation + 本地活性重验，之后才允许落到 conflict / create / reuse
- bound 路径在最终 binding owner 重验后，如果副作用已清掉旧 session，不得继续返回或 re-upsert 旧 `sess`
- 证明 Codex request / stats hot path 已不再调用 `ConfigureRedis(...)`；本轮不再把 provider/request-scoped snapshot 作为默认交付前提
- 如果实现保留显式 reload，需证明切换发生在 coarse boundary：一次公开操作只能落在旧 manager 或新 manager 之一，而不是中途混用两份 remote config

### 语义保持

- `RevocationRevoked` 会驱逐 session
- `RevocationUnknown` 不驱逐 session
- `backend == nil` / `m == nil` 时，兼容 wrapper 仍返回 `RevocationNotRevoked`
- `AcquireExisting(...)` 在简单失效场景下直接 miss，不做无限重试
- `AcquireExisting(...)` / 其他 simple path 在同 key 对象于 unlock 窗口内被替换时，允许返回 false miss；但不得返回旧指针，也不得删除新对象
- `GetOrCreate*` 在复杂失效场景下可以 bounded restart，并得到正确的创建/复用/冲突/容量结果
- bounded restart 耗尽后，结果优先级仍按最新一轮真实状态决定，不能回退到旧快照导出的错误码
- bounded restart 耗尽后，如果最终看到的是被 revoke 的 replacement session，plain 路径最终应落到 fresh create，bound 路径最终应落到最新 truth 决定的 conflict / create，而不是错误 reuse
- bounded restart 耗尽后，如果最终看到的是 replacement binding，bound 路径不得直接返回未验证 fallback binding；必须先完成 replacement owner 的 revocation + 本地活性重验
- bounded restart 不会重复执行 cleanup / pending remote delete，也不会泄漏 abandon attempt 的 lease reservation
- revocation 查询在现有无 `ctx` 的 manager 公开操作下继续只使用 manager timeout；当前阶段不承诺继承 parent `ctx` 取消
- `InvalidateBinding(...)` 继续保持 destructive cleanup 语义；orphan binding residue 仍会被本地清理，且不会因为没有 live session 而误返回一个新的 owner

### `collect` 路径

- `Sweep(...)` 先清理本地过期 session，再批量处理 revoked session
- `Sweep(fixedNow)` 即使在 Phase 2 故意阻塞 revocation probe，Phase 3 仍必须按同一个 `fixedNow` 判定本地 expiry
- batch revocation backend error 时，不清理 unknown session
- 回锁时如果 key 对应 session 已经不是原指针，不得删除新对象
- caller capacity 和 global capacity 的清理结果仍然正确

### lease / 配置接线

- `AcquireOrCreate(...)` / `GetOrCreate(...)` 在复用 live session 并返回 lease 时，并发 `Sweep(...)` 不得删除最终返回对象；测试注释要明确这里修的是 `Sweep(...)` / 本地过期清理竞态，不宣称 `Delete(...)` / `DeleteIf(...)` 尊重 `reservations`
- `InitExecutionSessionManager()` 需要为 Codex 注入 client、prefix、timeout 三者；生产路径不再把 `ConfigureRedis(...)` 当作完整配置入口
- request / stats hot path 不得顺手 reconfigure manager；如果兼容层保留 `ConfigureRedis(...)` / `ConfigureRemote(...)`，测试也应明确它们不再是生产路径语义来源

### 异常路径

- Redis 超时 / backend error 不会导致锁内长时间阻塞
- `backend == nil` 不会被误判成 `RevocationUnknown`
- fallback 到非 bulk backend 时仍保持正确语义
- bulk backend 返回长度不符时，整批结果按 `RevocationUnknown` 处理，不得误判为 `not_revoked`
- bulk backend 如果返回 `error` 或长度不符且附带局部结果，manager 也必须忽略这些局部结果，统一按本批次 `unknown` 处理
- revocation timeout 触发时，Codex realtime 仍保持 `unknown => 不 resume` 的既有语义

如果某条测试暂时无法稳定构造，需要在测试注释里明确说明剩余风险；不能只覆盖 happy path 后直接合并。

## 单次重构完成清单

下面顺序只是编码依赖顺序，不表示拆成多套方案或多次语义交付；合并标准是整张清单一次性满足。

1. 固定一致性模型、remote config 生命周期、ordered-slice bulk 返回 contract、timeout 所有权、`backend == nil` 兼容语义、`revoked > TryLock miss` 的回锁优先级，以及 bounded restart 的副作用归属，并在文档/实现注释中写明；其中要明确“不得返回 stale result”与“不承诺 `Delete(...)` / `DeleteIf(...)` fencing”
2. 收敛 `ConfigureRedis(...)` 生命周期：删除 Codex request / stats hot path 中“读请求顺手 reconfigure manager”的调用，把 backend 发布收敛到初始化或显式 reload 边界；Codex 生产路径通过 `InitExecutionSessionManager()` 一次性注入 client、prefix、timeout，`ConfigureRedis(...)` 仅作为兼容接口保留；若未来产品明确要求运行期热切换对 in-flight request 可见，再单独设计 manager swap、atomic snapshot 或 provider/request-scoped 组合 API
3. 给 revocation 查询补 manager-owned timeout helper 与操作级超时；如果保留无 `ctx` wrapper，仅作为兼容层，补 `backend == nil -> not_revoked` 与 `timeout/error -> RevocationUnknown` 语义测试，并明确当前阶段不继承 parent `ctx` 取消
4. 抽 `sessionLocallyExpiredLocked(...)`，补纯本地语义测试
5. 给 backend 增加可选 bulk revocation 能力，Redis 实现 pipeline，并明确“结果顺序与输入对齐”“`error != nil` 或长度不符时整批结果一律按 `unknown`”
6. 重写 `collectExpiredSessionsLocked(...)` 为两阶段 + pipeline 流程，并在注释里写明 Phase gap、“解锁后远端 delete 仍可能形成尾延迟”以及 `Sweep(non-zero now)` 必须固定 `operationNow` 这三个已知 contract / trade-off
7. 重构真正简单的单 session 路径，改成锁外 revocation + 单次重验，并显式记录 simple-path false miss trade-off
8. 单独重构 live binding 读路径：`liveLocalBindingLocked(...)`、`ResolveLocal(...)`、memory-only 模式下的 `ResolveBinding(...)` 本地分支；若未来引入 near-cache，再沿用同一控制流；simple binding live-read helper 不得返回未验证 fallback binding
9. 把 `InvalidateBinding(...)` 作为 destructive cleanup 路径单独收口，避免和 live binding 读路径混写
10. 重构 `GetOrCreate*` / `*Bound`，在公开入口层加入默认 `bound = 1` 的 bounded restart；同时把“session 是否仍可复用”的 revocation + same-pointer 重验收口为单一 helper，并给 restart exhausted 的 same-key replacement / replacement binding 场景保留一个很窄的 latest-truth 重观测预算，避免散落 fallback 分支；最终合法出口必须包含 latest capacity recheck 后的 capacity error
11. 补并发测试、unknown 语义测试、capacity 重算测试，以及“revoked 不被 `TryLock` miss 屏蔽”“restart 不泄漏副作用”“simple-path false miss 不返回旧对象/不误删新对象”“`ResolveLocal(...)` replacement 只能 false miss”“`GetOrCreateBound(...)` restart exhausted 后 replacement binding 仍按最新 truth 重验”“`Sweep(fixedNow)` 固定时间锚点”“lease 不被并发 `Sweep(...)` 误删”的语义测试
12. 补运行指标与关键注释，说明为何不采用负缓存、为何短超时不是唯一修复、为何 timeout 是业务拨盘、为何当前阶段不透传 parent `ctx`、为何复杂路径允许 bounded restart、为何 simple path 接受 false miss，以及为何 caller-scoped collect 被明确排除在本方案之外

如果产品目标还要求 realtime open planning 全路径 deadline，可在本轮之后单独补 `ResolveBinding(...)` / binding write wrapper 的 context 化；不要把这部分目标模糊塞进当前 patch。

## 结论

本次修复不是一次“把 Redis 调用搬个位置”的小 patch，而是一次锁边界收敛：

- `m.mu` 只保护本地 truth
- revocation 是锁外 best-effort 远端状态
- 回锁后按路径重建不变量
- `collect` 路径同时拿到正确性修复和 batch/pipeline 的高 ROI 优化

最终目标不是追求形式上最强的一致性，而是在不引入 owner lease / fencing token 的前提下，把当前 best-effort session manager 的锁边界、语义边界和性能边界同时收敛到清晰、可测试、可维护的状态。

同时要保持边界诚实：

- 这份方案能修复 revocation 把远端 I/O 带进 `m.mu` 的结构问题
- 也能让 revocation 自己的等待时间变成可控拨盘
- 这份方案修复的是“本地 manager correctness + 锁外 revocation 边界”，不是“跨节点唯一 owner”或“open 全链路 deadline”
- 这份文档描述的是一次性要达成的终态 contract，而不是“先上半套、后补半套”的路线图
- 但如果要进一步承诺 realtime open 全路径 deadline，可控对象就必须扩展到 `ResolveBinding(...)` 和后续 binding write，而不是把这部分需求偷偷假设为“已经顺带解决”
