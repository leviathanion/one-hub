package relay

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"one-api/common"
	"one-api/common/logger"
	"one-api/internal/billing"
	"one-api/model"
	providersBase "one-api/providers/base"
	"one-api/relay/relay_util"
	runtimesession "one-api/runtime/session"
	"one-api/types"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

type ResponsesWSTurnAdmission struct {
	RPMAllowed bool
}

func NewResponsesWSTurnAdmission() *ResponsesWSTurnAdmission {
	return &ResponsesWSTurnAdmission{}
}

func (a *ResponsesWSTurnAdmission) AllowRPMOnce(allow func() *types.OpenAIErrorWithStatusCode) *types.OpenAIErrorWithStatusCode {
	if a == nil {
		return common.StringErrorWrapperLocal("turn admission is required", "responses_ws_admission_missing", http.StatusInternalServerError)
	}
	if a.RPMAllowed {
		return nil
	}
	if allow == nil {
		return common.StringErrorWrapperLocal("request limiter is not configured", "rate_limiter_missing", http.StatusInternalServerError)
	}
	if err := allow(); err != nil {
		return err
	}
	a.RPMAllowed = true
	return nil
}

type ResponsesWSTurnAttemptInput struct {
	Context           *gin.Context
	Snapshot          *ResponsesWSRequestSnapshot
	OpeningID         string
	Admission         *ResponsesWSTurnAdmission
	Candidate         *ResponsesTurnAffinity
	SelectedChannelID int
	Session           runtimesession.RealtimeSession
	BillingModel      string
	PromptModel       string
	Request           *types.OpenAIResponsesRequest
	StartedAt         time.Time
}

type responsesWSOpenResult struct {
	Session       runtimesession.RealtimeSession
	Provider      providersBase.ProviderInterface
	ProviderModel string
	BillingModel  string
	Channel       *model.Channel
	Candidate     *ResponsesTurnAffinity
}

type SelectedChannelSnapshot struct {
	ChannelID            int
	ChannelType          int
	PreCost              int
	ProviderModel        string
	BillingModel         string
	OriginalModel        string
	BillingOriginalModel bool
	Channel              *model.Channel
}

var openAndPrimeResponsesWSSessionForActor = openAndPrimeResponsesWSSessionWithContext

type ResponsesWSTurnAttempt struct {
	OpeningID              string
	AttemptID              string
	Admission              *ResponsesWSTurnAdmission
	Candidate              *ResponsesTurnAffinity
	SelectedChannelID      int
	Session                runtimesession.RealtimeSession
	Quota                  *relay_util.Quota
	QuotaPreconsumed       bool
	PreconsumeAttempted    bool
	PreconsumeTruthApplied bool
	PreconsumeCacheApplied bool
	QuotaFinalized         bool
	RolledBack             bool
	RollbackErr            error
	QuotaEventSinkAttached bool
	CandidateBegun         bool
	SendOutcome            ResponsesWSSendOutcome
	Usage                  *types.Usage
	BillingEvidence        BillingEvidence
	StartedAt              time.Time
	FirstResponseAt        time.Time
	CompletedAt            time.Time
	snapshot               *ResponsesWSRequestSnapshot
	providerAPIErrorKeys   map[string]struct{}
}

func PrepareResponsesWSTurnAttempt(input ResponsesWSTurnAttemptInput) (*ResponsesWSTurnAttempt, *types.OpenAIErrorWithStatusCode) {
	if input.Context == nil || input.Request == nil {
		return nil, common.StringErrorWrapperLocal("responses websocket turn context is required", "invalid_request_error", http.StatusBadRequest)
	}
	promptModel := strings.TrimSpace(input.PromptModel)
	if promptModel == "" {
		promptModel = input.BillingModel
	}
	channelPreCost := 0
	if channel, ok := input.Context.Get("responses_ws_selected_channel"); ok {
		if typed, ok := channel.(*model.Channel); ok && typed != nil {
			channelPreCost = typed.PreCost
		}
	}
	promptTokens := common.CountTokenInputMessages(input.Request.Input, promptModel, channelPreCost)
	// The local prompt estimate is only the quota pre-consume ledger. Final
	// billing usage starts empty and is filled from provider usage/terminal events.
	usage := &types.Usage{}
	snapshot := input.Snapshot
	if snapshot == nil {
		snapshot = NewResponsesWSRequestSnapshot(input.Context)
	}
	startedAt := input.StartedAt
	if startedAt.IsZero() {
		startedAt = time.Now()
	}
	quota := relay_util.NewQuota(input.Context, input.BillingModel, promptTokens)
	quota.SetLogProtocol(relay_util.LogProtocolResponsesWS)
	return &ResponsesWSTurnAttempt{
		OpeningID:         input.OpeningID,
		Admission:         input.Admission,
		Candidate:         input.Candidate,
		SelectedChannelID: input.SelectedChannelID,
		Session:           input.Session,
		Quota:             quota,
		Usage:             usage,
		StartedAt:         startedAt,
		snapshot:          snapshot.Clone(),
	}, nil
}

func (a *ResponsesWSTurnAttempt) Context() *gin.Context {
	if a == nil || a.snapshot == nil {
		return nil
	}
	return a.snapshot.Context()
}

func (a *ResponsesWSTurnAttempt) BeginCandidate(actor *ResponsesWSSessionActor) error {
	if a == nil || actor == nil {
		return errors.New("responses websocket attempt and actor are required")
	}
	if a.CandidateBegun {
		return nil
	}
	a.AttemptID = uuid.NewString()
	a.CandidateBegun = true
	actor.pendingAttempt = a
	actor.pendingProviderEvents = nil
	actor.pendingProviderBytes = 0
	actor.pendingProviderEvidenceSeen = false
	return nil
}

func (a *ResponsesWSTurnAttempt) PreConsumeQuota() *types.OpenAIErrorWithStatusCode {
	if a == nil || a.Quota == nil {
		return common.StringErrorWrapperLocal("quota transaction is required", "quota_transaction_missing", http.StatusInternalServerError)
	}
	err := a.Quota.PreQuotaConsumptionRollbackable()
	a.PreconsumeAttempted = true
	if a.Quota.HasPreConsumedSideEffect() {
		a.QuotaPreconsumed = true
		a.PreconsumeTruthApplied = a.Quota.PreconsumeTruthApplied
		a.PreconsumeCacheApplied = a.Quota.PreconsumeCacheApplied
	}
	return err
}

func (a *ResponsesWSTurnAttempt) CommitLocalWriteOK() {
	if a == nil {
		return
	}
	a.SendOutcome = SendOutcomeLocalWriteOK
}

func (a *ResponsesWSTurnAttempt) CommitAmbiguousAdmission(reason string) {
	if a == nil {
		return
	}
	_ = reason
	a.SendOutcome = SendOutcomeAmbiguous
}

func (a *ResponsesWSTurnAttempt) MarkProviderUsageSeen() {
	if a == nil {
		return
	}
	a.BillingEvidence = BillingEvidenceProviderUsageSeen
}

func (a *ResponsesWSTurnAttempt) MarkProviderAcceptedTurnEvidence() {
	if a == nil || a.BillingEvidence == BillingEvidenceProviderUsageSeen {
		return
	}
	a.BillingEvidence = BillingEvidenceProviderAcceptedTurnEvidence
}

func (a *ResponsesWSTurnAttempt) MarkFirstProviderResponse(now time.Time) {
	if a == nil || !a.FirstResponseAt.IsZero() {
		return
	}
	if now.IsZero() {
		now = time.Now()
	}
	a.FirstResponseAt = now
}

func (a *ResponsesWSTurnAttempt) MarkCompleted(now time.Time) {
	if a == nil || !a.CompletedAt.IsZero() {
		return
	}
	if now.IsZero() {
		now = time.Now()
	}
	a.CompletedAt = now
}

func (a *ResponsesWSTurnAttempt) SeedQuotaTiming(now time.Time) {
	if a == nil || a.Quota == nil {
		return
	}
	if now.IsZero() {
		now = time.Now()
	}
	if a.CompletedAt.IsZero() {
		a.MarkCompleted(now)
	}
	a.Quota.SeedTiming(a.StartedAt, a.FirstResponseAt, a.CompletedAt)
}

func (a *ResponsesWSTurnAttempt) RollbackBeforeLocalWriteOK(reason string) error {
	if a == nil {
		return nil
	}
	_ = reason
	if a.QuotaPreconsumed && a.Quota != nil {
		ctx := a.Context()
		if err := a.Quota.UndoSynchronously(ctx); err != nil {
			a.RollbackErr = err
			logCtx := context.Background()
			if ctx != nil && ctx.Request != nil {
				logCtx = ctx.Request.Context()
			}
			logger.LogError(logCtx, "responses websocket quota rollback failed: "+err.Error())
			return err
		}
	}
	a.QuotaPreconsumed = false
	a.PreconsumeTruthApplied = false
	a.PreconsumeCacheApplied = false
	a.RolledBack = true
	a.RollbackErr = nil
	return nil
}

func (a *ResponsesWSTurnAttempt) FinalizeQuota(c *gin.Context) {
	if c == nil {
		c = a.Context()
	}
	a.finalizeQuota(c, false)
}

func (a *ResponsesWSTurnAttempt) FinalizeQuotaPreservingPreConsumed(c *gin.Context) {
	if c == nil {
		c = a.Context()
	}
	a.finalizeQuota(c, true)
}

func (a *ResponsesWSTurnAttempt) finalizeQuota(c *gin.Context, preservePreConsumed bool) {
	if a == nil || a.Quota == nil || a.QuotaFinalized || a.RolledBack {
		return
	}
	hasUsage := responsesWSUsageHasBillableEvidence(a.Usage)
	identity := a.settlementIdentity()
	a.SeedQuotaTiming(time.Now())
	if preservePreConsumed && !hasUsage {
		// Trade-off: once a turn has reached a send/active state, absence of a
		// terminal usage frame is not proof that the provider did no work. Keep
		// the pre-consumed floor to avoid making ambiguous completed work free;
		// explicit NotSent/pre-send paths still rollback before finalization.
		if err := a.Quota.ConsumeAtLeastPreConsumedWithIdentity(c, a.Usage, true, billing.SettlementRequestKindRealtimeTurn, identity, identity != ""); err != nil {
			logger.LogError(context.Background(), "responses websocket quota floor settlement failed: "+err.Error())
		}
	} else {
		if err := a.Quota.ConsumeWithIdentity(c, a.Usage, true, billing.SettlementRequestKindRealtimeTurn, identity, identity != ""); err != nil {
			logger.LogError(context.Background(), "responses websocket quota settlement failed: "+err.Error())
		}
	}
	a.QuotaFinalized = true
}

func (a *ResponsesWSTurnAttempt) settlementIdentity() string {
	if a == nil {
		return ""
	}
	if a.AttemptID == "" {
		a.AttemptID = uuid.NewString()
	}
	parts := make([]string, 0, 3)
	if a.OpeningID != "" {
		parts = append(parts, "opening="+a.OpeningID)
	}
	if a.AttemptID != "" {
		parts = append(parts, "attempt="+a.AttemptID)
	}
	if a.SelectedChannelID != 0 {
		parts = append(parts, fmt.Sprintf("channel=%d", a.SelectedChannelID))
	}
	return strings.Join(parts, "|") + "|finalize"
}
