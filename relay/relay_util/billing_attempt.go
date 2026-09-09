package relay_util

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"one-api/common"
	"one-api/internal/billing"
	"one-api/model"
	"one-api/types"

	"github.com/gin-gonic/gin"
)

func BillingAPIError(err error, fallbackCode string, fallbackStatus int) *types.OpenAIErrorWithStatusCode {
	if err == nil {
		return nil
	}
	var apiErr *types.OpenAIErrorWithStatusCode
	if errors.As(err, &apiErr) && apiErr != nil {
		return apiErr
	}
	return common.ErrorWrapperLocal(err, fallbackCode, fallbackStatus)
}

// BillingAttemptSpec contains lifecycle facts only. Pricing and pre-consume
// math remain owned by Quota so there is no second billing calculator.
type BillingAttemptSpec struct {
	ChannelID   int
	RequestKind billing.SettlementRequestKind
	LogProtocol string
	StartedAt   time.Time
}

type AttemptResult struct {
	Confirmed    bool
	ChargedQuota int64
	Unsettled    bool
}

const billingSettlementDeadline = 5 * time.Second
const billingAdmissionDeadline = 2 * time.Second

func BoundedBillingAdmissionContext(parent context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	return context.WithTimeout(parent, billingAdmissionDeadline)
}

// AttemptQuota adds one submission claim and one Confirm/Cancel claim around
// the established Quota implementation. It is request-local; durable async
// ownership is provided by the Task aggregate.
type AttemptQuota struct {
	mu sync.Mutex

	quota             *Quota
	requestKind       billing.SettlementRequestKind
	reserveApplied    bool
	submissionClaimed bool
	finalized         bool
	result            AttemptResult
	finalErr          error
}

func NewAttemptQuota(c *gin.Context, billingModel string, promptTokens int64, spec BillingAttemptSpec) (*AttemptQuota, error) {
	if c == nil {
		return nil, errors.New("billing request context is required")
	}
	billingModel = strings.TrimSpace(billingModel)
	if billingModel == "" {
		return nil, errors.New("billing model is required")
	}
	if promptTokens < 0 || int64(int(promptTokens)) != promptTokens {
		return nil, errors.New("billing prompt token count is invalid")
	}
	quota, err := NewPricedQuota(c, billingModel, int(promptTokens))
	if err != nil {
		return nil, err
	}
	return newAttemptQuotaFromQuota(quota, spec), nil
}

// NewObservationAttemptQuota records provider-initiated work that is already
// observable. It never grants authorization for additional provider work.
func NewObservationAttemptQuota(c *gin.Context, billingModel string, spec BillingAttemptSpec) (*AttemptQuota, error) {
	if c == nil {
		return nil, errors.New("billing request context is required")
	}
	billingModel = strings.TrimSpace(billingModel)
	if billingModel == "" {
		return nil, errors.New("billing model is required")
	}
	quota := NewQuota(c, billingModel, 0)
	return newAttemptQuotaFromQuota(quota, spec), nil
}

func newAttemptQuotaFromQuota(quota *Quota, spec BillingAttemptSpec) *AttemptQuota {
	if !spec.StartedAt.IsZero() {
		quota.startTime = spec.StartedAt
	}
	if spec.ChannelID > 0 {
		quota.channelId = spec.ChannelID
	}
	if spec.LogProtocol != "" {
		quota.SetLogProtocol(spec.LogProtocol)
	}
	requestKind := spec.RequestKind
	if requestKind == "" {
		requestKind = billing.SettlementRequestKindUnary
	}
	return &AttemptQuota{quota: quota, requestKind: requestKind}
}

// NewPricedQuota rejects unsupported models before provider work. Price
// updates may be observed independently by later billing stages.
func NewPricedQuota(c *gin.Context, billingModel string, promptTokens int) (*Quota, error) {
	if c == nil {
		return nil, errors.New("billing request context is required")
	}
	billingModel = strings.TrimSpace(billingModel)
	if billingModel == "" || promptTokens < 0 {
		return nil, errors.New("billing model and non-negative prompt tokens are required")
	}
	if model.PricingInstance == nil {
		return nil, errors.New("prices are not initialized")
	}
	price, ok := model.PricingInstance.FindPrice(billingModel)
	if !ok {
		return nil, fmt.Errorf("no price policy matches model %q", billingModel)
	}
	quota := NewQuota(c, billingModel, promptTokens)
	quota.price = cloneQuotaPrice(*price)
	quota.inputRatio = quota.price.GetInput() * quota.groupRatio
	quota.outputRatio = quota.price.GetOutput() * quota.groupRatio
	return quota, nil
}

func (q *AttemptQuota) Quota() *Quota {
	if q == nil {
		return nil
	}
	return q.quota
}

func (q *AttemptQuota) ModelName() string {
	if q == nil || q.quota == nil {
		return ""
	}
	return q.quota.ModelName()
}

func (q *AttemptQuota) ReservedQuota() int64 {
	if q == nil || q.quota == nil {
		return 0
	}
	return int64(q.quota.PreConsumedQuota())
}

func (q *AttemptQuota) ApplyReserve(ctx context.Context) error {
	if q == nil || q.quota == nil {
		return errors.New("billing attempt is required")
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.reserveApplied {
		return nil
	}
	if q.finalized {
		return errors.New("billing attempt is already finalized")
	}
	admissionCtx, cancelAdmission := BoundedBillingAdmissionContext(normalizeContext(ctx))
	defer cancelAdmission()
	if err := q.quota.preQuotaConsumptionWithContext(admissionCtx); err != nil {
		return err
	}
	q.reserveApplied = true
	return nil
}

func (q *AttemptQuota) ClaimSubmission() error {
	if q == nil || q.quota == nil {
		return errors.New("billing attempt is required")
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if !q.reserveApplied || q.finalized {
		return errors.New("billing attempt is not active")
	}
	if q.submissionClaimed {
		return errors.New("billing submission was already claimed")
	}
	q.submissionClaimed = true
	return nil
}

// ClaimObservedSubmission records provider work that was first observable only
// after it had started. No reservation is synthesized; final settlement charges
// against a zero pre-consume baseline.
func (q *AttemptQuota) ClaimObservedSubmission() error {
	if q == nil || q.quota == nil {
		return errors.New("billing attempt is required")
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.finalized {
		return errors.New("billing attempt is already finalized")
	}
	if q.reserveApplied {
		return errors.New("observed submission cannot reuse a reserved attempt")
	}
	if q.submissionClaimed {
		return errors.New("billing submission was already claimed")
	}
	q.submissionClaimed = true
	return nil
}

func (q *AttemptQuota) SubmissionClaimed() bool {
	if q == nil {
		return false
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.submissionClaimed
}

func (q *AttemptQuota) CloseWithoutSubmission(ctx context.Context) (AttemptResult, error) {
	return q.close(ctx, nil, false, 0, false)
}

func (q *AttemptQuota) CloseFromProviderResult(ctx context.Context, usage *types.Usage, isStream bool) (AttemptResult, error) {
	if q == nil || q.quota == nil {
		return AttemptResult{}, errors.New("billing attempt is required")
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.finalized {
		return q.result, q.finalErr
	}
	decision := q.quota.EvaluateProviderUsage(usage)
	for _, diagnostic := range componentDiagnostics(decision.Components) {
		q.quota.addBillingDiagnostic(diagnostic)
	}
	return q.closeLocked(ctx, usage, decision.Confirm, decision.FinalQuota, isStream)
}

func (q *AttemptQuota) close(ctx context.Context, usage *types.Usage, confirm bool, chargedQuota int64, isStream bool) (AttemptResult, error) {
	if q == nil || q.quota == nil {
		return AttemptResult{}, errors.New("billing attempt is required")
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.closeLocked(ctx, usage, confirm, chargedQuota, isStream)
}

func (q *AttemptQuota) closeLocked(ctx context.Context, usage *types.Usage, confirm bool, chargedQuota int64, isStream bool) (AttemptResult, error) {
	if q.finalized {
		return q.result, q.finalErr
	}
	q.finalized = true
	operationCtx, cancel := context.WithTimeout(context.WithoutCancel(normalizeContext(ctx)), billingSettlementDeadline)
	defer cancel()
	if !confirm {
		q.finalErr = q.quota.undoSynchronouslyWithContext(operationCtx)
		q.result.Unsettled = q.finalErr != nil
		return q.result, q.finalErr
	}

	_, q.finalErr = q.quota.consumeFinalQuota(
		operationCtx,
		chargedQuota,
		usage,
		isStream,
		q.requestKind,
	)
	q.result = AttemptResult{Confirmed: true, ChargedQuota: chargedQuota}
	q.result.Unsettled = q.finalErr != nil
	return q.result, q.finalErr
}

func normalizeContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}
