---
title: "用户自动分组最终一致性修复方案"
layout: doc
outline: deep
lastUpdated: true
---

# 用户自动分组最终一致性修复方案

## 文档状态

- 状态：当前实现。
- 适用范围：`quota`、`used_quota` 与自动分组的最终一致性，用户资料按操作更新，以及管理员零额度重算。

本方案修复 P1「预扣期间重分组，消费结束后没有恢复」和 P2「登录回写旧用户对象，撤销奖励晋级」。已按本方案实施。

后续精简与用户管理修复见[用户管理消融实验与精简方案](./user-management-ablation-plan.md)。其中所属组展示缓存已通过消融实验并删除；本篇保留额度核心与首次实施版本的记录。

## 1. 结论与边界

采用**正常结算自动收敛，异常情况人工重算**：

- 保留 `quota`、`used_quota`、`group`，不新增数据库字段、表、队列、后台扫描或分布式计数器。
- 自动分组继续按额度区间升降，包含充值、兑换、奖励和管理员增减。
- 最终消费额度与余额结算在同一 SQL 事务中更新，随后在该事务中计算用户组。
- Try 只预扣余额；Confirm、Cancel 以及其他最终余额调整会重算分组。接受多个请求在途时暂时匹配较低区间。
- 管理员可提交明确的 `quota=0`，只重算用户组；不需要先加额度再扣回。
- 用户资料按操作更新字段，取消把整份 User 对象保存回数据库的通用入口。

这里的最终一致性是：用户的在途操作均已完成结算、分组规则稳定后，分组与已提交的 `quota + used_quota` 匹配。有匹配区间才切换；没有匹配项时沿用已有规则保留原组。

短请求在进程崩溃、数据库持续故障或提交结果不明后，仍遵循现有 best-effort 计费边界。手动重算能够使分组与现有 SQL 数值一致，不能还原已经丢失的付款或消费证据，也不能判断一笔不明结算应当重放。这类资金异常继续人工核查。

## 2. 改动前核查的完整链路

| 链路 | 当前入口和流向 | 本次应处理的位置 |
| --- | --- | --- |
| 在线充值 | 支付通知/认证查询 → `CompleteOrderPayment` → `CreditUserRecharge` | 保留 Order 去重和事务，复用余额与分组更新 |
| 兑换 | API/Telegram → `Redeem` → `CreditUserRecharge` | 保留实际兑换者、兑换状态和余额同事务提交 |
| 注册与邀请奖励 | `User.Insert` → 初始奖励 → `IncreaseUserQuota` | 复用相同分组规则；返回对象不再获得通用回写权限 |
| 管理员增减 | `controller.ChangeUserQuota` → `model.ChangeUserQuota` | 正负增减照常重算，增加显式零额度重算 |
| HTTP、ResponsesWS、Realtime Try | `AttemptQuota.ApplyReserve` → `Quota` → `ApplyBillingReserve` | 保留预扣与一次执行权，不把预扣计入已消费 |
| 短 owner Confirm | `AttemptQuota.CloseFromProviderResult` → `consumeFinalQuota` → `billing.ApplySettlement` → `ApplyBillingSettlementBalances` | 同事务更新余额、最终已消费额度与用户组 |
| 短 owner Cancel | `AttemptQuota.closeLocked` → `undoSynchronouslyWithContext` → `ApplyBillingRefund` | 退回余额后同事务重算分组，已消费不增加 |
| Async Task | `CreateTaskBillingOwner` / `FinalizeTaskBillingOwner` → 共享余额函数 | 状态 CAS、余额、已消费和组属于原 Task 事务 |
| 消费统计 | `runSettlementProjection` → `UpdateUserUsedQuotaAndRequestCountWithContext` → 可选批量队列 | 移走已消费写入；请求次数、渠道统计和日志仍可异步 |
| 注册后登录 | GitHub 注册 → `User.Insert` → `setupLogin` → `user.Update(false)` | 登录只更新登录信息，不能回写旧 group/role/status |
| 其他用户更新 | OAuth 补全、绑定/解绑、AccessToken、邀请码、资料、管理员操作、删除 | 按操作明确可写字段，清除同类整对象回写 |
| 分组生效 | `GroupDistributor.SetupGroups`、`RefreshLongLivedPrincipal` | 保留 SQL 主体读取及下一工作单元刷新，缓存仅用于展示 |

Async Task 当前已接入的 provider 缺少可计价 usage，终态走 Cancel；共享仓储仍具备 Confirm 能力，方案与测试覆盖二者，不从任务成功状态伪造消费。

## 3. P1：统一资金状态更新与分组计算

令 `Q` 为可用余额，`U` 为已确认消费，`R` 为本次已预扣额度，`C` 为最终收费，`D` 为管理员增减或充值奖励。

| 操作 | 余额变化 | 已消费变化 | 分组检查 |
| --- | --- | --- | --- |
| Try | `Q -= R` | 不变 | 不额外重算 |
| Confirm | `Q += R - C` | `U += C` | 用更新后的 `Q + U` 重算 |
| Cancel / 预扣退回 | `Q += 实际退回额度` | 不变 | 用更新后的 `Q + U` 重算 |
| 充值、兑换、奖励、管理增减 | `Q += D` | 不变 | 用更新后的 `Q + U` 重算 |
| 人工重算 | 不变 | 不变 | 重新读取 SQL 值并重算 |

一次成功消费从 Try 到 Confirm 对 `Q + U` 的净影响为零。Confirm 时虽然 `C` 可以大于或小于 `R`，最终都把预扣造成的暂时缺口补回统计口径。多个请求交错时，最后一个成功结束的事务读取包含此前所有提交的用户状态，因此能够收敛。

示例：用户原有额度 200，预扣 100，再充值 10。充值时可能按 110 匹配较低组；随后 Confirm 收费 100，在同一事务中写入 `Q=110、U=100` 并按 210 恢复正确分组。若 Cancel，则写入 `Q=210、U=0` 并按同样的 210 重算。

**不能只在“余额差额不为零”时更新。** `C=R>0` 时余额差额为零，但 `used_quota` 必须增加 C；这是 P1 复现场景的关键。`R=0、C>0` 的高余额 fast path 和 provider-initiated Realtime 也必须正常记录最终消费。

实施方式：在 model 层保留一个事务内余额/已消费/分组更新函数，接收已锁定的用户及本次两个增量。充值 owner、管理入口和消费余额函数复用它；provider、controller 和 transport 不各自判断分组。

函数先校验增量和溢出，计算新 Q/U，查询启用的自动分组规则，然后一次更新所需字段。规则保持 `min <= Q+U < max`、`max=0` 无上限、较高 min 优先以及现有同 min 的稳定顺序。Q 允许按现有 TCC 契约为负；U 只累计成功 Confirm 的非负 C。

`User.UsedQuota` 保持“已确认消费”的含义，Try 不增加它。`Token.UsedQuota` 现有预扣/退回规则不随之改写，两者不能机械套用同一个增量。

移除 `runSettlementProjection` 对用户已消费额度的写入，同时删除 `BatchUpdateTypeUsedQuota` 和其专用批量 writer，防止同一 C 再次累计。请求次数的投影独立保留，语义不扩展为对所有 Cancel 计数。日志失败不再造成已消费缺失或分组停留在错误区间。

## 4. 事务、重试与缓存

- 复用现有事务和锁顺序。短消费锁 user → token；Order、Redemption、Task 先按自身协议取得 owner，再进入 user/token。共享更新函数不另起事务，不重复获取执行权。
- PostgreSQL/MySQL 在事务内锁用户行，再根据当前行计算；SQLite 沿用已有 `BEGIN IMMEDIATE` 和 busy timeout。纯进程锁不能替代 SQL 串行化。
- 用户余额、最终已消费和分组同成同败。Task 的终态/CAS 与这些字段在同一事务；已关闭 Task 不再次增加 U。
- 延续现有“明确回滚才允许有界本地重试，commit unknown 不重放”的边界。移动 U 到资金事务后，重试不会分别重放余额和统计。
- 规则查询、余额更新或 token 更新失败，回滚整次事务；不新增“先到账，稍后尽力改组”的第二写入路径。
- 零增量重算若目标组不变，直接成功，不发送无意义 UPDATE，也不能把 MySQL 的零 affected rows 当作用户不存在。
- 提交成功后按原边界失效展示缓存。缓存失败不改变事务结果，也不再执行余额命令；实际请求继续从 SQL 读取当前归属。

## 5. P2：按操作更新用户字段

P2 的根因是通用 `User.Update` 把旧快照中的非零字段一起保存。只在注册后重新查一次用户，仍会在查询与保存之间留下同类覆盖窗口。

替换为少量按用途组织的更新操作，内部使用明确的 `Select` 或 map。不要向调用方暴露一个能够修改任意 User 列的通用 map 接口。

| 操作 | 可写内容 |
| --- | --- |
| 登录记录 | `last_login_time`、`last_login_ip` |
| 用户自助资料 | 当前接口承诺的显示名、密码等自助字段 |
| GitHub/OIDC/微信/飞书绑定及资料补全 | 对应身份字段、该操作明确负责的头像/邮箱；保留现有校验与授权 |
| 解绑 | 对应身份字段的空值/零值，必须能明确落库 |
| AccessToken、邀请码、Telegram 绑定 | 各自字段；生成邀请码需要保留已有非空值 |
| 管理员启停、角色变更 | 本次指定的 status 或 role，保留权限校验 |
| 管理员编辑分组 | 显式 group 变更，不能由无关资料编辑顺带提交旧 group |
| 删除 | 删除所需的用户名变更及软删除状态，保留原删除语义 |

GitHub 登录过去借助 `setupLogin` 的整对象保存来顺带保存邮箱、头像和 GitHub ID 补全。改成登录字段更新后，必须把这些补全移到明确的 OAuth 更新步骤，避免修复覆盖问题却丢掉原功能。OIDC 按用户名匹配后的身份补全、其他绑定和 Telegram 入口一并迁移。

管理员编辑仍可主动改组。前端只提交实际修改字段，服务端保留“明确提交 group 就是管理员改组”的管理接口语义；普通资料操作没有修改 group 的权限。并发的两次明确管理改组按提交顺序生效，不为本方案增加版本字段或乐观锁协议。实施时同步核查管理脚本是否会在只改资料时附带旧 group。

`setupLogin` 必须处理登录信息写入的失败，不能忽略错误后继续返回成功；检查用户仍存在且有效后再完成正常登录响应。会话保存失败沿用既有错误处理，不为了重试登录记录而重新发放奖励。

注册初始奖励和邀请奖励继续使用已有资金入口。修复后登录不依赖 Insert 返回的 group 快照，因此奖励晋级不会被后续登录撤销。没有必要为 P2 新建跨用户奖励工作流或扩大成全注册分布式事务。

## 6. 简单的人工补救

沿用管理员额度接口，支持明确的 `quota=0`；缺少 quota、非整数、无效用户和无权限仍报错。管理页面提供“重新计算用户组”，调用同一入口，不增减资金。

人工操作流程：确认该用户的请求/任务已经结束，检查 SQL 余额与已消费额度，执行一次重算。若有真正的资金异常，先按可信证据修正余额/消费，再重算分组。重算记录管理日志，注明分组前后值，不能伪装成充值成功或额度增加。

正常 Confirm/Cancel 会自行收敛，不要求管理员逐笔处理。人工主要用于历史 P1/P2 已造成的错误分组、短 owner 崩溃或数据库故障后的核查，以及管理员主动要求重新应用当前规则。没有可信历史证据时不能承诺一次操作修复所有账务问题。

手动指定用户组保留为明确的管理操作，并按原语义可能在后续额度变动时被自动规则覆盖；本方案不引入“永久固定组”。修改自动分组规则本身不新增全量后台重分组，既有用户在后续资金变动或人工重算时应用新规则。

## 7. 验收矩阵

| 场景 | 必须验证的结果 |
| --- | --- |
| 原 P1：预扣 → 充值 → Confirm | 统计与结算提交后恢复到最终 Q+U 对应组 |
| 预扣 → 管理员扣减 → Cancel | 按扣减后的真实总额升降，不把退回当充值累计 |
| 多请求交错 Confirm/Cancel | 最后一笔成功结束后正确收敛，无 U 重复累计 |
| `C=R`、`C<R`、`C>R`、`R=0`、零费用 | 覆盖差额为零、退款、补扣、直接收费及 no-op |
| 批量统计开/关 | Q/U/G 相同，批量 flush 不再增加 U |
| Async Task 正常结束、重复终态、提交恢复 | 复用相同状态更新，一次最终消费和一次分组更新 |
| 用户/token 停用、删除、UnlimitedQuota 切换 | 保留原生命周期权限与 token 结算语义 |
| 规则查询失败、token 写失败、commit unknown | 同事务回滚或结果不明，不二次入账、不重复 U |
| 原 P2：GitHub 邀请注册并立即登录 | 数据库晋级组不被旧对象覆盖，OAuth 资料仍能保存 |
| 资料/绑定/邀请码更新与充值或管理改组交错 | 只修改本次拥有的字段，不覆盖 group/role/status |
| 管理员明确改组、普通资料编辑、解绑零值 | 保留授权与零值语义，无关资料提交不携带旧 group |
| 零额度重算两次 | Q/U 不变；第一次修正 group，第二次无副作用成功 |
| 缓存删除失败与旧查询晚回填 | 新请求、长连接下一工作单元仍使用 SQL 新归属 |

数据库检查至少覆盖 SQLite、实际部署的 MySQL/PostgreSQL；多连接交错测试验证 SQL 事务语义，Go race 检查不能替代它。现有两个复现测试作为验收基线，额外覆盖正常路径及上述边界；不为纯字段包装函数逐一写实现镜像测试。

## 8. 实施顺序与发布

1. 先建立上述链路测试，保留现有复现作为回归用例。
2. 收敛 Q/U/G 的事务内更新；同步消费 Confirm、Cancel、Task 与充值/奖励/管理入口；一次性移除旧 U 投影 writer。
3. 迁移全部 User 整对象更新入口，并同步管理员编辑提交语义。
4. 加入零额度人工重算及准确管理日志，更新当前支付/消费文档中已废弃的分组说明。
5. 发布时停止接收新工作，排空短请求并刷完旧批量统计，再停止旧实例；持久 Task 暂停后台处理后由新实例接续。新旧 U writer 不能混跑，否则可能重复累计。

无需充值累计字段迁移或历史充值回填，已有 Q/U 原样保留。历史统计已经丢失的数值不能凭余额猜测补齐；确有受影响用户时按证据人工处理。回滚也须停止新 writer 并排空工作，不可将一次 C 同时交给旧投影与新事务处理。

## 9. 官方资料与本项目取舍

- PostgreSQL 说明 `FOR UPDATE` 阻止其他事务修改同一行，并在等待后读取更新后的行；这里用它保证基于最新 Q/U 计算，而不把锁当作跨阶段业务状态。[PostgreSQL 行锁](https://www.postgresql.org/docs/current/explicit-locking.html#LOCKING-ROWS)
- MySQL 锁定读需要在事务中使用，锁在提交或回滚时释放；只给事务外查询补一个锁关键字不够。[MySQL Locking Reads](https://dev.mysql.com/doc/refman/8.4/en/innodb-locking-reads.html)
- SQLite 的 `BEGIN IMMEDIATE` 在开始时取得写事务，避免先读后写的锁升级窗口，但仍需处理忙错误。[SQLite Transactions](https://sqlite.org/lang_transaction.html)
- GORM 支持显式选择更新字段，也指出 struct 更新默认跳过零值；因此本方案按操作选择字段，并为解绑、清空和零额度重算明确处理零值。[GORM Updates](https://gorm.io/docs/update.html)
- Microsoft 的 CQRS 文档指出异步读模型可能陈旧。结合本项目代码得出的判断是：既然 U 参与分组和计费权益，最终消费数值应跟资金事务同步提交；请求次数和展示日志可以继续作为异步投影。这里不需要引入一整套 CQRS 或消息系统。[CQRS 的一致性取舍](https://learn.microsoft.com/en-us/azure/architecture/patterns/cqrs)

实现包含共享额度与分组更新、消费事务内累计已消费、按字段更新用户以及管理页面零额度重算。SQLite 链路测试覆盖预扣交错、Confirm/Cancel、零预扣、失败回滚、重复重算和旧登录对象；MySQL/PostgreSQL 仍须在实际部署环境验证。
