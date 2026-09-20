package base

import "one-api/types"

// Native image response delivery is optional; unary and cross-protocol image
// providers do not need to implement it.
type ImageGenerationsResponseInterface interface {
	ProviderInterface
	SupportsImageResponse() bool
	CreateImageGenerationsResponse(*types.ImageRequest) (*types.ImageResponseWrapper, *types.OpenAIErrorWithStatusCode)
}

type ImageEditsResponseInterface interface {
	ProviderInterface
	SupportsImageResponse() bool
	CreateImageEditsResponse(*types.ImageEditRequest) (*types.ImageResponseWrapper, *types.OpenAIErrorWithStatusCode)
}
