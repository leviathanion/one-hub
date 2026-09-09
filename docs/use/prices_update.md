---
title: "价格更新"
layout: doc
outline: deep
lastUpdated: true
---

# 价格更新

目前定价管理是通过管理员账户下的`运营 --> 模型价格 --> 更新价格`进行配置的。

统一倍率、缓存独立调整和峰谷／周末优惠见 [条件倍率配置](./pricing-rate-rules.md)。条件规则使用新版 v2 格式；基础价格和目录更新入口保持原有操作。

项目官方提供一张价格表：<https://raw.githubusercontent.com/MartialBE/one-api/prices/prices.json>

另外社区维护的价格表项目如下：

- [Oaklight/onehub_prices](https://github.com/Oaklight/onehub_prices)

  - 每 2 小时自动同步 Openrouter 和 Siliconflow 更新，定期手动核验其他供应商。
  - 供应商 id 映射：使用任何价格表前，请务必检查 [ownedby.json](https://raw.githubusercontent.com/Oaklight/onehub_prices/prices/ownedby.json) 以确保供应商 ID 与 价格表 channel id 对应，否则“可用模型”页面无法按供应商正确显示。
  - 完整价格表: 适用于 one-hub 的完整价格表，合并 MartialBE/one-hub 价格表，另外提供了更多供应商：<https://raw.githubusercontent.com/Oaklight/onehub_prices/prices/oneapi_prices.json>
  - 核心供应商价格表: 仅包含 MartialBE/one-hub 目前定义的供应商 id <= 1000 的核心供应商价格表：<https://raw.githubusercontent.com/Oaklight/onehub_prices/prices/onehub_only_prices.json>
  - siliconflow 价格表：<https://raw.githubusercontent.com/Oaklight/onehub_prices/prices/siliconflow_prices.json>
  - openrouter 价格表：<https://raw.githubusercontent.com/Oaklight/onehub_prices/prices/openrouter_prices.json>

- [woodchen-ink 维护](https://github.com/MartialBE/one-hub/issues/562#issuecomment-2746243372)

  - 价格链接(全部)：<https://ai-prices.sunai.net/api/one_hub/rates>
  - 价格链接(只含原版供应商,即厂商ID小于1000): <https://ai-prices.sunai.net/api/one_hub/official-rates> 
  - 供应商信息: <https://ai-prices.sunai.net/providers>

需要明确的是，除了项目官方默认提供的供应商列表（`运营 --> 模型归属`）外，即 `id > 1000`的供应商，需要和项目价格表的供应商 id 能够匹配的上才可以正确显示价格。所以你需要同时关注：

- ownedby 列表，一般通过 onehub 网页`运营 --> 模型归属`维护。
- prices 列表，通过`运营 --> 模型价格`手动维护，或 json 连接更新。
- Oaklight/onehub_prices 项目提供了 [sync_price.py](https://raw.githubusercontent.com/Oaklight/onehub_prices/refs/heads/master/src/sync_pricing.py) 和 [sync_ownedby.py](https://raw.githubusercontent.com/Oaklight/onehub_prices/refs/heads/master/src/sync_ownedby.py) 两份自动同步脚本。用法见项目[README](https://github.com/Oaklight/onehub_prices?tab=readme-ov-file#%E4%BB%B7%E6%A0%BC%E5%90%8C%E6%AD%A5%E6%8C%87%E5%AF%BC)。

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
    "cached_read_tokens": 0.1,
    "cache_write_tokens": 1.25,
    "cached_write_tokens": 1.25
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
2. `type` 设为 `tokens`，按上表填写 input/output ratio，channel type 按实际部署渠道填写。
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

::: tip 说明
可以使用环境变量`UPDATE_PRICE_SERVICE`设置默认价格更新服务器地址，详见[环境变量](../deployment/env)
:::
