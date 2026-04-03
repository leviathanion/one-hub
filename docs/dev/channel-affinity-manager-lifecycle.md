---
title: "Channel Affinity Manager 生命周期方案"
layout: doc
outline: deep
lastUpdated: true
---

# Channel Affinity Manager 生命周期方案

## 文档状态

- 状态：提案
- 范围：`runtime/channelaffinity` 默认 manager 的 janitor 生命周期
- 当前决策：把 janitor 视为显式单例 worker，首次初始化即带完整生命周期配置启动；`UpdateOptions(...)` 只更新运行时可调参数

## 问题定义

当前默认 manager 的初始化顺序存在语义缺口：

1. `runtime/channelaffinity/default_manager.go` 的 `DefaultManager()` 首次调用时使用空配置 `NewManagerWithOptions(ManagerOptions{})`
2. 后续再通过 `ConfigureDefault(...)` 调用 `UpdateOptions(...)` 热补配置
3. 但 `runtime/channelaffinity/manager.go` 的 `UpdateOptions(...)` 不会应用 `JanitorInterval`，也不会启动 janitor goroutine

这会带来几个直接问题：

1. 默认 manager 的本地过期项不会被周期清理
2. `Stats().LocalEntries` 可能持续偏大
3. `JanitorInterval` 看起来是可配置项，但实际上只有构造期才有意义
4. 生命周期语义被藏进“普通配置更新”API 里，不利于维护

## 设计目标

1. 默认 manager 第一次创建时就带着完整 options 启动
2. janitor 生命周期与普通配置更新解耦
3. 运行时只允许更新轻量参数
4. 不引入复杂的 goroutine 热切换状态机

## 外部与相邻实现参考

### 外部资料

- ABP background workers 文档把 worker 明确建模为 singleton，并用 `StartAsync` / `StopAsync` 管生命周期
  - https://abp.io/docs/10.0/framework/infrastructure/background-workers
- `patrickmn/go-cache` 文档明确说明：
  - 如果 `cleanupInterval < 1`，过期项不会自动删除
  - `ItemCount()` 可能包含尚未清理的过期项
  - https://pkg.go.dev/github.com/patrickmn/go-cache

这里的工程推断是：

- janitor goroutine 不是一个普通字段，它本质上是后台 worker
- 对这种组件，更自然的建模方式是“启动时确定生命周期”，而不是“任意时刻配置变更时顺便决定要不要启动 goroutine”

### 相邻项目

- `../CLIProxyAPI/sdk/cliproxy/usage/manager.go`
  - 默认 manager 采用 `StartDefault()` / `StopDefault()` 的显式生命周期
- `../new-api/service/channel_affinity.go`
  - 本地 cache 通过 `WithJanitor()` 在构造期启动清理器，而不是后补配置

这些实现都说明一件事：

- janitor 这类后台执行体，应该在构造或启动时明确建立生命周期

## 推荐方案

### 1. 默认 manager 第一次创建就传完整 options

`runtime/channelaffinity/default_manager.go` 不应继续：

1. 先 `NewManagerWithOptions(ManagerOptions{})`
2. 再 `UpdateOptions(...)`

更推荐的行为是：

1. 第一次初始化默认 manager 时就传入完整 `ManagerOptions`
2. 如果已经初始化，再只更新允许运行时变化的字段

这意味着 `ConfigureDefault(...)` 的语义应是：

- “确保默认 manager 已按完整配置创建；若已存在，只更新可热调配置”

### 2. 把配置分成生命周期配置与运行时配置

建议把 `ManagerOptions` 里的字段按语义拆成两类：

- 生命周期配置
  - `JanitorInterval`
- 运行时可调配置
  - `DefaultTTL`
  - `MaxEntries`
  - `RedisClient`
  - `RedisPrefix`

`UpdateOptions(...)` 只处理第二类。

### 3. 把 JanitorInterval 明确为 construction-time config

当前最稳妥的边界是：

- `JanitorInterval` 只在 `NewManagerWithOptions(...)` 时生效
- 一旦 manager 启动完成，不在 `UpdateOptions(...)` 内部重建 janitor goroutine

这能避免一个简单 cache manager 演化成复杂的并发状态机。

### 4. 如果未来必须支持热更 janitor，再做整实例 swap

如果将来真的需要热更新 janitor interval，建议单独设计清晰 API，例如：

- `SwapDefaultManager(newOptions)`

大致流程应是：

1. 创建新实例
2. 原子替换默认实例引用
3. `Close()` 旧实例

但当前问题不需要现在就引入这套机制。

## 为什么这是最佳实践

### 1. janitor 本质上就是后台 singleton worker

ABP 的 worker 模型虽然比 one-hub 重，但它给出的边界很清楚：

- worker 有启动与停止语义
- 生命周期应该显式管理

这里的工程推断是：

- one-hub 的 janitor goroutine 同样属于后台 worker
- 它不应该被隐藏在普通 `UpdateOptions(...)` 调用里

### 2. `go-cache` 的语义与当前问题完全对应

`patrickmn/go-cache` 文档明确指出：

1. cleanup interval 不工作时，过期项不会自动删除
2. `ItemCount()` 可能包含已过期但尚未清理的项

这正好解释了 one-hub 当前 `local_entries` 偏大的风险。

### 3. 相邻项目都倾向显式启动

`CLIProxyAPI` 用 `StartDefault()` / `StopDefault()` 管默认 manager 生命周期。

`new-api` 则在 cache 构造期通过 `WithJanitor()` 打开 janitor。

这两者都比“先创建空实例，再在配置更新里暗中决定 goroutine 生命周期”更清晰。

## 为什么不过度设计

这条方案刻意不做下面这些事情：

1. 不在 `UpdateOptions(...)` 里做“停旧 janitor、重建 stopCh、启动新 goroutine”的热切换
2. 不引入复杂的 manager registry
3. 不把本地 cache 组件升级成完整 worker framework

当前更合适的边界是：

1. 首次初始化即正确
2. 后续只调轻量参数
3. 真要变更生命周期，就整实例替换

## 实现落点

建议优先改这些位置：

1. `runtime/channelaffinity/default_manager.go`
   - 调整默认 manager 初始化策略
2. `runtime/channelaffinity/manager.go`
   - 明确 `UpdateOptions(...)` 的边界，不再承担 janitor 启动职责
3. `relay/channel_affinity.go`
   - 继续提供完整 `ManagerOptions`
4. `controller/option.go`
   - 读取 cache stats / clear cache 时复用同一默认 manager 语义

## 测试要求

至少补下面几类测试：

1. 默认 manager 首次按完整 options 创建时，`JanitorInterval` 生效
2. `UpdateOptions(...)` 不会隐式启动新的 janitor
3. 设置短 janitor interval 后，过期本地项能被周期清理
4. `Stats().LocalEntries` 不会长期保留大量已过期项

## 不建议的方案

### 1. 在 `UpdateOptions(...)` 里偷偷补 janitor 启动逻辑

问题：

1. API 边界变得不清晰
2. 并发状态管理变复杂
3. 以后要支持关闭、重启、替换时会越来越绕

### 2. 保持现状，只接受本地过期项滞留

问题：

1. 统计会失真
2. 本地缓存容量控制会变钝
3. 代码语义与配置表面能力不一致

## 参考资料

- ABP Background Workers
  - https://abp.io/docs/10.0/framework/infrastructure/background-workers
- `patrickmn/go-cache` package docs
  - https://pkg.go.dev/github.com/patrickmn/go-cache
- `../CLIProxyAPI/sdk/cliproxy/usage/manager.go`
- `../new-api/service/channel_affinity.go`
