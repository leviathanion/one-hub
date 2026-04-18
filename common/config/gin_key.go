package config

const (
	GinCanonicalRequestStateKey = "canonical_request_state"
	GinRequestBodyKey           = "cached_request_body"
	GinOriginalRequestBodyKey   = "original_request_body"
	GinRequestBodyMapKey        = "cached_request_body_map"
	GinWireRequestBodyKey       = "wire_request_body"
	GinRequestBodyDecodeMetaKey = "request_body_decode_meta"
	GinProviderCacheKey         = "cached_provider_selection"
	GinRequestBodyReparseKey    = "request_body_reparse_needed"
	GinChannelAffinityMetaKey   = "channel_affinity_meta"
	GinRoutingGroupKey          = "routing_group"
	GinRoutingGroupSourceKey    = "routing_group_source"
)
