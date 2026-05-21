---
title: "ResponsesWS 配置"
layout: doc
outline: deep
lastUpdated: true
---

# ResponsesWS 配置

本文说明 one-hub 中 `GET /v1/responses` WebSocket 入口的所有配置项、默认值、示例及对系统的影响。

ResponsesWS 是一个 **turn-based** WebSocket 入口：一条连接上可以发起多个 `response.create`，每次都是一个独立的事务（含 RPM 计次、配额预扣、affinity 记录）。所有系统级配置位于 `config.yaml`；渠道级行为通过 Web 后台 `渠道` 页面配置。

## 配置位置速查

| 配置项 | 位置 | 生效方式 |
|--------|------|----------|
| `responses_ws.connect_per_credential_per_minute` | `config.yaml` | 重启服务 |
| `responses_ws.pending_per_credential` | `config.yaml` | 重启服务 |
| `responses_ws.active_per_credential` | `config.yaml` | 重启服务 |
| `responses_ws.active_per_group` | `config.yaml` | 重启服务 |
| `responses_ws.active_global` | `config.yaml` | 重启服务 |
| `responses_ws.active_lease_redis_fail_open` | `config.yaml` | 重启服务 |
| `responses_ws.first_frame_timeout_ms` | `config.yaml` | 重启服务 |
| `responses_ws.client_pong_timeout_ms` | `config.yaml` | 重启服务 |
| `responses_ws.idle_timeout_ms` | `config.yaml` | 重启服务 |
| `responses_ws.max_lifetime_ms` | `config.yaml` | 重启服务 |
| `responses_ws.pending_provider_events_max_bytes` | `config.yaml` | 重启服务 |
| `responses_ws.allow_anonymous_capacity_bucket` | `config.yaml` | 重启服务 |
| `responses_ws.unsupported_scan_limit` | `config.yaml` | 重启服务 |
| 上游地址（Base URL） | Web 后台 → `渠道` → `渠道 API 地址` | 即刻生效（下次选渠道路由时） |
| 凭据（API Key） | Web 后台 → `渠道` → `密钥` | 即刻生效 |
| 模型列表 | Web 后台 → `渠道` → `模型` | 即刻生效 |
| 用户组 | Web 后台 → `渠道` → `用户组` | 即刻生效 |
| 计费倍率 | Web 后台 → `渠道` → `倍率` | 即刻生效 |
| Codex `websocket_mode` | Web 后台 → `渠道` → `Codex 配置(JSON)` | 即刻生效 |

---

## config.yaml 配置全景

```yaml
# config.yaml
responses_ws:
  # ---- 容量控制 ----
  connect_per_credential_per_minute: 600    # 每分钟每凭据建连尝试上限
  pending_per_credential: 96                # 等待首帧的 pending 槽位；-1 表示不限
  active_per_credential: 128                # 单凭据已建立连接上限
  active_per_group: 128                     # 单分组已建立连接上限
  active_global: 1024                       # 全局已建立连接上限
  active_lease_redis_fail_open: true        # Redis 故障时是否放行

  # ---- 超时管理 ----
  first_frame_timeout_ms: 30000             # 首帧超时
  client_pong_timeout_ms: 300000            # 客户端 pong/入站活性超时（5 分钟）
  idle_timeout_ms: 1800000                  # 空闲超时（30 分钟）
  max_lifetime_ms: 3600000                  # 最大存活时间（1 小时）

  # ---- 缓冲区 ----
  pending_provider_events_max_bytes: 2097152 # 待确认上游事件缓冲区上限（2 MiB）

  # ---- 安全策略 ----
  allow_anonymous_capacity_bucket: false    # 匿名容量桶

  # ---- 高级 ----
  unsupported_scan_limit: 5                 # 不支持时扫描的候选渠道数（默认跟随 RetryTimes）
```

---

## config.yaml 配置项详解

### 容量控制

ResponsesWS 使用三级容量控制，逐级检查：

```
建连尝试 → pending 槽位 → active 租约（三层：credential → group → global）
```

#### `responses_ws.connect_per_credential_per_minute` 🏷️ config.yaml

| 属性 | 值 |
|------|-----|
| 类型 | `int` |
| 默认值 | `600` |
| 必填 | 否 |

**作用**：每个凭据每分钟允许发起的 WebSocket 建连尝试次数（Upgrade 握手即计数，不保证最终升级成功）。防止单凭据高频建连耗尽服务端资源。

**可配置值**：

| 值 | 行为 | 影响 |
|----|------|------|
| `600`（默认） | 每分钟 600 次 | 支持单 token 多人多设备和短时重连风暴 |
| `60` | 每分钟 60 次 | 更保守的小团队场景 |
| `-1` | 不限 | 凭据可无限建连；风险：恶意凭据可发起连接洪水 |

**示例**：

```yaml
responses_ws:
  connect_per_credential_per_minute: 600
```

**对系统的影响**：
- Redis 可用时：分布式限流，多实例共享计数。
- Redis 不可用时：回退为进程内限流，多实例各算各的（总限流 = 实例数 × 配置值）。
- 超限返回 `429 Too Many Requests`。

---

#### `responses_ws.pending_per_credential` 🏷️ config.yaml

| 属性 | 值 |
|------|-----|
| 类型 | `int` |
| 默认值 | `96` |
| 必填 | 否 |

**作用**：WebSocket Upgrade 成功后、首个 `response.create` 到达前的 pending 槽位上限。每个凭据同时只能有一个连接处于"已升级但未发首帧"状态。这是针对"升级后不发首帧就挂起"的防守。

**可配置值**：

| 值 | 行为 | 影响 |
|----|------|------|
| `96`（默认） | 每凭据 96 个 pending 槽位 | 支持共享 token 下多设备同时启动，同时保留有限防守 |
| `16` | 16 个并发 pending | 小团队或低并发场景 |
| `-1` | 不限 | 关闭 pending 保护；风险：恶意客户端可升级大量连接但不发首帧 |

**示例**：

```yaml
responses_ws:
  pending_per_credential: 96
```

**对系统的影响**：
- 正常客户端：Upgrade 后通常很快发送首帧，pending 会在首帧校验和 active lease 建立后释放。
- `-1`：无防守，配合 `connect_per_credential_per_minute` 高频攻击时，数千空连接占用文件描述符和内存。

---

#### `responses_ws.active_per_credential` 🏷️ config.yaml

| 属性 | 值 |
|------|-----|
| 类型 | `int` |
| 默认值 | `128` |
| 必填 | 否 |

**作用**：单个凭据同时可维持的**已建立** ResponsesWS 连接数上限。established 定义为已打开上游 session。

**可配置值**：

| 值 | 行为 | 影响 |
|----|------|------|
| `128`（默认） | 单凭据最多 128 条 | 覆盖 10 人每人 6 设备，并为重连和多窗口预留余量 |
| `32` | 32 条 | 小团队共享同一凭据 |
| `-1` | 不限 | 不限制；风险：单凭据可耗尽全局连接 |

**示例**：

```yaml
responses_ws:
  active_per_credential: 128
```

**对系统的影响**：
- Redis 可用时：租约通过 Redis `INCR` + `EXPIRE` 跨实例协调；连接关闭时 `DECR`。后台 heartbeat goroutine 每 40s 刷新一次 TTL。
- Redis 不可用时：回退进程内计数（容量 = 实例数 × 每实例进程内值，失真）。
- 超限返回 `503 Service Unavailable`。

---

#### `responses_ws.active_per_group` 🏷️ config.yaml

| 属性 | 值 |
|------|-----|
| 类型 | `int` |
| 默认值 | `128` |
| 必填 | 否 |

**作用**：单个用户分组同时可维持的已建立 ResponsesWS 连接数上限。分组是令牌与渠道之间的关联实体（Web 后台 `用户组` 页面配置）。

**可配置值**：

| 值 | 行为 | 影响 |
|----|------|------|
| `128`（默认） | 单分组最多 128 条 | 覆盖中等规模部署 |
| `256` | 256 条 | 大规模分组 |
| `-1` | 不限 | 不限制分组级 |

**示例**：

```yaml
responses_ws:
  active_per_group: 128
```

---

#### `responses_ws.active_global` 🏷️ config.yaml

| 属性 | 值 |
|------|-----|
| 类型 | `int` |
| 默认值 | `1024` |
| 必填 | 否 |

**作用**：全局（所有凭据、所有分组）同时可维持的已建立 ResponsesWS 连接数上限。这是最后的容量防线。

**可配置值**：

| 值 | 行为 | 影响 |
|----|------|------|
| `1024`（默认） | 全局 1024 条 | 适合单机或小集群部署 |
| `4096` | 4096 条 | 大集群、高并发 |
| `-1` | 不限 | 关闭全局上限；风险：上游 provider 可能先到达限流 |

**示例**：

```yaml
responses_ws:
  active_global: 1024
```

**对系统的影响**：
- 每条 established 连接消耗：1 个 goroutine（actor loop）+ 1 个 goroutine（client read pump）+ 上游 WebSocket 连接 + pending provider events 缓冲区。
- 1024 条连接 ≈ 3000 goroutine + 上游 1024 条 TCP 连接 + 内存 ≈ 200–500 MiB（视活跃度）。

---

#### `responses_ws.active_lease_redis_fail_open` 🏷️ config.yaml

| 属性 | 值 |
|------|-----|
| 类型 | `bool` |
| 默认值 | `true` |
| 必填 | 否 |

**作用**：Redis 不可用时，active lease 容量检查的行为。

**可配置值**：

| 值 | 行为 | 影响 |
|----|------|------|
| `true`（默认） | Redis 出错时回退进程内计数 | **可用性优先**；多实例部署时容量按实例数放大（失真），可能超额放行 |
| `false` | Redis 出错时拒绝新建 active lease | **容量约束优先**；Redis 抖动期间所有新连接被拒 |

**示例**：

```yaml
# 可用性优先（默认）
responses_ws:
  active_lease_redis_fail_open: true

# 容量约束优先（生产多实例集群推荐）
responses_ws:
  active_lease_redis_fail_open: false
```

**对系统的影响**：
- `true`：Redis 抖动期间，3 实例 × 8 per credential 上限 = 最多 24 条连接（而非预期的 8），但用户不受影响。
- `false`：Redis 抖动期间所有新连接返回 `503`，用户中断，但不会突破容量上限。
- 启动时有一次性 warning 日志提示当前模式。

---

### 超时管理

#### `responses_ws.first_frame_timeout_ms` 🏷️ config.yaml

| 属性 | 值 |
|------|-----|
| 类型 | `int`（毫秒） |
| 默认值 | `30000`（30 秒） |
| 必填 | 否 |

**作用**：WebSocket Upgrade 成功后等待首个 `response.create` 的最大时间。超时后连接被关闭。只覆盖首帧读取阶段；首帧成功后 `ReadDeadline` 被清除，连接进入 client liveness 和业务 idle 超时管理。

**可配置值**：

| 值 | 行为 | 影响 |
|----|------|------|
| `30000`（默认） | 30 秒 | 覆盖慢速客户端、移动网络和首帧准备较慢的场景 |
| `10000` | 10 秒 | 更快回收异常连接 |
| `2000` | 2 秒 | 快速回收悬挂连接 |

**示例**：

```yaml
responses_ws:
  first_frame_timeout_ms: 30000
```

**对系统的影响**：
- 过小：正常慢速客户端被误关。
- 过大：恶意客户端可长时间占用 pending 槽位。
- 超时关闭走正常 close 流程（发送 close control frame）。

---

#### `responses_ws.client_pong_timeout_ms` 🏷️ config.yaml

| 属性 | 值 |
|------|-----|
| 类型 | `int`（毫秒） |
| 默认值 | `300000`（5 分钟） |
| 必填 | 否 |

**作用**：已建立 ResponsesWS 连接的客户端活性超时。服务端仍按 `realtime.websocket_ping_interval_ms` 发送 ping；客户端 data frame、ping、pong 都会刷新活性时间。超过此时间没有任何客户端入站活动时，连接以 `client_pong_timeout` 关闭。

**可配置值**：

| 值 | 行为 | 影响 |
|----|------|------|
| `300000`（默认） | 5 分钟 | 适合跨境链路、移动网络和长 Codex turn |
| `120000` | 2 分钟 | 更快回收断开的客户端 |
| `0` | 禁用 client-pong watchdog | 仅建议本地诊断，断开客户端会依赖 idle/max lifetime 回收 |

**示例**：

```yaml
responses_ws:
  client_pong_timeout_ms: 300000
```

**对系统的影响**：
- 过小：跨境链路或中间代理短暂抖动时，长 turn 可能被误关。
- 过大：真实断开的客户端会更久占用 active lease。
- 该超时不代表业务 idle；ping/pong 不会延长 `responses_ws.idle_timeout_ms`。

**与 ping 间隔的关系**：
- `client_pong_timeout_ms` 应至少为 `realtime.websocket_ping_interval_ms` 的 2–3 倍。默认值 300s 相对 25s ping 间隔有 12 倍余量，安全。
- 若调小至与 ping 间隔相近（如 30s），单个丢包或网络抖动就可能触发误关。

**排障信号**：
- 超时关闭码：`1000`（正常关闭），关闭原因字符串：`"client_pong_timeout"`
- 日志：搜索 `"client_pong_timeout"` 可定位所有因客户端活性超时关闭的连接
- 若用户频繁报告断连：先排查客户端是否正确回复 pong；若非客户端问题，适当调大 `client_pong_timeout_ms`

---

#### `responses_ws.idle_timeout_ms` 🏷️ config.yaml

| 属性 | 值 |
|------|-----|
| 类型 | `int`（毫秒） |
| 默认值 | `1800000`（30 分钟） |
| 必填 | 否 |

**作用**：已建立连接的业务空闲超时。连接上无客户端 data frame、无上游 provider frame/usage/error 超过此时间后，连接被关闭。ping/pong control frame 只刷新客户端连接活性，不刷新业务 idle。

**可配置值**：

| 值 | 行为 | 影响 |
|----|------|------|
| `1800000`（默认） | 30 分钟 | 适合间歇性交互 |
| `600000` | 10 分钟 | 更保守的资源回收 |
| `3600000` | 1 小时 | 长空闲场景（如等待用户输入） |

**示例**：

```yaml
responses_ws:
  idle_timeout_ms: 1800000
```

**对系统的影响**：
- 过短：用户思考期间连接被断开，下次交互需要重新建连（消耗额外建连尝试配额）。
- 过长：空闲连接长期占用 active lease 和上游 session。

---

#### `responses_ws.max_lifetime_ms` 🏷️ config.yaml

| 属性 | 值 |
|------|-----|
| 类型 | `int`（毫秒） |
| 默认值 | `3600000`（1 小时） |
| 必填 | 否 |

**作用**：单条 ResponsesWS 连接的最大存活时间。无论是否活跃，连接从 Upgrade 开始计时，超过此时间后强制关闭。防止连接无限存活。

**可配置值**：

| 值 | 行为 | 影响 |
|----|------|------|
| `3600000`（默认） | 1 小时 | 适合单次会话 |
| `7200000` | 2 小时 | 长时间工作会话 |
| `0` | 使用默认 1 小时 | `0` 等价于默认值，不表示无限 |

**示例**：

```yaml
responses_ws:
  max_lifetime_ms: 3600000
```

**对系统的影响**：
- 到期强制关闭：不影响正在进行中的 turn（turn 完成后正常关闭），但会阻止新 turn 创建。
- 建议 ≤ 上游 session TTL，避免连接到已过期的上游 session。

---

### 缓冲区

#### `responses_ws.pending_provider_events_max_bytes` 🏷️ config.yaml

| 属性 | 值 |
|------|-----|
| 类型 | `int`（字节） |
| 默认值 | `2097152`（2 MiB） |
| 必填 | 否 |

**作用**：`SendClient` 返回结果之前，上游 provider 可能已经开始下发事件（data frame、usage event、terminal）。这些事件必须缓存，等 send outcome 确认后再决定如何处理（是转发给客户端，还是作为 proof 触发 rollback/fail-closed）。此配置限制缓存的事件总字节数。

**可配置值**：

| 值 | 行为 | 影响 |
|----|------|------|
| `2097152`（默认） | 2 MiB | 覆盖典型 upstream burst（数秒内的 provider 响应） |
| `4194304` | 4 MiB | 更大的缓冲余量 |
| `1048576` | 1 MiB | 更严格的内存控制；burst 时可能提前 fail closed |

**示例**：

```yaml
responses_ws:
  pending_provider_events_max_bytes: 2097152
```

**对系统的影响**：
- 超限：连接 fail closed（发送 `protocol_violation` error + close control frame），当前 turn 的 quota 不回滚、不 retry。
- 此限制与硬编码的 32 条事件数上限共同起作用——两条件任一触发即 fail closed。
- 内存估算：1024 条连接 × 2 MiB = 理论峰值 2 GiB（实际瞬时值远低，因为 send outcome 通常毫秒级返回）。

---

### 安全策略

#### `responses_ws.allow_anonymous_capacity_bucket` 🏷️ config.yaml

| 属性 | 值 |
|------|-----|
| 类型 | `bool` |
| 默认值 | `false` |
| 必填 | 否 |

**作用**：无 token、无 user ID、无 auth namespace 的未认证请求，是否共享一个全局 `anonymous` 容量桶（参与 `active_per_credential` 限制）。

**可配置值**：

| 值 | 行为 | 影响 |
|----|------|------|
| `false`（默认） | 拒绝匿名容量桶 | 未认证请求无法通过容量检查，直接拒绝 |
| `true` | 启用匿名共享桶 | **仅测试/本地诊断可用**；所有匿名客户端共享同一 `active_per_credential` 配额，一个客户端可耗尽所有匿名连接 |

**示例**：

```yaml
# 生产环境（默认）
responses_ws:
  allow_anonymous_capacity_bucket: false

# 仅本地调试
responses_ws:
  allow_anonymous_capacity_bucket: true
```

**对系统的影响**：
- `false`：安全。未认证请求无法建立 ResponsesWS 连接。
- `true`：风险。若 `active_per_credential`=8，所有匿名客户端共享这 8 条连接——任何单个客户端都可以占满。
- 启动时如设为 `true`，会产生一条 warning 日志。

---

### 高级配置

#### `responses_ws.unsupported_scan_limit` 🏷️ config.yaml

| 属性 | 值 |
|------|-----|
| 类型 | `int` |
| 默认值 | 跟随 `RetryTimes`（未显式设置时默认 1） |
| 必填 | 否 |

**作用**：当首选渠道不支持 ResponsesWS 时，最多扫描多少个候选渠道寻找 WS-capable 渠道。超出后返回 `426 Upgrade Required` + wrapped error frame，客户端可 fallback 到 HTTP bridge。

**可配置值**：

| 值 | 行为 | 影响 |
|----|------|------|
| 未配置 | 使用 `RetryTimes` 值（默认 1） | 平衡性能与可用性 |
| `5` | 扫描 5 个候选 | 提高命中 WS-capable 渠道概率，但增量开销 |
| `1` | 只试首选 | 最快；首选不支持时直接 fallback |

**示例**：

```yaml
responses_ws:
  unsupported_scan_limit: 5
```

**对系统的影响**：
- 每次扫描一个候选渠道涉及：affinity 检查 + channel 配置读取 + provider WS 能力检查。扫描 5 个候选 ≈ 5 次 channel 配置读取（均从内存缓存，开销极小）。
- 不建议超过总渠道数的 1/3，避免在大量不支持 WS 的渠道上浪费时间。

---

## 渠道配置（Web 后台）

以下配置不在 `config.yaml` 中，而是通过 Web 后台设置。它们直接影响 ResponsesWS 的渠道路由和上游行为。

### 渠道 API 地址 🏷️ Web 后台 → 渠道

上游 Responses 服务的 Base URL。one-hub 会基于此地址建立 WebSocket 连接到上游。

- **OpenAI**：填写 `https://api.openai.com`
- **Azure**：填写 `https://<your-resource>.openai.azure.com`
- **Codex**：填写 `https://chatgpt.com`
- **自托管/代理**：填写你自己的中转地址

**安全说明**：上游 URL 在建立 WebSocket 连接前会经过严格 SSRF 校验。

---

### 密钥 🏷️ Web 后台 → 渠道

上游服务的 API Key。对于 OpenAI，填写 `sk-...` 格式的 key；对于 Codex，填写 OAuth 凭据 JSON。

**注意**：key 本身**不会**存入 ResponsesWS 的 upstream snapshot。snapshot 中仅存储 channel 引用（可间接获取 Base URL 等），不存储原始凭据。

---

### 模型列表 🏷️ Web 后台 → 渠道

该渠道承接的模型名称列表，如 `gpt-5`、`gpt-4o`。

客户端的 `response.create` 中的 `model` 参数必须匹配此列表，渠道才会被选中。连接建立后模型锁定——后续 turn 不可切换 model。

---

### 用户组 🏷️ Web 后台 → 渠道

按分组策略选择。渠道是否被选中受用户组、模型、权重、可用性等常规路由规则影响，和 ResponsesWS 专属配置无关。

---

### 计费倍率 🏷️ Web 后台 → 渠道

渠道的输入/输出倍率和补全倍率。影响 ResponsesWS 的配额预扣计算（`PreConsumeQuota`）。

---

### Codex 渠道专用 🏷️ Web 后台 → 渠道 → Codex 配置(JSON)

Codex 渠道作为 ResponsesWS 上游时的 `channel.Other` 配置，详见 [Codex 渠道文档](/use/Codex)。影响 ResponsesWS 的关键字段：

| 字段 | 作用 |
|------|------|
| `websocket_mode` | `"auto"`（默认）/ `"required"` / `"off"`。`"off"` 时该渠道不被 ResponsesWS 选中（返回 `unsupported`） |
| `prompt_cache_key_strategy` | 控制 prompt cache key 的生成策略，影响 channel affinity 命中率 |

---

## 连接生命周期总览

```
客户端 Upgrade 请求
    │
    ▼
[1] connect_per_credential_per_minute   ← RPM 限流 (config.yaml)
    │
    ▼
[2] pending_per_credential              ← 获取 pending 槽位 (config.yaml)
    │
    ▼
[3] first_frame_timeout_ms 内等待首帧  ← gorilla ReadDeadline (config.yaml)
    │
    ▼
[4] 首帧解析 + channel selection       ← 可能扫描 unsupported_scan_limit 个候选 (config.yaml)
    │                                       ← 渠道 API 地址/密钥/模型/用户组 (Web 后台)
    ▼
[5] active lease 获取                   ← 三级检查：credential → group → global (config.yaml)
    │
    ▼
[6] upstream session open              ← 连接上游 provider (渠道 API 地址)
    │
    ▼
[7] turn 循环（response.create → send → 转发 → terminal → finalize）
    │                                   ← pending_provider_events_max_bytes 缓冲 (config.yaml)
    │                                   ← idle_timeout_ms 空闲计时 (config.yaml)
    │                                   ← max_lifetime_ms 上限计时 (config.yaml)
    ▼
[8] close（正常关闭 / idle / max_lifetime / error）
    │
    ▼
释放 active lease + 清理 affinity + 关闭上游 session
```

## Redis 依赖

| 功能 | Redis 可用时 | Redis 不可用时 |
|------|-------------|---------------|
| `connect_per_credential_per_minute` | 分布式限流 | 进程内限流（容量 = 实例数 × 配置值） |
| `active_per_credential/group/global` | 分布式租约 + heartbeat 保持 | 进程内计数（`fail_open=true`）或拒绝（`fail_open=false`） |
| Heartbeat（active lease 刷新 TTL） | 后台 goroutine 每 40s 刷新 | 不刷新（进程内计数器已在 close 时减量） |

## 与 Realtime 配置的关系

| 维度 | Realtime | ResponsesWS |
|------|----------|-------------|
| 协议模型 | pass-through（帧透传） | turn-based（事务） |
| 系统级配置位置 | `config.yaml` → `realtime.*` | `config.yaml` → `responses_ws.*` |
| 渠道配置位置 | Web 后台 `渠道` 页面 | Web 后台 `渠道` 页面（同） |
| Origin 检查 | `realtime.allowed_origins` | 通过 `cors.allow_origins` |
| 容量控制 | 无专用容量限制 | 三级 active lease + pending slot |
| 写超时 | `realtime.websocket_write_timeout_ms` | 同（复用同一 writer primitive） |
| 读上限 | `realtime.websocket_read_limit` | 同（复用同一 read limit primitive） |
| Ping 间隔 | `realtime.websocket_ping_interval_ms` | 同 |
| Pong 超时 | 无（依赖 socket read deadline） | `responses_ws.client_pong_timeout_ms` |

## 相关文档

- [Realtime 配置](/use/realtime) — `/v1/realtime` WebSocket 配置
- [ResponsesWS 架构](/dev/responses-ws-architecture) — 内部架构说明与不变量
- [WebSocket Transport 架构](/dev/websocket-transport-architecture) — 底层复用方案
- [Codex 渠道](/use/Codex) — Codex 渠道的 `websocket_mode` 完整说明
