---
status: accepted
---

# 用保存确认支持渠道原地编辑

为避免管理员重复配置，以及自动 clone 所需的字段继承和运行状态重置规则，允许渠道连接配置原地编辑。单渠道和标签编辑表单每次保存前统一提示可能影响的范围，用户确认后提交；不查询资源占用，也不引入后端确认协议。本决策替代 [ADR-0028](./0028-make-channel-id-an-immutable-account-incarnation.md)。

接受的代价是渠道 ID 不再证明创建资源时的上游账号身份，任务、Stored Responses、其他上游资源及会话可能因新配置不可访问。资源仍绑定原 ID，凭据刷新并发保护、权限、配置校验及提交后禁止隐式重试的约束保持有效。实现和验证见 [方案](../dev/immutable-channel-identity-architecture.md)。
