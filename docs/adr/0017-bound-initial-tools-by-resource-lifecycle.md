---
status: accepted
---

# 按资源生命周期界定初始工具支持

初始 GPT-5.6 表面在选中 channel 可以表示它们时完整支持 Resource-Independent Tools，包括 function/custom tools、Programmatic Tool Calling、multi-agent、Web Search、Image Generation、local shell 以及适用的 client- 或 externally executed tools。其 wire 事件和独立计费证据仍是受支持契约的一部分。需要 Account-Scoped Tool Resource 的工具，包括 File Search、Shell 或 Code Interpreter 使用的 hosted container、上传的 Skill 引用，在代理提供该资源完整生命周期、用户 ownership 和 exact-channel routing 之前于 provider work 前失败。该语义边界保留了有用的工具广度，同时不与“Stored Response 是唯一初始 owned resource”的决定冲突。

Hosted-tool 基础价格只来自 System Tool Price Catalog，最终用户收费额外应用既有 group ratio。设计不新增动态全局设置、channel override 或其他价格继承层。Image Generation 在可用时使用精确返回的 model/output evidence；当证据缺失或为 `auto` 时，结算在 actual image model 和 output 约束下选择保守的最高合法单次调用价格，并记录 billing diagnostic。

Web Search 对完成的 `search` action 收费，但不收 `open_page` 或 `find_in_page`。类型缺失或未知的已完成 action 按同一请求规格的一次 search 保守收费并记录诊断，遵循“宁可有界小额多收，不可系统性少收”的既定偏好。
