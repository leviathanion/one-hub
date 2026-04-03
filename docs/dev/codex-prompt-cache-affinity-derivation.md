---
title: "Codex Prompt Cache Affinity 预派生方案"
layout: doc
outline: deep
lastUpdated: true
---

# Codex Prompt Cache Affinity 预派生方案

## 文档状态

- 状态：已实现
- 范围：`/v1/responses` 与 `/v1/responses/compact` 在 Codex 自动 `prompt_cache_key_strategy` 下的 pre-routing affinity
- 当前决策：通过 `internal/requesthints` 提供 routing-visible hint port，Codex 以 resolver 的形式生成 `responses.prompt_cache_key`，relay 与 provider 共同消费同一 hint

## 问题定义

当前路径里，稳定 `prompt_cache_key` 的生成发生在 provider 内部：

1. `relayResponses.setRequest()` 先调用 `prepareResponsesChannelAffinity(...)`
2. 如果客户端没显式传 `prompt_cache_key`，此时不会命中 prompt-cache affinity
3. 之后才会进入 provider 选择
4. 选中 Codex provider 后，`providers/codex/responses.go` 的 `prepareCodexRequest(...)` 才根据 `prompt_cache_key_strategy` 自动生成稳定 key
5. 请求成功后，系统会按这个新 key 记录 affinity
6. 但下一次请求在 provider 选择之前仍然看不到这个 key，因此还是无法命中同一 affinity

这会导致一个结构性问题：

- “记录 affinity”用的是 provider 内部派生值
- “命中 affinity”发生在 provider 选择之前

也就是说，写路径和读路径不在同一个时序层面上。

## 为什么这不是一个 provider 内部细节

OpenAI 的 prompt caching 文档明确说明：

1. 请求会按 prompt 前缀被路由到最近处理过相同前缀的机器。
2. `prompt_cache_key` 会与前缀哈希组合，用于影响路由并提高 cache hit rate。
3. 对共享前缀的请求持续复用相同 `prompt_cache_key`，能提升缓存效果。

因此，`prompt_cache_key` 不是单纯的“上游请求整形字段”，它本质上也是一个 routing hint。只在 provider 内部生成，架构上会错过本地 channel affinity 的命中窗口。

## 外部与相邻实现参考

### 外部资料

- OpenAI prompt caching 文档把 `prompt_cache_key` 明确定位成“可影响路由并提升 cache hit rate”的参数。
- OpenAI conversation state 文档表明，会话延续相关 key 应在发请求时就明确给出，而不是在后续链路里隐式补出。

### 相邻仓库

- `../sub2api/backend/internal/service/openai_gateway_service.go`
  - `GenerateSessionHash(...)` 在账号选择之前，按 `session_id -> conversation_id -> prompt_cache_key` 的优先级生成稳定 sticky key
- `../sub2api/backend/internal/service/openai_gateway_service_test.go`
  - 对上述优先级有直接测试
- `../new-api/setting/operation_setting/channel_affinity_setting.go`
  - channel affinity key 直接从请求体 `prompt_cache_key` 提取
- `../new-api/relay/common/override_test.go`
  - 存在 `session_id <-> prompt_cache_key` 的请求规范化 / 同步逻辑

这些实现都指向同一个最佳实践：

- 路由依赖的稳定 key，应该在路由之前就已经存在

## 设计目标

1. provider 选择前就能得到与最终上游请求一致的稳定 `prompt_cache_key`
2. 显式传入的 `prompt_cache_key` 永远优先
3. 不把全部 Codex request adaptation 都搬到 relay
4. 不做“先选 provider 再回头重算 affinity”的 speculative routing
5. 控制改动范围，只解决 `prompt_cache_key_strategy` 对 affinity 命中的时序错位

## 推荐方案

### 1. 抽出共享的纯函数派生 helper

把下面这组逻辑从 provider 实例方法里抽成共享纯函数：

1. `normalizePromptCacheStrategy(strategy string) string`
2. `deriveCodexPromptCacheIdentity(ctx, strategy) string`
3. `deriveCodexPromptCacheKey(ctx, strategy) string`

要求：

1. 算法与当前 provider 内部保持一致
2. 继续使用同样的 identity 来源：
   - `token_id`
   - `user_id`
   - 稳定认证头身份
3. 继续生成与当前实现一致的稳定 UUID

实现时最终没有把 helper 放进 `common`，而是保留在 `providers/codex` 包内，由 Codex resolver 和 Codex provider 复用同一套 helper。这样 relay 不需要感知 Codex 细节。

### 2. 在 request normalization 阶段提前生成 derived key

实际实现没有让 relay 直接调用 Codex 逻辑，而是：

1. `prepareResponsesChannelAffinity(...)` 先调用 generic `requesthints.ResolveResponses(...)`
2. Codex 通过注册的 resolver 生成 `responses.prompt_cache_key`
3. affinity 引擎通过 `request_hint` source 消费这个值

行为建议：

1. 如果客户端已显式传 `request.PromptCacheKey`，直接使用客户端值
2. 如果没有显式值，且当前请求满足 Codex Responses resolver 条件，则按 routing-visible 配置生成 request hint
3. 把这个 hint 存进 request-scoped context
4. `prepareResponsesChannelAffinity(...)` 通过 `request_hint` source 参与 affinity lookup

这里仍然坚持“先放入 context，再用于 affinity lookup”，而不是一上来就直接改写请求体。原因是：

1. 这样不会把 provider-specific 的字段过早写回原始请求对象
2. 只有真正选中 Codex provider 时，才需要把这个值落到最终上游请求里

### 3. provider 只消费已经派生好的 key

Codex provider 侧应调整为：

1. 如果请求里已经有显式 `PromptCacheKey`，直接用它
2. 如果 context 里已有 relay 预先派生的 key，直接复用它
3. 只有在前两者都没有时，才走 legacy provider-side fallback

这样 provider 仍然保留“最终把 key 写入上游请求”的职责，但不再负责决定 routing-affinity 使用的稳定 key。

### 4. 策略来源必须 relay 可见

这是这份设计里最关键的边界。

如果 `prompt_cache_key_strategy` 仍然只保存在“被选中的 provider 的 channel.Other”里，那么在 provider 被选出来之前，系统天然不知道应该按哪种策略派生 key。

因此最终实现为一个小但明确的调整：

1. 为 Codex Responses 增加一个 relay 可见的策略来源 `CodexRoutingHintSetting`
2. 该策略枚举仍沿用当前已有值：
   - `off`
   - `auto`
   - `token_id`
   - `user_id`
   - `auth_header`

最终优先级：

1. 显式请求 `prompt_cache_key`
2. relay 预派生 key
3. provider-side legacy fallback

最简单、收益最高的做法，是引入 provider-local 但 routing-visible 的配置项，让 routing 层在 provider 选择前就知道应该按哪种策略生成 key，而 generic affinity config 只负责读取 `request_hint`。

## 为什么这是复杂度与收益的最佳点

这个方案只把“稳定 key 的派生”上移，不把整套 Codex 请求整形都搬走。

收益：

1. 第二次请求开始，就能在 provider 选择前命中 affinity
2. relay 与 provider 使用同一套派生算法
3. 不需要 speculative provider selection
4. 不需要把 channel affinity 引擎泛化成一个新的 DSL

控制住的复杂度：

1. 只新增一个很小的 request normalization 步骤
2. 只抽取一组纯函数 helper
3. provider 仍保留最终请求写入职责

## 不建议的方案

### 1. 继续把自动 `prompt_cache_key` 生成留在 provider 内部

问题：

1. 下一次请求仍然无法在 provider 选择前命中 affinity
2. 写路径和读路径继续错位

### 2. 为了知道策略，先做一次 speculative provider selection

问题：

1. 这会重复一遍 channel 选择
2. 容易和 cooldown、retry、quota、日志语义缠在一起
3. 收益远小于复杂度

### 3. 扫描所有候选 channel，尝试推断共同策略

问题：

1. routing 逻辑会被候选池配置细节强绑定
2. 当不同 channel 配置不一致时，行为很难解释
3. 这比新增一个 relay-visible 策略来源更绕

### 4. 用完整请求体或 transcript hash 作为 affinity key

问题：

1. 过于敏感，轻微输入变化就失去粘性
2. 目标是“稳定缓存身份”，不是“精确请求去重”

## 兼容性建议

建议按两层语义拆开：

### 路由层

- 负责决定 affinity 使用的稳定 key
- 必须在 provider 选择前完成

### provider 层

- 负责把最终 `prompt_cache_key` 写到上游请求
- 优先复用路由层已经派生好的值

对于现有 `channel.Other.prompt_cache_key_strategy`：

1. 短期可保留，作为 legacy fallback
2. 一旦 routing 层已经派生出 key，provider 不应再生成不同的值
3. 如果将来二者并存，推荐以 routing 层为准，并在配置冲突时打日志提示

## 实现落点

### 新共享 helper

当前实现位置：

1. `providers/codex/promptcache.go`
2. `providers/codex/routing_hint.go`
3. `internal/requesthints/request_hints.go`

### `relay/responses.go`

当前由 `prepareResponsesChannelAffinity(...)` 内部先执行 generic hint resolve，再做 affinity lookup。

### `relay/channel_affinity.go`

当前实现不再扩展 provider-specific 输入，而是新增 generic `request_hint` source，读取 `responses.prompt_cache_key`。

### `providers/codex/responses.go`

当前 `ensureStablePromptCacheKey(...)` 的优先级为：

1. 优先读取显式请求值
2. 其次读取 context 中已派生好的值
3. 最后才做 legacy provider-side derivation

## 测试要求

### 必测 1：第二次请求能在 provider 选择前命中 affinity

场景：

1. 第一次请求没有显式 `prompt_cache_key`
2. routing 层按策略派生 key，并在成功后记录 affinity
3. 第二次相同身份请求再次到来

断言：

- 第二次请求在 provider 选择前就能命中同一 affinity

### 必测 2：显式 `prompt_cache_key` 优先级最高

断言：

- 客户端显式值覆盖自动派生逻辑

### 必测 3：provider 复用 routing 层派生值

断言：

- provider 不再生成与 routing 层不同的 key

### 必测 4：legacy fallback 仍可工作

场景：

- routing 层未配置该策略来源，但 provider 仍有 legacy `prompt_cache_key_strategy`

断言：

- 请求仍可正常执行
- 但不会错误宣称自己具备 pre-routing affinity 命中能力

### 必测 5：策略冲突时以 routing 层为准

断言：

- context 中已有 derived key 时，provider 不覆盖它

## 参考资料

外部资料：

- OpenAI Prompt Caching
  https://developers.openai.com/api/docs/guides/prompt-caching
- OpenAI Conversation State
  https://developers.openai.com/api/docs/guides/conversation-state

相关实现参考：

- `relay/responses.go`
- `relay/channel_affinity.go`
- `providers/codex/responses.go`
- `providers/codex/base.go`
- `docs/use/Codex.md`
- `../sub2api/backend/internal/service/openai_gateway_service.go`
- `../sub2api/backend/internal/service/openai_gateway_service_test.go`
- `../new-api/setting/operation_setting/channel_affinity_setting.go`
- `../new-api/relay/common/override_test.go`
