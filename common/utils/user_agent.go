package utils

import "strings"

const maxNormalizedUserAgentLength = 512

func NormalizeUserAgent(raw string) string {
	normalized := strings.TrimSpace(raw)
	if normalized == "" {
		return ""
	}

	runes := []rune(normalized)
	if len(runes) > maxNormalizedUserAgentLength {
		return string(runes[:maxNormalizedUserAgentLength])
	}

	return normalized
}

func AppendUserAgentMetadata(metadata map[string]any, raw string) map[string]any {
	normalized := NormalizeUserAgent(raw)
	if normalized == "" {
		return metadata
	}

	if metadata == nil {
		metadata = make(map[string]any, 1)
	}
	metadata["user_agent"] = normalized
	return metadata
}
