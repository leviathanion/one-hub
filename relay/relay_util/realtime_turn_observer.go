package relay_util

import (
	"fmt"
	"net/url"
	"one-api/common/logger"
	"one-api/internal/billing"
	runtimesession "one-api/runtime/session"
	"one-api/types"
	"strings"
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
	identity := buildRealtimeTurnSettlementIdentity(o.quota, payload)
	if err := o.quota.ConsumeUsageWithIdentity(finalUsage.ToChatUsage(), false, billing.SettlementRequestKindRealtimeTurn, identity, true); err != nil {
		logger.LogError(o.quota.requestContext, "realtime finalize settlement failed: "+err.Error())
		return
	}
}

func buildRealtimeTurnSettlementIdentity(quota *Quota, payload runtimesession.TurnFinalizePayload) string {
	sessionID := strings.TrimSpace(payload.SessionID)
	if sessionID == "" || payload.TurnSeq <= 0 {
		return ""
	}

	callerNS := ""
	channelID := 0
	if quota != nil {
		callerNS = strings.TrimSpace(quota.callerNS)
		channelID = quota.channelId
	}

	return fmt.Sprintf(
		"caller=%s|channel=%d|session=%s|turn=%d|finalize",
		url.QueryEscape(callerNS),
		channelID,
		url.QueryEscape(sessionID),
		payload.TurnSeq,
	)
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
