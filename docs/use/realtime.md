---
title: "Realtime 配置"
layout: doc
outline: deep
lastUpdated: true
---

# Realtime 配置

本文说明 one-hub 中 `/v1/realtime` WebSocket 入口的所有配置项、默认值、示例及对系统的影响。

## 配置位置速查

| 配置项 | 位置 | 生效方式 |
|--------|------|----------|
| `realtime.allowed_origins` | `config.yaml` | 重启服务 |
| `realtime.unsafe_allow_credential_subprotocol_any_origin` | `config.yaml` | 重启服务 |
| `realtime.websocket_read_limit` | `config.yaml` | 重启服务 |
| `realtime.websocket_ping_interval_ms` | `config.yaml` | 重启服务 |
| `realtime.websocket_write_timeout_ms` | `config.yaml` | 重启服务 |
| `openai.realtime_session_compat` | `config.yaml` | 重启服务 |
| `cors.allow_origins` | `config.yaml` | 重启服务（realtime 回退时读取） |
| 上游地址（Base URL） | Web 后台 → `渠道` → `渠道 API 地址` | 即刻生效（下次选渠道路由时） |
| 凭据（API Key） | Web 后台 → `渠道` → `密钥` | 即刻生效 |
| 模型列表 | Web 后台 → `渠道` → `模型` | 即刻生效 |
| 用户组 | Web 后台 → `渠道` → `用户组` | 即刻生效 |
| `OpenAI-Beta` 等自定义头 | Web 后台 → `渠道` → `模型自定义头部` | 即刻生效 |

---

## config.yaml 配置全景

```yaml
# config.yaml
openai:
  realtime_session_compat: false    # OpenAI Realtime 兼容模式
cors:
  allow_origins:                    # CORS Origin 白名单（realtime 会回退至此）
    - "https://app.example.com"
realtime:
  allowed_origins:                  # Realtime WebSocket Origin 白名单
    - "https://app.example.com"
  unsafe_allow_credential_subprotocol_any_origin: false
  websocket_read_limit: 16777216    # 单帧读取上限（字节）
  websocket_ping_interval_ms: 25000 # 服务端主动 Ping 周期（毫秒）
  websocket_write_timeout_ms: 10000 # WebSocket 写超时（毫秒）
```

---

## config.yaml 配置项详解

### `realtime.allowed_origins` 🏷️ config.yaml

| 属性 | 值 |
|------|-----|
| 类型 | `[]string` |
| 默认值 | 无（回退到 `cors.allow_origins`；两者均为空时允许任意 Origin） |
| 必填 | 生产环境**强烈建议配置** |

**作用**：WebSocket 升级请求的 `Origin` 头白名单。只允许列表中的 Origin 建立 WebSocket 连接。

**可配置值**：

| 值 | 行为 | 影响 |
|----|------|------|
| 显式配置 `["https://app.example.com"]` | 仅允许该 Origin | 生产推荐；防止未授权前端页面建立 WebSocket 连接 |
| 空（默认） | 回退到 `cors.allow_origins` | 若 CORS 也为空，则允许任意 Origin（兼容旧版，**不安全**） |
| 包含 `"*"` | 允许任意 Origin | 但不能与 `openai-insecure-api-key.*` 子协议共存（见下条） |

**示例**：

```yaml
# 单 Origin
realtime:
  allowed_origins:
    - "https://app.example.com"

# 多 Origin
realtime:
  allowed_origins:
    - "https://app.example.com"
    - "https://admin.example.com"

# 回退到 CORS 配置（推荐统一管理）
cors:
  allow_origins:
    - "https://app.example.com"
# realtime.allowed_origins 留空即可
```

**对系统的影响**：
- 白名单以外的 Origin 请求会在 WebSocket 握手阶段被拒绝，返回 `403 Forbidden` + `realtime_origin_not_allowed`。
- 空配置 + CORS 也为空：兼容旧版，但任何网页都可以通过 JavaScript 连接到你的 Realtime 入口。**生产环境应避免。**

---

### `realtime.unsafe_allow_credential_subprotocol_any_origin` 🏷️ config.yaml

| 属性 | 值 |
|------|-----|
| 类型 | `bool` |
| 默认值 | `false` |
| 必填 | 否 |

**作用**：控制 `openai-insecure-api-key.*` 子协议在**空/通配 Origin 白名单**下的行为。

OpenAI 旧版客户端会在 WebSocket 握手时通过 `Sec-WebSocket-Protocol` 头直接传递 API Key（格式 `openai-insecure-api-key.sk-xxx`）。出于安全考虑，one-hub 默认要求：使用该子协议时**必须配置显式非通配 Origin 白名单**。

**可配置值**：

| 值 | 行为 | 影响 |
|----|------|------|
| `false`（默认） | 要求显式 Origin 白名单 | 安全；旧版客户端需要配合 `realtime.allowed_origins` 使用 |
| `true` | 允许空/通配 Origin 下使用 key 子协议 | **仅兼容旧浏览器直连**；安全风险，不推荐生产使用 |

**示例**：

```yaml
# 安全配置（推荐）
realtime:
  allowed_origins:
    - "https://my-app.example.com"
  # unsafe_allow_credential_subprotocol_any_origin 默认 false，无需配置

# 仅兼容旧版测试（不推荐生产）
realtime:
  allowed_origins:
    - "*"
  unsafe_allow_credential_subprotocol_any_origin: true
```

**对系统的影响**：
- `false`（默认）：安全。客户端必须提供匹配的 Origin 头。
- `true`：any Origin + key 子协议可连接。API Key 可能通过浏览器 WebSocket API 暴露给任意网页。

---

### `realtime.websocket_read_limit` 🏷️ config.yaml

| 属性 | 值 |
|------|-----|
| 类型 | `int64`（字节） |
| 默认值 | `33554432`（32 MiB） |
| 必填 | 否 |

**作用**：WebSocket 单帧读取上限。超过此大小的帧会被拒绝，连接返回静态 `invalid_event` 错误后关闭。

**可配置值**：

| 值 | 行为 | 影响 |
|----|------|------|
| `33554432`（默认） | 单帧最大 32 MiB | 覆盖更大的 Codex 上下文和文件负载，但增加单连接峰值内存压力 |
| `16777216` | 16 MiB | 更保守的内存预算；大上下文或大首帧更容易被拒绝 |
| `1048576` | 1 MiB | 更严格的内存控制；可能截断大上下文或长音频帧 |

**示例**：

```yaml
realtime:
  websocket_read_limit: 33554432   # 32 MiB（默认）

# 内存受限环境
realtime:
  websocket_read_limit: 4194304    # 4 MiB

# 高负载大上下文
realtime:
  websocket_read_limit: 33554432   # 32 MiB
```

**对系统的影响**：
- 增大：允许更大的帧，每条连接可能占用更多内存（上限为此值）。
- 减小：内存安全，但可能导致合法大帧被拒绝。
- **注意**：此限制作用于每条连接。1000 条并发连接 × 16 MiB = 理论峰值 16 GiB（实际低得多，正常帧远小于上限）。

---

### `realtime.websocket_ping_interval_ms` 🏷️ config.yaml

| 属性 | 值 |
|------|-----|
| 类型 | `int`（毫秒） |
| 默认值 | `25000`（25 秒） |
| 必填 | 否 |

**作用**：one-hub 向客户端发送 WebSocket Ping 帧的间隔。用于保持连接活跃、检测死连接。

**可配置值**：

| 值 | 行为 | 影响 |
|----|------|------|
| `25000`（默认） | 每 25 秒 Ping 一次 | 平衡保活及时性与网络开销 |
| `<= 0` | 禁用服务端 Ping | 连接可能被中间代理（Nginx、ALB）超时断开；**仅建议测试环境使用** |
| `60000` | 每 60 秒 | 对代理超时配置宽松时减少开销 |
| `10000` | 每 10 秒 | 更快检测断连，但增加极少量 CPU/网络开销 |

**示例**：

```yaml
realtime:
  websocket_ping_interval_ms: 25000

# 代理超时为 120s，Ping 间隔可放宽
realtime:
  websocket_ping_interval_ms: 60000

# 严格保活（如经过多层代理）
realtime:
  websocket_ping_interval_ms: 10000
```

**对系统的影响**：
- 过小（<5s）：每条连接频繁发送 Ping 帧，增加无效网络流量。
- 过大（>代理超时）：代理可能在 Ping 前就关闭空闲连接。
- 设为 0：客户端断连不会被服务端主动感知，直到写超时或读超时。

---

### `realtime.websocket_write_timeout_ms` 🏷️ config.yaml

| 属性 | 值 |
|------|-----|
| 类型 | `int`（毫秒） |
| 默认值 | `10000`（10 秒） |
| 必填 | 否 |

**作用**：向客户端或上游写 WebSocket 帧的超时时间。写操作超时后连接可能被关闭。

**可配置值**：

| 值 | 行为 | 影响 |
|----|------|------|
| `10000`（默认） | 写超时 10 秒 | 覆盖正常网络抖动 |
| `5000` | 5 秒 | 更快释放慢连接资源 |
| `30000` | 30 秒 | 容忍高延迟网络（如跨国链路） |

**示例**：

```yaml
realtime:
  websocket_write_timeout_ms: 10000

# 高延迟跨国场景
realtime:
  websocket_write_timeout_ms: 30000

# 内网低延迟
realtime:
  websocket_write_timeout_ms: 3000
```

**对系统的影响**：
- 过小：正常慢速客户端可能被误关闭。
- 过大：慢消费者占用连接资源更久。
- 写超时不区分客户端侧还是上游侧——两者共用同一定时器。

---

### `openai.realtime_session_compat` 🏷️ config.yaml

| 属性 | 值 |
|------|-----|
| 类型 | `bool` |
| 默认值 | `false` |
| 必填 | 否 |

**作用**：OpenAI Realtime 兼容模式开关。开启后，上游 Realtime session 中的 error 事件在转发给客户端后**立即关闭当前会话**，不再等待上游主动断开。

**可配置值**：

| 值 | 行为 | 影响 |
|----|------|------|
| `false`（默认） | 上游 error 转发后继续等待上游关闭 | 兼容 OpenAI Realtime GA 协议；会话关闭节奏由上游控制 |
| `true` | 上游 error 转发后立即 close session | 旧版客户端可能在收到 error 后期望连接立即断开；代价是可能中断上游仍在发送的残留帧 |

**示例**：

```yaml
openai:
  realtime_session_compat: false
```

**对系统的影响**：
- `false`：正常转发，无额外影响。
- `true`：每次上游 error 都会触发额外 `session.Abort`，略微增加 goroutine 调度开销。若上游在 error 后仍有 data frame，会被截断。

---

## 渠道配置（Web 后台）

以下配置不在 `config.yaml` 中，而是通过 Web 后台 `渠道 → 新建/编辑` 页面设置，或在 Codex 渠道的 `Codex 配置(JSON)` 中设置。

### 渠道 API 地址 🏷️ Web 后台 → 渠道

上游 Realtime 服务的 Base URL。one-hub 会基于此地址拼接 WebSocket 路径（如 `/v1/realtime`）。

- **OpenAI**：填写 `https://api.openai.com`
- **Azure**：填写 `https://<your-resource>.openai.azure.com`
- **自托管/代理**：填写你自己的中转地址

**示例（Web 后台）**：

| 渠道类型 | 渠道 API 地址 |
|----------|---------------|
| OpenAI | `https://api.openai.com` |
| Azure | `https://my-eastus.openai.azure.com` |
| 自定义代理 | `https://proxy.example.com` |

**安全说明**：上游 URL 在建立 WebSocket 连接前会经过严格校验（SSRF 防护、DNS 重绑定防护、私有 IP 拦截、IDN 规范化），详见 [上游地址安全](#上游地址安全)。

---

### 密钥 🏷️ Web 后台 → 渠道

上游服务的 API Key。对于 OpenAI，填写 `sk-...` 格式的 key；对于 Codex，填写 OAuth 凭据 JSON。

**注意**：旧版 OpenAI 客户端可能通过 `Sec-WebSocket-Protocol: openai-insecure-api-key.sk-xxx` 直接在握手时传递 key。one-hub 会从该子协议中提取凭据，但要求客户端同时提供合法 Origin 头。

---

### 模型列表 🏷️ Web 后台 → 渠道

该渠道承接的模型名称列表，如 `gpt-4o-realtime-preview`、`gpt-4o-mini-realtime-preview`。

客户端的 `model` 参数必须匹配此列表，渠道才会被选中。

---

### Codex 渠道专用：`websocket_mode` 🏷️ Web 后台 → 渠道 → Codex 配置(JSON)

**位置**：`渠道 → Codex → Codex 配置(JSON)` 中的 `websocket_mode` 字段。**注意**：这是渠道级配置，不是 `config.yaml` 全局配置。

| 值 | 行为 | 影响 |
|----|------|------|
| `"auto"`（默认） | 优先 WebSocket，失败后回退 HTTP bridge | 推荐；兼顾性能与可用性 |
| `"required"` | 强制 WebSocket，不支持则拒绝 | WebSocket 不可用时请求失败 |
| `"off"` | 禁用 WebSocket，始终走 HTTP bridge | 不使用 Codex Realtime |

**示例（渠道 Codex 配置(JSON)）**：

```json
{
  "websocket_mode": "required"
}
```

**对系统的影响**：
- `"required"`：若 Codex upstream 不支持 WebSocket，客户端收到错误。
- `"off"`：关闭 WebSocket 连接能力，节省连接资源但失去 Realtime 低延迟优势。

Codex 渠道的完整 `channel.Other` 配置见 [Codex 渠道文档](/use/Codex)。

---

### Codex 渠道专用：`websocket_retry_cooldown_seconds` 🏷️ Web 后台 → 渠道 → Codex 配置(JSON)

**位置**：`渠道 → Codex → Codex 配置(JSON)` 中的 `websocket_retry_cooldown_seconds` 字段。WebSocket 连接失败后的冷却时间，在此期间不重试 WebSocket 而是走 HTTP bridge。

| 值 | 行为 |
|----|------|
| `120`（默认） | 2 分钟冷却 |
| `0` | 无冷却，立即重试 |

---

## Origin 检查策略

Realtime WebSocket 的 Origin 检查按以下优先级执行：

1. 无 Origin 头 + 无 `openai-insecure-api-key.*` 子协议 → 放行
2. 无 Origin 头 + 有 key 子协议 → 拒绝（`403`）
3. 有 Origin 头 → 依次检查：
   - `realtime.allowed_origins` 非空 → 只允许列表中的 Origin
   - `realtime.allowed_origins` 为空 → 回退到 `cors.allow_origins`
   - 两者均为空 → 允许任意 Origin（旧版兼容）
4. 列表中包含 `"*"` 但有 key 子协议 → 拒绝（除非启用 `unsafe_allow_credential_subprotocol_any_origin`）

## 上游地址安全

所有 Realtime WebSocket 上游 URL 在建立连接前会经过以下校验（由 `ValidateUpstreamRealtimeURL` 执行）：

| 校验项 | 说明 |
|--------|------|
| Scheme 限制 | 仅允许 `wss://`；自托管环境可通过渠道标记启用 `ws://` |
| Host 封禁 | 拒绝 `localhost`、`.localhost` 后缀、`0.0.0.0`、`[::]` 等 |
| IP 封禁 | 拒绝私有/链路本地/回环 IP，额外封禁阿里云元数据地址 `100.100.100.200` 和 AWS IMDS `169.254.169.254` |
| DNS 解析 | 默认开启：解析 hostname 后再次校验 IP，防止 DNS 重绑定攻击 |
| IDN 规范化 | 国际化域名经 Punycode 转换后校验，防止同形异义字绕过 |

## 相关文档

- [ResponsesWS 配置](/use/responses-ws) — `GET /v1/responses` WebSocket 配置
- [Codex 渠道](/use/Codex) — Codex 渠道的 `websocket_mode` 完整说明
- [WebSocket Transport 架构](/dev/websocket-transport-architecture) — 底层复用方案
