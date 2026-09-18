package billing

import (
	"context"
	"encoding/json"
	"errors"
	"one-api/common/config"
	"one-api/common/logger"
	"one-api/model"
	"one-api/types"
	"strings"
)

type SettlementRequestKind string

const (
	SettlementRequestKindUnary        SettlementRequestKind = "unary"
	SettlementRequestKindRealtimeTurn SettlementRequestKind = "realtime_turn"
	SettlementRequestKindResponsesWS  SettlementRequestKind = "responses_ws"
)

type UsageSummary struct {
	PromptTokens            int                           `json:"prompt_tokens"`
	CompletionTokens        int                           `json:"completion_tokens"`
	TotalTokens             int                           `json:"total_tokens"`
	PromptTokensDetails     types.PromptTokensDetails     `json:"prompt_tokens_details,omitempty"`
	CompletionTokensDetails types.CompletionTokensDetails `json:"completion_tokens_details,omitempty"`
	ExtraTokens             map[string]int                `json:"extra_tokens,omitempty"`
	ExtraUsageUnits         map[string]float64            `json:"extra_usage_units,omitempty"`
	ExtraBilling            map[string]types.ExtraBilling `json:"extra_billing,omitempty"`
}

func NewUsageSummary(usage *types.Usage) UsageSummary {
	if usage == nil {
		return UsageSummary{}
	}

	return UsageSummary{
		PromptTokens:            usage.PromptTokens,
		CompletionTokens:        usage.CompletionTokens,
		TotalTokens:             usage.TotalTokens,
		PromptTokensDetails:     usage.PromptTokensDetails,
		CompletionTokensDetails: usage.CompletionTokensDetails,
		ExtraTokens:             cloneSettlementExtraTokens(usage.GetExtraTokens()),
		ExtraUsageUnits:         cloneSettlementExtraUsageUnits(usage.ExtraUsageUnits),
		ExtraBilling:            cloneSettlementExtraBilling(usage.ExtraBilling),
	}
}

func (s UsageSummary) ToUsage() *types.Usage {
	usage := &types.Usage{
		PromptTokens:            s.PromptTokens,
		CompletionTokens:        s.CompletionTokens,
		TotalTokens:             s.TotalTokens,
		PromptTokensDetails:     s.PromptTokensDetails,
		CompletionTokensDetails: s.CompletionTokensDetails,
		ExtraTokens:             cloneSettlementExtraTokens(s.ExtraTokens),
		ExtraUsageUnits:         cloneSettlementExtraUsageUnits(s.ExtraUsageUnits),
		ExtraBilling:            cloneSettlementExtraBilling(s.ExtraBilling),
	}
	return usage
}

func cloneSettlementExtraUsageUnits(units map[string]float64) map[string]float64 {
	if len(units) == 0 {
		return nil
	}
	cloned := make(map[string]float64, len(units))
	for key, value := range units {
		cloned[key] = value
	}
	return cloned
}

type SettlementCommand struct {
	RequestKind            SettlementRequestKind `json:"request_kind,omitempty"`
	UserID                 int                   `json:"user_id"`
	TokenID                int                   `json:"token_id"`
	ChannelID              int                   `json:"channel_id"`
	ModelName              string                `json:"model_name,omitempty"`
	PreConsumedQuota       int                   `json:"pre_consumed_quota"`
	FinalQuota             int                   `json:"final_quota"`
	UsageSummary           UsageSummary          `json:"usage_summary"`
	PreconsumeTokenApplied bool                  `json:"preconsume_token_applied"`
}

func (cmd *SettlementCommand) Normalize() error {
	if cmd == nil {
		return errors.New("settlement command is nil")
	}
	if cmd.UserID <= 0 {
		return errors.New("settlement command user_id is required")
	}
	if cmd.PreConsumedQuota < 0 {
		return errors.New("settlement command pre_consumed_quota cannot be negative")
	}
	if cmd.FinalQuota < 0 {
		return errors.New("settlement command final_quota cannot be negative")
	}

	cmd.ModelName = strings.TrimSpace(cmd.ModelName)
	if cmd.RequestKind == "" {
		cmd.RequestKind = SettlementRequestKindUnary
	}
	return nil
}

func (cmd SettlementCommand) Delta() int {
	return cmd.FinalQuota - cmd.PreConsumedQuota
}

type SettlementProjection struct {
	TokenName   string         `json:"token_name,omitempty"`
	Content     string         `json:"content,omitempty"`
	RequestTime int            `json:"request_time,omitempty"`
	IsStream    bool           `json:"is_stream,omitempty"`
	Metadata    map[string]any `json:"metadata,omitempty"`
	SourceIP    string         `json:"source_ip,omitempty"`
}

type SettlementOptions struct {
	Projection SettlementProjection `json:"projection,omitempty"`
}

type SettlementEnvelope struct {
	Command SettlementCommand `json:"command"`
	Options SettlementOptions `json:"options,omitempty"`
}

type SettlementResult struct {
	Delta          int                         `json:"delta"`
	TruthApplied   bool                        `json:"truth_applied"`
	BalanceOutcome model.BillingBalanceOutcome `json:"balance_outcome"`
}

func ApplySettlement(ctx context.Context, cmd SettlementCommand, opts *SettlementOptions) (SettlementResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if opts == nil {
		opts = &SettlementOptions{}
	}
	if err := cmd.Normalize(); err != nil {
		return SettlementResult{}, err
	}

	result := SettlementResult{
		Delta: cmd.Delta(),
	}

	applyResult, applyErr := model.ApplyBillingSettlementBalances(ctx, cmd.UserID, cmd.TokenID, int64(cmd.PreConsumedQuota), int64(cmd.FinalQuota), cmd.PreconsumeTokenApplied)
	result.BalanceOutcome = applyResult.Outcome
	if applyErr != nil {
		return result, applyErr
	}
	result.TruthApplied = true

	runSettlementProjection(ctx, cmd, *opts)
	return result, nil
}

func runSettlementProjection(ctx context.Context, cmd SettlementCommand, opts SettlementOptions) {
	usage := cmd.UsageSummary.ToUsage()
	extraTokens := usage.GetExtraTokens()
	cacheTokens := extraTokens[config.UsageExtraCache]
	cacheReadTokens := extraTokens[config.UsageExtraCacheReadInputTokens]
	// Pricing keeps provider evidence keys distinct, while the consume-log schema
	// exposes a single cache-write total. Project both evidence fields into it.
	cacheWriteTokens := extraTokens[config.UsageExtraCacheWrite] +
		extraTokens[config.UsageExtraCacheCreationInputTokens] +
		extraTokens[config.UsageExtraEphemeral5mInputTokens] +
		extraTokens[config.UsageExtraEphemeral1hInputTokens]

	model.RecordConsumeLog(
		ctx,
		cmd.UserID,
		cmd.ChannelID,
		usage.PromptTokens,
		usage.CompletionTokens,
		cacheTokens,
		cacheReadTokens,
		cacheWriteTokens,
		cmd.ModelName,
		opts.Projection.TokenName,
		cmd.FinalQuota,
		opts.Projection.Content,
		opts.Projection.RequestTime,
		opts.Projection.IsStream,
		cloneSettlementMetadata(opts.Projection.Metadata),
		opts.Projection.SourceIP,
	)

	if cmd.ChannelID > 0 && cmd.FinalQuota > 0 {
		if err := model.UpdateChannelUsedQuotaWithContext(ctx, cmd.ChannelID, cmd.FinalQuota); err != nil {
			logger.LogError(ctx, "settlement channel usage projection failed: "+err.Error())
		}
	}
	if err := model.UpdateUserRequestCountWithContext(ctx, cmd.UserID); err != nil {
		logger.LogError(ctx, "settlement user request-count projection failed: "+err.Error())
	}
}

func cloneSettlementExtraTokens(extraTokens map[string]int) map[string]int {
	if len(extraTokens) == 0 {
		return nil
	}
	cloned := make(map[string]int, len(extraTokens))
	for key, value := range extraTokens {
		cloned[key] = value
	}
	return cloned
}

func cloneSettlementExtraBilling(extraBilling map[string]types.ExtraBilling) map[string]types.ExtraBilling {
	if len(extraBilling) == 0 {
		return nil
	}
	cloned := make(map[string]types.ExtraBilling, len(extraBilling))
	for key, value := range extraBilling {
		cloned[key] = value
	}
	return cloned
}

func cloneSettlementMetadata(metadata map[string]any) map[string]any {
	if len(metadata) == 0 {
		return nil
	}
	raw, err := json.Marshal(metadata)
	if err != nil {
		cloned := make(map[string]any, len(metadata))
		for key, value := range metadata {
			cloned[key] = value
		}
		return cloned
	}
	var cloned map[string]any
	if err = json.Unmarshal(raw, &cloned); err != nil {
		cloned = make(map[string]any, len(metadata))
		for key, value := range metadata {
			cloned[key] = value
		}
	}
	return cloned
}
