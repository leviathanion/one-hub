package relay

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"one-api/common"
	"one-api/common/logger"
	"one-api/common/responsesws"
	"one-api/internal/billing"
	"one-api/metrics"
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
	StartedAt         time.Time
}

type responsesWSOpenResult struct {
	Session       responsesws.Upstream
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
	OpeningID                   string
	AttemptID                   string
	Admission                   *ResponsesWSTurnAdmission
	Candidate                   *ResponsesTurnAffinity
	SelectedChannelID           int
	Session                     responsesws.Upstream
	Quota                       *relay_util.Quota
	QuotaPreconsumed            bool
	PreconsumeAttempted         bool
	PreconsumeTruthApplied      bool
	PreconsumeCacheApplied      bool
	QuotaFinalized              bool
	RolledBack                  bool
	RollbackErr                 error
	QuotaEventSinkAttached      bool
	CandidateBegun              bool
	TransportResult             responsesws.ResponsesWSTransportSendResult
	AttemptedPreviousResponseID string
	SeenProviderResponseID      string
	Usage                       *types.Usage
	TerminalEvidence            *ResponsesWSTerminalEvidence
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
	ReplayFailure               *types.OpenAIErrorWithStatusCode
	ReplayFailureOrigin         ResponsesAttemptFailureOrigin
	snapshot                    *ResponsesWSRequestSnapshot
	providerAPIErrorKeys        map[string]struct{}
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
		OpeningID:                   input.OpeningID,
		Admission:                   input.Admission,
		Candidate:                   input.Candidate,
		SelectedChannelID:           input.SelectedChannelID,
		Session:                     input.Session,
		Quota:                       quota,
		AttemptedPreviousResponseID: strings.TrimSpace(input.Request.PreviousResponseID),
		Usage:                       usage,
		StartedAt:                   startedAt,
		snapshot:                    snapshot.Clone(),
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
	if a == nil || a.Quota == nil {
		return common.StringErrorWrapperLocal("quota transaction is required", "quota_transaction_missing", http.StatusInternalServerError)
	}
	a.Quota.ForcePreConsume()
	startedAt := time.Now()
	err := a.Quota.PreQuotaConsumptionRollbackable()
	outcome := "success"
	if err != nil {
		outcome = "error"
	}
	metrics.RecordResponsesWSPreconsumeForced(outcome, time.Since(startedAt), a.Quota.PreConsumedQuota())
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
	terminalUsage := responsesWSTerminalUsageSnapshot(classified.Response)
	hasTerminalUsage := classified.Response != nil && classified.Response.Usage != nil
	billableQuota := int64(0)
	if hasTerminalUsage && a.Quota != nil {
		billableQuota = int64(a.Quota.GetTotalQuotaByUsage(terminalUsage))
	}
	responseID := ""
	if classified.Response != nil {
		responseID = classified.Response.ID
	}
	a.MarkProviderAccepted("terminal:"+classified.EventType, responseID)
	a.TerminalUsage = terminalUsage
	a.TerminalEvidence = &ResponsesWSTerminalEvidence{
		Kind:             classified.Kind,
		ResponseID:       responseID,
		HasTerminalUsage: hasTerminalUsage,
		BillableQuota:    billableQuota,
	}
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

func (a *ResponsesWSTurnAttempt) ApplyResponsesWSSettlementDecision(c *gin.Context, decision ResponsesWSSettlementDecision) (ResponsesWSAppliedSettlement, error) {
	applied := ResponsesWSAppliedSettlement{
		Action:             decision.Action,
		Basis:              decision.Basis,
		ExpectedFinalQuota: decision.ExpectedFinalQuota,
	}
	if a == nil {
		return applied, errors.New("responses websocket attempt is required")
	}
	attemptID := strings.TrimSpace(a.AttemptID)
	if attemptID == "" {
		return applied, errors.New("responses websocket attempt id is required for settlement")
	}
	applied.AttemptID = attemptID
	if c == nil {
		c = a.Context()
	}

	switch decision.Action {
	case ResponsesWSSettlementRollbackReserve:
		if decision.ExpectedFinalQuota != 0 {
			return applied, fmt.Errorf("responses websocket rollback settlement must expect zero final quota, got %d", decision.ExpectedFinalQuota)
		}
		if a.QuotaFinalized {
			return applied, errors.New("cannot rollback a finalized responses websocket attempt")
		}
		if a.RolledBack {
			if a.AppliedSettlement == nil {
				return applied, errors.New("responses websocket rolled back attempt is missing applied settlement")
			}
			stored := *a.AppliedSettlement
			if stored.Action != ResponsesWSSettlementRollbackReserve ||
				stored.Basis != decision.Basis ||
				stored.ExpectedFinalQuota != 0 ||
				stored.AppliedFinalQuota != 0 {
				return applied, errors.New("responses websocket duplicate rollback settlement mismatch")
			}
			return stored, nil
		}
		if err := a.RollbackBeforeLocalWriteOK("settlement_rollback"); err != nil {
			return applied, err
		}
		applied.AppliedFinalQuota = 0
		a.AppliedSettlement = cloneResponsesWSAppliedSettlement(applied)
		metrics.RecordResponsesWSPreconsumeSettlement("rollback")
		return applied, nil
	case ResponsesWSSettlementFinalizeExactUsage,
		ResponsesWSSettlementFinalizeFloor,
		ResponsesWSSettlementFinalizeObservedOrFloor:
		if a.QuotaFinalized {
			if a.AppliedSettlement == nil {
				return applied, errors.New("responses websocket finalized attempt is missing applied settlement")
			}
			stored := *a.AppliedSettlement
			if stored.ExpectedFinalQuota != decision.ExpectedFinalQuota ||
				stored.AppliedFinalQuota != decision.ExpectedFinalQuota ||
				stored.Action != decision.Action ||
				stored.Basis != decision.Basis {
				return applied, fmt.Errorf("responses websocket duplicate settlement mismatch: expected_final_quota=%d stored_expected=%d stored_applied=%d", decision.ExpectedFinalQuota, stored.ExpectedFinalQuota, stored.AppliedFinalQuota)
			}
			return stored, nil
		}
		if a.RolledBack {
			return applied, errors.New("cannot finalize a rolled back responses websocket attempt")
		}
		if a.Quota == nil {
			return applied, errors.New("responses websocket quota is required for final settlement")
		}
		if a.Quota.ModelName() == "" {
			return applied, errors.New("responses websocket quota model is required for final settlement")
		}
		if decision.ExpectedFinalQuota == 0 &&
			decision.Basis != ResponsesWSSettlementBasisTerminalUsage &&
			responsesWSSettlementHasFlag(decision, ResponsesWSSettlementFlagMissingSettlementFloor) {
			err := errors.New("responses websocket settlement missing floor for billable uncertain path")
			logger.LogError(responsesWSAttemptLogContext(c, a), err.Error())
			return applied, err
		}
		identity := a.settlementIdentity()
		a.SeedQuotaTiming(time.Now())
		usage := a.settlementUsageForDecision(decision)
		appliedQuota, settlementIdentity, err := a.Quota.ConsumeFixedFinalQuotaWithUsageIdentity(c, decision.ExpectedFinalQuota, usage, billing.SettlementRequestKindRealtimeTurn, identity, identity != "")
		applied.AppliedFinalQuota = appliedQuota
		applied.SettlementIdentity = settlementIdentity
		if err != nil {
			return applied, err
		}
		if appliedQuota != decision.ExpectedFinalQuota {
			err := fmt.Errorf("responses websocket settlement mismatch: expected_final_quota=%d applied_final_quota=%d", decision.ExpectedFinalQuota, appliedQuota)
			logger.LogError(responsesWSAttemptLogContext(c, a), err.Error())
			return applied, err
		}
		a.QuotaFinalized = true
		a.QuotaPreconsumed = false
		a.PreconsumeTruthApplied = false
		a.PreconsumeCacheApplied = false
		a.AppliedSettlement = cloneResponsesWSAppliedSettlement(applied)
		metrics.RecordResponsesWSPreconsumeSettlement("finalize")
		return applied, nil
	case ResponsesWSSettlementNoop:
		return applied, errors.New("responses websocket settlement noop is not executable")
	default:
		return applied, fmt.Errorf("unsupported responses websocket settlement action: %d", decision.Action)
	}
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

func (a *ResponsesWSTurnAttempt) settlementUsageForDecision(decision ResponsesWSSettlementDecision) *types.Usage {
	if a == nil {
		return nil
	}
	switch decision.Action {
	case ResponsesWSSettlementFinalizeExactUsage:
		return cloneResponsesWSUsage(a.TerminalUsage)
	case ResponsesWSSettlementFinalizeObservedOrFloor:
		if responsesWSUsageHasBillableEvidence(a.Usage) {
			return cloneResponsesWSUsage(a.Usage)
		}
	}
	return nil
}

func cloneResponsesWSAppliedSettlement(applied ResponsesWSAppliedSettlement) *ResponsesWSAppliedSettlement {
	cloned := applied
	return &cloned
}

func (a *ResponsesWSTurnAttempt) settlementIdentity() string {
	if a == nil {
		return ""
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
