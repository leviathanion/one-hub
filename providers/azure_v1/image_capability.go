package azure_v1

import "one-api/model"

func (AzureV1ProviderFactory) SupportsImageStreaming(channel *model.Channel) bool {
	return channel != nil
}
