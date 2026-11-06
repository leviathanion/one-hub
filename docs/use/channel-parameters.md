---
title: "渠道额外参数"
layout: doc
outline: deep
lastUpdated: true
---

# 渠道额外参数

在「渠道」中打开单渠道或标签编辑，找到「额外参数」，填写一个 JSON 对象，即可为支持此配置的请求补充、覆盖或删除请求体字段。留空或填写 `{}` 表示不配置额外参数。

先选择符合需求的示例，再按需修改字段和值，复制到「额外参数」中。使用标准 JSON，不要加入注释或尾随逗号。保存时确认影响范围后，配置才会提交。

## 常用配置

### 删除参数

例如，删除客户端传入的 `temperature`、`top_p`，以及 `metadata` 对象内的 `internal` 字段：

```json
{
  "remove_params": [
    "temperature",
    "top_p",
    "metadata.internal"
  ]
}
```

`remove_params` 是字段路径的字符串数组。顶层字段直接填写名称，嵌套对象用 `.` 分隔；不支持数组下标或通配符，例如 `messages.0.content`、`metadata.*` 不会按数组索引或通配符处理。字段不存在、或中间路径不是对象时跳过，删除最后一个子字段后会保留空对象。

删除不需要开启 `overwrite`。处理顺序是先删除，再补充或覆盖；如果同一配置又提供了被删除的字段，该字段会被重新加入。将字段设置为 `null` 是传入 JSON 空值，不等同于删除。

### 给请求补充默认值

```json
{
  "temperature": 0.7
}
```

当处理阶段的请求体没有 `temperature` 时补充 `0.7`；已有该字段时保留原值。如果协议转换已生成该字段，也算“已有”。嵌套对象有单独的合并规则，见下文。

### 强制覆盖参数

```json
{
  "overwrite": true,
  "temperature": 0.7
}
```

无论请求体是否已有 `temperature`，均改为 `0.7`。`overwrite` 对这一份配置中的所有普通字段生效；同名顶层字段会被整体替换，包括对象和数组。

## 嵌套对象如何合并

添加或覆盖嵌套字段时，使用 JSON 对象结构，例如 `{"metadata":{"source":"channel"}}`。点分路径只用于 `remove_params`，不要写成 `{"metadata.source":"channel"}` 来修改子字段。

| 配置 | 顶层同名字段 | 顶层两边都是对象时 |
| --- | --- | --- |
| 不写 `overwrite`，或设为 `false` | 保留请求中的已有值，缺失时补充 | 递归合并；嵌套同名值由渠道配置覆盖，其他子字段保留 |
| `overwrite: true` | 用渠道配置替换 | 整个对象替换，未写入配置的原有子字段也会被移除 |

例如，请求中的字段为：

```json
{
  "metadata": {
    "source": "client",
    "trace": "keep"
  }
}
```

额外参数填写：

```json
{
  "metadata": {
    "source": "channel"
  }
}
```

结果是 `{"metadata":{"source":"channel","trace":"keep"}}`。即使没有开启 `overwrite`，嵌套的 `source` 仍会被覆盖。如果加上 `"overwrite": true`，结果变为 `{"metadata":{"source":"channel"}}`，`trace` 不再保留。

## 组合覆盖与删除

如果既要固定 `temperature`，又要删除 `top_p`，可以在同一份配置中同时使用覆盖和删除：

```json
{
  "overwrite": true,
  "temperature": 0.7,
  "remove_params": ["top_p"]
}
```

这会先删除 `top_p`，再将 `temperature` 设为 `0.7`。

## 只对指定模型生效

按模型配置与上面的通用配置是两种结构，不能直接叠加。开启 `per_model` 后，顶层的普通参数和 `remove_params` 会被忽略；如果多个模型需要同一条规则，应将规则分别写入每个模型对象。

开启 `per_model`，以**模型映射后的名称**作为键。比如客户端请求 `my-model`，渠道将它映射为 `gpt-4o-mini`，这里应填写 `gpt-4o-mini`：

```json
{
  "per_model": true,
  "overwrite": true,
  "gpt-4o-mini": {
    "temperature": 0.7,
    "remove_params": ["top_p"]
  }
}
```

模型名必须精确匹配，不支持通配符；没有对应配置的模型不应用额外参数。`per_model`、`overwrite`、`pre_add` 放在最外层，普通参数与 `remove_params` 放在各模型的对象中。

## 选择生效阶段

通常保持默认即可；当客户端协议和上游协议不同、需要修改转换前的字段时，再设置 `pre_add`。

| 配置 | 使用哪种字段 |
| --- | --- |
| 不写 `pre_add`，或设为 `false` | 转换后的上游原生请求字段 |
| `pre_add: true` | 适配上游协议前的请求字段；后续协议转换仍可能生成或调整字段 |

例如，在适配上游协议前删除 `top_p`：

```json
{
  "pre_add": true,
  "remove_params": ["top_p"]
}
```

`pre_add` 只选择执行阶段，不会自动开启覆盖。需要覆盖已有值时还要设置 `overwrite: true`。同一份配置只在选定阶段执行，不能同时指定转换前后两组规则。

Responses 转 Chat 的兼容路径有一个区别：`pre_add` 修改的是**已经转换出来的 Chat 请求**，因此要填写 Chat 字段，而不是原始 Responses 字段。

## 适用范围和受保护字段

额外参数适用于接入此功能的请求体处理路径，不是所有接口的统一过滤器。例如，原生图片编辑路径不应用该配置。它也不能修改 URL 查询参数或请求头；自定义请求头使用渠道的对应配置项。

- `model`、`stream`、`stream_format` 由代理管理，额外参数中对它们及其子路径的修改或删除会被忽略。模型改名请使用「模型映射」。
- `prompt_cache_key` 不允许通过额外参数修改或删除，这类配置会在保存时被拒绝。
- Responses 创建等路径保护 `background`、`conversation`、`previous_response_id`、`prompt`、`store`。额外参数不能改变这些字段的存在性或值；发生冲突时，该渠道不能承接该请求。Responses 输入 token 计数路径单独保护 `previous_response_id`。

`overwrite`、`per_model`、`pre_add`、`remove_params` 是本地控制项，不作为普通参数发给上游。其余字段仍需符合目标模型和接口的要求；添加字段不会让上游获得原本不支持的能力，删除必需字段也可能导致请求失败。
