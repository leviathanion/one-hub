package responsesws

type RecvDetailOrigin string

const (
	RecvDetailOriginProviderFrame           RecvDetailOrigin = "provider_frame"
	RecvDetailOriginProviderStream          RecvDetailOrigin = "provider_stream"
	RecvDetailOriginProviderMalformed       RecvDetailOrigin = "provider_malformed_frame"
	RecvDetailOriginProxyLocal              RecvDetailOrigin = "proxy_local"
	RecvDetailOriginAdapterPanic            RecvDetailOrigin = "adapter_panic"
	RecvDetailOriginSyntheticBridge         RecvDetailOrigin = "synthetic_bridge"
	RecvDetailOriginBridgeStreamOpened      RecvDetailOrigin = "bridge_stream_opened"
	RecvDetailOriginBridgeOpenProviderError RecvDetailOrigin = "bridge_open_provider_error"
	RecvDetailOriginBridgeStreamError       RecvDetailOrigin = "bridge_stream_error"
	RecvDetailOriginBridgeStreamEOF         RecvDetailOrigin = "bridge_stream_eof"
	RecvDetailOriginBridgeLocalAbort        RecvDetailOrigin = "bridge_local_abort"
	RecvDetailOriginNativeProviderClose     RecvDetailOrigin = "native_provider_close"
	RecvDetailOriginNativeProviderEOF       RecvDetailOrigin = "native_provider_eof"
	RecvDetailOriginNativeLocalAbort        RecvDetailOrigin = "native_local_abort"
	RecvDetailOriginNativeLocalDetach       RecvDetailOrigin = "native_local_detach"
	RecvDetailOriginNativeBackpressure      RecvDetailOrigin = "native_backpressure"
	RecvDetailOriginNativeReadError         RecvDetailOrigin = "native_read_error"
)

type RecvDetailPhase string

const (
	RecvDetailPhasePrepareClientFrame  RecvDetailPhase = "prepare_client_frame"
	RecvDetailPhaseHandleProviderFrame RecvDetailPhase = "handle_provider_frame"
	RecvDetailPhaseMapProviderClose    RecvDetailPhase = "map_provider_close"
)

func RecvDetailOriginKnown(origin RecvDetailOrigin) bool {
	switch origin {
	case RecvDetailOriginProviderFrame,
		RecvDetailOriginProviderStream,
		RecvDetailOriginProviderMalformed,
		RecvDetailOriginProxyLocal,
		RecvDetailOriginAdapterPanic,
		RecvDetailOriginSyntheticBridge,
		RecvDetailOriginBridgeStreamOpened,
		RecvDetailOriginBridgeOpenProviderError,
		RecvDetailOriginBridgeStreamError,
		RecvDetailOriginBridgeStreamEOF,
		RecvDetailOriginBridgeLocalAbort,
		RecvDetailOriginNativeProviderClose,
		RecvDetailOriginNativeProviderEOF,
		RecvDetailOriginNativeLocalAbort,
		RecvDetailOriginNativeLocalDetach,
		RecvDetailOriginNativeBackpressure,
		RecvDetailOriginNativeReadError:
		return true
	default:
		return false
	}
}

func RecvDetailOriginCanCarryProviderTerminal(origin RecvDetailOrigin) bool {
	switch origin {
	case RecvDetailOriginProviderFrame, RecvDetailOriginProviderStream:
		return true
	default:
		return false
	}
}

func ExpectedPayloadOriginForRecvDetailOrigin(origin RecvDetailOrigin) (PayloadOrigin, bool) {
	switch origin {
	case RecvDetailOriginProviderFrame,
		RecvDetailOriginProviderStream,
		RecvDetailOriginBridgeStreamOpened,
		RecvDetailOriginBridgeOpenProviderError,
		RecvDetailOriginNativeProviderClose,
		RecvDetailOriginNativeProviderEOF:
		return PayloadOriginProvider, true
	case RecvDetailOriginProviderMalformed,
		RecvDetailOriginSyntheticBridge,
		RecvDetailOriginBridgeStreamError,
		RecvDetailOriginBridgeStreamEOF,
		RecvDetailOriginBridgeLocalAbort,
		RecvDetailOriginProxyLocal,
		RecvDetailOriginAdapterPanic,
		RecvDetailOriginNativeLocalAbort,
		RecvDetailOriginNativeLocalDetach,
		RecvDetailOriginNativeBackpressure,
		RecvDetailOriginNativeReadError:
		return PayloadOriginProxyLocal, true
	default:
		return PayloadOriginProxyLocal, false
	}
}
