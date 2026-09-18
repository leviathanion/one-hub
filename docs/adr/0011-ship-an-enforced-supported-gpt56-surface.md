---
status: accepted
---

# 交付强制执行的受支持 GPT-5.6 表面

> 本文中的 operation support 和 channel configuration 由 ADR-0023 取代。operation support 现在与模型无关并从 adapter 派生；模型名只参与普通渠道模型选择和模型映射。

首个生产版本保证的是刻意有界的 Supported Contract Surface，而不是部分暴露整个 GPT-5.6 API。生产 `/v1` 路由仍是 Enforced Relay Surface，并使用 Legacy Quota Settlement 而不是 observe-only accounting；尚无完整路由和生命周期语义的能力在 provider work 开始前 fail closed。这以即时广度换取更小的显式契约，同时不把半开放资源透传变成公共产品的一部分。

初始 Responses 表面包含 create、Native Responses WebSocket、Stored Response retrieve/delete/input-items、`/responses/compact` 和 `/responses/input_tokens` 作为显式 operation，各自有自己的 capability 和 representability gate。要求管理员 pin 精确 channel 的既有资源 operation 只作为 Administrator-Pinned Resource Relay 提供；它们不晋升为普通用户契约，也不意味着代理管理所有权。
