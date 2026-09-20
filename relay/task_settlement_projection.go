package relay

import (
	"context"
	"encoding/json"
	"math"

	"one-api/common/config"
	"one-api/common/logger"
	"one-api/internal/billing"
	"one-api/model"
	"one-api/types"
)

// ProjectAsyncTaskSettlement is authorized by the task transaction's first
// closure result, never by its current closed state. Replayed closes therefore
// cannot duplicate logs or counters. A crash/ambiguous commit may lose this
// best-effort projection, but never re-applies balances or retries projection.
func ProjectAsyncTaskSettlement(parent context.Context, task *model.Task, result model.BillingBalanceResult) {
	if task == nil || !result.FirstOwnerClosure || result.Outcome != model.BillingBalanceCommitted || task.ProviderState != model.TaskProviderStateClosed || task.ChargedQuota == nil {
		return
	}
	var modelName string
	var summary billing.UsageSummary
	metadata := map[string]any{"task_owner_id": task.OwnerID, "task_family": task.Platform, "settlement_decision": task.SettlementDecision, "provider_task_id": model.TaskProviderID(task)}
	switch task.Platform {
	case model.TaskPlatformOpenAIResponsesBackground:
		data := backgroundTaskData(task)
		modelName = data.Model
		summary = billing.NewUsageSummary(data.Evidence.restore())
	case model.TaskPlatformOpenAIBatch:
		var data openAIBatchData
		if json.Unmarshal(task.Data, &data) != nil {
			return
		}
		if data.LogUsage != nil {
			summary = *data.LogUsage
		} else {
			for _, evidence := range data.Evidence {
				accumulateTaskLogUsage(&summary, evidence.restore())
			}
		}
		for _, item := range data.Items {
			if item == nil {
				continue
			}
			if modelName == "" {
				modelName = item.Model
			} else if modelName != item.Model {
				modelName = "batch"
				break
			}
		}
		metadata["batch_endpoint"] = data.Endpoint
		metadata["batch_observed_items"] = data.ObservedItems
		metadata["batch_priced_items"] = data.PricedItems
		metadata["usage_summary_incomplete"] = data.LogUsage == nil && data.EvidenceSummaryTruncated
	default:
		return
	}
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := boundedResponsesLifecycleContext(context.WithoutCancel(parent))
	defer cancel()
	var token model.Token
	_ = model.DB.WithContext(ctx).Unscoped().Select("name").Where("id = ? AND user_id = ?", task.TokenID, task.UserId).Take(&token).Error
	requestTime := 0
	if task.OwnerClosedAt != nil && *task.OwnerClosedAt > task.SubmitTime && task.SubmitTime > 0 {
		elapsed := *task.OwnerClosedAt - task.SubmitTime
		if elapsed > int64(math.MaxInt/1000) {
			requestTime = math.MaxInt
		} else {
			requestTime = int(elapsed) * 1000
		}
	}
	cmd := billing.SettlementCommand{UserID: task.UserId, TokenID: task.TokenID, ChannelID: task.ChannelId, ModelName: modelName, FinalQuota: int(*task.ChargedQuota), UsageSummary: summary}
	opts := &billing.SettlementOptions{Projection: billing.SettlementProjection{TokenName: token.Name, Content: "async task: " + task.Platform, RequestTime: requestTime, Metadata: metadata}}
	if err := billing.ProjectSettlement(ctx, cmd, opts); err != nil {
		logger.LogError(ctx, "async task settlement projection failed: "+err.Error())
	}
}

// The consume-log schema has one aggregate token/cache row per task. Keep that
// fixed-size summary even if per-item billing evidence exceeds its own budget.
func accumulateTaskLogUsage(total *billing.UsageSummary, usage *types.Usage) {
	if total == nil || usage == nil {
		return
	}
	add := func(a, b int) int {
		if b <= 0 {
			return a
		}
		if a > math.MaxInt-b {
			return math.MaxInt
		}
		return a + b
	}
	total.PromptTokens = add(total.PromptTokens, usage.PromptTokens)
	total.CompletionTokens = add(total.CompletionTokens, usage.CompletionTokens)
	total.TotalTokens = add(total.TotalTokens, usage.TotalTokens)
	if total.ExtraTokens == nil {
		total.ExtraTokens = make(map[string]int)
	}
	extra := usage.GetExtraTokens()
	for _, key := range []string{config.UsageExtraCache, config.UsageExtraCacheReadInputTokens, config.UsageExtraCacheWrite, config.UsageExtraCacheCreationInputTokens, config.UsageExtraEphemeral5mInputTokens, config.UsageExtraEphemeral1hInputTokens} {
		total.ExtraTokens[key] = add(total.ExtraTokens[key], extra[key])
	}
}
