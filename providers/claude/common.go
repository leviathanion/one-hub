package claude

import (
	"one-api/common"
	"one-api/common/config"
	"one-api/types"
	"strconv"
	"strings"
)

func StringErrorWrapper(err string, code string, statusCode int, localError bool) *ClaudeErrorWithStatusCode {
	claudeError := ClaudeError{
		Type: "one_hub_error",
		ErrorInfo: ClaudeErrorInfo{
			Type:    code,
			Message: err,
		},
	}

	return &ClaudeErrorWithStatusCode{
		LocalError:  localError,
		StatusCode:  statusCode,
		ClaudeError: claudeError,
	}
}

func OpenaiErrToClaudeErr(err *types.OpenAIErrorWithStatusCode) *ClaudeErrorWithStatusCode {
	if err == nil {
		return nil
	}

	var typeStr string

	switch v := err.Code.(type) {
	case string:
		typeStr = v
	case int:
		typeStr = strconv.Itoa(v)
	default:
		typeStr = "unknown"
	}

	return &ClaudeErrorWithStatusCode{
		LocalError: err.LocalError,
		StatusCode: err.StatusCode,
		ClaudeError: ClaudeError{
			Type: typeStr,
			ErrorInfo: ClaudeErrorInfo{
				Type:    err.Type,
				Message: err.Message,
			},
		},
	}
}

func ErrorToClaudeErr(err error) *ClaudeError {
	if err == nil {
		return nil
	}
	return &ClaudeError{
		Type: "one_hub_error",
		ErrorInfo: ClaudeErrorInfo{
			Type:    "internal_error",
			Message: err.Error(),
		},
	}
}

// Claude 的 message_delta usage 是累计快照；只覆盖本次明确出现的字段。
func ClaudeUsageMerge(usage *Usage, snapshot *Usage) {
	if usage == nil || snapshot == nil {
		return
	}
	if snapshot.inputTokensPresent {
		usage.InputTokens = snapshot.InputTokens
		usage.inputTokensPresent = true
	}
	if snapshot.outputTokensPresent {
		usage.OutputTokens = snapshot.OutputTokens
		usage.outputTokensPresent = true
	}
	if snapshot.cacheCreationTokensPresent {
		usage.CacheCreationInputTokens = snapshot.CacheCreationInputTokens
		usage.cacheCreationTokensPresent = true
	}
	if snapshot.cacheReadTokensPresent {
		usage.CacheReadInputTokens = snapshot.CacheReadInputTokens
		usage.cacheReadTokensPresent = true
	}
	speedEvidence := &types.Usage{Speed: usage.Speed, SpeedConflict: usage.speedConflict}
	speedEvidence.MergeProviderSpeed(snapshot.Speed, snapshot.speedConflict)
	usage.Speed, usage.speedConflict = speedEvidence.Speed, speedEvidence.SpeedConflict
	usage.present = usage.present || snapshot.present
	if snapshot.CacheCreation != nil {
		if usage.CacheCreation == nil {
			usage.CacheCreation = &CacheCreationUsage{}
		}
		if snapshot.CacheCreation.ephemeral5mPresent {
			usage.CacheCreation.Ephemeral5mInputTokens = snapshot.CacheCreation.Ephemeral5mInputTokens
			usage.CacheCreation.ephemeral5mPresent = true
		}
		if snapshot.CacheCreation.ephemeral1hPresent {
			usage.CacheCreation.Ephemeral1hInputTokens = snapshot.CacheCreation.Ephemeral1hInputTokens
			usage.CacheCreation.ephemeral1hPresent = true
		}
	}
	if snapshot.ServerToolUse != nil && snapshot.ServerToolUse.webSearchRequestsPresent {
		copied := *snapshot.ServerToolUse
		usage.ServerToolUse = &copied
	}
	if usage.serviceTierConflictValue == "" && snapshot.serviceTierConflictValue != "" {
		usage.serviceTierConflictValue = snapshot.serviceTierConflictValue
	}
	// An omitted or empty tier is not an assertion that an earlier snapshot was
	// default. Keep the first non-empty provider observation and retain the
	// first disagreement so conversion can route it through the shared
	// attribution conflict path even for callers that only use this merge API.
	if snapshotTier := strings.TrimSpace(snapshot.ServiceTier); snapshotTier != "" {
		if strings.TrimSpace(usage.ServiceTier) == "" {
			usage.ServiceTier = snapshot.ServiceTier
		} else if strings.TrimSpace(usage.ServiceTier) != snapshotTier && usage.serviceTierConflictValue == "" {
			usage.serviceTierConflictValue = snapshotTier
		}
	}
}

func ClaudeUsageToOpenaiUsage(cUsage *Usage, usage *types.Usage) bool {
	if usage == nil || cUsage == nil {
		return false
	}
	// service_tier is provider evidence. It must come from the response usage,
	// never from the client request, and empty observations must not erase a
	// tier already accepted from an earlier response.
	usage.MergeProviderAttribution("", cUsage.ServiceTier)
	usage.MergeProviderSpeed(cUsage.Speed, cUsage.speedConflict)
	if cUsage.serviceTierConflictValue != "" {
		usage.MergeProviderAttribution("", cUsage.serviceTierConflictValue)
	}
	if cUsage.ServerToolUse != nil && cUsage.ServerToolUse.webSearchRequestsPresent {
		usage.SetProviderExtraBilling(types.APIToolTypeWebSearch, "", cUsage.ServerToolUse.WebSearchRequests)
	}
	if !cUsage.present || !cUsage.inputTokensPresent || !cUsage.outputTokensPresent {
		return false
	}

	// 每次从合并后的快照重建 token 投影，避免旧缓存分区和缺证据标记残留。
	tokens := &types.Usage{}
	tokens.PromptTokensDetails.CacheReadInputTokens = cUsage.CacheReadInputTokens
	partition := cUsage.CacheCreation
	if cUsage.CacheCreationInputTokens != 0 || (partition != nil && (partition.Ephemeral5mInputTokens != 0 || partition.Ephemeral1hInputTokens != 0)) {
		if !cUsage.cacheCreationTokensPresent || partition == nil || !partition.ephemeral5mPresent || !partition.ephemeral1hPresent || partition.Ephemeral5mInputTokens+partition.Ephemeral1hInputTokens != cUsage.CacheCreationInputTokens {
			tokens.PromptTokensDetails.CacheCreationInputTokens = cUsage.CacheCreationInputTokens
			tokens.RequireTokenExtraEvidence("claude_cache_creation_partition")
		} else {
			tokens.SetExtraTokens(config.UsageExtraEphemeral5mInputTokens, partition.Ephemeral5mInputTokens)
			tokens.SetExtraTokens(config.UsageExtraEphemeral1hInputTokens, partition.Ephemeral1hInputTokens)
			tokens.RequireTokenExtraEvidence(config.UsageExtraEphemeral5mInputTokens, config.UsageExtraEphemeral1hInputTokens)
		}
	}
	tokens.PromptTokens = cUsage.InputTokens + cUsage.CacheCreationInputTokens + cUsage.CacheReadInputTokens
	tokens.CompletionTokens = cUsage.OutputTokens
	tokens.TotalTokens = tokens.PromptTokens + tokens.CompletionTokens
	tokens.MarkProviderReported()
	usage.PromptTokens = tokens.PromptTokens
	usage.CompletionTokens = tokens.CompletionTokens
	usage.TotalTokens = tokens.TotalTokens
	usage.PromptTokensDetails = tokens.PromptTokensDetails
	usage.ExtraTokens = tokens.ExtraTokens
	usage.RequiredTokenExtraKeys = tokens.RequiredTokenExtraKeys
	usage.ProviderTokenFields = tokens.ProviderTokenFields
	usage.ProviderReported = tokens.ProviderReported
	usage.ProviderTokenConflict = cUsage.InputTokens < 0
	return true
}

func ClaudeOutputUsage(response *ClaudeResponse) int {
	var textMsg strings.Builder

	for _, c := range response.Content {
		if c.Type == "text" {
			textMsg.WriteString(c.Text + "\n")
		}
	}

	return common.CountTokenText(textMsg.String(), response.Model)
}
