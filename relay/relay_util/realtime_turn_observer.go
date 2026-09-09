package relay_util

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"one-api/common/logger"
	"one-api/internal/billing"
	runtimesession "one-api/runtime/session"
	"one-api/types"

	"github.com/gin-gonic/gin"
)

type RealtimeTurnObserver struct {
	mu sync.Mutex

	requestContext *gin.Context
	models         runtimesession.ModelBinding
	policy         runtimesession.RealtimeWorkPolicy
	sessionID      string
	workID         string
	inputItemID    string
	attempt        *AttemptQuota
	observedUsage  types.UsageEvent
	finalized      bool
	finalResult    runtimesession.TurnFinalizationResult
}

func (o *RealtimeTurnObserver) ObserveTurnUsage(usage *types.UsageEvent) error {
	if o == nil || usage == nil {
		return nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.finalized {
		return nil
	}
	if o.attempt == nil {
		return errors.New("realtime provider work has no admitted billing attempt")
	}
	if usage.Source == types.UsageSourceInputAudioTranscription {
		ownerItemID := strings.TrimSpace(o.inputItemID)
		eventItemID := strings.TrimSpace(usage.ItemID)
		if ownerItemID != "" && eventItemID != "" && ownerItemID != eventItemID {
			return nil
		}
	}
	o.observedUsage.Merge(ProviderUsageEventForBilling(usage))
	return nil
}

func (o *RealtimeTurnObserver) AdmitTurn() error {
	return errors.New("realtime work requires a controlled input or response.create admission")
}

func (o *RealtimeTurnObserver) bindAdmission(admission runtimesession.TurnAdmission) {
	if admission.Models.RequestedModel != "" {
		o.models = admission.Models
	}
	o.workID = admission.WorkID
	o.sessionID = admission.SessionID
	o.inputItemID = admission.InputItemID
	if o.policy == nil {
		o.policy = NewRealtimeWorkPolicy(o.requestContext, o.models, nil)
	}
}

func (o *RealtimeTurnObserver) checkAdmission(admission runtimesession.TurnAdmission) error {
	if admission.WorkAuthorized {
		return nil
	}
	return o.policy.CheckFutureWork(o.models, true)
}

func (o *RealtimeTurnObserver) snapshotPrincipal() {
	if policy, ok := o.policy.(*realtimeWorkPolicy); ok {
		o.requestContext = policy.snapshot()
	}
	// 转写可使用自己的映射规则，不能继承主会话的原模型计费开关。
	o.requestContext.Set("billing_original_model", o.models.BillingOriginal)
}

func (o *RealtimeTurnObserver) AdmitBoundedTurn(admission runtimesession.TurnAdmission) error {
	if o == nil || o.requestContext == nil {
		return errors.New("realtime billing context is required")
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.finalized || o.attempt != nil {
		return errors.New("realtime turn was already admitted")
	}
	if !admission.ExplicitClientCreate && !admission.Transcription {
		return errors.New("realtime billing requires an explicit response.create or controlled transcription input")
	}
	if admission.PromptTokens < 0 || admission.MaxOutputTokens < 0 {
		return errors.New("turn billing bounds cannot be negative")
	}
	o.bindAdmission(admission)
	if err := o.checkAdmission(admission); err != nil {
		return err
	}
	o.snapshotPrincipal()
	attempt, err := NewAttemptQuota(o.requestContext, o.models.BillingModel, admission.PromptTokens, realtimeAttemptSpec())
	if err != nil {
		return err
	}
	o.attempt = attempt
	ctx := realtimeBillingContext(o.requestContext)
	if err := attempt.ApplyReserve(ctx); err != nil {
		result, closeErr := attempt.CloseWithoutSubmission(ctx)
		o.finish(result, closeErr, runtimesession.TurnFinalizePayload{TerminationReason: "reserve_failed"})
		return errors.Join(err, closeErr)
	}
	if err := attempt.ClaimSubmission(); err != nil {
		result, closeErr := attempt.CloseWithoutSubmission(ctx)
		o.finish(result, closeErr, runtimesession.TurnFinalizePayload{TerminationReason: "claim_failed"})
		return errors.Join(err, closeErr)
	}
	return nil
}

func (o *RealtimeTurnObserver) ObserveProviderInitiatedTurn(admission runtimesession.TurnAdmission) error {
	if o == nil || o.requestContext == nil {
		return errors.New("realtime billing context is required")
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.finalized || o.attempt != nil {
		return errors.New("realtime turn was already admitted")
	}
	o.bindAdmission(admission)
	// 上游已产生工作。拒绝未来工作不能抹掉已有 owner，也不能伪造预扣。
	admissionErr := o.checkAdmission(admission)
	o.snapshotPrincipal()
	attempt, err := NewObservationAttemptQuota(o.requestContext, o.models.BillingModel, realtimeAttemptSpec())
	if err != nil {
		return errors.Join(admissionErr, err)
	}
	if err := attempt.ClaimObservedSubmission(); err != nil {
		return errors.Join(admissionErr, err)
	}
	o.attempt = attempt
	if admissionErr != nil {
		o.finalResult.StopFutureWork = true
	}
	return admissionErr
}

func realtimeAttemptSpec() BillingAttemptSpec {
	return BillingAttemptSpec{
		RequestKind: billing.SettlementRequestKindRealtimeTurn,
		LogProtocol: LogProtocolRealtimeWS,
		StartedAt:   time.Now(),
	}
}

func (o *RealtimeTurnObserver) RollbackTurnAdmission(reason string) error {
	if o == nil {
		return nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.finalized {
		return o.finalResult.Err
	}
	if o.attempt == nil {
		o.finalized = true
		return nil
	}
	result, err := o.attempt.CloseWithoutSubmission(realtimeBillingContext(o.requestContext))
	o.finish(result, err, runtimesession.TurnFinalizePayload{TerminationReason: reason})
	return err
}

func (o *RealtimeTurnObserver) FinalizeTurn(payload runtimesession.TurnFinalizePayload) {
	if o == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.finalized || o.attempt == nil {
		return
	}
	// payload 是生命周期累计快照；计费 delta 已经 Observe，不能再次合并。
	usage := o.observedUsage.ToChatUsage()
	result, err := o.attempt.CloseFromProviderResult(realtimeBillingContext(o.requestContext), usage, true)
	o.finish(result, err, payload)
}

func (o *RealtimeTurnObserver) finish(result AttemptResult, err error, payload runtimesession.TurnFinalizePayload) {
	o.finalized = true
	o.finalResult.Unsettled = result.Unsettled
	o.finalResult.Err = err
	if result.Unsettled || err != nil {
		o.finalResult.StopFutureWork = true
		o.logUnsettled(result, err, payload)
		return
	}
	if o.policy != nil {
		if policyErr := o.policy.CheckFutureWork(o.models, false); policyErr != nil {
			o.finalResult.StopFutureWork = true
			o.finalResult.Err = policyErr
		}
	}
}

func (o *RealtimeTurnObserver) FinalizationResult() runtimesession.TurnFinalizationResult {
	if o == nil {
		return runtimesession.TurnFinalizationResult{}
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.finalResult
}

func (o *RealtimeTurnObserver) logUnsettled(result AttemptResult, err error, payload runtimesession.TurnFinalizePayload) {
	models := o.models
	models.ReportedModel = payload.Models.ReportedModel
	if models.ReportedModel == "" {
		models.ReportedModel = payload.Model
	}
	workID, itemID := o.workID, o.inputItemID
	sessionID := o.sessionID
	if payload.SessionID != "" {
		sessionID = payload.SessionID
	}
	if workID == "" {
		workID = payload.WorkID
	}
	if payload.InputItemID != "" {
		itemID = payload.InputItemID
	}
	errorClass := "balance_action_failed"
	if errors.Is(err, context.DeadlineExceeded) {
		errorClass = "deadline_exhausted"
	} else if err != nil && (strings.Contains(err.Error(), "commit outcome is unknown") || strings.Contains(err.Error(), "commit outcome is indeterminate")) {
		errorClass = "commit_unknown"
	}
	diagnostic := map[string]any{
		"session_id": sessionID, "work_id": workID, "input_item_id": itemID,
		"user_id": o.requestContext.GetInt("id"), "token_id": o.requestContext.GetInt("token_id"),
		"channel_id": o.requestContext.GetInt("channel_id"), "models": models,
		"reserved_quota": o.attempt.ReservedQuota(), "charged_quota": result.ChargedQuota,
		"phase": "finalize", "reason": payload.TerminationReason,
		"unsettled": result.Unsettled, "error_class": errorClass, "error": fmt.Sprint(err),
	}
	encoded, _ := json.Marshal(diagnostic)
	logger.LogError(realtimeBillingContext(o.requestContext), "realtime billing finalization: "+string(encoded))
}

func realtimeBillingContext(c *gin.Context) context.Context {
	if c != nil && c.Request != nil {
		return context.WithoutCancel(c.Request.Context())
	}
	return context.Background()
}

func NewRealtimeTurnObserverFactory(c *gin.Context, models runtimesession.ModelBinding, policy runtimesession.RealtimeWorkPolicy) runtimesession.TurnObserverFactory {
	snapshot := copyRealtimeContext(c)
	if policy == nil {
		policy = NewRealtimeWorkPolicy(snapshot, models, nil)
	}
	return func() runtimesession.TurnObserver {
		return &RealtimeTurnObserver{requestContext: copyRealtimeContext(snapshot), models: models, policy: policy}
	}
}
