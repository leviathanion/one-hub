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
	case responsesws.RecvDetailOriginNativeLocalAbort,
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
