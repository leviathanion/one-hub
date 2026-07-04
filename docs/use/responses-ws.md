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
| `responses_ws.bridge_open_timeout_ms` | `config.yaml` | 重启服务 |
| `responses_websocket_client_ping_interval_ms` | `config.yaml` | 重启服务 |
| `responses_websocket_client_pong_miss_timeout_ms` | `config.yaml` | 重启服务 |
| `responses_websocket_client_inbound_activity_timeout_ms` | `config.yaml` | 重启服务 |
| `responses_ws.idle_timeout_ms` | `config.yaml` | 重启服务 |
| `responses_ws.active_turn_timeout_ms` | `config.yaml` | 重启服务 |
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
| `responses_ws_transport` | Web 后台 → `渠道` → `Other(JSON)` / `Codex 配置(JSON)` | 即刻生效 |
| `responses_ws_native` | Web 后台 → `渠道` → `Other(JSON)` | 即刻生效 |
| `responses_ws_self_hosted` | Web 后台 → `渠道` → `Other(JSON)` / `Codex 配置(JSON)` | 即刻生效 |

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
  bridge_open_timeout_ms: 30000             # HTTP bridge 打开上游 stream 超时
  idle_timeout_ms: 1800000                  # 空闲超时（30 分钟）
  active_turn_timeout_ms: 120000            # 单 turn active 超时（2 分钟）
  max_lifetime_ms: 3600000                  # 最大存活时间（1 小时）

  # ---- 缓冲区 ----
  pending_provider_events_max_bytes: 2097152 # 待确认上游事件缓冲区上限（2 MiB）

  # ---- 安全策略 ----
  allow_anonymous_capacity_bucket: false    # 匿名容量桶

  # ---- 高级 ----
  unsupported_scan_limit: 0                 # 显式 unsupported 扫描上限；0/未配置按当前加载渠道数扫描

responses_websocket_client_ping_interval_ms: 25000              # 客户端连接主动 Ping 周期
responses_websocket_client_pong_miss_timeout_ms: 0              # 发出 ping 后未收到对应 pong 的超时；0 禁用
responses_websocket_client_inbound_activity_timeout_ms: 300000  # 任意客户端入站活动超时（5 分钟）
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

**作用**：WebSocket Upgrade 成功后、首个 `response.create` 到达前的 pending 槽位上限。每个凭据最多允许配置值数量的连接同时处于"已升级但未发首帧"状态。这是针对"升级后不发首帧就挂起"的防守。

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
- 单凭据 active 连接超限返回 `429 Too Many Requests`；group/global active 连接超限返回 `503 Service Unavailable`。

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

#### `responses_ws.bridge_open_timeout_ms` 🏷️ config.yaml

| 属性 | 值 |
|------|-----|
| 类型 | `int`（毫秒） |
| 默认值 | `30000`（30 秒） |
| 必填 | 否 |

**作用**：仅限制 ResponsesWS HTTP bridge 等待上游 HTTP `/responses` stream 打开的时间，也就是等待请求发出、上游返回响应头并创建 stream reader 的阶段。stream 已打开后，该 timeout 不再作用于后续 SSE 读取；后续活性仍由 `responses_ws.active_turn_timeout_ms` 和连接 idle/lifetime watchdog 管理。

```yaml
responses_ws:
  bridge_open_timeout_ms: 30000
```

`<=0` 表示禁用这个 opening watchdog。Trade-off：多一个配置项和一个 opening 阶段 watchdog，换取代理/上游卡在响应头前时的资源保护；代价是极慢上游可能在 30 秒边界被本地取消，需要按部署网络调整。

---

#### ResponsesWS 客户端 liveness 🏷️ config.yaml

| 属性 | 值 |
|------|-----|
| 类型 | `int`（毫秒） |
| 默认值 | ping interval `25000`；pong miss `0`；inbound activity `300000` |
| 必填 | 否 |

**作用**：已建立 ResponsesWS 连接的传输层 liveness 拆为两个独立语义。`responses_websocket_client_pong_miss_timeout_ms` 只表示发出 ping 后未收到对应 pong；`responses_websocket_client_inbound_activity_timeout_ms` 表示多久没有任何客户端入站 control frame 或完整 data frame。当前 ResponsesWS established 阶段使用 inbound activity watchdog；超过该时间没有任何客户端入站活动时，连接以 `inbound_idle` 关闭。旧配置 `responses_ws.client_pong_timeout_ms` 已移除，不再读取或 fallback，请迁移到 `responses_websocket_client_inbound_activity_timeout_ms`。

**可配置值**：

| 配置项 | 默认值 | 行为 |
|--------|--------|------|
| `responses_websocket_client_ping_interval_ms` | `25000` | 服务端主动 ping 周期；`<=0` 禁用主动 ping |
| `responses_websocket_client_pong_miss_timeout_ms` | `0` | 对应 pong 缺失判死；`0` 禁用 |
| `responses_websocket_client_inbound_activity_timeout_ms` | `300000` | 客户端入站活性超时；`0` 禁用 watchdog |

这些 liveness 配置在 WebSocket 连接创建时取快照；修改 `config.yaml` 后只影响新连接，已建立连接不会热重载。`pong_miss_timeout_ms=0` 是有意的宽松默认，需要更快发现半开连接时可显式启用。

**示例**：

```yaml
responses_websocket_client_ping_interval_ms: 25000
responses_websocket_client_pong_miss_timeout_ms: 0
responses_websocket_client_inbound_activity_timeout_ms: 300000
```

**对系统的影响**：
- 过小：跨境链路或中间代理短暂抖动时，长 turn 可能被误关。
- 过大：真实断开的客户端会更久占用 active lease。
- 该超时不代表业务 idle；ping/pong 不会延长 `responses_ws.idle_timeout_ms`。

**与 ping 间隔的关系**：
- `responses_websocket_client_inbound_activity_timeout_ms` 应至少为 `responses_websocket_client_ping_interval_ms` 的 2–3 倍。默认值 300s 相对 25s ping 间隔有 12 倍余量，安全。
- 若调小至与 ping 间隔相近（如 30s），单个丢包或网络抖动就可能触发误关。

**排障信号**：
- 超时关闭码：`1000`（正常关闭），关闭原因字符串：`"inbound_idle"`
- 日志：搜索 `"inbound_idle"` 可定位所有因客户端入站活性超时关闭的连接
- 若用户频繁报告断连：先排查客户端是否持续处理服务端 ping 并产生入站 pong；若非客户端问题，适当调大 `responses_websocket_client_inbound_activity_timeout_ms`

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

#### `responses_ws.active_turn_timeout_ms` 🏷️ config.yaml

| 属性 | 值 |
|------|-----|
| 类型 | `int`（毫秒） |
| 默认值 | `120000`（2 分钟） |
| 必填 | 否 |

**作用**：单个 `response.create` active turn 的 provider inactivity watchdog。成功发送到上游后，provider-originated activity 会刷新该计时器；超过该时间没有新的 provider activity / terminal 时，actor 关闭当前 downstream session 并 abort upstream，避免迟到的 provider terminal 污染下一次 turn。它不是单个 turn 的绝对最长运行时间。

**示例**：

```yaml
responses_ws:
  active_turn_timeout_ms: 120000
```

`<=0` 使用默认值，不表示禁用。

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
- 到期强制关闭：立即关闭当前连接；如果正在进行 turn，会中断该 turn 并 abort upstream。
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
| 默认值 | 未显式设置时按当前已加载渠道数扫描；渠道缓存为空时回退 `RetryTimes` |
| 必填 | 否 |

**作用**：当首选渠道不支持 ResponsesWS 时，限制一次打开流程最多扫描多少个 unsupported 候选。默认按当前已加载渠道数扫描，确保 `426 Upgrade Required` + wrapped fallback 只表示候选已明确耗尽且都不支持 ResponsesWS。若管理员显式配置的值低于当前渠道数，命中上限时返回 `503 responses_ws_unsupported_scan_limited`，不会伪装成 `426`；系统也不会把 native 自动切到 HTTP bridge，客户端可以改用普通 HTTP Responses，或管理员显式配置 `responses_ws_transport=http_bridge` 后重试。

**可配置值**：

| 值 | 行为 | 影响 |
|----|------|------|
| 未配置 / `0` | 按当前已加载渠道数扫描 | `426` 只表示候选明确耗尽 |
| `5` | 最多扫描 5 个 unsupported 候选 | 提高命中 WS-capable 渠道概率，但若渠道数更多且命中上限，会返回 `503 responses_ws_unsupported_scan_limited` |
| `1` | 最多扫描 1 个 unsupported 候选 | 最快；如果后面仍可能有可用候选，返回 `503 responses_ws_unsupported_scan_limited`，不返回 `426` |

**示例**：

```yaml
responses_ws:
  unsupported_scan_limit: 0
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

**安全说明**：上游 URL 在建立 WebSocket 连接前会经过严格 SSRF 校验；默认拒绝 loopback、内网、link-local、云 metadata IP，并硬拦截 `metadata.google.internal` 等 metadata hostname。显式 self-hosted 只放开本地/内网/明文自建地址，不放开 metadata host。

---

### 密钥 🏷️ Web 后台 → 渠道

上游服务的 API Key。对于 OpenAI，填写 `sk-...` 格式的 key；对于 Codex，填写 OAuth 凭据 JSON。

**注意**：key 本身**不会**存入 ResponsesWS 的 upstream snapshot。snapshot 中仅存储 channel 引用（可间接获取 Base URL 等），不存储原始凭据。

---

### `Other` JSON 🏷️ Web 后台 → 渠道

所有渠道的 `Other` 非空时都必须是 JSON object。旧的纯字符串格式不再作为合法配置保存或运行时读取；需要把原先的字符串语义转换成对应字段，例如：

```json
{
  "api_version": "2024-10-01-preview"
}
```

对没有必填 `Other` 字段的渠道，空值表示使用该渠道的默认配置；Azure classic 仍必须提供 `api_version`，VertexAI 仍必须提供 `region` / `project_id`。`responses_ws_transport` 是跨 provider 的公共字段，非法值会返回 `invalid_responses_ws_transport`。

常见渠道的 `Other` JSON 形态如下。这里的字段都是渠道级数据库配置，不是 `config.yaml` 项：

| 渠道 | `Other` JSON 示例 | 说明 |
| --- | --- | --- |
| OpenAI / 自定义 OpenAI-compatible | `{"responses_ws_transport":"http_bridge"}` | 空值可用默认配置；自定义兼容上游若支持原生 ResponsesWS，需要额外声明 `responses_ws_native=true` |
| Azure classic | `{"api_version":"2024-10-01-preview","responses_ws_transport":"native"}` | `api_version` 必填；不再接受旧的纯字符串 API version |
| Azure V1 | `{"responses_ws_transport":"native"}` | `base_url` 必须是 resource-level endpoint，不要包含 `/openai/deployments`；不读取 `api_version` |
| Ali | `{"dashscope_plugin":"plugin-name"}` | `dashscope_plugin` 可选；旧纯字符串插件参数会在迁移/保存时转换为该字段 |
| Xunfei | `{"api_version":"v3.1"}` | `api_version` 可选；旧纯字符串版本号会转换为该字段 |
| Gemini | `{"api_version":"v1"}` | `api_version` 可选；旧纯字符串版本号会转换为该字段 |
| Azure Speech | `{"region":"eastasia"}` | `region` 和 `base_url` 至少配置一个；旧纯字符串区域会转换为 `region` |
| VertexAI | `{"region":"us-central1","project_id":"my-project"}` | `region` / `project_id` 都必填；旧 `Region|ProjectID` 格式会转换为 JSON |
| Codex | `{"websocket_mode":"auto","responses_ws_transport":"native"}` | 只支持 native ResponsesWS；`http_bridge` 会在保存/校验时被拒绝；不接受 `extra` / `vendor_extra` |

有限字段合同的 trade-off：显式 JSON 增加了一点填写成本，但可以在保存时发现拼写错误和类型错误，避免旧字符串或错字段被静默忽略。对确实需要保留 provider 私有数据的非 Codex 渠道，请放到 `extra` 或 `vendor_extra` 命名空间；运行时不会解释这两个命名空间。

对第三方 OpenAI-compatible / 自定义渠道，HTTP `Responses` endpoint 非空不等于 native ResponsesWS 可用。若上游确实支持原生 Responses WebSocket，需要在 `Other` 中显式声明：

```json
{
  "responses_ws_native": true
}
```

该字段只允许 boolean。未声明时，请显式使用 `responses_ws_transport=http_bridge`，或保持不启用 ResponsesWS。

私有或本地自建上游需要额外声明：

```json
{
  "responses_ws_self_hosted": true
}
```

`responses_ws_self_hosted` 只影响 ResponsesWS，`self_hosted` 只影响 Realtime；二者不是别名，且都只允许 boolean。

---

### Azure classic `Other` JSON 🏷️ Web 后台 → 渠道

classic Azure OpenAI (`ChannelTypeAzure`) 的 `Other` 必须是 JSON object，不再兼容旧的纯 `api-version` 字符串：

```json
{
  "api_version": "2024-10-01-preview",
  "responses_ws_transport": "native",
  "self_hosted": false,
  "responses_ws_self_hosted": true
}
```

- `api_version` 必填，必须是非空字符串。
- `responses_ws_transport` 可选，只允许 `native` 或 `http_bridge`。
- `self_hosted` 可选，只影响 Azure `/v1/realtime` 私有/本地上游。
- `responses_ws_self_hosted` 可选，只影响 ResponsesWS native/HTTP bridge 私有/本地上游；它不是 `self_hosted` 的别名。
- 旧格式 `"2024-10-01-preview"` 会在保存或批量更新时被拒绝；升级迁移只会把可无损识别的历史 provider `Other` 字符串一次性转换为 JSON object，无法判断的旧值保持原样并由运行时校验 fail-closed；运行时不会 fallback 读取旧格式。
- classic Azure 与 Azure V1 的 ResponsesWS native endpoint 都使用 resource-level `/openai/v1/responses`；deployment name 放在 `response.create.model` 中。classic `api_version` 仍用于该 Azure 渠道的非 WS HTTP relay mode，不写入 ResponsesWS native URL。Azure V1 不读取顶层 `api_version`，保存时也会拒绝该字段；只配置 `responses_ws_transport`、`self_hosted`、`responses_ws_self_hosted` 等公共 runtime 字段。

---

### ResponsesWS transport 🏷️ Web 后台 → 渠道

OpenAI、classic Azure、Azure V1、自定义 OpenAI-compatible 等支持 ResponsesWS provider contract 的渠道可以在 `Other` JSON 中显式选择 transport：

```json
{
  "responses_ws_transport": "http_bridge"
}
```

| 值 | 行为 |
| --- | --- |
| 空 / `native` | 官方 OpenAI、Azure、Codex 默认使用真实上游 ResponsesWS / provider native WS；自定义兼容渠道需要 `responses_ws_native=true` |
| `http_bridge` | 主动使用 HTTP Responses stream bridge 模拟 ResponsesWS，需要上游 HTTP Responses endpoint 可用 |

`http_bridge` 是主动兼容模式，不是 native WS 建连失败后的自动 fallback。Codex 不支持该模式；Codex 渠道设置 `http_bridge` 会在保存/校验时失败。其它 provider 的非法值会返回 `invalid_responses_ws_transport`。

自定义兼容渠道未声明 `responses_ws_native=true` 却请求 native，或显式 bridge 缺少 HTTP Responses endpoint，会返回 `responses_ws_unsupported_for_channel`。

classic Azure 渠道配置该字段时，仍必须同时保留上一节的 `api_version`。

HTTP bridge 会保留 ResponsesWS `response.create` raw JSON 中未知字段，以便兼容未来官方字段；只拒绝当前 bridge 明确无法支持的 transport 字段，例如显式 `stream=false` 或 `background=true`。这条 raw preservation contract 不受普通 HTTP relay 的 `AllowExtraBody` 开关控制：`AllowExtraBody` 只约束普通 HTTP 请求体额外字段透传，ResponsesWS bridge 以 WebSocket raw frame 作为转发真相。Trade-off：bridge 对未来字段更兼容，但需要在 transport 边界保留少量显式拒绝列表。

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
| `websocket_mode` | `"auto"`（默认）/ `"force"` / `"off"`。`"off"` 关闭 Codex ResponsesWS native，因此 ResponsesWS 返回 `unsupported` |
| `responses_ws_transport` | `"native"`（默认）。Codex 不支持 `"http_bridge"`；配置后保存/校验失败，运行时也会拒绝 stale 配置 |
| `responses_ws_self_hosted` | `true` / `false`。只允许 ResponsesWS 使用私有或本地自建上游；不影响 Codex Realtime |
| `prompt_cache_key_strategy` | 控制 prompt cache key 的生成策略，影响 channel affinity 命中率 |

Codex ResponsesWS 是 native-only。HTTP bridge 的有限兼容规则只适用于支持该 transport 的非 Codex provider：`stream` 会被 bridge 强制为 `true`，显式 `stream=false` 会被拒绝；`background=true` 暂不支持并会在请求上游前失败；`stream_options` 会按原始 JSON 透传给 HTTP `/responses`。

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
[4] 首帧解析 + channel selection       ← 默认扫描当前加载候选；显式 unsupported_scan_limit 可提前返回 503
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
    │                                   ← active_turn_timeout_ms 单 turn 超时 (config.yaml)
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
| Origin 检查 | `realtime.allowed_origins`，为空时 fallback 到 `cors.allow_origins` | 同 Realtime：优先 `realtime.allowed_origins`，为空时 fallback 到 `cors.allow_origins` |
| 容量控制 | 无专用容量限制 | 三级 active lease + pending slot |
| 写超时 | `realtime.websocket_write_timeout_ms` | 同（复用同一 writer primitive） |
| 读上限 | `realtime.websocket_read_limit` | 同（复用同一 read limit primitive） |
| 客户端 Ping 间隔 | `realtime_websocket_client_ping_interval_ms` | `responses_websocket_client_ping_interval_ms` |
| 客户端 Pong 超时 | `realtime_websocket_client_pong_miss_timeout_ms` | `responses_websocket_client_pong_miss_timeout_ms` |
| 客户端入站活性超时 | `realtime_websocket_client_inbound_activity_timeout_ms` | `responses_websocket_client_inbound_activity_timeout_ms` |

## 相关文档

- [Realtime 配置](/use/realtime) — `/v1/realtime` WebSocket 配置
- [ResponsesWS 架构](/dev/responses-ws-architecture) — 内部架构说明与不变量
- [WebSocket Transport 架构](/dev/websocket-transport-architecture) — 底层复用方案
- [Codex 渠道](/use/Codex) — Codex 渠道的 `websocket_mode` 完整说明
