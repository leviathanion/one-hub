---
status: accepted
---

# 保持原生 Responses WebSocket 的传输和计费边界

原生客户端帧保留 wire 语义，连接固定渠道，每个 create 独立使用该渠道明确支持的精确 model。首个 create 先进入 actor，再启动客户端读取；后续 create 使用有界 FIFO。这继续替代 ADR-0006 的首模型锁定决定，不授权跨渠道重放、HTTP bridge 或翻译会话状态机。

交付与观察分开：代理负责权限、资源 owner、准入、容量和一次结算，上游负责参数语义与执行生命周期。inject 是已准入 Response 的输入追加，其回执不延长父账务、不阻挡下一次 create；完成目标的注入也由上游返回结果。steering 可能产生独立后继，因此保留其事前预扣和最小关联。此前 pending-inject 回执屏障及本地模拟恢复的约定已删除。

公共事件名和状态原样保留，序号缺失或未知值只影响依赖它的本地观察。`response.completed/failed/incomplete` 提供已准入执行的终结事实；泛化 error 不等于当前 create 的拒绝。供应商私有终态只在有明确映射依据时由 adapter 转换。单个计费组件缺失或冲突不妨碍原帧交付及其他独立组件，结算遵循 ADR-0030/0033。实现与验收见 [Responses 透明转发设计](../dev/responses-transparent-relay-design.md)。
