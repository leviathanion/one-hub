# models.dev 价格导入

打开价格检查弹窗时仅显示获取入口和取消按钮；成功获取非空目录后才显示同步方式、相关提示和确认应用按钮。修改 URL、重新获取、空目录／获取失败及关闭重开时恢复到获取状态。管理员在原获取入口下方点击“从 models.dev 获取”，获取后直接显示现有服务端差异预览，使用同一组“只新增／只更新／覆盖”按钮切换模式，再确认应用。没有额外的模型勾选或供应商选择步骤；获取不会写入价格，也不会修改普通价格目录 URL 或定时同步配置。

`GET /api/prices/modelsdev` 仅管理员可访问，固定获取 `https://models.dev/api.json`，20 秒超时、16 MiB 响应上限、禁止重定向。返回 `url`、转换后的 `prices`、按模型计数的 `skipped`，以及用于追溯的 `candidates`：每项包含 provider、model、price（可用时）、reason（转换失败时）、conflict 和 selected。候选元数据不进入 Price 目录。

选源借鉴 [done-hub 的官方供应商优先方式](https://github.com/deanxv/done-hub/blob/main/model/pricing_modelsdev.go)。对应关系集中在后端 `modelsDevOfficialProviders`：OpenAI、Anthropic、Google、xAI、DeepSeek、Mistral、Cohere、Meta/Llama、Moonshot AI、Zhipu AI/Z.ai、MiniMax、Alibaba、StepFun、Perplexity。Vertex、Azure 和中转商可托管其他厂商模型，不作为官方发布源。同一精确模型 ID 只有一个官方来源时采用该来源；只有一个候选时采用该候选；多官方或仅多托管来源的歧义模型跳过，不按低价或遍历顺序择优。官方来源价格无效时不回退到托管商。模型 ID 原样保留，不去供应商前缀或推测别名。供应商表随来源目录演进维护。

`prices` 进入原有 `/api/prices/sync/preview` 和 `/api/prices/sync/apply`，复用当前选择的模式：add 只新增，update 只更新，overwrite 新增、更新并删除目录外未锁定的本地模型。跳过的模型同样不在目录中；页面提示跳过数量和覆盖语义，删除也会出现在原有差异卡片中。空目录不能预览或应用。应用仍要求 source、base_version 和 digest，锁定模型保持不变，旧版本、摘要不匹配或重复提交拒绝。既有 merge API 继续支持已发布的调用方，但此页面不再使用单独的 merge 流程。

上下文分档（long_context）完整跟随本次来源：有分档则替换，没有分档则清除已有分档，包括管理员手工维护的分档。仅分档变化或删除也进入服务端预览与发布；非分档规则沿用普通同步的省略／显式提供语义。渠道类型及额外倍率也遵循所选普通同步模式，最终变化以服务端预览为准。

cost.input/output 单位为美元/百万 token，分别除以 2 转成 one 的独立基础倍率。缓存、reasoning、audio 价格转成各自输入/输出的相对倍率，合法零与缺失分开。上下文 tiers 转成互斥的 GT/LTE 多档 RateRules，包括额外计量项的绝对价格调整。源的 cache_write 映射到 one 的缓存写入/creation/5m，1h 沿用 one 的基础价回退并同步分档倍率；来源没有独立 TTL 价格，不能视为已验证供应商的完整缓存价目。

缺少输入/输出、负价、不能相对零基价表达的额外价格、未知费用字段、未知或重复上下文档位，以及只有旧 context_over_200k 而无 tiers 的条目被跳过，原因保留在 API 候选元数据中。来源数据仅为配置参考，人工预览确认后才成为售价，不代表上游账单或真实用量。

无需 Schema 迁移。回退代码不会撤销已应用价格；要恢复价格须使用管理员预览确认流程重新应用备份目录。

相同的分档替换规则适用于普通价格目录的 update／overwrite 和自动价格服务更新；add 仍只新增模型。普通目录对非分档规则保留既有的省略／显式提供语义，手工编辑省略 rate_rules 仍保持原规则。覆盖目录仍按原模式删除未提供且未锁定的模型，不能当作只更新选中模型使用。
