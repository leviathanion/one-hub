---
title: "Codex Credential Refresh Fence 架构设计"
layout: doc
outline: deep
lastUpdated: true
---

# Codex Credential Refresh Fence 架构设计

## 文档状态

- 状态：当前实现；生产数据库升级需按本文停机步骤执行。
- 适用范围：Codex OAuth 普通、自动与 forced refresh，凭证数据库提交、渠道编辑与生命周期；当前部署为单机。
- 当前实现：`common/credentials` 统一管理刷新协议，渠道只提供 `bizdata + version` 持久化能力。没有旧字段运行时兼容、Redis 刷新锁、内存 pending journal 或 DB-less 提交适配。
- 设计取向：用非 secret、无 TTL 的数据库标记记录不可逆调用；不确定故障保留标记，接受同账号独立 OAuth 授权恢复。
- 关联代码：`common/credentials/rotation.go`、`common/credentials/state.go`、`model/channel_snapshot.go`、`model/channel_bizdata_migration.go`、`providers/codex/base.go`、`controller/codex_oauth.go`。

## 已选型结论

本方案把“旧 refresh token 是否还允许再次 exchange”记录为数据库事实，而不让 Codex 专属字段进入通用渠道表。

核心不变量只有一句：

> `bizdata.credentials.refresh` 存在，表示当前 durable refresh token 可能已经被上游消费；其他 attempt 不得再次用它发起 OAuth refresh。

数据库 fence 不是普通互斥锁：锁回答“当前谁在工作”，fence 记录“一个不可逆动作是否可能已经发生”。锁可以超时，fence 只能由同一 attempt 的安全终态或独立授权恢复解除。

目标状态机（没有其他写入时）：

```text
READY(key=K0, version=V, refresh=absent)
    |
    | Claim(A): DB CAS before OAuth dispatch, version++
    v
FENCED(key=K0, version=V+1, refresh=A)
    |-- Rotated(K1) + Commit(A) ----------> READY(K1, V+2, absent)
    |-- ProvablyNotDispatched + Cancel(A) -> READY(K0, V+2, absent)
    |-- Ambiguous / owner crash ----------> 保持 FENCED，等待独立 OAuth 授权
    `-- 同账号独立 OAuth + Recover -------> READY(Knew, V+2, absent)
```

普通配置编辑也推进 `version`，因此实际终态版本可能更大。版本表示受并发控制的数据变化，不是凭证轮换次数，也不是用量缓存的资源归属 generation。普通、自动和 forced refresh 共用同一协议。

## 真实问题

被替代的旧刷新链路依次执行：

```text
local channel mutex
    -> Redis TTL lock
    -> OAuth exchange(K0) produces K1
    -> process-local pending(K0, K1)
    -> DB CAS(K0 -> K1)
```

它把一个协议事实拆散到了不同故障域：

| 状态 | 旧存放位置 | 其他节点能否可靠观察 | 是否跨进程重启 |
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
6. stale attempt 不能清除或提交到新的 attempt、version 或 channel lifecycle incarnation。
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

Claim 写入回执丢失时必须 reload，确认行仍存活、类型匹配且标记属于自己的 `attempt_id`。配置编辑可能推进版本，但不会转移 attempt 所有权；不能要求回读版本仍等于 Claim 前版本。确认前不得 dispatch。

### 2. Once-possibly-dispatched, never auto-unfence

一旦 HTTP client 可能把 refresh 请求交给网络，只有以下两种事件可以解除 fence：

- rotated credential 已 durable commit；
- 管理员通过同账号独立 OAuth durable recover credential。

超时、断连、成功响应不可解析、DB commit 失败、owner crash 都不能自动解除。

### 3. Fence has no TTL

时间流逝不会让 one-time token 重新安全。`bizdata.credentials.refresh.started_at` 只用于观测和告警，不参与自动回收。

### 4. Attempt-scoped mutation

Commit 或 Cancel 每次读取最新快照，确认渠道存活、类型匹配、`attempt_id` 仍属于自己，再以该快照的 `version` 执行 CAS。允许不改变连接身份的配置编辑推进版本，必须保留它们及未知业务命名空间；旧 attempt 不得清除新 attempt 的标记。

### 5. Lifecycle uses revision, not value coincidence

`type + key` 可能发生 ABA：Codex -> 非 Codex -> Codex，或 key 改走后又改回相同字节，不能只靠值判断并发身份。

这里的逻辑 revision 由通用 `version` 承担：配置、凭证、内部业务状态和删除都推进版本；用量、余额、测速等独立统计列不参与。字段不叫 `credential_revision` 或 `edit_version`，因为它同时约束管理编辑与自动凭证操作。Claim 使用原始版本，独立授权恢复使用固定授权快照；Commit/Cancel 则检查 attempt 所有权后使用最新版本。

### 6. No generic unlock

不提供按 channel ID 无条件清除标记的接口。合法解除仅来自 `credentials.Service` 的：

- `Commit(ticket, rotatedKey)`：新凭证已获得，原子持久化后解除；
- `Cancel(ticket)`：provider 已证明 OAuth 没有 dispatch；
- `Recover(expectedSnapshot, attemptID, newKey)`：provider 已验证同账号的独立授权，并以原授权快照 CAS 恢复。

## 数据模型

渠道持久化采用 `key + other + bizdata + version`：

| 字段 | 责任 | 写入者 |
| --- | --- | --- |
| `key` | 唯一凭证事实来源，支持直接输入 key 与 OAuth | 管理编辑、凭证服务 |
| `other` | 可编辑、复制、按标签同步的连接配置 | 管理编辑、配置同步 |
| `bizdata` | 按业务命名空间组织的服务端内部状态 | 业务服务，经数据库 CAS |
| `version` | 配置、凭证、内部状态和生命周期的并发版本 | 数据库原子递增 |

```go
BizData datatypes.JSON `json:"-" gorm:"column:bizdata;type:json"`
Version uint64         `json:"version" gorm:"not null;default:0"`
```

凭证模块仅拥有以下 JSON 路径，静态 key 无需刷新状态：

```json
{
  "credentials": {
    "refresh": {
      "attempt_id": "每次操作唯一的 UUID",
      "started_at": 1790985600
    }
  }
}
```

- 完成后删除 `credentials.refresh`，空命名空间随之移除；未知命名空间及兄弟字段原样保留。
- 只保存 nonce 和观测时间，不重复保存 token；每个渠道只有一个未决标记，没有历史列表和自动清理 TTL。
- `other` 参与管理表单、复制和标签同步，不能放入会被这些操作复制或清除的运行状态。
- `bizdata` 不参与普通 JSON/导出、复制或标签同步，不接受管理请求提交。新建、复制和标签新增渠道从空业务状态、版本 0 开始。
- 使用现有 GORM JSON 类型，数据库 CAS 更新整段 JSON，不依赖数据库特定 JSON 路径语法。
- 删除旧的 `credential_revision`、`credential_refresh_fence`、`credential_refresh_started_at` 三列，迁移保留原版本与未决状态。

将来其他 OAuth provider 可复用凭证模块；独立业务可拥有自己的命名空间。需要索引、唯一约束、历史或独立生命周期时再设计关系表，不把所有业务都塞进 JSON。

### 为什么不用单独 fence 表

刷新状态与 credential authority 同属渠道行，可以一次 CAS 原子更新 `key + bizdata + version`。单独表会增加跨表事务、软删除清理和生命周期协调，当前单个未决标记没有这些额外成本所换取的查询或生命周期收益。业务规则由凭证服务拥有，通用数据库表不需要 Codex 专属列。

### 为什么不用 credential envelope 覆盖 `Channel.Key`

把 `Key` 变成 `Ready | Refreshing | Blocked` envelope 可以不加字段，但 `Key` 是用户可感知的持久化 credential payload。控制面状态与 secret payload 分列更清晰，也避免 admin/export/import 路径被临时协议状态污染。

## 协议数据类型

provider 持有一次 attempt 的短生命周期 `credentials.Ticket`：

```go
type Ticket struct {
    ChannelID       int
    Type            int
    AttemptID       string
    ExpectedVersion uint64
}
```

凭证服务通过 `Store.Load` 获取包含 key、类型、版本、业务数据和删除状态的快照，通过 `Store.CompareAndSwap` 执行原子写入。接口不认识 Codex、JWT 或 OAuth 端点。

Claim 返回 `ClaimAcquired / ClaimBusy / ClaimSuperseded`；Commit 返回 `CommitApplied / CommitAlreadyApplied / CommitSuperseded / CommitStillFenced`。数据库错误单独返回，不能把未知结果压成成功或普通冲突。

## Claim 协议

### 条件更新

provider 先读取权威凭证并确认渠道类型。凭证服务检查原版本及空刷新状态，生成带 attempt UUID 的新 JSON，再执行：

```sql
UPDATE channels
SET bizdata = :updated_bizdata,
    version = version + 1
WHERE id = :channel_id
  AND version = :expected_version
  AND deleted_at IS NULL;
```

- 成功写入：`ClaimAcquired`；已有其他 attempt：`ClaimBusy`。
- 行不存在、已删除、类型或 Claim 前版本已变：`ClaimSuperseded`。
- 写入错误或 CAS 未命中：重新读取；只有类型、存活状态及自己的 attempt 所有权确认后，才认为已 acquired。

所有运行时凭证和类型修改都推进版本，所以 SQL 无需重复比较 type 或整段 secret key。类型及业务状态检查在 provider/凭证服务边界执行，数据库层只提供中性 CAS。

Claim 不发布路由或清理 access token cache，只改变后续 refresh 权限。

### Peer 行为

同机请求先经过 per-channel mutex，等待前一个请求结束后读取并复用已提交凭证。看到持久化 `ClaimBusy` 时，不重新 exchange：尚有效的 access token 可以继续服务，需要新 token 时返回 typed 状态。

Redis、过期等待或旧内存快照都不能成为绕过标记的理由。类型已变化或 attempt 已被取代时，也不能回退使用旧 provider token。

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

当前 Codex 实现没有把非 2xx 默认视为 `RejectedNotConsumed`；该分类只有上游明确提供未消费保证时才可使用。

## Commit 协议

拿到完整 `K1` 后，凭证服务读取最新行并确认原 attempt 所有权；删除自己的刷新标记、保留其他业务数据，然后执行：

```sql
UPDATE channels
SET key = :rotated_key,
    bizdata = :updated_bizdata,
    version = version + 1
WHERE id = :channel_id
  AND version = :latest_version
  AND deleted_at IS NULL;
```

只有 durable commit 成功或回读确认已成功，才发布 runtime credential、写 token cache 并报告成功。获得新凭证后，provider 使用不继承请求取消、有 5 秒上限的提交上下文，避免客户端断开直接丢弃已轮换结果。

### Commit 返回错误或 affected rows = 0

重新读取 authoritative row：

| 最新状态 | 结论 |
| --- | --- |
| 存活且类型匹配、`key=K1`、版本大于 ticket 原版本、无刷新标记 | 已提交，可能只是回执丢失 |
| 存活且类型匹配、标记仍属于 A | 可以基于最新版本重试数据库 CAS，保留并发元数据编辑 |
| 不存在、已删除或类型改变 | 丢弃 K1，不写回、不使用旧 provider token |
| 标记属于其他 attempt，或无标记但不是已提交 K1 | 当前 attempt 被取代，不得覆盖 |
| 回读失败或 JSON 无法解析 | 保持未知，不清标记、不发布 K1 |

最多执行三次数据库 CAS，再作一次最终回读确认。不会重发 OAuth，也不将 K1 放进等待下一次请求或 cron 处理的内存 journal。

预算耗尽后保留未决状态，返回 typed persistence/reauthorization error；未落库的新 token 不进入缓存。进程退出后允许丢失 K1，由同账号独立授权恢复。

## CancelBeforeDispatch 协议

只有 provider 能证明旧 refresh token 尚未 dispatch，才调用 `Cancel(ticket)`。凭证服务读取最新快照并验证存活、类型和原 attempt 所有权，移除对应 JSON 标记，然后执行：

```sql
UPDATE channels
SET bizdata = :updated_bizdata,
    version = version + 1
WHERE id = :channel_id
  AND version = :latest_version
  AND deleted_at IS NULL;
```

Cancel 同样推进版本，因为内部业务状态发生了变化。CAS 竞争最多尝试三次，每次重新确认 owner；数据库错误直接返回，没有无条件清理或 TTL fallback。当前上游没有明确的“已发送但未消费”保证时，不得仅凭 HTTP 错误取消。

## 管理员 Supersede 与 channel 生命周期

### 手工替换 credential

没有未决刷新时，用户可通过带 `expected_version` 的显式身份编辑替换 key，写入原子推进版本。

存在未决刷新时，普通 key/连接身份编辑被拒绝；恢复必须重新完成同账号的独立 OAuth。授权会话固定渠道 ID、账号、version 和 attempt，provider 验证账号证据后调用 `Recover`：

```text
原始授权快照 CAS
    -> key = 新授权凭证
    -> 删除原 attempt 的 refresh 标记，保留其他 bizdata
    -> version++
```

期间任何版本或 attempt 变化都使原授权提交失败。迟到的旧 Commit/Cancel 无法覆盖新凭证；重复提交相同 key 或人工只清标记都不能替代恢复流程。

### Type change

类型修改属于显式连接身份编辑，必须带正确版本并推进 `version`；有未决刷新时禁止执行。

旧 Codex provider 每次从权威行读取凭证时检查类型。渠道已改为其他 provider 时，停止刷新和旧 token 回退，不得采用另一个 provider 的 key。没有未决刷新时发生 type ABA，旧 Claim 仍因版本不匹配失败。

### Soft delete 与 restore

- soft delete 原子推进 `version` 并保留未决标记；后续凭证 CAS 必须匹配存活行。
- 没有 process-local secret journal，不会因默认 scope 隐藏已删除行而无限保留新 token。
- 当前没有 restore API。将来增加时必须推进版本并保留未决保护，不能恢复后复用可能已消费的旧 refresh token。
- 迁移包含软删除行，不能在升级时丢掉它们的历史版本和未决状态。

### 普通 channel 更新

配置、名称、分组、模型、状态、优先级、权重、标签等管理修改都推进 `version`，不写凭证业务数据。余额、用量、测速等统计走独立指定列或原子增量更新，不推进此版本。

- 单渠道 GET 返回 `version`；编辑和列表快捷操作必须提交打开时的 `expected_version`。
- 标签 GET 返回成员版本 `versions`；提交 `expected_versions`，成员集合或任何成员版本冲突均整批回滚。
- 冲突返回 HTTP 409，服务端不把旧输入套到新快照上重试；前端保留输入并提示重新核对。
- OAuth 独立保存后读取新快照；仅配置仍与表单初始快照一致、凭证仍是刚保存值时推进表单版本，不清掉未保存输入。
- 未决刷新允许不改变连接身份的元数据编辑，Commit 重新确认 attempt 后保留这些修改。
- 保留保存前影响范围确认；确认弹窗不能替代后端版本和身份检查。

## 统一 refresh 流程

普通、auto、forced 和 usage/reset-credit 刷新共用 provider 的执行流程：

```text
local channel mutex
    -> load authoritative channel and check provider type
    -> reuse current credential when refresh is unnecessary
    -> credentials.Service.Claim
    -> OAuth exchange once
       -> not dispatched: Cancel
       -> ambiguous: retain marker, return typed error
       -> rotated: Commit with bounded DB retries
    -> publish runtime/cache only after durable commit
```

调用方只决定是否需要刷新。forced 路径通过最初权威快照比较请求凭证与当前凭证，不额外重复读取；之后的竞争由 CAS 检测。类型变化和 superseded 直接终止，不能走旧 token fallback。

## 包与职责边界

`common/credentials` 拥有状态格式及 Claim、Commit、Cancel、Recover 规则，通过 `Store` 接口使用数据库快照和 CAS；不包含 provider 账号判断或 OAuth 网络请求。

### `model`

`model.ChannelCredentialStore`（`channel_snapshot.go`）只提供权威读取和存活行的 `id/version` CAS，一次写入原子更新 bizdata、可选 key 和 version；含凭证的 SQL 禁止日志输出。

管理编辑由 model 处理白名单、版本和事务，调用通用凭证策略检查未决状态。Codex JWT、账号解析和 OAuth 编排不进入通用持久化层。

### `providers/codex`

负责刷新时机、渠道类型检查、OAuth 网络协议和 dispatch 分类、账号证据验证、调用通用凭证服务，以及 durable commit 后的 runtime/cache 发布。typed errors、指标和脱敏日志仍由 provider 提供。

数据库提交重试规则集中在 `common/credentials`；provider 不拼 GORM query，不自行清除状态，也不保留无用途的方法别名或第二套提交实现。

### `controller`

处理管理编辑版本协议和 OAuth 授权会话。独立授权固定原渠道快照，完成账号校验后提交恢复；不暴露 clear-fence endpoint，也不接受普通请求直接修改 bizdata。

### `cron` 与节点角色

cron 只决定何时做 proactive refresh，不参与 pending credential correctness。slave、master、手工 API 与 request path 都遵守相同 DB protocol。

## Redis 与本地锁的角色

- Redis 刷新锁、poll/reload 和 release script 已删除；Redis 仍可用于 OAuth 会话状态与缓存，但不决定刷新所有权。
- 保留本地 per-channel mutex，让同机请求等待胜者并复用提交结果。消融实验移除它后第二个并发请求立即返回 busy，保留时两个请求获得同一新 token、OAuth 仅调用一次。
- 数据库标记保证崩溃和重启后不能重放；本地锁无法替代它，Redis 是否启用也不改变这个边界。
- 用量缓存是可丢弃的展示数据。OAuth CAS 不推进用量 generation，条目仍按凭证指纹隔离；清缓存不清业务状态。

## 故障与竞态矩阵

| 场景 | durable 状态 | 处理 |
| --- | --- | --- |
| Claim 前 DB 不可用 | READY 或未知 | 不发 OAuth |
| Claim 回执丢失 | 可能 FENCED(A) | 回读确认存活、类型与 attempt 后才首次发送 |
| Claim 后、dispatch 前崩溃 | FENCED(A) | 保守保留，独立授权恢复 |
| 明确未 dispatch | FENCED(A) | 仅原 attempt 可以 Cancel |
| 网络、响应读取或解析歧义 | FENCED(A) | 不重发 OAuth，不自动清标记 |
| OAuth 成功、Commit 短暂失败 | FENCED(A) | 有界重试数据库 CAS |
| Commit 回执丢失 | 可能 READY(K1) | 最终回读识别已提交 |
| OAuth 成功、进程在 Commit 前退出 | FENCED(A) | 新凭证丢失，需要独立授权 |
| 并发元数据编辑 | version 递增，owner 不变 | Commit 使用最新版本保留编辑 |
| 未决时普通身份编辑 | FENCED(A) | 拒绝编辑，必须独立授权恢复 |
| 独立授权恢复与旧完成竞态 | 新 version/key，无旧标记 | 原快照 CAS 决定胜负，旧 attempt 不覆盖 |
| 类型已改为其他 provider | 新 version/type | 旧 provider 停止刷新与旧 token 回退 |
| soft delete 与旧完成竞态 | deleted + version 递增 | 删除后旧提交失败 |
| type/key 改走再改回 | version 已变 | 原 Claim 快照失效 |
| Redis 故障、缓存清理或进程重启 | 持久化标记不变 | 不因此恢复旧 refresh 权限 |

## 可观测性与管理员恢复

### Typed 状态

至少区分：

- `credential_refresh_in_progress`：短窗口内另一个 attempt 正在处理；
- `credential_refresh_unresolved`：fence 持续存在，无法安全自动 refresh；
- `credential_reauthorization_required`：需要管理员提供新 credential；
- `credential_refresh_superseded`：当前 attempt 被更新的 credential/lifecycle 取代。

不要把这些状态都压成普通 401，否则管理员无法区分上游拒绝、并发刷新和安全 fence。

### 指标与日志

现有指标：

```text
codex_credential_rotation_claim_total{outcome}
codex_credential_rotation_total{outcome,reason}
codex_credential_rotation_unresolved_total{reason}
```

日志不输出 key、access token、refresh token 或上游 body secret，凭证 SQL 同样禁止泄漏参数。

`started_at` 用于区分近期操作与长期未决状态，不能触发自动清理。bizdata 不在普通渠道 JSON 中暴露；未来增加管理展示时应提供经过筛选的状态，而非直接公开内部 JSON。

### 恢复动作

默认恢复路径是同账号独立 OAuth：

```text
固定 channel ID / account / version / attempt 的授权会话
    -> 独立 OAuth 获取完整 credential
    -> provider 验证账号证据
    -> Recover 以原始快照 CAS
    -> 新 key + 删除原刷新标记 + version++
```

会话只消费一次；渠道快照发生变化时重新授权。不提供 raw key 强制解除、仅清标记或继续使用旧 refresh token 的按钮。

## 可删除的旧复杂度

当前实现已删除：

- `pendingCredentialCommits` process-local journal；
- `ambiguousCredentialRefreshes` process-local breaker；
- `ReconcilePendingCredentials` 及其 auto-refresh/cron 接线；
- Redis refresh lock、TTL、poll loop、release script；
- normal refresh 与 forced refresh 两套重复锁/等待/commit orchestration；
- “下一次请求恰好落到原进程才恢复”的隐式行为。

保留这些旧机制作为第二套 authority 会让协议再次出现双真相。不保留旧字段双读、双写或 DB-less 运行时兼容。

## 迁移与发布顺序

采用单机停机、一步迁移。以下 Phase 是同一次升级的准备、替换、启动和验收步骤，不是允许新旧程序共存的分阶段发布；升级期间不接收业务流量。

### Phase 1：Schema 与 persistence primitives

1. 验证 `bizdata/version`、通用凭证服务、CAS 与离线迁移测试，准备配套前后端产物。
2. 停止整个旧实例及后台任务，备份完整数据库和旧程序。SQLite 备份必须包含一致的数据库状态，不能遗漏尚在 WAL 中的数据。
3. 不手动清理旧 fence；已有未决状态必须被迁移保留。

### Phase 2：统一 provider executor

1. 同时替换后端与前端，所有刷新入口均使用新协议。
2. 新程序不包含旧字段读写、DB-less 提交或旧 Redis 刷新锁；不存在运行时切换开关。
3. 保持停流，不能让旧程序在新 schema 上继续运行。

### Phase 3：协调启用

1. 启动新实例，迁移 `202610030001` 在渠道 AutoMigrate 前执行，增加 `bizdata/version`。
2. 按批转换全部渠道，包括软删除行；搬迁旧 revision、attempt 和 started-at，保留未知业务命名空间。
3. 回填事务提交后依次 DROP 旧 timestamp、fence、revision 三列。
4. MySQL DDL 可能隐式提交，迁移支持中断后重启继续；这只是迁移恢复，不是业务兼容。
5. JSON 损坏或新旧 attempt 冲突时迁移失败，保留旧列供核查，不猜测或自动清除状态。

### Phase 4：删除旧协议

1. 验收迁移完成，确认旧三列已删除，只有新 schema 与新运行时协议。
2. 检查保留的未决渠道，按同账号独立 OAuth 恢复，不复用原 refresh token。
3. 恢复流量，刷新浏览器并重新打开编辑表单；升级前未完成的 OAuth 会话重新发起。
4. 确认状态、日志和凭证刷新正常；生产升级需要实际执行这些步骤，代码测试不代表已完成生产迁移。

### 回滚约束

迁移不可自动逆转，不能仅换回旧二进制。必须停止新程序，恢复完整的升级前数据库备份和旧程序，再检查备份内的未决状态；升级后新增数据也会随备份恢复而丢失。

## 测试矩阵

### Model CAS

- 并发 Claim 只有一个胜者，原版本失效、删除或类型不符时不交换 OAuth。
- 只有 matching attempt 可以 Commit/Cancel，全部业务状态写入递增版本。
- Commit 保留并发元数据和未知 JSON 命名空间；回执丢失后最终回读可确认成功。
- 单渠道旧表单返回冲突，标签成员/版本冲突整批回滚。
- SQLite、MySQL、PostgreSQL 覆盖静态 key、未决刷新、软删除行迁移、旧列删除与中断 DDL 恢复。

### OAuth dispatch boundary

- DB Claim 失败时 OAuth mock 调用次数为零。
- request build/proxy validation failure 可以安全 Cancel。
- `Do` 后 timeout/reset/read error 保持 fence。
- 2xx malformed/oversized/missing rotated token 保持 fence。
- 完整 rotated response 不重复 exchange；数据库提交可有限重试，不提前发布 runtime token。

### 多节点与缓存

当前部署为单机；独立 provider 实例与并发请求仍需验证相同持久化边界：

- A 持有标记时，B 或新实例不能用旧快照再次 OAuth。
- 本地锁使并发请求复用一次成功刷新，Redis 配置不改变持久化保护。
- 仍有效的 access token 可以用于普通请求；类型变化或 superseded 禁止旧 token fallback。
- 未持久化 token 不进入缓存；OAuth CAS 不推进用量 generation。

### Lifecycle 与 ABA

- 未决 attempt 后 soft delete，旧 Commit 失败且不产生内存 secret journal。
- 未决时 type/key 身份编辑被拒绝，无未决时 type ABA 使旧 Claim 快照失效。
- 类型已改为其他 provider 后旧 Codex 实例不能采用新 key 或回退旧 token。
- 同账号独立授权以原 version/attempt 恢复，迟到提交和取消不能覆盖新凭证。
- 重复 key、账号不匹配或授权期间快照变化不能解除未决状态。
- 迁移保留软删除行的版本和标记；未来 restore API 必须补充生命周期验证。

### 故障恢复

- owner Claim 后崩溃，fence 跨进程重启保留且无 TTL。
- OAuth 成功、Commit 连续失败，peer 始终不能重放。
- bounded retry 内 DB 恢复时 Commit 成功。
- retry budget 耗尽后返回 typed reauthorization error。
- 管理员提供新 credential 后 channel 恢复，旧 owner 后续返回也不能覆盖。

### 安全与观测

- fence/metrics/logs 不包含 credential secret。
- API channel JSON 不暴露 bizdata；version 公开用于编辑 CAS。
- 不存在普通 clear-fence endpoint。
- unresolved fence 的年龄不会触发自动清理。

## 接受的 Trade-off

### 获得什么

- one-time refresh token 不会被 peer 自动重放；
- safety 不依赖 Redis TTL、节点角色、下一次请求落点或 owner 存活；
- delete/type/manual update 使用统一的 version/CAS 与 attempt 所有权规则；
- 故障状态共享、非 secret、可观测；
- 可以删除多套锁、journal、reconciler 和 duplicated forced-refresh logic。

### 牺牲什么

- OAuth ambiguous、owner crash、长 DB outage 可能让 channel 需要人工重新授权；
- Claim 后、dispatch 前崩溃也可能保守地 block 一个其实尚未消费的 token；
- 每次真实 refresh 增加一次 DB CAS；
- DB 不可写时不允许自动 refresh；
- 发布必须协调升级，不能让旧节点继续执行 refresh；
- channel schema 使用两个通用字段 bizdata/version，替换三个专属字段；配置编辑与自动凭证写入竞争时，旧表单会收到版本冲突。

### 为什么这是当前甜蜜点

DB 在 OAuth 前刚刚成功完成 Claim，因此“同一个短 HTTP 窗口内 DB 又长期不可写或 owner 崩溃”属于低概率事件。用少量人工恢复换取全局 at-most-once safety，比复制 rotated secret、建立第二持久化 authority 和维护双存储恢复协议更符合 KISS。

当前单机允许停机升级，适合一次性删除旧列与旧路径。通用 JSON 命名空间加版本 CAS 已能表达所需原子性，不需要 provider_state、额外状态表或凭证副本。

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

1. 所有 OAuth refresh path 执行 Claim-before-dispatch，持久化标记是唯一恢复依据。
2. ambiguous 和最终提交失败不自动清标记，只有安全取消、成功提交或独立授权恢复可以解除。
3. 配置、凭证、内部业务状态和删除推进 version；统计列独立更新。
4. provider 规则、通用凭证协议和数据库 CAS 各自集中，未知业务命名空间不会被覆盖。
5. 旧字段只出现在一次性迁移及相应测试中，没有运行时兼容或双写。
6. 内存 pending/ambiguous journal、scheduled reconciler 和 Redis 刷新锁已删除。
7. 并发、ABA、软删除、提交歧义、请求取消和 owner crash 的关键测试通过。
8. 管理编辑使用 expected_version，标签更新使用 expected_versions，冲突可明确识别。
9. 同账号独立 OAuth 可恢复未决状态，旧 attempt 不能覆盖恢复结果。
10. 单机停机迁移、旧列删除与完整备份回滚步骤明确。
