---
status: accepted
---

# 让 Stored Responses 成为唯一的初始 owned resource

首个受支持的有状态表面限于 Stored Responses。`store` 省略或为 `true` 的 Responses 请求在 `response.created` 暴露其 ID 时立即创建持久 principal/channel/account ownership，后续 continuation 以及受支持的 retrieve、delete 和 input-item 生命周期 operation 在 provider retention period 内严格路由到该 owner。ownership 写入必须在首个携带 ID 的 JSON、SSE 或 WebSocket 事件交付前提交；写入失败会结束 relay，即使 provider 可能保留一个 orphan。显式 `store:false` 不创建持久 ownership：同一 Native Responses WebSocket 上的 continuation 保持 connection-local，而之后独立请求只有在有界 user-scoped provenance proof 仍然存在时才能进入普通调度。该 proof 使用既有 memory/Redis affinity backend，其 key 包含 `UserId`，默认 TTL 为一小时。Background Responses、Stored Chat、conversations、saved prompts 以及外部 file、vector-store、container 或 Skill 引用在各自完整生命周期得到支持前于 provider work 前失败。

拥有 principal 是 `UserId`；创建的 `TokenId` 只保留用于审计，因此凭据轮换不会让 Stored Response 变成 orphan。精确 `channel_id` 是上游账号边界：owner 不可用就失败而不 fallback，把该 channel 改成另一个 provider account 是管理员控制的破坏性操作。已知 owner mismatch 和 ownership miss 对普通用户都 fail closed；只有 administrator-pinned relay 可以尝试未知 ID。代理绝不从选中的 channel 发明 owner。

Ownership 在 provider retention period 加七天 grace period 内保留，初始为 30 + 7 天。成功 delete 会把记录转换为在同样期限内保留的 tombstone，而不是立即删除。

provider 返回新的 stored Response ID 之后，ownership commit 使用与下游取消分离的有界 context。任何后续 ownership 或交付失败都是 provider-accepted work：它可能留下不可见 orphan，且不能触发 provider retry。计费遵循 ADR-0030：有 provider usage 才 Confirm，否则 Cancel。

可能在通用 provider stream 结束前停止的 consumer 使用 close-and-drain 而不是裸 close。关闭会取消 emitter-based reader；drain 额外释放可能已阻塞在原始 channel send 的 legacy handler。该生命周期规则适用于 Stored Responses owner barrier 和其他通用 stream aggregator，不改变 provider 协议语义。

初始 owner-table 上线是完整停机破坏性切换。上线前创建的 Responses 不从 legacy affinity、选中 channel 或 provider probe 回填；升级后对这些 ID 的普通访问变成 ownership miss。Administrator-pinned relay 仍是唯一的未知 ID escape hatch。要求兼容切换前 ID 会触发单独的双版本迁移决策，而不是永久 fallback 分支。

`/responses/compact` 返回不同的 `response.compaction` 结果，而不创建 Stored Response 生命周期资源。其 `output` item 作为输入被重放以继续 compacted conversation。因此代理不会仅仅因为 compacted 结果也有 `id` 就创建 `response_owners` 行；标识符形状不是生命周期证据。只有当上游公共契约明确让 compacted ID 成为 retrieve/delete/input-items 或 `previous_response_id` 的合法目标时，才重新审视该边界，届时必须一并加入完整授权、保留和 exact-channel 生命周期。

省略或为 `true` 的 `store` 只能选择显式按 operation 声明完整 Stored Response 生命周期的 channel。内置 adapter 提供项目默认值；自定义 channel 需要管理员 opt-in。能力不能仅从 channel type、`CompatibleResponse` 或运行时 probe 推断。`store:false` 保留既有 channel 和 adapter 调度规则。
