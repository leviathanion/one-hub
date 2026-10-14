package gemini

import (
	"one-api/common/requester"
	"one-api/providers/base"
	"one-api/types"
)

type GeminiChatInterface interface {
	base.ProviderInterface
	CreateGeminiChat(request *NativeRequest) (*GeminiChatResponse, *types.OpenAIErrorWithStatusCode)
	CreateGeminiChatStream(request *NativeRequest) (requester.StreamReaderInterface[string], *types.OpenAIErrorWithStatusCode)
}
