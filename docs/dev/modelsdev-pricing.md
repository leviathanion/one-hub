# models.dev 价格导入

管理员在价格检查页点击“从 models.dev 获取”，选择模型及来源供应商，再点击获取服务端预览和确认合并。默认不会拉取或定时应用；不使用该入口即可维持原路径。

`GET /api/prices/modelsdev` 仅管理员可访问，固定获取 `https://models.dev/api.json`，20 秒超时、16 MiB 响应上限、禁止重定向。返回独立的 `url` 和 `candidates` 元数据：每项包含 provider、model、price（可用时）、reason（跳过时）和 conflict。同名模型保留各供应商候选，界面必须明确选择一个，不自动取低价。

选中的 Price 目录进入原有 `/api/prices/sync/preview` 和 `/api/prices/sync/apply`；新增 `mode: "merge"` 表示新增与更新选中模型，不删除其他模型。应用仍要求 source、base_version 和 digest；旧版本、摘要不匹配或重复提交会拒绝。锁定模型不修改；已有渠道类型、非空本地 RateRules 和来源未提供的 ExtraRatios 保留，变更在预览中展示。已有本地规则优先于来源的上下文分档。

cost.input/output 单位为美元/百万 token，分别除以 2 转成 one 的独立基础倍率。缓存、reasoning、audio 价格转成各自输入/输出的相对倍率，合法零与缺失分开。上下文 tiers 转成互斥的 GT/LTE 多档 RateRules，包括额外计量项的绝对价格调整。源的 cache_write 映射到 one 的缓存写入/creation/5m，1h 沿用 one 的基础价回退并同步分档倍率；来源没有独立 TTL 价格，不能视为已验证供应商的完整缓存价目。

缺少输入/输出、负价、不能相对零基价表达的额外价格、未知费用字段、未知或重复上下文档位，以及只有旧 context_over_200k 而无 tiers 的条目被跳过并展示原因。来源数据仅为配置参考，人工预览确认后才成为售价，不代表上游账单或真实用量。

无需 Schema 迁移。合并顺序：先合并共同价格发布执行器（PR #1），再合并 models.dev 导入。回退代码不会撤销已应用价格；要恢复价格须使用管理员预览确认流程重新应用备份目录。
