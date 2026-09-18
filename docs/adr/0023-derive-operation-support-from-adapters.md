---
status: accepted
---

# 从 adapter 派生 operation support

公共 operation support 是代理 adapter 与模型无关的事实，而不是管理员关于上游产品的声明。因此代理从已安装 adapter 派生支持，并移除 `other.capabilities.operations`；当 adapter 能忠实转发某 operation 时，请求被发送，由真实上游决定其 endpoint 或账号是否接受。本地 fail-closed 错误只保留给代理无法表示的 operation，而 Stored Response ownership 和 exact-channel routing 仍是代理职责。

候选级 representability 拒绝是 selector filter，不是对之后所有选择错误的笼统替代。选择失败后，routing snapshot 用同样的 request-static filter 分类，同时故意忽略运行时 cooldown/disable 状态：只有当每个已配置候选都不可表示时才返回 `unsupported_capability`。如果存在兼容候选但不可用，可用性错误仍然权威；请求取消和 deadline 总是优先。

有两个显式选择仍然保留，因为它们描述代理行为而不是上游能力：`compatible_response` 选择加入 Responses-to-Chat 跨协议转换，未知 compatible endpoint 在代理尝试 native Responses WebSocket transport 前需要 opt-in。既有 `channel.Other` JSON 是 `responses_ws_native` 和 `responses_ws_self_hosted` 的唯一配置 owner；Web UI 暴露 Other(JSON)、校验该值并显示 self-hosted 警告，而不维护镜像开关。不新增 channel 列。Native 是唯一受支持的 transport。项目采用停机更新；删除为废弃的 `responses_ws_transport` 与 `capabilities` 设置的特殊允许项，不提供旧 transport 回滚兼容或自动迁移。已有配置需移除这些字段，不能据此启用桥接。Private、loopback 和明文 WebSocket 目标继续需要单独的安全确认。

模型可用性和 alias 仍完全属于 channel model 配置和 model mapping。adapter operation support 和定价不从模型名推断。有文档说明的 Responses-only 模型是公共 Chat 兼容端点的窄例外：它要求 Responses-capable 候选，并使用既有显式 Chat-to-Responses adapter，而不是向上游发送无效的 Chat Completions 请求。
