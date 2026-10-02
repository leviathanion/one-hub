package relay

import (
	"context"
	"encoding/json"

	"one-api/internal/lifecycle"
	"one-api/model"
	"one-api/providers/openai"
	"one-api/types"
)

type backgroundObservation struct {
	ID       string                   `json:"id"`
	Status   string                   `json:"status"`
	Evidence *backgroundUsageEvidence `json:"evidence"`
}

// Each delivery owns a bounded, lossy accounting observer. A slow database does
// not backpressure provider frames. Durable polling is the recovery path; only
// the ownership acceptance barrier is allowed to precede ID delivery.
const responsesBackgroundObserverDoneContextKey = "responses_background_observer_done"

var backgroundObservers = &lifecycle.Group{}

// Called after request handlers stop, so no admitted delivery can create a new
// observer while this group closes. Durable task polling owns recovery.
func StopBackgroundObservers(ctx context.Context) error {
	backgroundObservers.Close()
	return backgroundObservers.Wait(ctx)
}

type backgroundObservationSink struct {
	queue chan []byte
	done  chan struct{}
}

func newBackgroundObservationSink(parent context.Context, ownerID string) *backgroundObservationSink {
	sink := &backgroundObservationSink{queue: make(chan []byte, 8), done: make(chan struct{})}
	finish, ok := backgroundObservers.Start()
	if !ok {
		close(sink.done)
		return sink
	}
	parent = context.WithoutCancel(parent)
	go func() {
		defer finish()
		defer close(sink.done)
		for raw := range sink.queue {
			var observation backgroundObservation
			if json.Unmarshal(raw, &observation) != nil {
				continue
			}
			ctx, cancel := boundedResponsesLifecycleContext(parent)
			task, err := model.GetBackgroundResponseTask(ctx, ownerID)
			if err == nil && task.ProviderState == model.TaskProviderStateAccepted && model.TaskProviderID(task) == observation.ID {
				observeBackgroundTask(ctx, task, &types.OpenAIResponsesResponses{ID: observation.ID, Status: observation.Status}, observation.Evidence.restore())
			}
			cancel()
		}
	}()
	return sink
}
func (sink *backgroundObservationSink) Close() {
	if sink != nil {
		close(sink.queue)
	}
}
func (sink *backgroundObservationSink) Submit(response *types.OpenAIResponsesResponses, usage *types.Usage) {
	if sink == nil || response == nil || response.ID == "" {
		return
	}
	if usage == nil {
		usage = &types.Usage{}
		openai.ObserveStoredResponsesUsage(response, usage)
	}
	raw, err := json.Marshal(backgroundObservation{ID: response.ID, Status: response.Status, Evidence: captureBackgroundUsage(response.ID, usage)})
	if err != nil || len(raw) > backgroundEvidenceLimit {
		return
	}
	select {
	case sink.queue <- raw:
		return
	default:
	}
	// Retain the newest evidence, including terminal evidence, within the same
	// fixed budget. Discarding an observation never discards a provider frame.
	select {
	case <-sink.queue:
	default:
	}
	select {
	case sink.queue <- raw:
	default:
	}
}
