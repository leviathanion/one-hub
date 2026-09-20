package base

import (
	"one-api/common/requester"
	"one-api/types"
)

// ChatStreamConversionInterface explicitly selects the source protocol's
// terminal/error boundary when Chat is consumed by a cross-protocol adapter.
// Native downstream delivery must keep using CreateChatCompletionStream.
type ChatStreamConversionInterface interface {
	SupportsChatStreamConversion() bool
	CreateChatCompletionStreamForConversion(*types.ChatCompletionRequest) (requester.StreamReaderInterface[string], *types.OpenAIErrorWithStatusCode)
}
