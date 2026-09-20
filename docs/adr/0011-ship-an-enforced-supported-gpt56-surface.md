---
status: accepted
---

# 交付强制执行的受支持 GPT-5.6 表面

> 本文中的 operation support 和 channel configuration 由 ADR-0023 取代。operation support 现在与模型无关并从 adapter 派生；模型名只参与普通渠道模型选择和模型映射。

首个生产版本保证的是刻意有界的 Supported Contract Surface，而不是部分暴露整个 GPT-5.6 API。生产 `/v1` 路由使用 ADR-0030/0033 的 usage-only TCC：本地权限、能力与容量失败在 provider work 前拒绝；上游参数和业务状态交给上游判断。缺少可归属用量时放弃相应收费，不因此阻断原始交付。这以即时广度换取更小的显式契约，同时不把半开放资源透传变成公共产品的一部分。

初始 Responses 表面包含 create、Native Responses WebSocket、Stored Response retrieve/delete/input-items、`/responses/compact` 和 `/responses/input_tokens` 作为显式 operation，各自有自己的 capability 和 representability gate。要求管理员 pin 精确 channel 的既有资源 operation 只作为 Administrator-Pinned Resource Relay 提供；它们不晋升为普通用户契约，也不意味着代理管理所有权。

2026-09-20 扩展原先的初始范围：Files、Uploads、Conversations、Stored Chat、后台 Responses 和 Batch 使用普通用户 owner 与固定渠道入口；Native Responses WS 支持并行 lane 的原帧交付及独立 work 观察。管理员 raw 仍是独立权限面；普通用户账号级顶层 list 不开放。新增能力的范围与验证以 [实施方案](../dev/openai-transparent-relay-implementation-plan.md) 为准，未满足授权/计价边界的托管资源及实时直连专项不因增加路由自动开放。
