package relay

import (
	"context"
	"errors"
	"net/http"
	"one-api/common"
	"one-api/common/logger"
	commonresponses "one-api/common/responses"
	"one-api/common/responsesws"
	"one-api/internal/billing"
	"one-api/metrics"
	"one-api/middleware"
	"one-api/model"
	providersBase "one-api/providers/base"
	"one-api/relay/relay_util"
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
	Session           responsesws.Upstream
	BillingModel      string
	PromptModel       string
	Request           *types.OpenAIResponsesRequest
	MultiAgentEnabled bool
	StartedAt         time.Time
}

type responsesWSOpenResult struct {
	Session       responsesws.Upstream
	ActiveLease   middleware.ResponsesWSLease
	Provider      providersBase.ProviderInterface
	ProviderModel string
	BillingModel  string
	Channel       *model.Channel
	Candidate     *ResponsesTurnAffinity
}

type responsesWSOpenAdmission func(*gin.Context) (middleware.ResponsesWSLease, *types.OpenAIErrorWithStatusCode)

var openAndPrimeResponsesWSSessionForActor = openAndPrimeResponsesWSSessionWithContextAndFrameAndAdmission

type ResponsesDownstreamCommitKind int

const (
	DownstreamCommitNone ResponsesDownstreamCommitKind = iota
	DownstreamCommitProviderFrame
	DownstreamCommitProxyError
	DownstreamCommitSyntheticFrame
	DownstreamCommitKeepalive
	DownstreamCommitClosePayload
)

type ResponsesWSTurnAttempt struct {
	OpeningID                   string
	AttemptID                   string
	Admission                   *ResponsesWSTurnAdmission
	Candidate                   *ResponsesTurnAffinity
	SelectedChannelID           int
	Session                     responsesws.Upstream
	Billing                     *relay_util.AttemptQuota
	QuotaPreconsumed            bool
	PreconsumeAttempted         bool
	PreconsumeTruthApplied      bool
	QuotaFinalized              bool
	RolledBack                  bool
	RollbackErr                 error
	QuotaEventSinkAttached      bool
	CandidateBegun              bool
	TransportResult             responsesws.ResponsesWSTransportSendResult
	AttemptedPreviousResponseID string
	SeenProviderResponseID      string
	Usage                       *types.Usage
	TerminalObserved            bool
	TerminalUsage               *types.Usage
	AppliedSettlement           *ResponsesWSAppliedSettlement
	StartedAt                   time.Time
	FirstResponseAt             time.Time
	CompletedAt                 time.Time
	DownstreamCommitted         bool
	DownstreamCommittedAt       time.Time
	DownstreamCommitReason      string
	DownstreamCommitKind        ResponsesDownstreamCommitKind
	DownstreamCommitSeq         uint64
	ProviderAccepted            bool
	ProviderAcceptedAt          time.Time
	ProviderAcceptedReason      string
	ProviderAcceptedID          string
	RequireStoredOwner          bool
	StoredOwnerPersisted        bool
	MultiAgentEnabled           bool
	snapshot                    *ResponsesWSRequestSnapshot
	providerAPIErrorKeys        map[string]struct{}
	imageGenerationTracker      commonresponses.ImageGenerationStreamTracker
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
	billingAttempt, billingErr := relay_util.NewAttemptQuota(input.Context, input.BillingModel, int64(promptTokens), relay_util.BillingAttemptSpec{
		ChannelID:   input.SelectedChannelID,
		RequestKind: billing.SettlementRequestKindResponsesWS,
		LogProtocol: relay_util.LogProtocolResponsesWS,
		StartedAt:   startedAt,
	})
	if billingErr != nil {
		return nil, common.ErrorWrapperLocal(billingErr, "responses_ws_billing_admission_failed", http.StatusServiceUnavailable)
	}
	return &ResponsesWSTurnAttempt{
		OpeningID:                   input.OpeningID,
		Admission:                   input.Admission,
		Candidate:                   input.Candidate,
		SelectedChannelID:           input.SelectedChannelID,
		Session:                     input.Session,
		Billing:                     billingAttempt,
		AttemptedPreviousResponseID: strings.TrimSpace(input.Request.PreviousResponseID),
		RequireStoredOwner:          responseRequiresDurableOwner(input.Request),
		MultiAgentEnabled:           input.MultiAgentEnabled,
		Usage:                       usage,
		StartedAt:                   startedAt,
		snapshot:                    snapshot.Clone(),
	}, nil
}

func (a *ResponsesWSTurnAttempt) EnsureResponseOwnership(c *gin.Context, responseID string) *types.OpenAIErrorWithStatusCode {
	if a == nil || strings.TrimSpace(responseID) == "" || a.StoredOwnerPersisted {
		return nil
	}
	if a.RequireStoredOwner {
		if err := persistStoredResponseOwner(c, responseID, a.SelectedChannelID); err != nil {
			return err
		}
	} else {
		recordResponsesEphemeralProof(c, responseID, a.SelectedChannelID)
	}
	a.StoredOwnerPersisted = true
	return nil
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
	actor.clearPendingProviderState("begin_candidate")
	pending := actor.turns.pending
	pending.attempt = a
	pending.openingID = a.OpeningID
	if err := actor.turns.AttachPending(pending); err != nil {
		return err
	}
	return nil
}

func (a *ResponsesWSTurnAttempt) PreConsumeQuota() *types.OpenAIErrorWithStatusCode {
	return a.PreConsumeQuotaWithContext(nil)
}

func (a *ResponsesWSTurnAttempt) PreConsumeQuotaWithContext(ctx context.Context) *types.OpenAIErrorWithStatusCode {
	if a == nil || a.Billing == nil {
		return common.StringErrorWrapperLocal("quota transaction is required", "quota_transaction_missing", http.StatusInternalServerError)
	}
	startedAt := time.Now()
	err := a.Billing.ApplyReserve(ctx)
	outcome := "success"
	if err != nil {
		outcome = "error"
	}
	metrics.RecordResponsesWSPreconsumeForced(outcome, time.Since(startedAt), int(a.Billing.ReservedQuota()))
	a.PreconsumeAttempted = true
	if err == nil {
		a.QuotaPreconsumed = true
		a.PreconsumeTruthApplied = a.Billing.ReservedQuota() > 0
	}
	if err != nil {
		return relay_util.BillingAPIError(err, "responses_ws_billing_reserve_failed", http.StatusServiceUnavailable)
	}
	return nil
}

func (a *ResponsesWSTurnAttempt) ClaimSubmission() *types.OpenAIErrorWithStatusCode {
	if a == nil || a.Billing == nil {
		return common.StringErrorWrapperLocal("billing attempt is required", "responses_ws_billing_attempt_missing", http.StatusInternalServerError)
	}
	if err := a.Billing.ClaimSubmission(); err != nil {
		return common.ErrorWrapperLocal(err, "responses_ws_submission_claim_failed", http.StatusInternalServerError)
	}
	return nil
}

func (a *ResponsesWSTurnAttempt) CommitLocalWriteOK() {
	if a == nil {
		return
	}
	a.TransportResult = responsesws.ResponsesWSTransportSendResult{Status: responsesws.ResponsesWSTransportSendAttempted}
}

func (a *ResponsesWSTurnAttempt) CommitAmbiguousAdmission(reason string) {
	if a == nil {
		return
	}
	err := errors.New("responses websocket transport send ambiguous")
	if strings.TrimSpace(reason) != "" {
		err = errors.New(reason)
	}
	a.TransportResult = responsesws.ResponsesWSTransportSendResult{Status: responsesws.ResponsesWSTransportSendAmbiguous, Err: err}
}

func (a *ResponsesWSTurnAttempt) MarkProviderTerminalEvidence(classified responsesws.ResponsesTerminalResult) {
	if a == nil {
		return
	}
	if classified.Response != nil && classified.Response.Usage != nil {
		classified.Response.Usage.MarkProviderReported()
	}
	terminalUsage := responsesWSTerminalUsageSnapshot(classified.Response, &a.imageGenerationTracker)
	if terminalUsage != nil && a.Usage != nil {
		terminalUsage.ExtraBilling = mergeExtraBillingMapsMax(terminalUsage.ExtraBilling, a.Usage.ExtraBilling)
		if len(a.Usage.ProviderExtraBilling) > 0 {
			if terminalUsage.ProviderExtraBilling == nil {
				terminalUsage.ProviderExtraBilling = make(map[string]bool, len(a.Usage.ProviderExtraBilling))
			}
			for key, present := range a.Usage.ProviderExtraBilling {
				if present {
					terminalUsage.ProviderExtraBilling[key] = true
				}
			}
		}
		mergeResponsesWSIndependentUsageUnits(terminalUsage, a.Usage.ExtraUsageUnits, a.Usage.ProviderIndependentUsageUnits)
		if a.Usage.ProviderOperationUnits != nil {
			terminalUsage.ProviderOperationUnits = new(int)
			*terminalUsage.ProviderOperationUnits = *a.Usage.ProviderOperationUnits
		}
		terminalUsage.AttributionConflict = terminalUsage.AttributionConflict || a.Usage.AttributionConflict
		terminalUsage.ProviderTokenConflict = terminalUsage.ProviderTokenConflict || a.Usage.ProviderTokenConflict
		terminalUsage.MergeProviderAttribution(a.Usage.ResponseModel, a.Usage.ServiceTier)
		terminalUsage.MergeProviderSpeed(a.Usage.Speed, a.Usage.SpeedConflict)
		terminalUsage.MergeBillingDiagnostics(a.Usage.BillingDiagnostics)
	}
	responseID := ""
	if classified.Response != nil {
		responseID = classified.Response.ID
	}
	a.MarkProviderAccepted("terminal:"+classified.EventType, responseID)
	a.TerminalUsage = terminalUsage
	a.TerminalObserved = true
}

func (a *ResponsesWSTurnAttempt) ObserveResponsesStreamPayload(payload []byte) error {
	if a == nil {
		return nil
	}
	event, ok := commonresponses.ParseStreamUsageEvent(payload)
	if !ok {
		return nil
	}
	return a.imageGenerationTracker.ObserveUsageEvent(event)
}

func (a *ResponsesWSTurnAttempt) RememberProviderResponseID(responseID string) bool {
	if a == nil {
		return true
	}
	responseID = strings.TrimSpace(responseID)
	if responseID == "" {
		return true
	}
	a.MarkProviderAccepted("response_id", responseID)
	if a.SeenProviderResponseID == "" {
		a.SeenProviderResponseID = responseID
		return true
	}
	return a.SeenProviderResponseID == responseID
}

func (a *ResponsesWSTurnAttempt) MarkFirstProviderResponse(now time.Time) {
	if a == nil || !a.FirstResponseAt.IsZero() {
		return
	}
	if now.IsZero() {
		now = time.Now()
	}
	a.FirstResponseAt = now
	a.MarkProviderAccepted("first_provider_response", "")
}

func (a *ResponsesWSTurnAttempt) MarkProviderAccepted(reason string, responseID string) {
	if a == nil || a.ProviderAccepted {
		return
	}
	a.ProviderAccepted = true
	a.ProviderAcceptedAt = time.Now()
	a.ProviderAcceptedReason = strings.TrimSpace(reason)
	a.ProviderAcceptedID = strings.TrimSpace(responseID)
}

func (a *ResponsesWSTurnAttempt) MarkDownstreamCommitted(kind ResponsesDownstreamCommitKind, reason string, seq uint64) {
	if a == nil || a.DownstreamCommitted {
		return
	}
	a.DownstreamCommitted = true
	a.DownstreamCommittedAt = time.Now()
	a.DownstreamCommitKind = kind
	a.DownstreamCommitReason = strings.TrimSpace(reason)
	a.DownstreamCommitSeq = seq
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
	if a == nil || a.Billing == nil || a.Billing.Quota() == nil {
		return
	}
	if now.IsZero() {
		now = time.Now()
	}
	completedAt := a.CompletedAt
	if completedAt.IsZero() {
		completedAt = now
	}
	a.Billing.Quota().SeedTiming(a.StartedAt, a.FirstResponseAt, completedAt)
}

func (a *ResponsesWSTurnAttempt) RollbackBeforeLocalWriteOK(reason string) error {
	return a.RollbackBeforeLocalWriteOKWithContext(nil, reason)
}

func (a *ResponsesWSTurnAttempt) RollbackBeforeLocalWriteOKWithContext(operationCtx context.Context, reason string) error {
	if a == nil {
		return nil
	}
	_ = reason
	if a.QuotaPreconsumed && a.Billing != nil {
		ctx := a.Context()
		var result relay_util.AttemptResult
		var err error
		if operationCtx != nil {
			result, err = a.Billing.CloseWithoutSubmission(operationCtx)
		} else {
			rollbackCtx := context.Background()
			if ctx != nil && ctx.Request != nil {
				rollbackCtx = ctx.Request.Context()
			}
			result, err = a.Billing.CloseWithoutSubmission(rollbackCtx)
		}
		if err != nil {
			a.RollbackErr = err
			logCtx := context.Background()
			if ctx != nil && ctx.Request != nil {
				logCtx = ctx.Request.Context()
			}
			logger.LogError(logCtx, "responses websocket quota rollback failed: "+err.Error())
			return err
		}
		if result.Confirmed {
			a.QuotaFinalized = true
			a.QuotaPreconsumed = false
			a.PreconsumeTruthApplied = false
			return nil
		}
	}
	a.QuotaPreconsumed = false
	a.PreconsumeTruthApplied = false
	a.RolledBack = true
	a.RollbackErr = nil
	return nil
}

func (a *ResponsesWSTurnAttempt) ApplyResponsesWSSettlementDecision(c *gin.Context, decision ResponsesWSSettlementDecision) (ResponsesWSAppliedSettlement, error) {
	return a.ApplyResponsesWSSettlementDecisionWithContext(nil, c, decision)
}

func (a *ResponsesWSTurnAttempt) ApplyResponsesWSSettlementDecisionWithContext(operationCtx context.Context, c *gin.Context, decision ResponsesWSSettlementDecision) (ResponsesWSAppliedSettlement, error) {
	applied := ResponsesWSAppliedSettlement{}
	if a == nil {
		return applied, errors.New("responses websocket attempt is required")
	}
	attemptID := strings.TrimSpace(a.AttemptID)
	if attemptID == "" {
		return applied, errors.New("responses websocket attempt id is required for settlement")
	}
	applied.AttemptID = attemptID
	if a.AppliedSettlement != nil {
		return *a.AppliedSettlement, a.RollbackErr
	}
	if a.Billing == nil {
		return applied, errors.New("responses websocket billing attempt is required")
	}
	a.SeedQuotaTiming(time.Now())
	ctx := operationCtx
	if ctx == nil {
		ctx = responsesWSAttemptLogContext(c, a)
	}
	var result relay_util.AttemptResult
	var err error
	switch decision.Action {
	case ResponsesWSSettlementRollbackReserve:
		result, err = a.Billing.CloseWithoutSubmission(ctx)
	case ResponsesWSSettlementFinalizeExactUsage:
		result, err = a.Billing.CloseFromProviderResult(ctx, a.TerminalUsage, true)
	case ResponsesWSSettlementFinalizeProviderUsage:
		result, err = a.Billing.CloseFromProviderResult(ctx, a.Usage, true)
	default:
		return applied, errors.New("responses websocket settlement decision is invalid")
	}
	a.RollbackErr = err
	if err != nil {
		return applied, err
	}
	applied.AppliedFinalQuota = result.ChargedQuota
	switch {
	case !result.Confirmed:
		applied.Action = ResponsesWSSettlementRollbackReserve
		a.RolledBack = true
		metrics.RecordResponsesWSPreconsumeSettlement("rollback")
	case decision.Action == ResponsesWSSettlementFinalizeExactUsage:
		applied.Action = decision.Action
		a.QuotaFinalized = true
		metrics.RecordResponsesWSPreconsumeSettlement("finalize")
	default:
		applied.Action = decision.Action
		a.QuotaFinalized = true
		metrics.RecordResponsesWSPreconsumeSettlement("observed_usage")
	}
	a.QuotaPreconsumed = false
	a.PreconsumeTruthApplied = false
	a.RollbackErr = nil
	a.AppliedSettlement = cloneResponsesWSAppliedSettlement(applied)
	return applied, nil
}

func responsesWSAttemptLogContext(c *gin.Context, attempt *ResponsesWSTurnAttempt) context.Context {
	if c == nil && attempt != nil {
		c = attempt.Context()
	}
	if c != nil && c.Request != nil {
		return c.Request.Context()
	}
	return context.Background()
}

func cloneResponsesWSAppliedSettlement(applied ResponsesWSAppliedSettlement) *ResponsesWSAppliedSettlement {
	cloned := applied
	return &cloned
}
