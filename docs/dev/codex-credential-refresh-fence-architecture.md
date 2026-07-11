---
title: "Codex Credential Refresh Fence 架构设计"
layout: doc
outline: deep
lastUpdated: true
---

# Codex Credential Refresh Fence 架构设计

## 文档状态

- 状态：当前实现。
- 适用范围：Codex OAuth credential 的普通自动刷新、请求时刷新、401/403 forced refresh、数据库提交、频道 credential/type/delete 生命周期，以及多节点并发协调。
- 当前实现：持久化 channel 全部使用本文的 durable fence；Redis lock、pending/ambiguous journal 与 scheduled reconciliation 不再参与持久化 channel 的正确性路径。DB-less 的隔离 provider 测试夹具保留旧的进程内提交适配，它没有共享持久化身份，不属于部署运行路径。
- 设计取向：以一个共享、非 secret、无 TTL 的数据库 fence 实现 at-most-once refresh；极少数不可判定故障 fail closed，接受人工重新授权，不引入 shared secret WAL。
- 关联代码：`providers/codex/base.go`、`providers/codex/type.go`、`providers/codex/credential_pending.go`、`providers/codex/auto_refresh.go`、`model/channel.go`、`cron/main.go`。

## 已选型结论

本方案不继续完善 process-local credential journal，而是把“旧 refresh token 是否还允许再次 exchange”变成所有节点共同观察的数据库事实。

核心不变量只有一句：

> `refresh_fence != NULL` 表示当前 durable refresh token 可能已经被上游消费；任何节点都不得再次用它发起 OAuth refresh。

数据库 fence 不是普通互斥锁：

- 锁回答“当前谁在工作”；
- fence 记录“一个不可逆动作是否可能已经发生”；
- 锁可以超时，fence 不能因为时间流逝自动消失；
- fence 只能由同一 attempt 的安全终态或显式的新 credential 替换解除。

目标状态机：

```text
READY(key=K0, revision=G, fence=NULL)
    |
    | Claim(A): DB CAS before OAuth dispatch
    v
FENCED(key=K0, revision=G, fence=A)
    |-- Rotated(K1) + Commit(A) ----------> READY(K1, G+1, NULL)
    |-- ProvablyNotDispatched + Cancel(A) -> READY(K0, G, NULL)
    |-- Ambiguous / owner crash ----------> 保持 FENCED，等待人工重新授权
    `-- Admin supplies Kmanual -----------> READY(Kmanual, G+1, NULL)
```

普通 refresh 与 forced refresh 使用同一个 rotation protocol；二者只在“是否需要刷新”的判断上不同，不维护两套分布式锁与提交协议。

## 真实问题

当前刷新链路依次执行：

```text
local channel mutex
    -> Redis TTL lock
    -> OAuth exchange(K0) produces K1
    -> process-local pending(K0, K1)
    -> DB CAS(K0 -> K1)
```

它把一个协议事实拆散到了不同故障域：

| 状态 | 当前存放位置 | 其他节点能否可靠观察 | 是否跨进程重启 |
| --- | --- | --- | --- |
| 当前 worker 正在 refresh | 本地 mutex / Redis TTL lock | 部分可见 | 否 / 有 TTL |
| OAuth 成功但 DB 未提交 | process-local pending map | 否 | 否 |
| OAuth 结果不确定 | process-local ambiguous map | 否 | 否 |
| durable credential | channel DB row | 是 | 是 |

因此 peer 从 DB 读到 `K0` 时无法区分：

1. `K0` 从未使用，可以安全 refresh；
2. `K0` 已被 OAuth 消费，但 `K1` 尚未提交；
3. OAuth 请求结果不确定，`K0` 可能已经失效。

Redis lease 只能暂时减少并发，不能表达后两种不可逆历史。CAS 只能阻止两个结果都写入 DB，不能阻止两个节点都先调用上游。

这也是下列问题的共同根因：

- slave 可以在请求路径执行 OAuth，却不会运行 master-only scheduled reconciliation；
- OAuth 成功、DB 失败后 Redis lock 释放或过期，peer 会重放旧 refresh token；
- soft delete 被默认 GORM scope 隐藏后，process-local secret journal 无法终止；
- channel type 改变但 key 保留时，key-only CAS 可以把 Codex rotated key 写进非 Codex row；
- ambiguous breaker 同样只在本进程生效，peer 不受约束。

## 第一性原理与不可兼得边界

OAuth rotation 与本地数据库提交属于两个独立系统：

```text
R0 -- OAuth server --> R1 -- one-hub DB --> durable R1
```

如果上游没有 idempotency key、结果查询或 token recovery API，one-hub 无法在这两个动作之间建立原子事务。进程可能在任何位置退出：

```text
OAuth 已消费 R0 / 产生 R1
                |
                | crash or DB outage
                v
DB 仍保存 R0，进程也丢失 R1
```

因此下面三件事不能在没有新增持久化 secret authority 的前提下同时满足：

1. 永不重放 `R0`；
2. owner 任意时刻崩溃都能自动恢复 `R1`；
3. 不把 rotated secret 写入另一个 durable WAL / credential broker。

本方案明确选择 1 和 3，牺牲极端故障下的自动恢复：

- 一旦 OAuth 请求可能发出，旧 token 永不自动重用；
- owner 丢失且 rotated credential 未落 DB 时，channel 保持 blocked；
- 管理员必须提供一份新的 credential 才能恢复；
- 不用“等一段时间再试旧 token”伪装成恢复能力。

这是安全优先、可解释的 at-most-once 协议，不承诺 exactly-once。

## 设计目标

1. 任意 API 节点都可以安全尝试 refresh，不依赖 `node_type`、master cron 或静态单写者假设。
2. OAuth 前必须先持久化全局 fence；DB 不可写时 OAuth 调用次数必须为零。
3. OAuth 一旦可能 dispatch，任何 peer 都不能再次消费同一 durable refresh token。
4. normal refresh、forced refresh、auto refresh 共享同一个 protocol executor。
5. channel delete、type change、manual credential replacement 与 in-flight attempt 有确定的胜负关系。
6. stale attempt 不能清除或提交到新的 attempt、credential revision 或 channel lifecycle incarnation。
7. fence 不保存 credential，不扩大 refresh token 的持久化副本和读取主体。
8. 允许仍有效的 access token 在 refresh fence 存在时继续服务；fence 只禁止新的 refresh exchange。
9. 故障状态可观测、可告警、可由明确的管理员动作恢复。

## 非目标

- 不提供 owner crash 后的 rotated secret 自动恢复。
- 不把 Redis 升格为 secret WAL。
- 不实现 OAuth server 与 channel DB 的 2PC。
- 不用无限 lease、自动续租或长 TTL 模拟不可逆事实。
- 不允许管理员只清 fence 而继续使用原 durable refresh token。
- 不在本文中改变 Codex access token cache、usage cache 或普通 channel routing 的业务语义。
- 不为混合新旧二进制设计长期双轨协议；启用 fence 需要协调升级。

## 强不变量

### 1. Claim-before-dispatch

只有数据库已确认当前 attempt 拥有 fence，才允许进入可能发送 OAuth 请求的代码。

如果 Claim 返回超时或连接错误，但写入可能已经提交，必须 reload 并确认 DB 中完整匹配自己的 `attempt_id + revision`。确认前不得 dispatch。

### 2. Once-possibly-dispatched, never auto-unfence

一旦 HTTP client 可能把 refresh 请求交给网络，只有以下两种事件可以解除 fence：

- rotated credential 已 durable commit；
- 管理员 durable replace credential。

超时、断连、成功响应不可解析、DB commit 失败、owner crash 都不能自动解除。

### 3. Fence has no TTL

时间流逝不会让 one-time token 重新安全。`refresh_started_at` 只用于观测和告警，不参与自动回收。

### 4. Attempt-scoped mutation

Commit 或 Cancel 只能修改仍属于同一个 `attempt_id` 和 `credential_revision` 的 row。旧 attempt 不得清除新 attempt 的 fence。

### 5. Lifecycle uses revision, not value coincidence

`type + key` 可能发生 ABA：Codex -> 非 Codex -> Codex，或 key 改走后又改回相同字节。生命周期身份不能只靠当前值判断。

所有 credential/type/delete/restore 语义变更都必须推进 `credential_revision`，使旧 ticket 永久失效。

### 6. No generic unlock

持久化层不提供 `ClearCredentialRefreshFence(channelID)` 这类接口。合法解除只能来自：

- `CommitRotation(ticket, rotatedKey)`；
- OAuth 尚未越过 dispatch boundary 时的 `CancelBeforeDispatch(ticket)`；
- 携带新 credential 的 `ReplaceCredential(...)`。

## 数据模型

建议给 `model.Channel` 增加三个不对 API 暴露、也不参与 tag sync 的内部字段：

```go
CredentialRevision         uint64  `json:"-" gorm:"column:credential_revision;not null;default:0"`
CredentialRefreshFence     *string `json:"-" gorm:"column:credential_refresh_fence;type:varchar(36)"`
CredentialRefreshStartedAt *int64  `json:"-" gorm:"column:credential_refresh_started_at"`
```

字段职责：

| 字段 | 是否 correctness 必需 | 含义 |
| --- | --- | --- |
| `credential_revision` | 是 | credential/type/delete/restore 的单调 lifecycle generation，阻止 ABA |
| `credential_refresh_fence` | 是 | 当前 refresh attempt UUID；非空表示旧 refresh token 不可再次自动使用 |
| `credential_refresh_started_at` | 否 | 诊断、指标与管理员提示，不参与超时清理 |

约束：

- `fence` 只保存随机 nonce，不保存 expected key、access token 或 refresh token；
- `revision` 只在 credential 或 lifecycle identity 变化时递增，普通 name/group/model 等更新不递增；
- refresh CAS 以 `revision` 作为 credential identity，不把包含 secret 的完整 `key` 放入 SQL 条件或诊断日志；
- generic channel update 必须显式 `Omit` 三个内部字段；
- 只有 model credential protocol 方法可以写这些字段；
- migration 对现有 row 使用 `revision=0, fence=NULL, started_at=NULL`，无需 credential backfill。

### 为什么不用单独 fence 表

单独表在概念上更纯，但会引入 channel update 与 fence insert 的跨表事务、软删除 GC、type change 协调和额外竞态。把 fence 放在 credential authority 所在的同一 row 上，可以用一个条件 UPDATE 完成 Claim 或 Commit。

### 为什么不用 credential envelope 覆盖 `Channel.Key`

把 `Key` 变成 `Ready | Refreshing | Blocked` envelope 可以不加字段，但 `Key` 是用户可感知的持久化 credential payload。控制面状态与 secret payload 分列更清晰，也避免 admin/export/import 路径被临时协议状态污染。

## 协议数据类型

provider 只持有一次 attempt 的短生命周期 ticket：

```go
type CredentialRotationTicket struct {
    ChannelID        int
    AttemptID        string
    ExpectedRevision uint64
}
```

持久化接口应返回显式 outcome，不用裸 `bool` 混合冲突、忙碌和未知结果：

```go
type ClaimOutcome int
const (
    ClaimAcquired ClaimOutcome = iota
    ClaimBusy
    ClaimSuperseded
)

type CommitOutcome int
const (
    CommitApplied CommitOutcome = iota
    CommitAlreadyApplied
    CommitSuperseded
    CommitStillFenced
)
```

`unknown` 不应被压成成功或 conflict；数据库错误与 outcome 分开返回。

## Claim 协议

### 条件更新

OAuth 前生成 attempt UUID，然后执行等价 CAS：

```sql
UPDATE channels
SET credential_refresh_fence = :attempt_id,
    credential_refresh_started_at = :now
WHERE id = :channel_id
  AND type = :codex_type
  AND deleted_at IS NULL
  AND credential_revision = :expected_revision
  AND credential_refresh_fence IS NULL;
```

结果语义：

- affected rows = 1：`ClaimAcquired`；
- row 不存在、type/revision 已变：`ClaimSuperseded`；
- row 仍是同一 credential，但 fence 非空：`ClaimBusy`；
- DB error：结果未知，reload；只有 row 完整匹配自己的 attempt 才可视为 acquired。

`RotateOnce` 必须先从 authoritative row 读取 revision 与 credential，再执行 Claim。所有 credential 写路径都推进 revision，因此 CAS 不需要重复比较整段 key。若仍存在绕过 revision 的 direct key update，则迁移尚未完成，不能启用新协议。

Claim 不触发 channel route publication，也不删除 access token cache。它改变的是 refresh 权限，不是当前 access token 的可用性。

### Peer 行为

peer 看到 `ClaimBusy` 后只能：

1. 在当前 request deadline 内短暂等待并 reload；
2. 如果旧 access token 尚未过期，继续使用旧 access token；
3. 如果请求必须 refresh，返回 typed unresolved/reauthorization error。

peer 绝不能退回 Redis lock、local snapshot 或旧 DB key 再次发 OAuth。

## OAuth dispatch outcome

OAuth exchange 应返回结构化 outcome：

```text
NotDispatched          请求确定未越过网络 dispatch boundary
RejectedNotConsumed    上游 contract 明确证明 refresh token 未被消费
Rotated(K1)            收到完整、可持久化的新 credential
Ambiguous              请求可能发送，但无法证明是否消费或无法获得完整 K1
```

保守规则：

- request 构造、proxy URL 解析、调用 HTTP client 前的 context cancellation 可以是 `NotDispatched`；
- 一旦调用可能 dispatch 网络请求，transport timeout/reset/read error 默认是 `Ambiguous`；
- 非 2xx 只有在 OAuth provider contract 明确保证 token 未消费时才能是 `RejectedNotConsumed`；
- 2xx body 超限、解析失败、缺少必要 rotated credential 是 `Ambiguous`；
- `Ambiguous` 保留 fence，并返回需要人工重新授权的 typed error。

不要把“错误可重试”与“旧 refresh token 可安全重用”混为一谈。

## Commit 协议

拿到完整 `K1` 后执行：

```sql
UPDATE channels
SET key = :rotated_key,
    credential_revision = credential_revision + 1,
    credential_refresh_fence = NULL,
    credential_refresh_started_at = NULL
WHERE id = :channel_id
  AND type = :codex_type
  AND deleted_at IS NULL
  AND credential_revision = :expected_revision
  AND credential_refresh_fence = :attempt_id;
```

只有这条 durable UPDATE 成功后，provider 才能：

- 发布 rotated credential 到 runtime provider；
- 写 content-addressed access token cache；
- 让等待 peer 观察并采用新 credential；
- 报告 refresh 成功。

### Commit 返回错误或 affected rows = 0

必须 reload authoritative row 并分类：

| 最新状态 | 结论 |
| --- | --- |
| `key=K1, revision=G+1, fence=NULL` | commit 已生效，可能只是响应丢失；按成功处理 |
| `key=K0, revision=G, fence=A` | 仍由当前 attempt fenced；允许在短预算内重试 Commit |
| not found / deleted / type changed | lifecycle 显式胜出；丢弃 K1，不得写回 |
| key 或 revision 已变化 | manual/newer credential 显式胜出；丢弃 K1 |
| fence 是另一个 attempt | 当前 ticket stale；不得清除或提交 |
| reload 也失败 | 保持未知；不清 fence、不发布 K1 |

Commit 可以在当前进程持有 `K1` 时进行短小、有界、带 jitter 的重试，以吸收瞬时 DB 抖动。默认方案不把 `K1` 放入无限期 process-local map，也不依赖下一次请求或 master cron。

重试预算耗尽后：

- 返回 typed persistence/reauthorization error；
- DB fence 保留；
- 当前请求不使用未 durable 的新 access token；
- 进程退出后允许丢失 `K1`，由管理员重新授权恢复。

这是明确接受的 availability trade-off，不是隐藏的 recovery 承诺。

## CancelBeforeDispatch 协议

只有 OAuth outcome 为 `NotDispatched`，或 provider contract 严格证明 `RejectedNotConsumed`，才能执行：

```sql
UPDATE channels
SET credential_refresh_fence = NULL,
    credential_refresh_started_at = NULL
WHERE id = :channel_id
  AND type = :codex_type
  AND deleted_at IS NULL
  AND credential_revision = :expected_revision
  AND credential_refresh_fence = :attempt_id;
```

Cancel 不递增 revision，因为 credential lifecycle 没有变化。

Cancel 失败时不做宽松清理；reload 后若仍属于当前 attempt 可重试，否则按最新状态返回。不存在按 channel ID 无条件清 fence 的 fallback。

## 管理员 Supersede 与 channel 生命周期

### 手工替换 credential

管理员提供与当前 durable key 不同的新 credential 时，使用一个专用 mutation 原子执行：

```text
key = Kmanual
credential_revision++
fence = NULL
refresh_started_at = NULL
```

这表示管理员用新的 credential 显式 supersede 所有旧 attempt。

仅重复提交相同 key 不能证明 refresh token 已重新授权，因此不能解除 unresolved fence。UI/API 应返回“需要新的 credential”，而不是提供 force clear。

### Type change

任何进入或离开 Codex 的 type change 都推进 `credential_revision`。

- Codex -> 非 Codex：旧 ticket 因 revision/type 不匹配永久失效；若原 fence 非空则保留，防止未来把相同旧 key 改回 Codex 后重新消费。
- 非 Codex -> Codex：必须同时提交新的 Codex credential；该专用 mutation 推进 revision 并清除旧 fence。
- 不允许仅切回 Codex 并复用一个 unresolved 的旧 key。

### Soft delete 与 restore

- soft delete 在同一事务中推进 `credential_revision`，保留非空 fence；最终 Commit 还必须匹配 `deleted_at IS NULL`；
- delete 后不存在 process-local rotated secret journal，因此不会因 default scope 的 not-found 永久保留新 secret；
- restore 必须视为新的 lifecycle incarnation；若旧 row 有 unresolved fence，恢复时必须同时提供新 credential；
- hard delete 可以物理删除 fence，因为 channel identity 同时消失；旧 ticket 仍因 attempt/revision/row 不匹配无法写入新 row。

### 普通 channel 更新

name、group、models、status、proxy 等不改变 credential identity 的 mutation：

- 不写 `credential_revision`；
- 不写或清除 fence；
- generic `Select("*")` / map update 必须显式 omit protocol fields；
- controller 不直接操作 fence，由 model credential mutation 统一持有协议。

## 统一 refresh 流程

普通刷新、auto refresh 与 forced refresh 收敛为：

```text
RotateOnce(ctx, channelID, reason, duePolicy)
    -> Load authoritative credential state
    -> If valid cached/current access token satisfies duePolicy: return
    -> ClaimRotation
       -> busy: wait/reload/use still-valid access token/fail typed
       -> superseded: reload and re-evaluate
       -> acquired: continue
    -> ExchangeOAuthExactlyOnce
       -> not-dispatched/rejected-not-consumed: CancelBeforeDispatch
       -> ambiguous: leave fence, surface reauthorization required
       -> rotated: CommitRotation with bounded retry
    -> Publish runtime/cache only after durable Commit
```

调用方差异只保留在 `duePolicy`：

| 调用方 | due 条件 | fence busy 时 |
| --- | --- | --- |
| request-time normal refresh | access token 接近或已经过期 | 尚有效则 fallback；否则 typed error |
| scheduled auto refresh | 进入 refresh lead | 记录 skipped/busy，不发 OAuth |
| forced refresh | upstream 已对 access token 返回 401/403 | 短暂等待 peer；仍 fenced 则 typed error |
| usage/reset-credit forced refresh | usage endpoint 401/403 | 与普通 forced refresh 完全相同 |

## 包与职责边界

### `model`

持有唯一 persistence contract：

- Claim CAS；
- Commit CAS；
- Cancel-before-dispatch CAS；
- manual credential replacement；
- type/delete/restore revision mutation；
- authoritative reload 与 outcome 分类所需 snapshot。

建议集中在独立文件，例如 `model/channel_credential_rotation.go`，避免继续向通用 `channel.go` 堆叠 provider orchestration。

### `providers/codex`

负责：

- due policy；
- OAuth request/response 与 dispatch outcome 分类；
- `RotateOnce` orchestration；
- bounded Commit retry；
- runtime credential/cache publication；
- typed errors、指标与安全日志。

provider 不直接拼 GORM query，不自行清 fence。

### `controller`

负责把管理员 credential/type/delete 操作路由到 model 的专用 mutation，不暴露 clear-fence endpoint。

### `cron` 与节点角色

cron 只决定何时做 proactive refresh，不参与 pending credential correctness。slave、master、手工 API 与 request path 都遵守相同 DB protocol。

## Redis 与本地锁的角色

DB fence 落地后：

- Redis refresh lock 不再属于 correctness path；
- 可以在迁移完成后删除 Redis lock、poll/reload 和 release script；
- 本地 per-channel mutex 可以保留为减少同进程无效 DB Claim 的优化；
- 无论 Redis 是否启用或可达，refresh safety contract 相同；
- 任何优化失效都只能增加 DB contention，不能允许第二次 OAuth。

## 故障与竞态矩阵

| 场景 | durable 状态 | 允许再次 OAuth | 处理 |
| --- | --- | --- | --- |
| DB 在 Claim 前不可用 | READY 或未知 | 否 | 返回错误；OAuth 调用数为零 |
| Claim response 丢失但实际成功 | FENCED(A) | 仅 A 在 reload 确认后可首次 dispatch | reload 完整 ticket identity |
| owner Claim 后、dispatch 前崩溃 | FENCED(A) | 否 | false-positive block，人工替换 credential |
| OAuth 明确未 dispatch | FENCED(A) | Cancel 成功后可以 | ticket-scoped Cancel |
| OAuth transport/read/parse ambiguous | FENCED(A) | 否 | typed reauthorization error |
| OAuth 成功、Commit 短暂失败 | FENCED(A) | 否 | owner 有界重试 Commit |
| OAuth 成功、owner 在 Commit 前崩溃 | FENCED(A) | 否 | K1 丢失，人工重新授权 |
| Commit 已成功但 response 丢失 | READY(K1,G+1) | 按新 credential 正常判断 | reload 识别 already applied |
| peer 使用 stale routing snapshot | DB 可能 FENCED | 否 | Claim CAS 失败；可继续用仍有效 access token |
| admin 换新 key 与 owner completion 竞态 | revision/key 改变 | 仅新 credential 后续 attempt | admin 胜出，旧 K1 丢弃 |
| type 改变但 key 保留 | revision/type 改变 | 否 | stale Commit 失败，fence 保留 |
| soft delete 与 owner completion 竞态 | deleted + revision 改变 | 否 | delete 胜出，旧 K1 丢弃 |
| channel 改走后再改回相同 type/key | revision 已改变 | 否，除非提交新 credential | 防止 ABA |
| Redis 故障或未配置 | 不影响 DB fence | 由 DB 决定 | safety 不降级 |
| slave request 触发 refresh | 与 master 相同 | 仅 Claim winner | 不依赖 slave cron |

## 可观测性与管理员恢复

### Typed 状态

至少区分：

- `credential_refresh_in_progress`：短窗口内另一个 attempt 正在处理；
- `credential_refresh_unresolved`：fence 持续存在，无法安全自动 refresh；
- `credential_reauthorization_required`：需要管理员提供新 credential；
- `credential_refresh_superseded`：当前 attempt 被更新的 credential/lifecycle 取代。

不要把这些状态都压成普通 401，否则管理员无法区分上游拒绝、并发刷新和安全 fence。

### 指标与日志

建议指标：

```text
codex_credential_rotation_claim_total{outcome}
codex_credential_rotation_total{outcome,reason}
codex_credential_rotation_fenced_channels
codex_credential_rotation_commit_retry_total{outcome}
codex_credential_rotation_unresolved_total{reason}
```

日志只记录 channel ID、attempt ID 的短 hash、revision、reason 和 outcome，不记录 key、access token、refresh token 或 upstream body secret。

`started_at` 可以支持管理员页面显示 fence 年龄，但年龄只影响提示与告警，绝不触发自动清理。

### 恢复动作

唯一安全的默认恢复动作是：

```text
Reauthorize / import new credential
    -> validate new credential shape
    -> atomic ReplaceCredential
    -> revision++ and fence=NULL
```

不提供“我知道风险，继续用旧 token”按钮。若未来确需 break-glass，必须是独立、强审计、默认关闭的运维能力，且不属于本文目标协议。

## 可删除的旧复杂度

新协议完整启用后，应删除而不是并行保留：

- `pendingCredentialCommits` process-local journal；
- `ambiguousCredentialRefreshes` process-local breaker；
- `ReconcilePendingCredentials` 及其 auto-refresh/cron 接线；
- Redis refresh lock、TTL、poll loop、release script；
- normal refresh 与 forced refresh 两套重复锁/等待/commit orchestration；
- “下一次请求恰好落到原进程才恢复”的隐式行为。

保留这些旧机制作为第二套 authority 会让协议再次出现双真相。迁移期可以短暂保留代码，但 fence 启用后它们不能影响是否允许 OAuth。

## 迁移与发布顺序

### Phase 1：Schema 与 persistence primitives

1. 增加 revision/fence/started-at 字段，现有 row 初始化为 READY revision 0。
2. 实现 model 层 Claim/Commit/Cancel/Replace/Lifecycle mutation。
3. 给 generic channel update 增加 protocol-field omit，禁止 controller 直接写内部字段。
4. 完成 DB 级 CAS、ABA 与 lifecycle 单元测试。

此阶段不能启用 OAuth fence；旧节点仍会忽略它。

### Phase 2：统一 provider executor

1. 引入 `RotateOnce`，让 normal、auto、forced、usage/reset-credit refresh 全部接入。
2. OAuth exchange 返回结构化 dispatch outcome。
3. runtime/cache publication 移到 durable Commit 之后。
4. 接入 typed error、指标、管理员状态展示。

### Phase 3：协调启用

旧二进制不认识 DB fence，mixed-version deployment 会破坏安全不变量。启用时必须：

1. 暂停 Codex 自动刷新并 drain 会触发 request-time refresh 的旧节点；
2. 协调升级全部 API 节点；
3. 确认所有节点都执行 Claim-before-dispatch；
4. 再启用 Codex refresh。

不为这个一次性切换引入长期 dual-read/dual-write 协议。

### Phase 4：删除旧协议

1. 删除 process-local pending/ambiguous journal；
2. 删除 scheduled reconciliation；
3. 删除 Redis correctness lock；
4. 收敛测试与监控；
5. 将本文状态从“目标方案”更新为“当前实现”。

### 回滚约束

不能在存在非空 fence 时直接回滚到旧二进制；旧代码会忽略 fence 并重放 durable old token。

回滚前必须暂停 refresh，并确认：

- 所有 fence 已通过成功 Commit 或新 credential replacement 安全解除；
- unresolved channel 已禁用或完成重新授权；
- 不存在仍可能返回的旧 attempt。

## 测试矩阵

### Model CAS

- 同一 channel 两个并发 Claim 只有一个 affected row。
- stale revision、非 Codex、soft-deleted row 均不能 Claim。
- 任意 key mutation 必须原子推进 revision，持有旧 credential snapshot 的 Claim 必须失败。
- 只有 matching attempt 可以 Commit 或 Cancel。
- old attempt 不能清除 newer attempt fence。
- Commit 原子更新 key、revision 和 fence。
- Commit response ambiguous 后 reload 能识别 already applied。

### OAuth dispatch boundary

- DB Claim 失败时 OAuth mock 调用次数为零。
- request build/proxy validation failure 可以安全 Cancel。
- `Do` 后 timeout/reset/read error 保持 fence。
- 2xx malformed/oversized/missing rotated token 保持 fence。
- 完整 rotated response 只调用一次 Commit，不提前发布 runtime token。

### 多节点与缓存

- A 持有 fence 时，B 即使持有旧 channel snapshot 也不能调用 OAuth。
- Redis enabled/disabled/down 三种模式具有相同 safety 行为。
- master/slave 节点都只能通过 DB Claim 成为 owner。
- peer 可以继续使用尚有效的旧 access token，但到期后必须 fail closed。

### Lifecycle 与 ABA

- pending attempt 后 soft delete，旧 Commit 必须失败且不保留 process-local secret。
- pending attempt 后 type 改为非 Codex 且 key 不变，旧 Commit 必须失败。
- Codex -> 非 Codex -> Codex 且 key 字节相同，旧 revision ticket 必须失败。
- key 改走后又改回相同字节，旧 ticket 必须失败。
- manual new credential replacement 清 fence 并推进 revision。
- 重复提交相同 key 不得解除 unresolved fence。
- restore unresolved soft-deleted row 时必须要求新 credential。

### 故障恢复

- owner Claim 后崩溃，fence 跨进程重启保留且无 TTL。
- OAuth 成功、Commit 连续失败，peer 始终不能重放。
- bounded retry 内 DB 恢复时 Commit 成功。
- retry budget 耗尽后返回 typed reauthorization error。
- 管理员提供新 credential 后 channel 恢复，旧 owner 后续返回也不能覆盖。

### 安全与观测

- fence/metrics/logs 不包含 credential secret。
- API channel JSON 不暴露 internal protocol fields。
- 不存在普通 clear-fence endpoint。
- unresolved fence 的年龄不会触发自动清理。

## 接受的 Trade-off

### 获得什么

- one-time refresh token 不会被 peer 自动重放；
- safety 不依赖 Redis TTL、节点角色、下一次请求落点或 owner 存活；
- delete/type/manual update 使用同一个 ticket/revision contract；
- 故障状态共享、非 secret、可观测；
- 可以删除多套锁、journal、reconciler 和 duplicated forced-refresh logic。

### 牺牲什么

- OAuth ambiguous、owner crash、长 DB outage 可能让 channel 需要人工重新授权；
- Claim 后、dispatch 前崩溃也可能保守地 block 一个其实尚未消费的 token；
- 每次真实 refresh 增加一次 DB CAS；
- DB 不可写时不允许自动 refresh；
- 发布必须协调升级，不能让旧节点继续执行 refresh；
- channel schema 增加两个 correctness 字段和一个诊断字段。

### 为什么这是当前甜蜜点

DB 在 OAuth 前刚刚成功完成 Claim，因此“同一个短 HTTP 窗口内 DB 又长期不可写或 owner 崩溃”属于低概率事件。用少量人工恢复换取全局 at-most-once safety，比复制 rotated secret、建立第二持久化 authority 和维护双存储恢复协议更符合 KISS。

## 备选方案与拒绝理由

### 所有节点运行 process-local reconciler

只能改善 originating process 仍存活时的 liveness；peer 仍看不到 pending/ambiguous 事实，重启后仍丢状态。不能作为正确性方案。

### 只允许 master refresh

这是部署约束，不是 leader protocol。它要求唯一 master、无 split-brain、slave 遇 401 可以失败，而且 master 重启仍无法判断旧 token 是否已经消费。

### 延长或永久持有 Redis lock

- 有 TTL：到期后仍会重放；
- 无 TTL：owner pre-dispatch crash 会永久死锁；
- 加续租、phase、fencing 和 crash recovery 后，复杂度已接近本文状态机；
- 普通 cache Redis 未必具有 AOF/fsync/noeviction 的安全边界。

### DB outbox / pending table

经典 outbox 解决“本地先提交，再可靠投递外部命令”。本问题的不可替代结果 `K1` 是外部调用之后才产生；如果 channel DB 此时不可写，写 pending table 与直接更新 channel 同样失败。真正有价值的是外部调用前的 durable intent/fence。

### Shared secret WAL / credential broker

如果业务要求 owner crash 后仍自动恢复 `K1`，必须引入独立于 channel DB 故障窗口的 durable secret authority，并至少具备：

- encryption at rest 与密钥轮换；
- AOF/fsync 或等价 durability；
- no-eviction、无 TTL；
- 严格 ACL、审计和备份；
- secret GC、双存储一致性与灾备演练。

这是更高可用性的有效方案，但显著扩大 refresh token footprint 和运维成本，不作为默认架构。

### 上游非轮换或幂等 refresh

如果上游未来提供关闭 rotation、稳定 refresh token、idempotency key 或查询 attempt result 的正式能力，应优先重新评估并删除本地复杂度。客户端单纯忽略上游返回的新 refresh token 不等于关闭 rotation。

## 完成定义

本文方案只有在以下条件全部满足后才能改为“当前实现”：

1. 所有 OAuth refresh path 都执行 Claim-before-dispatch；
2. DB fence/revision 是是否允许 OAuth 的唯一共享 authority；
3. ambiguous 与 post-success DB failure 永不自动清 fence；
4. channel type/delete/restore/manual credential mutation 全部推进正确的 revision；
5. 不存在绕过 revision 的 direct key update，generic channel update 不能覆盖 protocol fields；
6. process-local pending/ambiguous journal 和 scheduled reconciler 已删除；
7. Redis refresh lock 已退出 correctness path；
8. 多节点、ABA、soft delete、commit ambiguity 与 owner crash 测试通过；
9. 管理员可以识别 unresolved 状态并用新 credential 恢复；
10. 混合版本启用与回滚约束已有明确发布 runbook。
