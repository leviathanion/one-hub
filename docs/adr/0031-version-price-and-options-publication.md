---
status: accepted
---

# 为 Price 和 Options 发布引入版本

Price 和 Options 各自使用单调数据库 revision 作为写入顺序 truth，并且只发布已校验的完整值。一次读取观察一个完整发布；不同阶段可以按 ADR-0034 的决定观察到不同 revision。直接编辑表在受支持表面之外，已发布 revision 与数据库 head 不同的实例不 ready。

Price 和 Options 仍是独立 owner，只共享小的 CAS 和 complete-publication primitive。历史目录、请求级快照、分布式锁、event bus 和通用配置平台被拒绝，因为有界轮询可以检测陈旧实例，而 ADR-0034 允许后续阶段读取更新的发布。

Price readiness 可以额外检查有界 `pricing.required_models` 启动列表。该列表是部署配置，而不是持久 manifest 或运行时管理 API。
