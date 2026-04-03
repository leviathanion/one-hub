---
title: "Channel Affinity 路由分组方案"
layout: doc
outline: deep
lastUpdated: true
---

# Channel Affinity 路由分组方案

## 文档状态

- 状态：提案
- 范围：`channel affinity` 的分组作用域语义
- 当前决策：把“声明分组”和“实际生效的路由分组”拆开，用一个 request-scoped `routing_group` 或 `effective_group` 承载实际选路结果

## 问题定义

当前链路里，`token_group` 同时承担了两层语义：

1. token 声明的主分组
2. 当前请求实际使用的路由分组

这在 fallback 路径上会出错。

当前行为大致是：

1. `middleware/auth.go` 写入 `token_group` / `token_backup_group`
2. `middleware/distributor.go` 在请求初始化阶段继续把“当前用于路由的分组”写回 `token_group`
3. `relay/common.go` 的 `GroupManager.TryWithGroups(...)` 在主组失败、备用组成功后，只更新：
   - `is_backupGroup`
   - `group_ratio`
4. `relay/channel_affinity.go` 的 `newChannelAffinityTemplate(...)` 在 `IncludeGroup` 时仍直接读取 `token_group`
5. `relay/relay_util/quota.go` 的 quota 统计也仍直接读取 `token_group`

这会导致：

1. 请求实际已在 `backup_group` 上成功选路
2. affinity key 仍可能继续落在旧的 `token_group` 上
3. quota / 日志 / meta 也可能继续使用旧组语义

根本问题不是 fallback 本身，而是“实际生效的路由作用域”没有被单独建模。

## 设计目标

1. affinity key 绑定实际生效的路由组，而不是初始声明组
2. fallback 成功后，调用方不必再靠 `is_backupGroup + group_ratio + token_group` 事后拼语义
3. quota、日志、meta 与实际选路保持一致
4. 不引入重型 `RouteContext` 或大规模接口重写

## 外部与相邻实现参考

### 外部资料

- HAProxy session persistence 文档把 persistence 建模成“后续请求继续路由到已选中的 backend”
  - https://www.haproxy.com/documentation/haproxy-configuration-tutorials/proxying-essentials/session-persistence/
- HAProxy sticky sessions guide 延续了同样的建模方式，sticky 绑定的对象是实际选中的 server
  - https://www.haproxy.com/blog/enable-sticky-sessions-in-haproxy

这里的工程推断是：

- 对 one-hub 来说，`group` 是 channel routing scope 的一部分
- 因此 sticky / affinity key 应绑定实际使用的组，而不是 fallback 前的声明组

### 相邻项目

- `../sub2api/backend/internal/repository/gateway_cache.go`
  - sticky key 直接包含 `groupID`，格式为 `sticky_session:{groupID}:{sessionHash}`
- `../sub2api/backend/internal/service/request_metadata.go`
  - 请求元数据里同时保存 `PrefetchedStickyAccountID` 和 `PrefetchedStickyGroupID`
- `../new-api/service/channel_affinity.go`
  - affinity meta 明确包含 `UsingGroup`
  - cache key suffix 在 `IncludeUsingGroup` 时直接拼入实际使用组

这些实现都说明一件事：

- sticky / affinity 归属的分组应该显式建模，而不是从别的字段反推

## 推荐方案

### 1. 拆分声明分组与生效分组

建议在请求上下文里至少保留下面两类信息：

- `token_group`
  - token 声明的原始主分组
- `routing_group` 或 `effective_group`
  - 当前请求实际用于选路、affinity scope、计费分组倍率的分组

如果需要更好的观测，再增加：

- `routing_group_source`
  - 例如 `token_group` / `user_group` / `backup_group` / `specific_channel`

这里不建议再把 `token_group` 回写成“当前生效值”。

### 2. 初始化阶段同时建立两类语义

建议把 `middleware/distributor.go` 调整为：

1. 保留 token 原始分组不变
2. 初始化 `routing_group`
3. 主组命中时，`routing_group = token_group`
4. 没有 token 组而回落到用户组时，`routing_group = user_group`

`distributor` 的职责应是“建立第一版路由作用域”，而不是改写 token 元数据。

### 3. fallback 成功时只更新 routing group

`relay/common.go` 的 `GroupManager.TryWithGroups(...)` 在备用组成功后，应该显式更新：

1. `routing_group = backup_group`
2. `routing_group_source = backup_group`
3. `is_backupGroup = true`
4. `group_ratio` 按 `routing_group` 重新计算

不要继续依赖：

- `token_group` 继续指向旧组
- 调用方再用 `is_backupGroup + token_group + group_ratio` 拼装真实语义

### 4. 统一通过 helper 读取当前生效组

建议新增统一 helper，例如：

- `currentRoutingGroup(c)`

后续至少这些路径都改成读 helper，而不是直接读 `token_group`：

- `relay/channel_affinity.go`
- `relay/common.go`
- `relay/relay_util/quota.go`
- 任何依赖当前分组做 scope、日志、meta、计费或运营观测的路径

这样可以把“哪个字段才代表当前生效组”的判断收敛到一处。

### 5. 在日志与 meta 中显式暴露 using_group

建议补充以下字段：

- `using_group`
- `routing_group_source`
- `token_group`
- `is_backup_group`

目的不是堆更多状态，而是把原本分散在多个 context key 里的隐式语义显式化。

## 为什么这是最佳实践

### 1. sticky key 本来就应该绑定实际路由作用域

HAProxy 的 session persistence / sticky sessions 文档，本质上都在记录“当前真实绑定到了哪个目标”。这类系统记录的是实际绑定结果，而不是最早的意图字段。

对 one-hub 的工程推断是：

- `group` 是 channel affinity key 的 scope 维度
- 这个 scope 应该来自当前真实选路结果

### 2. `sub2api` 和 `new-api` 都在做显式 scope 建模

`sub2api` 直接把 `groupID` 编进 sticky key，并单独保存 sticky group 元数据。

`new-api` 则把 `UsingGroup` 放进 affinity meta，并把它直接拼入 cache key suffix。

这两种做法都比 one-hub 当前“让多个字段共同隐式表示当前组”的方式更稳定。

## 为什么不过度设计

这条方案刻意不做下面这些事情：

1. 不引入完整 `RouteContext` 结构体贯穿全链路
2. 不重写所有 middleware / relay 接口签名
3. 不把 group 相关状态升级成新的路由 DSL

当前更合适的边界是：

1. 新增一个 request-scoped context key
2. 新增一个统一 helper
3. 把真正依赖“当前生效组”的路径切过去

如果只想快速止血，次优解是 fallback 成功后直接回写 `token_group`，再额外保存 `original_token_group`。这比现在正确，但长期仍会把“声明配置”和“实际选路”混在一起，因此不建议作为目标设计。

## 实现落点

建议优先改这些位置：

1. `middleware/auth.go`
   - 保留 token 原始分组信息
2. `middleware/distributor.go`
   - 初始化 `routing_group`
3. `relay/common.go`
   - fallback 成功时更新 `routing_group`
4. `relay/channel_affinity.go`
   - `IncludeGroup` 改为读 `currentRoutingGroup(c)`
5. `relay/relay_util/quota.go`
   - quota 的 `groupName` 改为当前生效组
6. `common/config/gin_key.go`
   - 如果项目已在集中管理 gin key，新增路由分组相关 key

## 测试要求

至少补下面几类测试：

1. 主组命中时，affinity key 的 group scope 使用主组
2. 备用组成功时，affinity key 的 group scope 使用备用组，而不是旧 `token_group`
3. quota / 日志 / meta 中的 `using_group` 与实际成功选路一致
4. 相同 affinity 输入在不同 `routing_group` 下不会互相命中

## 不建议的方案

### 1. 继续让 `token_group` 同时承担声明分组和当前生效组

问题：

1. fallback 后语义仍不稳定
2. affinity、quota、日志会继续各读各的
3. 后续再加 group 相关能力时会更难解释

### 2. 只修 affinity，不统一 quota / log / meta 读取方式

问题：

1. 表面 bug 会少一部分
2. 但系统内部仍会同时存在两套“当前组”定义

## 参考资料

- HAProxy Session persistence
  - https://www.haproxy.com/documentation/haproxy-configuration-tutorials/proxying-essentials/session-persistence/
- HAProxy sticky sessions guide
  - https://www.haproxy.com/blog/enable-sticky-sessions-in-haproxy
- `../sub2api/backend/internal/repository/gateway_cache.go`
- `../sub2api/backend/internal/service/request_metadata.go`
- `../new-api/service/channel_affinity.go`
