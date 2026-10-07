# models.dev 价格导入

价格同步仅保留 models.dev，弹窗分为获取目录和核对变化两屏。移除自定义 URL、URL 本地缓存、`GET /api/prices/updateService`、旧价格服务抓取和定时任务，以及 `auto_price_updates*`、`update_price_service` 配置；不保留兼容分支。启动只为空价格库填入内置价格，不因旧配置覆盖已有价格。

核对界面默认选择“只新增”，通过单选卡片切换“只更新现有／覆盖所有”。服务端预览按新增、价格变动、删除、锁定分组显示；基本倍率、扩展倍率及分档规则以“当前 → 同步后”展示，不显示 JSON 或内部版本号。变更明细按分类筛选、每页渲染 25 项，汇总始终按完整预览计数；筛选和翻页不改变确认应用的范围。新预览重置展示页，差异行只为当前页计算；分组按预览缓存，列表组件跳过无关状态更新。预览成功后的展示更新用 React 18 transition 让位给交互，获取、应用与取消仍由原会话管理。内容区统一滚动，底部固定唯一的“确认应用 N 项变更”按钮，锁定保留项不计入数量。覆盖模式明确提示删除来源外的未锁定模型。没有额外的模型勾选或供应商选择步骤；获取不会写入价格。可展开来源明细查看采用的供应商和未采用原因，报价来源不持久化。

来源、模式和预览由同一个同步会话管理。切换模式、重新获取或关闭弹窗时取消未完成的读取，旧响应不能覆盖当前预览。获取目录与计算预览分别显示进度；临时预览失败可“重新计算变化”，复用已获取目录，不重新获取、不自动应用。无效目录返回获取界面，不提供无效重试。重复模型由服务端返回明确模型名，沿用既有拒绝语义，不自动取第一条或最后一条。应用期间禁止切换或重复提交；应用失败清除旧预览，重新计算后再次人工确认，不自动重放写入。预览过期明确提示本次未修改价格。

`GET /api/prices/modelsdev` 仅管理员可访问，固定获取 `https://models.dev/api.json`，20 秒超时、16 MiB 响应上限、禁止重定向。返回 `url`、转换后的 `prices`、按模型计数的 `skipped`，以及用于追溯的 `candidates`：每项包含 provider、model、price（可用时）、reason（未采用原因）、conflict 和 selected。候选元数据不进入 Price 目录。

选源借鉴 [done-hub 的官方供应商优先方式](https://github.com/deanxv/done-hub/blob/main/model/pricing_modelsdev.go)。对应关系集中在后端 `modelsDevOfficialProviders`：OpenAI、Anthropic、Google、xAI、DeepSeek、Mistral、Cohere、Meta/Llama、Moonshot AI、Zhipu AI/Z.ai、MiniMax、Alibaba、StepFun、Perplexity。Vertex、Azure 和中转商可托管其他厂商模型，不作为官方发布源。同一精确模型 ID 只有一个官方来源时采用该来源；只有一个候选时采用该候选；多官方或仅多托管来源的歧义模型跳过，不按低价或遍历顺序择优。官方来源价格无效时不回退到托管商。模型 ID 原样保留，不去供应商前缀或推测别名。供应商表随来源目录演进维护。若来源提供 [`canonical_model_id`](https://github.com/anomalyco/models.dev#adding-model-metadata)，其 lab 必须与供应商对应厂商一致才能获得官方优先；Alibaba、Mistral 等托管其他厂商模型时不因目录名称获得优先权。该字段只验证来源，不用于改名或合并不同模型 ID；缺失时使用上述明确表，新增厂商或来源变化需复核。

`prices` 进入原有 `/api/prices/sync/preview` 和 `/api/prices/sync/apply`，复用当前选择的模式：add 只新增，update 只更新，overwrite 新增、更新并删除目录外未锁定的本地模型。跳过的模型同样不在目录中；页面提示跳过数量和覆盖语义，删除也会出现在原有差异卡片中。空目录不能预览或应用。应用仍要求 source、base_version 和 digest，锁定模型保持不变，旧版本、摘要不匹配或重复提交拒绝。既有 merge API 继续支持已发布的调用方，但此页面不再使用单独的 merge 流程。

上下文分档（long_context）完整跟随本次来源：有分档则替换，没有分档则清除已有分档，包括管理员手工维护的分档。仅分档变化或删除也进入服务端预览与发布；非分档规则及额外倍率沿用普通同步的省略／显式提供语义，最终变化以服务端预览为准。报价 provider 只用于选价，不修改模型展示归属；归属由模型说明中的 `owned_by_id` 管理。

cost.input/output 单位为美元/百万 token，分别除以 2 转成 one 的独立基础倍率。缓存、reasoning、audio 价格转成各自输入/输出的相对倍率，合法零与缺失分开。上下文 tiers 转成互斥的 GT/LTE 多档 RateRules，包括额外计量项的绝对价格调整。源的 cache_write 映射到 one 的缓存写入/creation/5m，1h 沿用 one 的基础价回退并同步分档倍率；来源没有独立 TTL 价格，不能视为已验证供应商的完整缓存价目。

缺少输入/输出、负价、不能相对零基价表达的额外价格、未知费用字段、未知或重复上下文档位，以及只有旧 context_over_200k 而无 tiers 的条目被跳过，原因保留在 API 候选元数据中。来源数据仅为配置参考，人工预览确认后才成为售价，不代表上游账单或真实用量。

模型归属迁出价格表由主实例启动时的 Go 迁移代码完成，更新前须停机备份，见 [模型信息与价格关联调整方案](model-catalog-pricing-architecture.md)。共享价格预览／应用 API 继续严格拒绝 `channel_type`，只读 `model_info` 不参与售价写入。回退代码不会撤销已应用价格，恢复必须同时核对数据库结构和升级后业务写入。

共享预览／应用 API、手工编辑和价格导出继续保留，add 仍只新增模型；手工编辑省略 rate_rules 仍保持原规则。此次移除旧同步无需数据库迁移，部署不应用新价格。

借鉴 [Oaklight 的价格维护方式](https://github.com/Oaklight/onehub_prices)：保留明确报价来源、单位换算与冲突核对。复用 models.dev 的数据维护及本地手工定价，不另建采集仓库、供应商 ID 映射服务或定时写价任务。供应商＋模型可以标识候选报价，发布给 one-hub 前必须选成每个精确模型唯一的售价；不按 `(model, channel_type)` 并存后再靠写入顺序覆盖，也不自动展开别名。

渲染优化采用限制重复渲染与低优先级更新原则，并参考[主线程开销分析](https://kciter.so/posts/the-expensive-main-thread/)。先限制 DOM 数量，再缩短连续占用时间；不把完整计费预览裁成当前页，也不另建 Worker 或异步写入流程。
