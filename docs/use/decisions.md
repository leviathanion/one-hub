---
title: "Decisions API"
layout: doc
lastUpdated: true
---

# Decisions API

one-hub 提供 `POST /v1/decisions`。它接受共享的文本、图片或结构化 JSON 证据，再按问题返回概率、分类、评分或拒答。OpenAI 当前支持 `gpt-6-luna`，单次请求可包含最多 400 个问题。[OpenAI 接口参考](https://developers.openai.com/api/reference/resources/decisions/methods/create)

## 配置与调用

在 OpenAI 渠道启用模型并设置本地价格，给调用令牌授予相应分组和模型权限。Custom 渠道在上游实现该接口时，可在接口目录启用 **Decisions**（`openai.decisions`）。地址留空时使用 `/v1/decisions`，也可按现有规则配置相对路径或完整 URL。[自定义渠道配置](../dev/channel-endpoints.md)

```bash
curl "$ONE_HUB_URL/v1/decisions" \
  -H "Authorization: Bearer $ONE_HUB_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt-6-luna",
    "input": "客户反馈包裹比预计晚到一天。",
    "questions": [
      {"type":"predicate","name":"late","instructions":"客户是否反馈配送延迟？"}
    ]
  }'
```

图片可以使用公网 HTTP(S) URL 或内联 data URL，结构化数据可用 `input_json`。问题的 `instructions` 也支持文本和图片组成的数组。one-hub 透传这些字段及上游扩展，应用根据返回的 `answers` 决定接下来的动作。[OpenAI 使用指南](https://developers.openai.com/api/docs/guides/decisions)

## 计费

一次请求按上游实际报告的用量结算一次。缓存读写均已包含在输入 token 总数中，按本地价格中的相应倍率计算。缺少可计费用量时释放预扣，正常交付已取得的上游响应。

OpenAI 对 Decisions 采用独立的上游输入价，输出免费。one-hub 使用管理员配置的售价。需要和同一模型的 Chat 或 Responses 区分时，可为客户端创建 `decisions-luna`，按现有模型映射设置 `{"decisions-luna":"+gpt-6-luna"}`。调用时将 `model` 改为 `decisions-luna`，即可单独配置该别名的输入价、缓存读写倍率及输出价。[OpenAI 价格说明](https://developers.openai.com/api/docs/guides/decisions#pricing-and-availability) · [one-hub 价格规则](./pricing-rate-rules.md)
