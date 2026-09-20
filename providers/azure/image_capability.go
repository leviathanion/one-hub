package azure

import "one-api/model"

func (AzureProviderFactory) SupportsImageStreaming(channel *model.Channel) bool {
	return channel != nil
}
