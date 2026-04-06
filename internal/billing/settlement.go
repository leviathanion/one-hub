package billing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"one-api/common/config"
	"one-api/common/logger"
	commonredis "one-api/common/redis"
	"one-api/model"
	"one-api/types"
	"strings"
	"time"
)

type SettlementRequestKind string

const (
	SettlementRequestKindUnary        SettlementRequestKind = "unary"
	SettlementRequestKindRealtimeTurn SettlementRequestKind = "realtime_turn"
	SettlementRequestKindAsyncTask    SettlementRequestKind = "async_task"
)

const settlementGateTTL = 24 * time.Hour

var settlementAcquireGateScriptSource = `
local key = KEYS[1]
local fingerprint = ARGV[1]
local ttl_ms = tonumber(ARGV[2])

local current = redis.call('GET', key)
if not current then
	redis.call('SET', key, fingerprint, 'PX', ttl_ms)
	return 1
end
if current == fingerprint then
	return 0
end
return -1
`

var settlementAcquireGateScript = commonredis.NewScript(settlementAcquireGateScriptSource)

type UsageSummary struct {
	PromptTokens            int                           `json:"prompt_tokens"`
	CompletionTokens        int                           `json:"completion_tokens"`
	TotalTokens             int                           `json:"total_tokens"`
	PromptTokensDetails     types.PromptTokensDetails     `json:"prompt_tokens_details,omitempty"`
	CompletionTokensDetails types.CompletionTokensDetails `json:"completion_tokens_details,omitempty"`
	ExtraTokens             map[string]int                `json:"extra_tokens,omitempty"`
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
		ExtraBilling:            cloneSettlementExtraBilling(s.ExtraBilling),
	}
	return usage
}

type SettlementCommand struct {
	Identity         string                `json:"identity,omitempty"`
	Fingerprint      string                `json:"fingerprint,omitempty"`
	RequestKind      SettlementRequestKind `json:"request_kind,omitempty"`
	UserID           int                   `json:"user_id"`
	TokenID          int                   `json:"token_id"`
	ChannelID        int                   `json:"channel_id"`
	ModelName        string                `json:"model_name,omitempty"`
	PreConsumedQuota int                   `json:"pre_consumed_quota"`
	FinalQuota       int                   `json:"final_quota"`
	UsageSummary     UsageSummary          `json:"usage_summary"`
	UnlimitedQuota   bool                  `json:"unlimited_quota"`
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

	cmd.Identity = strings.TrimSpace(cmd.Identity)
	cmd.Fingerprint = strings.TrimSpace(cmd.Fingerprint)
	cmd.ModelName = strings.TrimSpace(cmd.ModelName)
	if cmd.RequestKind == "" {
		cmd.RequestKind = SettlementRequestKindUnary
	}
	if cmd.Fingerprint == "" {
		cmd.Fingerprint = buildSettlementFingerprint(*cmd)
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

type SettlementCleanup struct {
	RealtimeQuotaDelta    int  `json:"realtime_quota_delta,omitempty"`
	RefreshUserQuotaCache bool `json:"refresh_user_quota_cache,omitempty"`
}

type SettlementOptions struct {
	Deduplicate bool                 `json:"deduplicate,omitempty"`
	Cleanup     SettlementCleanup    `json:"cleanup,omitempty"`
	Projection  SettlementProjection `json:"projection,omitempty"`
}

type SettlementEnvelope struct {
	Command SettlementCommand `json:"command"`
	Options SettlementOptions `json:"options,omitempty"`
}

type SettlementResult struct {
	Delta               int  `json:"delta"`
	TruthApplied        bool `json:"truth_applied"`
	Deduplicated        bool `json:"deduplicated"`
	FingerprintConflict bool `json:"fingerprint_conflict"`
}

type settlementFingerprintPayload struct {
	RequestKind      SettlementRequestKind `json:"request_kind"`
	UserID           int                   `json:"user_id"`
	TokenID          int                   `json:"token_id"`
	ChannelID        int                   `json:"channel_id"`
	ModelName        string                `json:"model_name,omitempty"`
	PreConsumedQuota int                   `json:"pre_consumed_quota"`
	FinalQuota       int                   `json:"final_quota"`
	UsageSummary     UsageSummary          `json:"usage_summary"`
	UnlimitedQuota   bool                  `json:"unlimited_quota"`
}

func buildSettlementFingerprint(cmd SettlementCommand) string {
	payload := settlementFingerprintPayload{
		RequestKind:      cmd.RequestKind,
		UserID:           cmd.UserID,
		TokenID:          cmd.TokenID,
		ChannelID:        cmd.ChannelID,
		ModelName:        cmd.ModelName,
		PreConsumedQuota: cmd.PreConsumedQuota,
		FinalQuota:       cmd.FinalQuota,
		UsageSummary:     cmd.UsageSummary,
		UnlimitedQuota:   cmd.UnlimitedQuota,
	}
	raw, _ := json.Marshal(payload)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:16])
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

	gateKey, acquired, conflict, err := acquireSettlementGate(ctx, cmd, *opts)
	if err != nil {
		logger.LogWarn(ctx, fmt.Sprintf("settlement gate unavailable for %s %s, continuing without dedupe: %v", cmd.RequestKind, cmd.Identity, err))
		gateKey = ""
		acquired = true
	}
	if !acquired {
		result.Deduplicated = true
		result.FingerprintConflict = conflict
		return result, nil
	}

	if err = model.ApplyTokenUserQuotaDeltaDirect(cmd.TokenID, cmd.UserID, cmd.UnlimitedQuota, result.Delta); err != nil {
		releaseSettlementGate(ctx, gateKey)
		return result, err
	}
	result.TruthApplied = true

	runSettlementCleanup(ctx, cmd, *opts)
	runSettlementProjection(ctx, cmd, *opts)
	return result, nil
}

func acquireSettlementGate(ctx context.Context, cmd SettlementCommand, opts SettlementOptions) (string, bool, bool, error) {
	if !opts.Deduplicate || cmd.Identity == "" || !config.RedisEnabled || commonredis.GetRedisClient() == nil {
		return "", true, false, nil
	}

	key := fmt.Sprintf("settlement:v1:%s:%s", cmd.RequestKind, cmd.Identity)
	status, err := settlementAcquireGateScript.Run(
		ctx,
		commonredis.GetRedisClient(),
		[]string{key},
		cmd.Fingerprint,
		settlementGateTTL.Milliseconds(),
	).Int64()
	if err != nil {
		return "", false, false, err
	}
	if status == 1 {
		return key, true, false, nil
	}
	if status == 0 {
		return key, false, false, nil
	}

	existingFingerprint, getErr := commonredis.GetRedisClient().Get(ctx, key).Result()
	if getErr == nil && existingFingerprint != "" && existingFingerprint != cmd.Fingerprint {
		logger.LogWarn(ctx, fmt.Sprintf("settlement identity %s already exists with different fingerprint: existing=%s current=%s", cmd.Identity, existingFingerprint, cmd.Fingerprint))
	}
	return key, false, true, nil
}

func releaseSettlementGate(ctx context.Context, gateKey string) {
	if gateKey == "" || !config.RedisEnabled || commonredis.GetRedisClient() == nil {
		return
	}
	if err := commonredis.GetRedisClient().Del(ctx, gateKey).Err(); err != nil {
		logger.LogError(ctx, "release settlement gate failed: "+err.Error())
	}
}

func runSettlementCleanup(ctx context.Context, cmd SettlementCommand, opts SettlementOptions) {
	if opts.Cleanup.RealtimeQuotaDelta > 0 {
		if _, err := model.CacheDecreaseUserRealtimeQuota(cmd.UserID, opts.Cleanup.RealtimeQuotaDelta); err != nil {
			logger.LogError(ctx, "settlement realtime quota cleanup failed: "+err.Error())
		}
	}
	if opts.Cleanup.RefreshUserQuotaCache {
		if err := model.CacheUpdateUserQuota(cmd.UserID); err != nil {
			logger.LogError(ctx, "settlement user quota cache refresh failed: "+err.Error())
		}
	}
}

func runSettlementProjection(ctx context.Context, cmd SettlementCommand, opts SettlementOptions) {
	usage := cmd.UsageSummary.ToUsage()
	extraTokens := usage.GetExtraTokens()
	cacheTokens := extraTokens[config.UsageExtraCache]
	cacheReadTokens := extraTokens[config.UsageExtraCachedRead]
	cacheWriteTokens := extraTokens[config.UsageExtraCachedWrite]

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
		model.UpdateChannelUsedQuota(cmd.ChannelID, cmd.FinalQuota)
	}
	model.UpdateUserUsedQuotaAndRequestCount(cmd.UserID, cmd.FinalQuota)
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
