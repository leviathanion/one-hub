---
title: "价格更新"
layout: doc
outline: deep
lastUpdated: true
---

# 价格更新

目前定价管理是通过管理员账户下的`运营 --> 模型价格 --> 更新价格`进行配置的。

统一倍率、缓存独立调整和峰谷／周末优惠见 [条件倍率配置](./pricing-rate-rules.md)。条件规则使用新版 v2 格式；基础价格和目录更新入口保持原有操作。

价格同步只使用 [models.dev](https://models.dev) 目录：

1. 在 `运营 → 模型价格 → 更新价格` 点击“从 models.dev 获取”。获取只读取报价，不修改现有售价。
2. 核对报价来源、跳过的模型及变化，选择“只新增”（默认）、“只更新现有”或“覆盖所有”。覆盖会删除目录外未锁定的价格，包括来源歧义或无法转换而被跳过的模型，务必检查删除项。
3. 确认应用后才发布售价。锁定价格保留；预览过期需要重新核对。

同名模型优先采用唯一的官方来源；多个来源无法确定时跳过，不按最低价格或出现顺序选价。美元／百万 token 报价转换为本系统倍率，缓存和上下文分档一起核对。未收录或不能完整表达的报价继续在模型价格中手工维护；本地特殊定价可锁定。

自定义 JSON URL、默认价格服务器和定时价格更新已移除，旧 `AUTO_PRICE_UPDATES*`、`UPDATE_PRICE_SERVICE` 配置不再生效。启动只在价格库为空时填入内置价格，已有价格不会因重启更新。models.dev 不会在后台自动覆盖售价。

展示归属在 `运营 → 模型详情 → 编辑 → 模型归属` 配置；`运营 → 模型归属` 管理分类名称与图标。报价供应商仅用于选价与追溯，不会修改本地展示归属。

## GPT-5.6 价格配置

下列价格于 2026-08-24 按 OpenAI 官方 Pricing 与模型页面核验。上游价格可能变化；首次配置、每次发布以及促销截止日前都必须重新核对官方页面，不能把本文数值当作运行时内置价格。

官方来源：

- [OpenAI API Pricing](https://developers.openai.com/api/docs/pricing)
- [GPT-5.6 Sol](https://developers.openai.com/api/docs/models/gpt-5.6-sol)
- [GPT-5.6 Terra](https://developers.openai.com/api/docs/models/gpt-5.6-terra)
- [GPT-5.6 Luna](https://developers.openai.com/api/docs/models/gpt-5.6-luna)

标准模式短上下文价格，单位为 USD / 1M tokens：

| 模型 | Input | Cached input | Cache write | Output | one-hub input/output ratio |
| --- | ---: | ---: | ---: | ---: | ---: |
| `gpt-5.6`（指向 Sol） | 4.00 | 0.40 | 5.00 | 20.00 | 2 / 10 |
| `gpt-5.6-sol` | 4.00 | 0.40 | 5.00 | 20.00 | 2 / 10 |
| `gpt-5.6-terra` | 2.00 | 0.20 | 2.50 | 12.00 | 1 / 6 |
| `gpt-5.6-luna` | 0.20 | 0.02 | 0.25 | 1.20 | 0.1 / 0.6 |

换算使用项目既有定义 `1 ratio = $2 / 1M tokens`。四个精确模型行都应显式配置：

```json
{
  "extra_ratios": {
    "cached_tokens": 0.1,
    "cache_read_input_tokens": 0.1,
    "cache_write_tokens": 1.25,
    "cache_creation_input_tokens": 1.25
  },
  "rate_rules": {
    "version": 2,
    "service_tier": [
      {"id": "flex", "when": {"service_tier": ["flex"]}, "multipliers": {"all": 0.5}},
      {"id": "priority", "when": {"service_tier": ["fast", "priority"]}, "multipliers": {"all": 2}}
    ],
    "long_context": [
      {"id": "long", "when": {"input_tokens": {"gt": 272000}}, "multipliers": {"input": 2, "output": 1.5}}
    ]
  }
}
```

规则语义：

- 长上下文只在实际 input tokens `> 272000` 时作用于整次请求；272000 不触发，272001 触发。
- Flex 使用标准价的 0.5 倍；Fast 与兼容请求值 Priority 使用标准价的 2 倍。最终结算以 provider 回显的实际 tier 为证据。
- 长上下文规则与实际 tier 规则相乘。
- 区域处理的价格不进入这些模型行；当前普通用户不支持 OpenAI Data Residency 区域端点。

### 配置步骤

1. 在 `运营 -> 模型价格` 创建或更新 `gpt-5.6`、`gpt-5.6-sol`、`gpt-5.6-terra`、`gpt-5.6-luna` 四个精确模型行。
2. `type` 设为 `tokens`，按上表填写 input/output ratio，模型展示归属在模型详情中单独配置。
3. 按示例填写 `extra_ratios` 和 `rate_rules`，并将价格行锁定，避免目录同步覆盖本地显式规则。
4. 不创建 `gpt-5.6-pro`：`pro` 是请求能力，不是独立模型名。
5. 不创建 `gpt-5.6-*` 宽泛通配行。新增 snapshot 或 alias 前必须先核价，再添加精确行。

### 配置核验

- `GET /api/prices?type=db` 返回四个精确模型行，input/output、locked、四个 extra ratio 和三个 rate rule 均与配置一致。
- 分别检查 272000/272001 的 Standard、Flex、Fast/Priority 预览；tier 倍率应为 `1/1`、`0.5/0.5`、`2/2`，长上下文再乘 `2/1.5`。
- 使用 provider 真实 usage 分别验证 `cached_tokens`、`cache_write_tokens` 以及二者同时出现，确认倍率只应用一次。
- 分别请求 `service_tier=fast` 和 `service_tier=priority`，确认按响应回显的实际 tier 结算。未知回显 tier 使用基础价并记录 `billing_tier_unknown`。
- 模型渠道配置与价格目录必须同时包含上述精确模型；任一价格行缺失时，该模型会在 provider work 前因缺少 Price Policy 拒绝准入。

发现实际账单、tier 或长上下文金额不一致时，应立即从渠道模型列表关闭受影响模型或 tier。不要删除已记录用量，也不要回退到 DefaultPrice；修正并复算样本后再恢复流量。
