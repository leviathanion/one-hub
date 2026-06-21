package relay

import "one-api/common/responsesws"

type responsesWSProviderPayloadPolicy struct {
	PayloadOrigin      responsesws.PayloadOrigin
	PayloadOriginKnown bool
	CanCarryUsage      bool
	CanCarryTerminal   bool
}

type responsesWSProviderLifecyclePolicy struct {
	DeliverRecvLifecycleEvent        bool
	DeliverRecvFailureLifecycleEvent bool
	IdleRecvFailureClosesSession     bool
	BridgeStreamEOF                  bool
	ProviderMalformedClientPayload   bool
}

func responsesWSProviderPayloadPolicyForEvent(event responsesws.UpstreamEvent) responsesWSProviderPayloadPolicy {
	projected := responsesws.ProjectProviderObservationTransportPolicy(responsesws.NewProviderObservation(event))
	return responsesWSProviderPayloadPolicy{
		PayloadOrigin:      projected.PayloadOrigin,
		PayloadOriginKnown: projected.PayloadOriginKnown,
		CanCarryUsage:      projected.CanCarryUsage,
		CanCarryTerminal:   projected.CanCarryTerminal,
	}
}

func responsesWSProviderLifecyclePolicyForEvent(event responsesws.UpstreamEvent) responsesWSProviderLifecyclePolicy {
	obs := responsesws.NewProviderObservation(event)
	var out responsesWSProviderLifecyclePolicy
	switch obs.DetailOrigin {
	case responsesws.RecvDetailOriginBridgeStreamOpened:
		out.DeliverRecvLifecycleEvent = true
	case responsesws.RecvDetailOriginBridgeStreamEOF:
		out.DeliverRecvFailureLifecycleEvent = true
		out.BridgeStreamEOF = true
	case responsesws.RecvDetailOriginBridgeStreamError,
		responsesws.RecvDetailOriginNativeLocalAbort,
		responsesws.RecvDetailOriginNativeLocalDetach,
		responsesws.RecvDetailOriginAdapterPanic:
		out.DeliverRecvFailureLifecycleEvent = true
	case responsesws.RecvDetailOriginNativeBackpressure,
		responsesws.RecvDetailOriginNativeReadError,
		responsesws.RecvDetailOriginNativeProviderEOF:
		out.DeliverRecvFailureLifecycleEvent = true
		out.IdleRecvFailureClosesSession = true
	case responsesws.RecvDetailOriginProviderMalformed:
		out.DeliverRecvFailureLifecycleEvent = true
		out.IdleRecvFailureClosesSession = true
		out.ProviderMalformedClientPayload = true
	}
	return out
}

func responsesWSProviderDownstreamIsSyntheticBridgeCancelTerminal(event ResponsesWSEventProviderDownstream) bool {
	upstream := upstreamEventFromProviderDownstream(event)
	payload := responsesWSProviderPayloadPolicyForEvent(upstream)
	accounting := projectResponsesWSUpstreamAccountingEvent(upstream)
	if payload.PayloadOrigin != responsesws.PayloadOriginProxyLocal ||
		event.Kind != ProviderDownstreamFrame ||
		event.Frame == nil ||
		event.Frame.PayloadLen() == 0 ||
		accounting.Diagnostic.DetailOrigin != responsesws.RecvDetailOriginSyntheticBridge {
		return false
	}
	classified := responsesws.ClassifyResponsesWSEvent(event.Frame.Payload())
	return !classified.Malformed && classified.Kind == responsesws.ResponsesCancelledTerminal
}
