package relay

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"one-api/common"
	"one-api/common/groupctx"
	"one-api/common/logger"
	commonresponses "one-api/common/responses"
	"one-api/model"
	"one-api/providers"
	providersBase "one-api/providers/base"
	"one-api/providers/openai"
	"one-api/relay/relay_util"
	"one-api/types"
)

const backgroundEvidenceLimit = 64 << 10

// Only accounting evidence is durable. Neither generated output nor a price
// catalogue is stored in the Task. The group identifies the current policy.
type backgroundResponseData struct {
	Model                string                   `json:"model"`
	Group                string                   `json:"group"`
	Store                bool                     `json:"store"`
	BillingOriginalModel bool                     `json:"billing_original_model,omitempty"`
	Failures             int                      `json:"poll_failures,omitempty"`
	Evidence             *backgroundUsageEvidence `json:"evidence,omitempty"`
}

type backgroundUsageEvidence struct {
	Usage               types.Usage                   `json:"usage"`
	ResponseID          string                        `json:"response_id"`
	Model               string                        `json:"response_model,omitempty"`
	Tier                string                        `json:"service_tier,omitempty"`
	Speed               string                        `json:"speed,omitempty"`
	Reported            bool                          `json:"reported"`
	Fields              map[string]bool               `json:"fields,omitempty"`
	ExtraTokens         map[string]int                `json:"extra_tokens,omitempty"`
	ExtraUnits          map[string]float64            `json:"extra_units,omitempty"`
	ExtraBilling        map[string]types.ExtraBilling `json:"extra_billing,omitempty"`
	ProviderExtra       map[string]bool               `json:"provider_extra,omitempty"`
	Diagnostics         map[string]bool               `json:"diagnostics,omitempty"`
	Required            []string                      `json:"required,omitempty"`
	Partitions          [][]string                    `json:"partitions,omitempty"`
	AttributionConflict bool                          `json:"attribution_conflict,omitempty"`
	TokenConflict       bool                          `json:"token_conflict,omitempty"`
	SpeedConflict       bool                          `json:"speed_conflict,omitempty"`
	Independent         map[string]bool               `json:"independent,omitempty"`
	OperationUnits      *int                          `json:"operation_units,omitempty"`
}

func captureBackgroundUsage(id string, u *types.Usage) *backgroundUsageEvidence {
	if u == nil {
		return nil
	}
	return &backgroundUsageEvidence{Usage: *u, ResponseID: id, Model: u.ResponseModel, Tier: u.ServiceTier, Speed: u.Speed, Reported: u.ProviderReported, Fields: u.ProviderTokenFields, ExtraTokens: u.ExtraTokens, ExtraUnits: u.ExtraUsageUnits, ExtraBilling: u.ExtraBilling, ProviderExtra: u.ProviderExtraBilling, Diagnostics: u.BillingDiagnostics, Required: u.RequiredTokenExtraKeys, Partitions: u.TokenExtraEvidenceGroups, AttributionConflict: u.AttributionConflict, TokenConflict: u.ProviderTokenConflict, SpeedConflict: u.SpeedConflict, Independent: u.ProviderIndependentUsageUnits, OperationUnits: u.ProviderOperationUnits}
}
func (e *backgroundUsageEvidence) restore() *types.Usage {
	if e == nil {
		return &types.Usage{}
	}
	u := e.Usage
	u.ResponseModel = e.Model
	u.ServiceTier = e.Tier
	u.Speed = e.Speed
	u.ProviderReported = e.Reported
	u.ProviderTokenFields = e.Fields
	u.ExtraTokens = e.ExtraTokens
	u.ExtraUsageUnits = e.ExtraUnits
	u.ExtraBilling = e.ExtraBilling
	u.ProviderExtraBilling = e.ProviderExtra
	u.BillingDiagnostics = e.Diagnostics
	u.RequiredTokenExtraKeys = e.Required
	u.TokenExtraEvidenceGroups = e.Partitions
	u.AttributionConflict = e.AttributionConflict
	u.ProviderTokenConflict = e.TokenConflict
	u.SpeedConflict = e.SpeedConflict
	u.ProviderIndependentUsageUnits = e.Independent
	u.ProviderOperationUnits = e.OperationUnits
	return &u
}
func backgroundTaskData(task *model.Task) backgroundResponseData {
	var data backgroundResponseData
	if task != nil {
		_ = json.Unmarshal(task.Data, &data)
	}
	return data
}
func encodeBackgroundTaskData(data backgroundResponseData) ([]byte, error) {
	raw, err := json.Marshal(data)
	if err == nil && len(raw) > backgroundEvidenceLimit {
		return nil, errors.New("background accounting evidence exceeds local capacity")
	}
	return raw, err
}
func (r *relayResponses) backgroundRequest() bool {
	return r != nil && r.operation == responsesOperationCreate && r.responsesRequest.Background != nil && *r.responsesRequest.Background
}

func (r *relayResponses) prepareBackgroundTask() (*model.Task, *types.OpenAIErrorWithStatusCode) {
	quota, err := relay_util.NewPricedQuota(r.c, r.getModelName(), 1000)
	if err != nil {
		return nil, common.ErrorWrapperLocal(err, "billing_admission_failed", http.StatusServiceUnavailable)
	}
	amount, err := quota.ReservationQuota()
	if err != nil {
		return nil, common.ErrorWrapperLocal(err, "billing_admission_failed", http.StatusServiceUnavailable)
	}
	channel := r.provider.GetChannel()
	namespace, scope, err := model.ConservativeResponseOwnerIdentity(channel)
	if err != nil {
		return nil, storedResponsesUnavailableError()
	}
	raw, _ := common.GetCanonicalRequestBody(r.c)
	fingerprint := sha256.Sum256(raw)
	// Only an explicit persistent-store request selects the ordinary owner retention.
	data := backgroundResponseData{Model: r.getModelName(), Group: groupctx.CurrentRoutingGroup(r.c), BillingOriginalModel: r.c.GetBool("billing_original_model"), Store: r.responsesRequest.Store != nil && *r.responsesRequest.Store}
	encoded, err := encodeBackgroundTaskData(data)
	if err != nil {
		return nil, storedResponsesUnavailableError()
	}
	task := &model.Task{Platform: model.TaskPlatformOpenAIResponsesBackground, UserId: r.c.GetInt("id"), TokenID: r.c.GetInt("token_id"), ChannelId: channel.Id, Status: model.TaskStatusSubmitted, SubmitTime: time.Now().Unix(), ProviderNamespace: namespace, ProviderTaskScopeIncarnation: scope, RequestFingerprint: fmt.Sprintf("%x", fingerprint[:]), ReservedQuota: int64(amount), Data: encoded}
	ctx, cancel := relay_util.BoundedBillingAdmissionContext(r.c.Request.Context())
	defer cancel()
	reserved, err := model.CreateTaskBillingOwner(ctx, task)
	if err != nil || reserved.Outcome != model.BillingBalanceCommitted {
		if errors.Is(err, model.ErrBackgroundResponseCapacity) {
			return nil, common.StringErrorWrapperLocal("background task capacity exhausted", "background_task_capacity", http.StatusTooManyRequests)
		}
		return nil, relay_util.BillingAPIError(err, "background_task_admission_failed", http.StatusServiceUnavailable)
	}
	claim, err := model.ClaimTaskSubmission(ctx, task, uuid.NewString())
	if err != nil || claim.Outcome != model.TaskMutationApplied {
		if claim.Outcome == model.TaskMutationDefinitelyNotApplied {
			closeBackgroundWithoutHandle(ctx, task, "submission claim failed")
		}
		return nil, common.StringErrorWrapperLocal("background submission claim failed", "background_submission_claim_failed", http.StatusServiceUnavailable)
	}
	return task, nil
}

func closeBackgroundWithoutHandle(parent context.Context, task *model.Task, reason string) {
	ctx, cancel := boundedResponsesLifecycleContext(context.WithoutCancel(parent))
	defer cancel()
	task.Status = model.TaskStatusUnknown
	task.FailReason = reason
	if task.ProviderState == model.TaskProviderStatePrepared {
		task.Status = model.TaskStatusLocalFailure
	}
	result, err := model.FinalizeTaskBillingOwner(ctx, task, 0, "cancel")
	if err == nil {
		ProjectAsyncTaskSettlement(ctx, task, result)
	}
	if err != nil {
		logger.LogError(ctx, "background task close failed: "+err.Error())
	}
}
func acceptBackgroundResponse(parent context.Context, task *model.Task, id string) error {
	if task.ProviderState == model.TaskProviderStateAccepted || task.ProviderState == model.TaskProviderStateClosed {
		if model.TaskProviderID(task) != id {
			return model.ErrResponseOwnerConflict
		}
		return nil
	}
	data := backgroundTaskData(task)
	owner, err := model.NewBackgroundResponseOwner(id, task.UserId, task.TokenID, task.ChannelId, time.Now(), data.Store, task.ProviderNamespace, task.ProviderTaskScopeIncarnation)
	if err != nil {
		return err
	}
	ctx, cancel := boundedResponsesLifecycleContext(context.WithoutCancel(parent))
	defer cancel()
	result, err := model.AcceptBackgroundResponseSubmission(ctx, task, id, owner)
	if err != nil {
		logger.LogError(ctx, fmt.Sprintf("background response ownership could not be committed: task=%s response=%s: %v", task.OwnerID, id, err))
		return err
	}
	if result.Outcome != model.TaskMutationApplied {
		return model.ErrTaskBillingState
	}
	return nil
}

func (r *relayResponses) sendBackgroundResponse() (*types.OpenAIErrorWithStatusCode, bool) {
	provider, ok := r.provider.(providersBase.ResponsesInterface)
	_, hasLifecycle := r.provider.(providersBase.StoredResponsesInterface)
	if !ok || !hasLifecycle || !providers.ResolveAdapterSupport(r.provider.GetChannel()).SupportsStoredResponses() || r.selectedDataPath == providersBase.DataPathCrossProtocol {
		return common.StringErrorWrapperLocal("background responses require a native adapter", "channel_error", http.StatusServiceUnavailable), true
	}
	task, apiErr := r.prepareBackgroundTask()
	if apiErr != nil {
		return apiErr, true
	}
	// Once claimed this POST is never retried, including when no response ID is
	// observed. A client disconnect does not cancel an accepted durable Task.
	defer func() {
		if task.ProviderState == model.TaskProviderStateSubmitStarted {
			closeBackgroundWithoutHandle(r.c.Request.Context(), task, "submission outcome has no recoverable response id")
		}
	}()
	sink := newBackgroundObservationSink(r.c.Request.Context(), task.OwnerID)
	r.c.Set(responsesBackgroundObserverDoneContextKey, sink.done)
	defer sink.Close()
	usage := &types.Usage{}
	r.provider.SetUsage(usage)
	r.responsesRequest.Model = r.modelName
	if r.responsesRequest.Stream {
		ioOwner, err := newResponsesHTTPIO(r.c)
		if err != nil {
			return common.ErrorWrapperLocal(err, "response_write_deadline_unsupported", http.StatusInternalServerError), true
		}
		defer ioOwner.Close()
		r.c.Set(responsesHTTPIOContextKey, ioOwner)
		stream, apiErr := provider.CreateResponsesStream(ioOwner.ctx, r.providerRequest(commonresponses.ResponsesCreate))
		if apiErr != nil {
			return apiErr, true
		}
		wrapped := commonresponses.NewEventStream(stream, func(event string) error {
			payload, ok := commonresponses.SSEDataPayload(event)
			if !ok {
				return nil
			}
			observed, err := commonresponses.ObserveEventLifecycle([]byte(payload))
			if err != nil || !commonresponses.IsResponseLifecycleEvent(observed.Type) || observed.Response == nil {
				_ = stream.ObserveResponsesEvent(event)
				return nil
			}
			if observed.Response.ID != "" {
				if err := acceptBackgroundResponse(r.c.Request.Context(), task, observed.Response.ID); err != nil {
					return err
				}
			}
			_ = stream.ObserveResponsesEvent(event)
			if task.ProviderState == model.TaskProviderStateAccepted {
				sink.Submit(observed.Response, usage)
			}
			return nil
		})
		first, apiErr := responseNativeResponsesStreamClient(r.c, wrapped, commonresponses.NewStreamObserver())
		r.SetFirstResponseTime(first)
		return apiErr, true
	}
	submitCtx, cancelSubmit := context.WithTimeout(r.c.Request.Context(), 10*time.Minute)
	defer cancelSubmit()
	response, apiErr := provider.CreateResponses(submitCtx, r.providerRequest(commonresponses.ResponsesCreate))
	if apiErr != nil {
		return apiErr, true
	}
	if response == nil || response.ID == "" {
		return common.StringErrorWrapperLocal("background response did not establish a resource id", "invalid_provider_response", http.StatusBadGateway), true
	}
	if err := acceptBackgroundResponse(r.c.Request.Context(), task, response.ID); err != nil {
		apiErr := storedResponsesUnavailableError()
		apiErr.UpstreamAccepted = true
		return apiErr, true
	}
	sink.Submit(response, usage)
	return responseJsonClient(r.c, response), true
}

func backgroundTerminal(status string) bool {
	switch status {
	case "completed", "failed", "incomplete", "cancelled":
		return true
	}
	return false
}

// All observers use the same durable Task and its version fence. Stale reads
// cannot overwrite a terminal decision. Observation failures never stop wire
// delivery; the durable poller retries reads of the same handle only.
func observeBackgroundTask(parent context.Context, task *model.Task, response *types.OpenAIResponsesResponses, provided *types.Usage) {
	if task == nil || task.ProviderState != model.TaskProviderStateAccepted || response == nil || response.ID != model.TaskProviderID(task) {
		return
	}
	// A poll may have advanced the fence since an earlier frame on this
	// connection. Refresh only live delivery observers; poll results keep the
	// version they claimed before their read.
	if provided != nil {
		ctx, cancel := boundedResponsesLifecycleContext(context.WithoutCancel(parent))
		current, err := model.GetBackgroundResponseTask(ctx, task.OwnerID)
		cancel()
		if err != nil || current.ProviderState != model.TaskProviderStateAccepted {
			if err == nil {
				*task = *current
			}
			return
		}
		*task = *current
	}
	data := backgroundTaskData(task)
	usage := data.Evidence.restore()
	incoming := provided
	if incoming == nil {
		incoming = &types.Usage{}
		openai.ObserveStoredResponsesUsage(response, incoming)
	}
	usage = mergeBackgroundUsage(usage, incoming)
	data.Evidence = captureBackgroundUsage(response.ID, usage)
	data.Failures = 0
	encoded, err := encodeBackgroundTaskData(data)
	if err != nil {
		logger.LogWarn(parent, err.Error())
		return
	}
	task.Data = encoded
	ctx, cancel := boundedResponsesLifecycleContext(context.WithoutCancel(parent))
	defer cancel()
	if backgroundTerminal(response.Status) {
		switch response.Status {
		case "completed":
			task.Status = model.TaskStatusSuccess
		case "cancelled":
			task.Status = model.TaskStatusCancel
		default:
			task.Status = model.TaskStatusFailure
		}
		err = finalizeBackgroundTask(ctx, task, data)
	} else {
		_, err = model.SaveBackgroundResponseEvidence(ctx, task, time.Now().Add(model.TaskPollInterval))
	}
	if err != nil && !errors.Is(err, model.ErrTaskBillingState) {
		logger.LogError(ctx, "background evidence persistence failed: "+err.Error())
	}
}

func finalizeBackgroundTask(ctx context.Context, task *model.Task, data backgroundResponseData) error {
	c := backgroundTaskContext(ctx, task, data)
	quota, err := relay_util.NewPricedQuota(c, data.Model, 0)
	decision := relay_util.UsageSettlementDecision{}
	if err == nil {
		decision = quota.EvaluateProviderUsage(data.Evidence.restore())
	}
	action := "cancel"
	if decision.Confirm {
		action = "confirm"
	}
	encoded, err := encodeBackgroundTaskData(data)
	if err != nil {
		return err
	}
	result, err := model.FinalizeBackgroundResponseBillingOwner(ctx, task, decision.FinalQuota, action, encoded)
	if err == nil {
		ProjectAsyncTaskSettlement(ctx, task, result)
	}
	return err
}
func backgroundTaskContext(ctx context.Context, task *model.Task, data backgroundResponseData) *gin.Context {
	c, _ := gin.CreateTestContext(nil)
	c.Request = (&http.Request{Method: http.MethodGet, Header: make(http.Header), URL: backgroundResponsesURL()}).WithContext(ctx)
	c.Set("id", task.UserId)
	c.Set("token_id", task.TokenID)
	c.Set("channel_id", task.ChannelId)
	c.Set("group", data.Group)
	c.Set("billing_original_model", data.BillingOriginalModel)
	groupctx.SetRoutingGroup(c, data.Group, "task")
	c.Set("requestStartTime", time.Unix(task.SubmitTime, 0))
	return c
}

// Missing observations cannot erase earlier evidence. These are snapshots of
// the same work, so tool counters merge by maximum, never by summation.
func mergeBackgroundUsage(previous, incoming *types.Usage) *types.Usage {
	if previous == nil {
		previous = &types.Usage{}
	}
	if incoming == nil {
		return previous
	}
	chosen, other := incoming, previous
	if previous.HasProviderBaseUsage() && !incoming.HasProviderBaseUsage() {
		chosen, other = previous, incoming
	}
	merged := *chosen
	commonresponses.MergeResponsesExtraBillingMax(&merged, other.ExtraBilling)
	if merged.ProviderExtraBilling == nil {
		merged.ProviderExtraBilling = make(map[string]bool)
	}
	for key, present := range other.ProviderExtraBilling {
		if present {
			merged.ProviderExtraBilling[key] = true
		}
	}
	merged.MergeBillingDiagnostics(other.BillingDiagnostics)
	merged.MergeProviderAttribution(other.ResponseModel, other.ServiceTier)
	merged.MergeProviderSpeed(other.Speed, other.SpeedConflict)
	merged.AttributionConflict = merged.AttributionConflict || other.AttributionConflict
	merged.ProviderTokenConflict = merged.ProviderTokenConflict || other.ProviderTokenConflict
	return &merged
}
