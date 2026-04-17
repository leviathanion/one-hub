---
title: "Codex 渠道"
layout: doc
outline: deep
lastUpdated: true
---

# Codex 渠道

本文说明 `one-hub` 中 Codex 渠道的后台配置方式，以及 `channel.Other` 和全局 Codex 高级项的配置方法。

多数用户只需要在 Web 后台完成配置：`渠道 -> 新建渠道 -> Codex`。本文优先按页面操作说明，再补充少量系统级注意事项。

## 基本说明

Codex 渠道当前通过 OpenAI 兼容接口使用，支持以下路径：

- Chat Completions: `/v1/chat/completions`
- Responses: `/v1/responses`
- Responses Compact: `/v1/responses/compact`
- Realtime: `/v1/realtime`

`Key` 应填写 Codex OAuth 凭据 JSON 或可直接使用的访问令牌。后台页面里的 `Codex 配置(JSON)` 对应后端的 `channel.Other`。

## 页面操作

### 最小可用配置

如果你只是想先在 Web 页面里把 Codex 跑起来，按下面做即可：

1. 进入 `渠道`
2. 点击 `新建渠道`
3. 渠道类型选择 `Codex`
4. 填写 `渠道名称`
5. 填写 `渠道 API 地址`
   默认可使用 `https://chatgpt.com`
6. 在 `密钥` 中填入凭据
   可直接粘贴完整的 Codex OAuth 凭据 JSON
7. 选择模型、用户组后保存

大多数场景下，`Codex 配置(JSON)` 可以先留空；留空时等效于：

```json
{
  "prompt_cache_key_strategy": "off"
}
```

### Web 页面里有哪几种导入方式

在 `渠道 -> 新建渠道 -> Codex` 页面，`密钥` 附近有 3 种常见入口：

1. `OAuth Authorization`
   页面会打开授权链接。完成登录和授权后，把浏览器回调的完整 URL 粘贴回弹窗，系统会自动把凭据填回 `密钥`。
2. `Import Auth File`
   导入单个 Codex 凭据 JSON 文件。导入后会自动填充 `密钥`，如果当前 `渠道名称` 还是空的，还会顺带填一个建议名称。
3. `Batch Import Auth Files`
   只在新建渠道时显示。适合一次导入多个 Codex 凭据 JSON，系统会按当前表单里的 `渠道 API 地址`、`Codex 配置(JSON)`、模型、用户组等设置，批量创建多个 Codex 渠道。

### 页面里各个输入框怎么填

- `渠道 API 地址`
  默认填 `https://chatgpt.com` 即可。只有你明确做了上游代理或中转时，才需要改成自己的地址。
- `密钥`
  填 Codex OAuth 凭据 JSON，或者使用上面的页面按钮导入/授权。
- `Codex 配置(JSON)`
  这是 Codex 渠道的额外配置，对应后端的 `channel.Other`。`websocket_mode` 就是在这里配置，不是全局配置项。大多数用户只会改这里，不需要去碰系统级高级项。
- `模型`
  这里填写你希望这个渠道承接的模型名，例如 `gpt-5`。如果你给多个渠道分流，建议按实际可用模型填写，避免把不支持的模型也挂进来。
- `用户组`
  按你自己的分组策略选择。Codex 渠道是否会被选中，仍然受用户组、模型、权重、可用性等常规规则影响。

## 配置项

`Codex 配置(JSON)` 当前支持的字段如下：

| 字段 | 是否必填 | 默认值 | 作用 |
| --- | --- | --- | --- |
| `prompt_cache_key_strategy` | 否 | `off` | 控制未显式传 `prompt_cache_key` 时，系统如何自动生成稳定值 |
| `websocket_mode` | 否 | `auto` | 控制 Codex realtime 优先 websocket、强制 websocket，还是直接关闭 websocket |
| `execution_session_ttl_seconds` | 否 | `600` | execution session 空闲保留时长 |
| `websocket_retry_cooldown_seconds` | 否 | `120` | websocket 失败后切回 HTTP bridge 的冷却时间 |
| `user_agent` | 否 | 内置 Codex CLI UA | 覆盖向 Codex 上游发送的 `User-Agent` |

## 从 Web 页面看，哪些配置改哪里

### 渠道级配置

以下内容都在 `渠道 -> 新建/编辑 -> Codex` 里改：

- `渠道 API 地址`
- `密钥`
- `Codex 配置(JSON)`
- 模型
- 用户组
- 代理地址

### 系统级高级配置

以下属于全局行为，不是某一个 Codex 渠道自己的字段：

- `CodexRoutingHintSetting`
- `ChannelAffinitySetting`
- `PreferredChannelWaitMilliseconds`
- `PreferredChannelWaitPollMilliseconds`

当前版本已经可以直接在 Web 后台配置这些项。页面入口是：

- `设置 -> 运营设置 -> Codex 高级配置`

这 4 项仍然属于根管理员全局选项，底层依旧走 `options` 表和 `/api/option/` 接口存储；前端页面只是把它们接到了现有全局选项保存链路上。

如果你只是通过 Web 页面正常创建 Codex 渠道，一般不需要额外配置这些项。只有你明确要调优 Responses/Realtime 的渠道亲和、等待首选渠道回归等行为时，才需要额外调整。

注意：按当前实现，这几项都属于根管理员全局选项，走 `options` 表和 `/api/option/` 接口存储，不是 `channel.Other`，也没有接入 `config.yaml` / 环境变量读取链路。也就是说，像 `CODEX_ROUTING_HINT_SETTING` 这样的环境变量当前不会生效。

页面上这 4 个字段的填写方式如下：

- `PreferredChannelWaitMilliseconds`
  数字输入框，单位毫秒。表示在首选渠道暂时不可用时，是否额外等待它回归。
- `PreferredChannelWaitPollMilliseconds`
  数字输入框，单位毫秒。表示等待首选渠道期间的轮询间隔。
- `CodexRoutingHintSetting`
  JSON 文本框。用于控制 routing 层是否在 provider 选择前派生稳定的 hint，例如 Responses 的 `prompt_cache_key`。
- `ChannelAffinitySetting`
  JSON 文本框。用于控制渠道亲和缓存自身的开关、TTL、容量和规则。

其中两个 JSON 文本框支持直接留空；留空时会回退到后端默认值。

如果你完全不额外修改这 4 个全局项，当前默认等效于：

- `PreferredChannelWaitMilliseconds: 0`
- `PreferredChannelWaitPollMilliseconds: 50`
- `CodexRoutingHintSetting`

```json
{
  "prompt_cache_key_strategy": "off",
  "model_regex": "",
  "user_agent_regex": ""
}
```

- `ChannelAffinitySetting`
  等效于下文的“模板 1：默认推荐模板”，也就是当前代码内置默认值

### `CodexRoutingHintSetting` 完整配置

当前 `CodexRoutingHintSetting` 的完整字段如下：

| 字段 | 是否必填 | 默认值 | 说明 |
| --- | --- | --- | --- |
| `prompt_cache_key_strategy` | 否 | `off` | 控制 routing 层是否在 provider 选择前派生稳定的 `responses.prompt_cache_key` |
| `model_regex` | 否 | 空 | 只对匹配该模型名正则的请求生效 |
| `user_agent_regex` | 否 | 空 | 只对匹配该 `User-Agent` 正则的请求生效 |

`prompt_cache_key_strategy` 当前支持的完整取值为：

- `off`
  默认值，不生成 routing hint。
- `auto`
  自动按优先级选稳定身份：`session_id` -> 外部认证头 -> token id -> user id。
- `session_id`
  固定按 `x-session-id` / `session_id` 派生。
- `token_id`
  固定按 one-hub token id 派生。
- `user_id`
  固定按 one-hub user id 派生。
- `auth_header`
  固定按外部认证头派生。

完整默认值等效于：

```json
{
  "prompt_cache_key_strategy": "off",
  "model_regex": "",
  "user_agent_regex": ""
}
```

### `CodexRoutingHintSetting` 怎么配置

这是最容易被误解的一项，因为它名字像“系统配置”，但实际不是写进 `config.example.yaml` 的。

当前可用的配置位置是：

1. 根管理员登录后的 Web 页面：`设置 -> 运营设置 -> Codex 高级配置`
2. 根管理员登录后的全局选项接口 `PUT /api/option/`
3. 直接写数据库 `options` 表中的 `CodexRoutingHintSetting`

如果只是人工维护，优先用 Web 页面即可；只有批量变更、自动化部署或排障时，才更适合直接调接口或改库。

如果要一次同时改多个互相关联的字段，优先用 `PUT /api/option/batch` 保证原子更新；`PUT /api/option/` 更适合单项调整，或在已有错误配置上逐项修复。

它的值本身是一个 JSON 对象，但通过 `/api/option/` 提交时，`value` 字段仍然要用字符串传，也就是“JSON 字符串里再包一层 JSON”。

如果你在 Web 页面里填写，则直接粘贴普通 JSON 即可，不需要再额外转义一层字符串。

示例：把 Responses 的 pre-routing prompt cache affinity 打开，并只对 `gpt-5` 生效：

```bash
curl --request PUT \
  --url https://你的域名/api/option/ \
  --header 'Content-Type: application/json' \
  --header 'Cookie: session=你的-root-登录会话' \
  --data '{
    "key": "CodexRoutingHintSetting",
    "value": "{\"prompt_cache_key_strategy\":\"auto\",\"model_regex\":\"^gpt-5$\"}"
  }'
```

如果你只想对特定客户端生效，还可以再加 `user_agent_regex`：

```json
{
  "prompt_cache_key_strategy": "auto",
  "model_regex": "^gpt-5$",
  "user_agent_regex": "CodexClient"
}
```

查看当前是否生效，推荐用下面两个入口：

- `GET /api/option/`
  可以直接看到 `CodexRoutingHintSetting` 当前保存的原始字符串值
- `GET /api/option/channel_affinity_cache`
  可以同时看到当前 `ChannelAffinitySetting`、缓存后端、缓存条目数、以及等待首选渠道相关配置

如果你需要把它和渠道级配置配合起来，推荐这样理解：

- `CodexRoutingHintSetting`
  决定 relay 层是否在 provider 选择前派生 `responses.prompt_cache_key`
- `channel.Other.prompt_cache_key_strategy`
  决定 Codex provider 最终向上游写什么 `prompt_cache_key`

最佳实践是两个地方保持同一套策略，例如都用 `auto`。这样 routing 命中的 key 和最终写给上游的 key 会一致。

### `ChannelAffinitySetting` 完整配置

`ChannelAffinitySetting` 是分层 JSON，不只是顶层 4 个字段。当前完整结构如下。

补充两个容易误解的行为：

- `rules` 会按数组顺序依次评估
  每条命中的规则都可能贡献 request binding / record-on-success recorder；如果某个 binding 已经命中历史 record，则第一个命中的 record 会成为本次请求的 preferred channel。
- `include_group`、`include_model`、`include_path`、`include_rule_name`
  这些字段不是“匹配条件”，而是“affinity key 分区条件”。打开后，同一个输入值会在不同 group / model / path / 规则名下生成不同的 affinity key，避免串用。

顶层字段：

| 字段 | 是否必填 | 默认值 | 说明 |
| --- | --- | --- | --- |
| `enabled` | 否 | `true` | 全局总开关。`false` 时本次请求不会读取 affinity，也不会写回新的 affinity 记录 |
| `default_ttl_seconds` | 否 | `3600` | 默认记录保留时长。某条规则未设置 `ttl_seconds`，或填了 `0/负数` 时，回退到这里 |
| `max_entries` | 否 | `50000` | 本地内存 affinity 缓存的最大条目数，用于限制单机内存占用；若启用 Redis，Redis 仍会按各条记录自己的 TTL 维护 |
| `rules` | 否 | 内置 3 条默认规则 | 规则列表。决定“哪些请求参与 affinity”“从请求里取哪个值做 affinity identity”“命中后采用什么复用/回写策略” |

`rules[*]` 字段：

| 字段 | 是否必填 | 默认值 | 说明 |
| --- | --- | --- | --- |
| `name` | 建议填写 | 空 | 规则名。主要用于调试和 key 分区；当 `include_rule_name=true` 时会拼进 affinity key，让两条规则即使取到同一个值也互不污染 |
| `enabled` | 否 | `false` | 规则开关。`false` 时该规则既不参与命中，也不会在成功后回写记录 |
| `kind` | 是 | 空 | 规则适用的请求类型。`responses` 只匹配 `/v1/responses` 这类请求；`realtime` 只匹配 `/v1/realtime` |
| `model_regex` | 否 | 空 | 请求模型名的过滤条件。只有模型名匹配正则时，这条规则才会参与评估 |
| `path_regex` | 否 | 空 | 请求路径的过滤条件。用于把规则限制在某些 API 路径上，例如只作用于 `/v1/responses` |
| `user_agent_regex` | 否 | 空 | 请求 `User-Agent` 的过滤条件。用于只对特定客户端打开 affinity |
| `include_group` | 否 | `false` | 将当前 routing group 拼进 affinity key。适合不同 group 不应共享 affinity 的场景 |
| `include_model` | 否 | `false` | 将模型名拼进 affinity key。适合不同模型即使带同一个 `prompt_cache_key` / `session_id` 也要隔离的场景 |
| `include_path` | 否 | `false` | 将请求路径拼进 affinity key。适合相同 identity 在不同 API 路径下不应复用同一渠道的场景 |
| `include_rule_name` | 否 | `false` | 将规则名拼进 affinity key。适合多条规则读取到相同 identity 时仍保持彼此隔离 |
| `ignore_preferred_cooldown` | 否 | `false` | 命中 affinity 后，选路时是否忽略 preferred channel 当前 cooldown。开启后更偏向“强行回原渠道” |
| `strict` | 否 | `false` | 命中 affinity 后，如果最终拿不到那个 preferred channel，是否直接失败而不是回退到别的渠道。开启后亲和更强，但可用性更低 |
| `skip_retry_on_failure` | 否 | `false` | 命中 affinity 的首选渠道后，如果这次调用失败，是否跳过后续常规重试/切换。适合 continuation 这类“换渠道也大概率无意义”的场景 |
| `record_on_success` | 否 | `false` | 当前请求成功后，是否把本次 identity -> channel 的映射写回 affinity 缓存。关闭后这条规则只读不写 |
| `ttl_seconds` | 否 | `0` | 这条规则自己写出的 affinity 记录保留多久。填 `0` 或负数表示使用顶层 `default_ttl_seconds` |
| `key_sources` | 是 | 空 | 这条规则从哪里提取 affinity identity。可以配置多个来源，按顺序逐个尝试；取到值后会生成 binding/key |

`rules[*].key_sources[*]` 字段：

| 字段 | 是否必填 | 默认值 | 说明 |
| --- | --- | --- | --- |
| `source` | 是 | 空 | 取值来源，当前支持 `request_field`、`header`、`query`、`request_hint` |
| `key` | 是 | 空 | 在该来源里实际读取哪个字段，例如请求头名、请求体字段名、query 参数名 |
| `alias` | 否 | 空 | 这个来源在 affinity key 里的逻辑名。它决定 key 的“identity 类型”，例如 `prompt_cache_key`、`response_id`、`session_id` |
| `value_regex` | 否 | 空 | 对取出的原始值再做一层过滤。只有值匹配正则时才会参与 affinity；不匹配就当成“没取到值” |

`source` 语义补充：

- `request_field`
  当前主要用于 Responses 请求体，内置支持 `prompt_cache_key`、`previous_response_id`，以及 `metadata.<key>`。
- `header`
  从请求头读取，例如 `x-session-id`。若 `alias=session_id` 且请求头里没值，realtime 还会回退到 runtime 已解析出的 session id。
- `query`
  从 URL query 读取。
- `request_hint`
  从 one-hub 内部 request hint 读取，例如 `responses.prompt_cache_key`。这类值通常是 routing 层或 provider 层在请求处理中派生出来的稳定 identity。

完整默认值等效于：

```json
{
  "enabled": true,
  "default_ttl_seconds": 3600,
  "max_entries": 50000,
  "rules": [
    {
      "name": "responses-continuation",
      "enabled": true,
      "kind": "responses",
      "path_regex": "^/v1/responses(?:/compact)?$",
      "include_group": true,
      "include_model": false,
      "include_path": false,
      "include_rule_name": true,
      "ignore_preferred_cooldown": false,
      "strict": true,
      "skip_retry_on_failure": true,
      "record_on_success": true,
      "ttl_seconds": 0,
      "key_sources": [
        {
          "source": "request_field",
          "key": "previous_response_id",
          "alias": "response_id"
        }
      ]
    },
    {
      "name": "responses-prompt-cache-key",
      "enabled": true,
      "kind": "responses",
      "path_regex": "^/v1/responses(?:/compact)?$",
      "include_group": true,
      "include_model": true,
      "include_path": false,
      "include_rule_name": true,
      "ignore_preferred_cooldown": false,
      "strict": false,
      "skip_retry_on_failure": false,
      "record_on_success": true,
      "ttl_seconds": 0,
      "key_sources": [
        {
          "source": "request_field",
          "key": "prompt_cache_key",
          "alias": "prompt_cache_key"
        },
        {
          "source": "request_hint",
          "key": "responses.prompt_cache_key",
          "alias": "prompt_cache_key"
        }
      ]
    },
    {
      "name": "realtime-session",
      "enabled": true,
      "kind": "realtime",
      "path_regex": "^/v1/realtime$",
      "include_group": true,
      "include_model": false,
      "include_path": false,
      "include_rule_name": true,
      "ignore_preferred_cooldown": false,
      "strict": false,
      "skip_retry_on_failure": false,
      "record_on_success": true,
      "ttl_seconds": 0,
      "key_sources": [
        {
          "source": "header",
          "key": "x-session-id",
          "alias": "session_id"
        },
        {
          "source": "header",
          "key": "session_id",
          "alias": "session_id"
        }
      ]
    }
  ]
}
```

### `ChannelAffinitySetting` 配置模板

下面这些模板都可以直接粘到：

- `设置 -> 运营设置 -> Codex 高级配置 -> ChannelAffinitySetting`

如果你只是想微调某一部分，也可以只提交部分字段；未填写的字段会沿用默认值。

#### 模板 1：默认推荐模板

这就是当前“不额外修改 `ChannelAffinitySetting`”时的内置默认配置。

适用场景：

- 你希望开启基础 affinity
- Responses continuation 要尽量回原渠道
- prompt cache key 和 realtime session 保持常规复用
- 更关注稳妥，不想过早把策略调得太激进

trade-off：

- 获得：行为最接近当前内置默认值，风险最低
- 牺牲：不会强行等待或强行绑定 preferred channel

```json
{
  "enabled": true,
  "default_ttl_seconds": 3600,
  "max_entries": 50000,
  "rules": [
    {
      "name": "responses-continuation",
      "enabled": true,
      "kind": "responses",
      "path_regex": "^/v1/responses(?:/compact)?$",
      "include_group": true,
      "include_rule_name": true,
      "strict": true,
      "skip_retry_on_failure": true,
      "record_on_success": true,
      "key_sources": [
        {
          "source": "request_field",
          "key": "previous_response_id",
          "alias": "response_id"
        }
      ]
    },
    {
      "name": "responses-prompt-cache-key",
      "enabled": true,
      "kind": "responses",
      "path_regex": "^/v1/responses(?:/compact)?$",
      "include_group": true,
      "include_model": true,
      "include_rule_name": true,
      "record_on_success": true,
      "key_sources": [
        {
          "source": "request_field",
          "key": "prompt_cache_key",
          "alias": "prompt_cache_key"
        },
        {
          "source": "request_hint",
          "key": "responses.prompt_cache_key",
          "alias": "prompt_cache_key"
        }
      ]
    },
    {
      "name": "realtime-session",
      "enabled": true,
      "kind": "realtime",
      "path_regex": "^/v1/realtime$",
      "include_group": true,
      "include_rule_name": true,
      "record_on_success": true,
      "key_sources": [
        {
          "source": "header",
          "key": "x-session-id",
          "alias": "session_id"
        },
        {
          "source": "header",
          "key": "session_id",
          "alias": "session_id"
        }
      ]
    }
  ]
}
```

#### 模板 2：Responses continuation 强亲和

适用场景：

- 你明确把 `previous_response_id` 看成“必须接着同一条上游会话继续”
- 一旦原渠道不可用，宁可直接失败，也不希望 silently 切到别的渠道

trade-off：

- 获得：continuation 语义最强，不容易串到别的渠道
- 牺牲：可用性下降，原渠道挂了就更容易直接报错

建议同时配合：

- `PreferredChannelWaitMilliseconds: 250`
- `PreferredChannelWaitPollMilliseconds: 50`

```json
{
  "enabled": true,
  "default_ttl_seconds": 3600,
  "max_entries": 50000,
  "rules": [
    {
      "name": "responses-continuation-strict",
      "enabled": true,
      "kind": "responses",
      "path_regex": "^/v1/responses(?:/compact)?$",
      "include_group": true,
      "include_model": true,
      "include_rule_name": true,
      "ignore_preferred_cooldown": true,
      "strict": true,
      "skip_retry_on_failure": true,
      "record_on_success": true,
      "ttl_seconds": 1800,
      "key_sources": [
        {
          "source": "request_field",
          "key": "previous_response_id",
          "alias": "response_id"
        }
      ]
    }
  ]
}
```

#### 模板 3：Responses prompt cache 弱亲和

适用场景：

- 你希望同一个 `prompt_cache_key` 尽量复用同一渠道
- 但不希望因为 affinity 把请求“锁死”在某个渠道上
- 更看重吞吐和可用性，而不是强一致恢复

trade-off：

- 获得：大多数情况下能复用同一渠道，命中缓存更稳定
- 牺牲：preferred channel 不可用时会自动切换，亲和性不是强约束

```json
{
  "enabled": true,
  "default_ttl_seconds": 1800,
  "max_entries": 50000,
  "rules": [
    {
      "name": "responses-prompt-cache-soft",
      "enabled": true,
      "kind": "responses",
      "path_regex": "^/v1/responses(?:/compact)?$",
      "include_group": true,
      "include_model": true,
      "include_rule_name": true,
      "strict": false,
      "skip_retry_on_failure": false,
      "record_on_success": true,
      "ttl_seconds": 1800,
      "key_sources": [
        {
          "source": "request_field",
          "key": "prompt_cache_key",
          "alias": "prompt_cache_key"
        },
        {
          "source": "request_hint",
          "key": "responses.prompt_cache_key",
          "alias": "prompt_cache_key"
        }
      ]
    }
  ]
}
```

#### 模板 4：Realtime session 强复用

适用场景：

- 你把 `x-session-id` / `session_id` 当成强 resume identity
- websocket / realtime 会话恢复必须尽量回到原渠道
- 比起自动降级，更希望优先保持会话连续性

trade-off：

- 获得：同一 realtime session 更倾向持续绑定同一渠道
- 牺牲：渠道处于 cooldown 或短时故障时，更可能等待或直接失败

建议同时配合：

- `PreferredChannelWaitMilliseconds: 500`
- `PreferredChannelWaitPollMilliseconds: 50`

```json
{
  "enabled": true,
  "default_ttl_seconds": 7200,
  "max_entries": 50000,
  "rules": [
    {
      "name": "realtime-session-strict",
      "enabled": true,
      "kind": "realtime",
      "path_regex": "^/v1/realtime$",
      "include_group": true,
      "include_rule_name": true,
      "ignore_preferred_cooldown": true,
      "strict": true,
      "skip_retry_on_failure": true,
      "record_on_success": true,
      "ttl_seconds": 7200,
      "key_sources": [
        {
          "source": "header",
          "key": "x-session-id",
          "alias": "session_id"
        },
        {
          "source": "header",
          "key": "session_id",
          "alias": "session_id"
        }
      ]
    }
  ]
}
```

#### 模板 5：按 group + model 严格隔离

适用场景：

- 你的不同 group 或不同模型之间绝不能共享 affinity
- 同一用户可能跨组、跨模型访问，但你要完全隔离渠道复用痕迹

trade-off：

- 获得：隔离性最好，不容易出现跨模型/跨组串用
- 牺牲：缓存复用面更窄，命中率会下降

```json
{
  "enabled": true,
  "default_ttl_seconds": 1800,
  "max_entries": 50000,
  "rules": [
    {
      "name": "responses-prompt-cache-isolated",
      "enabled": true,
      "kind": "responses",
      "path_regex": "^/v1/responses(?:/compact)?$",
      "include_group": true,
      "include_model": true,
      "include_path": true,
      "include_rule_name": true,
      "record_on_success": true,
      "ttl_seconds": 1800,
      "key_sources": [
        {
          "source": "request_field",
          "key": "prompt_cache_key",
          "alias": "prompt_cache_key"
        },
        {
          "source": "request_hint",
          "key": "responses.prompt_cache_key",
          "alias": "prompt_cache_key"
        }
      ]
    },
    {
      "name": "realtime-session-isolated",
      "enabled": true,
      "kind": "realtime",
      "path_regex": "^/v1/realtime$",
      "include_group": true,
      "include_model": true,
      "include_path": true,
      "include_rule_name": true,
      "record_on_success": true,
      "ttl_seconds": 7200,
      "key_sources": [
        {
          "source": "header",
          "key": "x-session-id",
          "alias": "session_id"
        },
        {
          "source": "header",
          "key": "session_id",
          "alias": "session_id"
        }
      ]
    }
  ]
}
```

## 推荐模板

### 默认行为

不填写 `Codex 配置(JSON)` 时，默认等效于：

```json
{
  "prompt_cache_key_strategy": "off"
}
```

### 最常见：`websocket_mode` 怎么配

填写位置：

- `渠道 -> 新建/编辑 -> Codex -> Codex 配置(JSON)`

这个字段只影响 Codex 的 `/v1/realtime`，不影响普通的 `/v1/responses` 和 `/v1/chat/completions`。

推荐默认配置：

```json
{
  "websocket_mode": "auto"
}
```

如果你希望“必须走 websocket，失败就直接报错”，可以改成：

```json
{
  "websocket_mode": "force"
}
```

如果你希望“完全禁用 websocket，固定走 HTTP bridge”，可以改成：

```json
{
  "websocket_mode": "off"
}
```

### 自动生成稳定缓存身份

```json
{
  "prompt_cache_key_strategy": "auto"
}
```

`auto` 的实际优先级是：

1. 显式请求字段 `prompt_cache_key`
2. 请求头 `x-session-id` / `session_id`
3. 外部认证头
4. One Hub 令牌 ID
5. One Hub 用户 ID

### 按 session_id 绑定缓存

```json
{
  "prompt_cache_key_strategy": "session_id"
}
```

### 同一用户多个令牌共享缓存

```json
{
  "prompt_cache_key_strategy": "user_id"
}
```

### 每个令牌独立缓存

```json
{
  "prompt_cache_key_strategy": "token_id"
}
```

### 按外部认证头共享缓存

```json
{
  "prompt_cache_key_strategy": "auth_header"
}
```

### Realtime 优先 websocket，失败自动回退

```json
{
  "prompt_cache_key_strategy": "off",
  "websocket_mode": "auto",
  "execution_session_ttl_seconds": 600,
  "websocket_retry_cooldown_seconds": 120
}
```

## `prompt_cache_key_strategy`

当请求本身没有显式传 `prompt_cache_key` 时，Codex 渠道会按策略自动生成稳定值，并映射到上游的：

- `prompt_cache_key`
- `Conversation_id`
- `Session_id`

这个稳定值同时会作为 Responses 路径的 channel affinity key。也就是说，同一个 `prompt_cache_key` 会优先回到上次成功的 Codex 渠道。

如果客户端已经显式传入 `prompt_cache_key`，客户端值优先，自动生成逻辑不会覆盖它。

如果你希望“未显式传 `prompt_cache_key`，但由 one-hub 自动生成的稳定值”也能在下一次请求里于 provider 选择前命中 channel affinity，需要额外配置系统级的 `CodexRoutingHintSetting`。这是 routing 层的唯一策略来源；`channel.Other.prompt_cache_key_strategy` 只保留 provider-side fallback 语义：

- `CodexRoutingHintSetting`
  - 负责在 relay 层提前派生 `responses.prompt_cache_key` request hint，让 affinity 命中发生在 provider 选择前
- `channel.Other.prompt_cache_key_strategy`
  - 负责 Codex provider 最终向上游写入 `prompt_cache_key` 的兼容 fallback

默认的 `ChannelAffinitySetting` 已经同时读取：

- 显式请求字段 `prompt_cache_key`
- request hint `responses.prompt_cache_key`

因此只要 `CodexRoutingHintSetting` 生成了稳定 hint，Responses affinity 就会自动复用它；provider 也会优先复用同一个 hint，不会再单独生成另一份值。

推荐模板：

```json
{
  "prompt_cache_key_strategy": "auto",
  "model_regex": "^gpt-5$",
  "user_agent_regex": "CodexClient"
}
```

如果不配置 `CodexRoutingHintSetting`，那么自动生成的 `prompt_cache_key` 仍然会在 Codex provider 内作为 legacy fallback 生效，但不会具备 pre-routing affinity 命中能力。

也就是说，只在渠道里把 `Codex 配置(JSON)` 设成下面这样：

```json
{
  "prompt_cache_key_strategy": "auto"
}
```

只能保证 provider 侧会补出稳定 `prompt_cache_key` 并写给上游；下一次请求在“选渠道之前”仍然看不到这个值，因此不能依赖它提前命中 Responses affinity。

| 策略 | 稳定身份来源 | 适用场景 |
| --- | --- | --- |
| `off` | 不自动生成 | 默认行为，或希望完全由客户端自己控制 |
| `auto` | 显式 `prompt_cache_key` -> `x-session-id/session_id` -> 请求头认证值 -> `token_id` -> `user_id` | 大多数场景的推荐配置 |
| `session_id` | 请求头中的 `x-session-id` / `session_id` | 客户端稳定传会话 ID，希望按会话维度粘住缓存 |
| `token_id` | One Hub 令牌 ID | 希望每个令牌独立维护缓存 |
| `user_id` | One Hub 用户 ID | 同一用户多个令牌共享缓存 |
| `auth_header` | 请求头中的认证值 | 想按外部调用凭证划分缓存身份 |

如果你打算在请求体中自己传 `prompt_cache_key`，保持默认 `off` 即可，不需要额外模板。

## Responses 使用注意事项

### `/v1/responses/compact`

`/v1/responses/compact` 只支持非流式请求，不支持 `stream: true`。

如果客户端要用流式输出，请改走普通的 `/v1/responses`。

### `previous_response_id` 失效时不会自动补救

当上游返回陈旧的 `previous_response_id` 时，one-hub 会直接返回：

- `409 Conflict`
- 错误码 `previous_response_not_found`

此时 one-hub 不会自动帮客户端改写请求并重试，客户端需要携带完整上下文重新发送请求。

## Realtime 相关配置

这些配置只影响 Codex 的 `/v1/realtime` 路径，不影响普通的 `/v1/responses` 和 `/v1/chat/completions`。

### `websocket_mode`

配置位置：

- `渠道 -> 新建/编辑 -> Codex -> Codex 配置(JSON)`

最小示例：

```json
{
  "websocket_mode": "auto"
}
```

| 值 | 行为 |
| --- | --- |
| `auto` | 优先 `responses-ws`，握手失败或后续发送失败时自动回退到 `responses-http-bridge` |
| `force` | 必须使用 websocket，握手失败直接报错，不做回退 |
| `off` | 不尝试 websocket，直接走 HTTP bridge |

推荐默认使用 `auto`。

补充说明：

- `auto` 适合大多数场景，优先吃到 websocket 的低延迟；如果上游暂时不支持或握手失败，会自动回退
- `force` 适合你明确要求上游必须支持 realtime websocket 的场景；任何 websocket 建连失败都会直接返回错误
- `off` 适合网络环境对 websocket 不友好，或者你希望行为更稳定、更容易排查时使用

### `execution_session_ttl_seconds`

控制 execution session 在空闲状态下保留多久。默认 `600` 秒，也就是 10 分钟。

作用：

- 同一个调用方带着同一个 `session_id` / `x-session-id` 回到同一个渠道时，可以复用之前的 execution session
- 超过 TTL 后，runtime 会清理空闲 session，释放上游连接

### 全局 `codex.execution_session_revocation_timeout_ms`

配置位置：

- 服务端配置文件，例如 `config.yaml`
- 环境变量 `CODEX_EXECUTION_SESSION_REVOCATION_TIMEOUT_MS`

默认值：`200` 毫秒。

作用：

- 控制 Codex execution session manager 做 revocation 查询时的超时
- 超时或 backend error 会收敛为 `unknown`
- `unknown` 不会 resume 旧 session，而是走 fresh/local-only 路径

trade-off：

- 值越短，最坏等待时间越小，但 `unknown` 比例会上升，resume 命中率会下降
- 值越长，resume 命中率通常更高，但会增加 revocation probe 的尾延迟

注意：

- 这是全局 manager 拨盘，不是 `渠道 -> Codex 配置(JSON)` 的字段
- 它影响的是 revocation probe，不改变 `execution_session_ttl_seconds` 的本地空闲回收语义

### `websocket_retry_cooldown_seconds`

当 Codex websocket 握手失败，或者 session 内 websocket 发送失败后，runtime 会进入 bridge 冷却时间。默认 `120` 秒。

在冷却时间内：

- 同一个 execution session 继续走 HTTP bridge
- 不会每次请求都重新尝试 websocket 握手

## Realtime 渠道亲和

Codex realtime 现在采用 channel affinity + same-channel resume 语义：

- 渠道亲和 key 是 `caller namespace + session_id`
- `caller namespace` 默认按 `token_id -> user_id -> 外部认证值` 推导
- 命中 affinity 时，系统只会先尝试上次成功的那个 channel
- 只有在同一个 channel 内，provider 才会尝试复用本地 execution session / 上游 websocket
- 如果该 channel 不可用，或 same-channel resume 因模型、headers、UA、base URL、credential 等兼容性变化失败，请求会走 fresh route，并在成功后把 affinity 改写到新 channel
- 不支持跨 channel 延续旧的上游 realtime 会话

## Realtime `session_id` 规则

Codex realtime 会读取请求头中的 `x-session-id`，如果没有则读取 `session_id`。

这个值用于建立 `caller namespace + session_id` 的 affinity key，不是全局共享的会话标识。

当前允许的 session ID 规则：

- 最长 `128` 个字符
- 只允许字母、数字、`-`、`_`、`.`、`:`
- 为空、超长、或包含其他字符会被拒绝

建议客户端直接使用 UUID 或其它稳定的短 ID。

如果客户端希望 Realtime 会话可恢复，应该稳定传入同一个 `x-session-id` 或 `session_id`。

如果客户端完全不传 `x-session-id` / `session_id`，one-hub 会为当前请求生成临时的上游 session id，但不会建立可恢复的 resume binding。也就是说，这种请求只适合当前连接使用，后续不能依赖它继续恢复到同一个 Codex Realtime 会话。
