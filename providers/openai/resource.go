package openai

import (
	"fmt"
	"strings"

	"one-api/common/config"
)

func (p *OpenAIProvider) BuildResourceRelayURL(escapedPath, rawQuery string) (string, error) {
	if p == nil || p.Channel == nil {
		return "", fmt.Errorf("resource provider is not initialized")
	}
	switch p.Channel.Type {
	case config.ChannelTypeOpenAI, config.ChannelTypeAzureV1, config.ChannelTypeCustom:
	default:
		return "", fmt.Errorf("channel does not implement native OpenAI resources")
	}
	for _, root := range []string{"/v1/chat/completions", "/v1/files", "/v1/uploads", "/v1/conversations"} {
		if escapedPath == root || strings.HasPrefix(escapedPath, root+"/") {
			return p.BuildRawRelayURL(escapedPath, rawQuery)
		}
	}
	return "", fmt.Errorf("resource operation is not implemented")
}
