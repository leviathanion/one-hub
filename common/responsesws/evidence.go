package responsesws

// ProviderActivityFact is the only accounting-facing provider activity shape
// derived from transport detail. It deliberately keeps DetailOrigin out of the
// settlement core.
type ProviderActivityFact struct {
	ProviderFrameSeen     bool
	ProviderUsageSeen     bool
	ProviderPeerCloseSeen bool
}

func (f ProviderActivityFact) HasActivity() bool {
	return f.ProviderFrameSeen ||
		f.ProviderUsageSeen ||
		f.ProviderPeerCloseSeen
}

func (f *ProviderActivityFact) Merge(other ProviderActivityFact) {
	if f == nil {
		return
	}
	f.ProviderFrameSeen = f.ProviderFrameSeen || other.ProviderFrameSeen
	f.ProviderUsageSeen = f.ProviderUsageSeen || other.ProviderUsageSeen
	f.ProviderPeerCloseSeen = f.ProviderPeerCloseSeen || other.ProviderPeerCloseSeen
}

// DiagnosticDetail preserves the provider/transport facts needed for trace and
// debugging. These facts are not settlement-core inputs.
type DiagnosticDetail struct {
	DetailOrigin     RecvDetailOrigin
	DetailPhase      RecvDetailPhase
	HasFrame         bool
	FrameKind        FrameKind
	HasUsage         bool
	HasProviderClose bool
	HasError         bool
}

// ProviderObservation is the canonical event log entry kept by the relay actor.
// It stores the transport detail once; activity evidence and diagnostics are
// projected from this shape.
type ProviderObservation struct {
	DetailOrigin     RecvDetailOrigin
	DetailPhase      RecvDetailPhase
	HasFrame         bool
	FrameKind        FrameKind
	HasUsage         bool
	HasProviderClose bool
	HasError         bool
}

func NewProviderObservation(event UpstreamEvent) ProviderObservation {
	origin := NormalizeUpstreamEventDetailOrigin(event)
	frameKind := FrameKind(0)
	hasFrame := event.Frame != nil
	if hasFrame {
		frameKind = event.Frame.Kind()
	}
	hasProviderClose := event.ProviderClose != nil
	hasErr := event.Err != nil
	if event.ProviderClose != nil && event.ProviderClose.Err != nil {
		hasErr = true
	}
	return ProviderObservation{
		DetailOrigin:     origin,
		DetailPhase:      event.DetailPhase,
		HasFrame:         hasFrame,
		FrameKind:        frameKind,
		HasUsage:         event.Usage != nil,
		HasProviderClose: hasProviderClose,
		HasError:         hasErr,
	}
}

func (o ProviderObservation) IsZero() bool {
	return o.DetailOrigin == "" &&
		o.DetailPhase == "" &&
		!o.HasFrame &&
		!o.HasUsage &&
		!o.HasProviderClose &&
		!o.HasError
}

type ProviderSettlementProjection struct {
	Activity            ProviderActivityFact
	Diagnostic          DiagnosticDetail
	HasProviderActivity bool
}

type ProviderTransportPolicy struct {
	PayloadOrigin      PayloadOrigin
	PayloadOriginKnown bool
	CanCarryUsage      bool
	CanCarryTerminal   bool
}

type providerObservationOriginSpec struct {
	Activity  ProviderActivityFact
	Transport ProviderTransportPolicy
}

func ProjectProviderObservationForSettlement(obs ProviderObservation) ProviderSettlementProjection {
	spec := providerObservationOriginSpecFor(obs)
	projection := ProviderSettlementProjection{
		Activity:   spec.Activity,
		Diagnostic: providerObservationDiagnostic(obs),
	}
	if obs.HasUsage && spec.Transport.CanCarryUsage {
		projection.Activity.ProviderUsageSeen = true
	}
	projection.HasProviderActivity = projection.Activity.HasActivity()
	return projection
}

func ProjectProviderObservationTransportPolicy(obs ProviderObservation) ProviderTransportPolicy {
	return providerObservationOriginSpecFor(obs).Transport
}

func providerObservationDiagnostic(obs ProviderObservation) DiagnosticDetail {
	return DiagnosticDetail{
		DetailOrigin:     obs.DetailOrigin,
		DetailPhase:      obs.DetailPhase,
		HasFrame:         obs.HasFrame,
		FrameKind:        obs.FrameKind,
		HasUsage:         obs.HasUsage,
		HasProviderClose: obs.HasProviderClose,
		HasError:         obs.HasError,
	}
}

func providerObservationOriginSpecFor(obs ProviderObservation) providerObservationOriginSpec {
	payloadOrigin, payloadOriginKnown := ExpectedPayloadOriginForRecvDetailOrigin(obs.DetailOrigin)
	spec := providerObservationOriginSpec{
		Transport: ProviderTransportPolicy{
			PayloadOrigin:      payloadOrigin,
			PayloadOriginKnown: payloadOriginKnown,
			CanCarryUsage:      recvDetailOriginCanCarryProviderUsage(obs.DetailOrigin),
			CanCarryTerminal:   RecvDetailOriginCanCarryProviderTerminal(obs.DetailOrigin),
		},
	}
	switch obs.DetailOrigin {
	case RecvDetailOriginProviderFrame,
		RecvDetailOriginProviderStream,
		RecvDetailOriginProviderMalformed:
		spec.Activity.ProviderFrameSeen = true
	case RecvDetailOriginAdapterPanic:
		if obs.DetailPhase == RecvDetailPhaseHandleProviderFrame {
			spec.Activity.ProviderFrameSeen = true
		}
	case RecvDetailOriginNativeProviderClose:
		spec.Activity.ProviderPeerCloseSeen = true
	}
	return spec
}

func recvDetailOriginCanCarryProviderUsage(origin RecvDetailOrigin) bool {
	switch origin {
	case RecvDetailOriginProviderFrame, RecvDetailOriginProviderStream, RecvDetailOriginProviderMalformed:
		return true
	default:
		return false
	}
}

type ProviderActivityProjection struct {
	Activity   ProviderActivityFact
	LastOrigin RecvDetailOrigin
}

func (p *ProviderActivityProjection) Observe(obs ProviderObservation) {
	if p == nil || obs.IsZero() {
		return
	}
	projected := ProjectProviderObservationForSettlement(obs)
	p.Activity.Merge(projected.Activity)
	if projected.Activity.HasActivity() && projected.Diagnostic.DetailOrigin != "" {
		p.LastOrigin = projected.Diagnostic.DetailOrigin
	}
}

func (p *ProviderActivityProjection) Merge(other ProviderActivityProjection) {
	if p == nil {
		return
	}
	p.Activity.Merge(other.Activity)
	if other.LastOrigin != "" {
		p.LastOrigin = other.LastOrigin
	}
}

func (p ProviderActivityProjection) HasActivity() bool {
	return p.Activity.HasActivity()
}

func (p ProviderActivityProjection) IsZero() bool {
	return !p.Activity.HasActivity()
}

func (p ProviderActivityProjection) LastActivityOrigin() RecvDetailOrigin {
	return p.LastOrigin
}

func NormalizeUpstreamEventDetailOrigin(event UpstreamEvent) RecvDetailOrigin {
	if event.DetailOrigin != "" {
		return event.DetailOrigin
	}
	if event.ProviderClose != nil {
		return RecvDetailOriginNativeProviderClose
	}
	if event.Frame != nil || event.Usage != nil {
		return RecvDetailOriginProviderFrame
	}
	return ""
}

func UpstreamEventHasProviderEvidence(event UpstreamEvent) bool {
	return ProjectProviderObservationForSettlement(NewProviderObservation(event)).Activity.HasActivity()
}

func UpstreamEventHasProviderTerminalEvidence(event UpstreamEvent) bool {
	origin := NormalizeUpstreamEventDetailOrigin(event)
	if !RecvDetailOriginCanCarryProviderTerminal(origin) || event.Frame == nil {
		return false
	}
	if event.Frame.Kind() != FrameKindText {
		return false
	}
	classified := ClassifyResponsesWSEvent(event.Frame.Payload())
	return !classified.Malformed && classified.Kind != ResponsesNonTerminal
}

func UpstreamEventIsProxyLocalTerminal(event UpstreamEvent) bool {
	origin := NormalizeUpstreamEventDetailOrigin(event)
	switch origin {
	case RecvDetailOriginProxyLocal,
		RecvDetailOriginNativeBackpressure,
		RecvDetailOriginNativeLocalAbort,
		RecvDetailOriginNativeLocalDetach,
		RecvDetailOriginNativeReadError,
		RecvDetailOriginAdapterPanic,
		RecvDetailOriginProviderMalformed:
		return true
	default:
		return false
	}
}
