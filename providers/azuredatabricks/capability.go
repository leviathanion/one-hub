package azuredatabricks

import (
	"one-api/model"
	"one-api/providers/base"
	"one-api/types"
)

func (AzureDatabricksProviderFactory) AssessChatRequest(_ *model.Channel, _ string, _ *types.ChatCompletionRequest) (base.ChatRequestSupport, error) {
	return base.ChatRequestSupport{}, nil
}
