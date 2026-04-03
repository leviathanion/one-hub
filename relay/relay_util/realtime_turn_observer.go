package relay_util

import (
	runtimesession "one-api/runtime/session"
	"one-api/types"
	"sync"
)

type RealtimeTurnObserver struct {
	mu            sync.Mutex
	quota         *Quota
	observedUsage types.UsageEvent
}

func (o *RealtimeTurnObserver) Quota() *Quota {
	if o == nil {
		return nil
	}
	return o.quota
}

func (o *RealtimeTurnObserver) ObserveTurnUsage(usage *types.UsageEvent) error {
	if o == nil || o.quota == nil || usage == nil {
		return nil
	}

	o.mu.Lock()
	defer o.mu.Unlock()
	return o.quota.UpdateUserRealtimeQuota(&o.observedUsage, usage)
}

func (o *RealtimeTurnObserver) FinalizeTurn(payload runtimesession.TurnFinalizePayload) {
	if o == nil || o.quota == nil {
		return
	}

	o.mu.Lock()
	defer o.mu.Unlock()

	finalUsage := payload.Usage.Clone()
	if finalUsage == nil {
		finalUsage = (&o.observedUsage).Clone()
	}
	if finalUsage == nil {
		finalUsage = &types.UsageEvent{}
	}

	o.quota.SeedTiming(payload.StartedAt, payload.FirstResponseAt, payload.CompletedAt)
	o.quota.ConsumeUsage(finalUsage.ToChatUsage(), false)
}

func NewRealtimeTurnObserverFactory(quotaTemplate *Quota) runtimesession.TurnObserverFactory {
	return func() runtimesession.TurnObserver {
		var quota *Quota
		if quotaTemplate != nil {
			quota = quotaTemplate.Clone()
		}
		return &RealtimeTurnObserver{quota: quota}
	}
}
