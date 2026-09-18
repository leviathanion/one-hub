---
status: accepted
---

# 在每次使用时读取可配置策略

Price、group ratio、Options 和其他可配置策略在每个决策点从当前完整发布读取；请求、billing attempt、session 或异步 action 不跨阶段 pin 配置 revision。因此 reserve 和 settlement 可以使用不同价格，其他阶段可以在操作中途观察到更新，而已经执行的金额、已选择的上游 ownership、provider evidence 和其他 action 事实保持稳定。

这有意接受配置运行时变化时的操作内不一致；运营商可以在想避免观察到该行为时使用 stop-the-world 更新。该决策避免历史目录、请求快照、version fence 和重复生命周期管道；控制面 CAS 防止丢失写入，但不承诺请求级一致性。未来对法律可审计或对外承诺的时点策略的要求，将是为该特定边界引入持久策略 identity 的触发器。
