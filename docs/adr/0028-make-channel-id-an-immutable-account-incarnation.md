---
status: superseded by ADR-0035
---

# 让 channel ID 表示不可变上游账号 incarnation

本决策已被 [ADR-0035](./0035-confirm-channel-edit.md) 替代。以下保留历史取舍，不再作为当前实现约束。

一个 channel row 拟永久表示一个上游账号 incarnation。provider type、账号主体、tenant/project、task scope、账号边界 BaseURL 或无法证明同主体的 credential 变化必须创建新 channel；名称、权重、限流、route status 等非身份字段可以原地修改。

Task、Midjourney 与 Stored Response 只冻结 `channel_id`，不新增 `channel_generation`。`CredentialRevision` 仅作为同主体 credential refresh 的并发 CAS/version，不承担 provider resource identity。

Stored Response owner自己持有指向`channel_id`的execution binding。新binding为`verified`；无法证明创建时账号incarnation的legacy binding为`legacy_unverified`并fail closed，不能按当前channel猜测。全站停机migration必须先完成该constraint，再启动新Async owner；新binary不保留旧identity writer。

该选择以新增 channel row、统计不自动连续和更严格的管理操作换取单一身份语义，删除 generation 递增、active owner 修改屏障与 generation mismatch 分支。

## 影响

- 管理端换账号必须创建新 channel。
- provider 无法证明 refresh 后 subject 不变时，不能原地替换 key。
- soft-deleted channel 在持久 owner 的真实保留期内继续保存执行配置。
- 新 invariant 不能证明历史 row 从未换过账号；缺少审计证据的 legacy owner 仍需 fail closed，不能由当前 row 猜测回填。
