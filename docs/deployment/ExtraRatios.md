---
title: "扩展价格设置"
layout: doc
outline: deep
lastUpdated: true
---

# 扩展价格设置

多模态模型除了基础输入／输出价格，还会按音频、缓存、推理、文本、图像等计量项分别计价。扩展价格不为每个计量项维护绝对单价，而是配置它相对所绑定基础价的倍率。

## 配置位置

在管理员后台「运营 → 模型价格」中编辑模型价格，展开「额外扩展倍率」，从下拉选择计量项并填写倍率。倍率保存在该模型价格行的 `extra_ratios` 字段，随价格发布一起生效，并参与锁定价格与价格表同步。价格管理的整体操作见 [价格更新](../use/prices_update.md)。

升级迁移会把历史键改写为下表键名；迁移之后只识别新键，价格表和接口配置需要使用新键名。这是一次停机变更，迁移完成后旧版本实例不能继续与已迁移的价格配置混跑。

使用 JSON 价格表时，`extra_ratios` 是价格条目内的计量项到倍率映射：

```json
{
  "model": "gpt-image-1",
  "type": "tokens",
  "input": 10,
  "output": 40,
  "extra_ratios": {
    "input_text_tokens": 0.5
  }
}
```

## 计量项

| 计量项 | 含义 | 绑定 | 内置默认 |
| --- | --- | --- | ---: |
| `cached_tokens` | 缓存命中 | 输入价 | 1 |
| `cache_write_tokens` | 缓存写入（OpenAI 字段） | 输入价 | 1.25 |
| `cache_creation_input_tokens` | 缓存写入总额（Claude `cache_creation_input_tokens`） | 输入价 | 1.25 |
| `cache_read_input_tokens` | 缓存读取（Claude `cache_read_input_tokens`） | 输入价 | 0.1 |
| `ephemeral_5m_input_tokens` | Claude 5 分钟缓存写入（`cache_creation.ephemeral_5m_input_tokens`） | 输入价 | 1.25 |
| `ephemeral_1h_input_tokens` | Claude 1 小时缓存写入（`cache_creation.ephemeral_1h_input_tokens`） | 输入价 | 2 |
| `tool_use_prompt_tokens` | Gemini 工具输入 | 输入价 | 1 |
| `input_audio_tokens` | 输入音频 | 输入价 | 1 |
| `input_audio_transcription` | 音频转写（按秒） | 输入价 | 1 |
| `input_text_tokens` | 输入文本 | 输入价 | 1 |
| `input_image_tokens` | 输入图像 | 输入价 | 1 |
| `input_video_tokens` | 输入视频 | 输入价 | 1 |
| `output_audio_tokens` | 输出音频 | 输出价 | 1 |
| `reasoning_tokens` | 推理 | 输出价 | 1 |
| `output_text_tokens` | 输出文本 | 输出价 | 1 |
| `output_image_tokens` | 输出图像 | 输出价 | 1 |
| `output_video_tokens` | 输出视频 | 输出价 | 1 |

证据键直接使用 provider 原始字段名：OpenAI 用 `cache_write_tokens`，Claude 用 `cache_creation_input_tokens`、`cache_read_input_tokens` 和 `cache_creation` 下的两个 TTL 字段。它们都是内部计费证据，不会作为价格字段出现在公共响应里；每条 usage 只携带所属协议的字段，各自独立计价，不做别名或合并。

Claude 会同时上报 `cache_creation_input_tokens` 总额和 `cache_creation` 拆分，官方保证总额等于两个 TTL 之和。计价只在两者之间二选一：拆分完整且与总额一致时按两个 TTL 键计价，否则按总额键计价，绝不重复计算。

「额外扩展倍率」下拉当前提供其中 14 项；`tool_use_prompt_tokens`、`input_video_tokens`、`output_video_tokens` 需要通过 JSON 价格表的 `extra_ratios` 配置。它们的用量证据照常计费，未显式配置时使用内置默认倍率。

## 计费方法

有效单价由绑定侧的基础价乘以倍率得到：

```text
有效单价 = 绑定侧基础价（input 或 output）
         × extra_ratios[计量项]
         × 命中的条件倍率
         × 用户分组倍率
```

例如：`gpt-image-1` 的图片输入价为 $10/M、图片输出价为 $40/M，希望文字输入按 $5/M 计价。把模型基础价格设为图片的输入／输出价，再配置 `input_text_tokens: 0.5`，文字输入单价即为 10 × 0.5 = 5/M。

倍率可以显式配置为 0，表示该项免费；显式 0 不会被当成缺失，但对应的 provider 证据仍须完整。

`input_audio_transcription` 的计量单位是秒，不是 token（Whisper 转写、Realtime 音频转写）。它属于独立计价路径：必须在该价格行显式配置，否则该组件按缺证据处理、不产生费用。内置 `whisper-1` 价格行已显式配置为 1。

## 默认值与回退顺序

未显式配置某项时，按以下顺序取值，结果唯一：

1. 该模型价格行 `extra_ratios` 的显式值（包括显式 0）；
2. Claude TTL 例外：`ephemeral_5m_input_tokens`、`ephemeral_1h_input_tokens` 没有显式值时，读取同一行的显式 `cache_creation_input_tokens`；
3. 上表的内置默认倍率；
4. 1。

回退只改变价格查找：它不会别名 provider usage 证据、不合并计量项，也不会复制或迁移已配置的倍率。

## 与条件倍率规则的关系

模型价格还可以配置 `rate_rules` 条件倍率（服务档位、速度、长上下文、时间折扣）。扩展项先得到 `绑定侧基础价 × extra_ratios`，再乘以该次操作命中的条件倍率；条件规则中的独立项倍率只影响当前规则。

条件倍率只作用于按 token 计价及其扩展项，不调整 `times` 价格、独立工具费用和 `input_audio_transcription` 等独立计价路径。

配置方法见 [条件倍率配置](../use/pricing-rate-rules.md)，开发契约见 [条件倍率规则设计](../dev/pricing-rate-rules.md)。
