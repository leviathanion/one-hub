package task

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"one-api/common"
	"one-api/common/config"
	"one-api/common/logger"
	"one-api/metrics"
	"one-api/model"
	"one-api/relay/relay_util"
	"one-api/relay/task/base"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

var getTaskAdaptorFunc = GetTaskAdaptor

func RelayTaskSubmit(c *gin.Context) {
	var taskErr *base.TaskError
	taskAdaptor, err := getTaskAdaptorFunc(GetRelayMode(c), c)
	if err != nil {
		taskErr = base.StringTaskError(http.StatusBadRequest, "adaptor_not_found", "adaptor not found", true)
		c.JSON(http.StatusBadRequest, taskErr)
		return
	}

	taskErr = taskAdaptor.Init()
	if taskErr != nil {
		taskAdaptor.HandleError(taskErr)
		return
	}

	taskErr = taskAdaptor.SetProvider()
	if taskErr != nil {
		taskAdaptor.HandleError(taskErr)
		return
	}

	taskErr = prepareTaskAttemptOwner(c, taskAdaptor)
	if taskErr != nil {
		taskAdaptor.HandleError(taskErr)
		return
	}

	submitCtx, cancelSubmit := context.WithTimeout(c.Request.Context(), asyncTaskSubmitDeadline)
	originalRequest := c.Request
	c.Request = c.Request.WithContext(submitCtx)
	taskErr = taskAdaptor.Relay()
	c.Request = originalRequest
	cancelSubmit()
	if taskErr == nil {
		if err = CompletedTask(c, taskAdaptor); err != nil {
			taskAdaptor.HandleError(base.StringTaskError(
				http.StatusInternalServerError,
				"task_persist_failed",
				"task accepted by provider but local persistence failed",
				true,
			))
			return
		}
		// 返回结果
		taskAdaptor.GinResponse()
		metrics.RecordProvider(c, 200)
		return
	}
	recordTaskProviderFailure(c, taskAdaptor, taskErr)
	if task := taskAdaptor.GetTask(); task != nil {
		switch {
		case taskErr.UpstreamNotAttempted:
			task.Status = model.TaskStatusLocalFailure
		case taskErr.ProviderRejected:
			task.Status = model.TaskStatusFailure
		default:
			task.Status = model.TaskStatusUnknown
		}
		ownerCtx, cancelOwner := context.WithTimeout(context.WithoutCancel(c.Request.Context()), taskOwnerMutationDeadline)
		if closeErr := base.FailTaskWithSettlement(ownerCtx, task, taskErr.Message); closeErr != nil {
			logger.LogError(c.Request.Context(), "close ambiguous task owner failed: "+closeErr.Error())
		}
		cancelOwner()
	}
	taskAdaptor.HandleError(taskErr)
}

func recordTaskProviderFailure(c *gin.Context, taskAdaptor base.TaskInterface, taskErr *base.TaskError) {
	if c == nil || taskAdaptor == nil || taskErr == nil || taskErr.LocalError {
		return
	}
	metrics.RecordProvider(c, taskErr.StatusCode)
	provider := taskAdaptor.GetProvider()
	if provider == nil || provider.GetChannel() == nil || taskErr.StatusCode != http.StatusTooManyRequests {
		return
	}
	channel := provider.GetChannel()
	model.ChannelGroup.SetCooldowns(channel.Id, taskAdaptor.GetModelName())
}

func prepareTaskAttemptOwner(c *gin.Context, taskAdaptor base.TaskInterface) *base.TaskError {
	task := taskAdaptor.GetTask()
	if task == nil {
		return base.StringTaskError(http.StatusInternalServerError, "task_prepare_failed", "task is nil", true)
	}
	if provider := taskAdaptor.GetProvider(); provider != nil && provider.GetChannel() != nil {
		task.ChannelId = provider.GetChannel().Id
	}
	if task.ChannelId <= 0 {
		return base.StringTaskError(http.StatusInternalServerError, "task_prepare_failed", "task channel_id is empty", true)
	}
	ownerChannelID := taskAdaptor.OwnerChannelIncarnationID()
	if ownerChannelID > 0 && task.ChannelId != ownerChannelID {
		return base.StringTaskError(http.StatusConflict, "task_owner_channel_conflict", "task provider does not match the parent owner channel", true)
	}
	if task.ID != 0 {
		return base.StringTaskError(http.StatusConflict, "task_prepare_failed", "task owner already exists", true)
	}
	model.SetTaskProviderID(task, "")
	task.Status = model.TaskStatusSubmitted
	task.Progress = 0
	task.FailReason = ""
	task.SubmitTime = time.Now().Unix()
	task.ProviderNamespace = fmt.Sprintf("task-platform:%s", task.Platform)
	task.ProviderTaskScopeIncarnation = "provider-wide"
	requestBody, _ := common.GetCanonicalRequestBody(c)
	fingerprint := sha256.Sum256(append([]byte(c.Request.URL.Path+"\x00"), requestBody...))
	task.RequestFingerprint = fmt.Sprintf("%x", fingerprint[:])
	var (
		reserve model.BillingBalanceResult
		err     error
	)
	quota, priceErr := relay_util.NewPricedQuota(c, taskAdaptor.GetModelName(), 1000)
	if priceErr != nil {
		return base.StringTaskError(http.StatusServiceUnavailable, "task_price_unavailable", priceErr.Error(), true)
	}
	reservationQuota, priceErr := quota.ReservationQuota()
	if priceErr != nil {
		return base.StringTaskError(http.StatusServiceUnavailable, "task_price_unavailable", priceErr.Error(), true)
	}
	task.ReservedQuota = int64(reservationQuota)
	admissionCtx, cancelAdmission := relay_util.BoundedBillingAdmissionContext(c.Request.Context())
	if ownerChannelID > 0 {
		reserve, err = model.CreateTaskBillingOwnerForBoundChannel(admissionCtx, task)
	} else {
		reserve, err = model.CreateTaskBillingOwner(admissionCtx, task)
	}
	cancelAdmission()
	if err != nil || reserve.Outcome != model.BillingBalanceCommitted {
		status, code := taskBillingAdmissionStatus(err, reserve.Outcome)
		return base.StringTaskError(status, code, fmt.Sprintf("task reserve failed: %v", err), true)
	}
	claimID := uuid.NewString()
	var claim model.TaskMutationResult
	for attempt := 0; attempt < 2; attempt++ {
		claim, err = model.ClaimTaskSubmission(c.Request.Context(), task, claimID)
		if claim.Outcome != model.TaskMutationDefinitelyNotApplied || err == nil {
			break
		}
	}
	if err != nil || claim.Outcome != model.TaskMutationApplied {
		if claim.Outcome == model.TaskMutationDefinitelyNotApplied {
			task.Status = model.TaskStatusLocalFailure
			ownerCtx, cancelOwner := context.WithTimeout(context.WithoutCancel(c.Request.Context()), taskOwnerMutationDeadline)
			_ = base.FailTaskWithSettlement(ownerCtx, task, "submission claim failed")
			cancelOwner()
		}
		return base.StringTaskError(http.StatusInternalServerError, "task_submission_claim_failed", fmt.Sprintf("task submission claim failed: %v", err), true)
	}
	return nil
}

func taskBillingAdmissionStatus(err error, outcome model.BillingBalanceOutcome) (int, string) {
	switch {
	case errors.Is(err, model.ErrBillingUserQuotaInsufficient):
		return http.StatusPaymentRequired, "insufficient_user_quota"
	case errors.Is(err, model.ErrTokenQuotaInsufficient):
		return http.StatusForbidden, "insufficient_token_quota"
	case errors.Is(err, model.ErrBillingUserUnavailable), errors.Is(err, model.ErrBillingTokenUnavailable), errors.Is(err, model.ErrBillingOwnership):
		return http.StatusForbidden, "billing_principal_unavailable"
	case outcome == model.BillingBalanceCommitUnknown:
		return http.StatusServiceUnavailable, "task_billing_commit_unknown"
	default:
		return http.StatusServiceUnavailable, "task_billing_admission_unavailable"
	}
}

func CompletedTask(c *gin.Context, taskAdaptor base.TaskInterface) error {
	task := taskAdaptor.GetTask()
	if task == nil {
		return errors.New("task is nil")
	}
	if task.ID == 0 {
		return errors.New("task local id is empty")
	}
	if task.ChannelId <= 0 {
		return errors.New("task channel_id is empty")
	}
	providerTaskID := model.TaskProviderID(task)
	ownerCtx, cancelOwner := context.WithTimeout(context.WithoutCancel(c.Request.Context()), taskOwnerMutationDeadline)
	defer cancelOwner()
	var result model.TaskMutationResult
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		result, err = model.AcceptTaskSubmission(ownerCtx, task, providerTaskID)
		if result.Outcome != model.TaskMutationDefinitelyNotApplied || err == nil {
			break
		}
	}
	if err != nil || result.Outcome != model.TaskMutationApplied {
		if result.Outcome == model.TaskMutationDefinitelyNotApplied {
			task.Status = model.TaskStatusUnknown
			if closeErr := base.FailTaskWithSettlement(ownerCtx, task, "provider acceptance persistence failed"); closeErr != nil {
				logger.LogError(ownerCtx, "close task after definite acceptance persistence failure: "+closeErr.Error())
			}
		} else if result.Outcome == model.TaskMutationCommitUnknown {
			if _, preserveErr := model.PreserveTaskSubmissionHandle(ownerCtx, task, providerTaskID); preserveErr != nil {
				logger.LogError(ownerCtx, "preserve task handle after acceptance commit-unknown: "+preserveErr.Error())
			}
		}
		return fmt.Errorf("persist provider acceptance: %w", err)
	}
	ActivateUpdateTaskBulk()
	return nil
}

func GetRelayMode(c *gin.Context) int {
	relayMode := config.RelayModeUnknown
	path := c.Request.URL.Path
	if strings.HasPrefix(path, "/suno") {
		relayMode = config.RelayModeSuno
	} else if strings.HasPrefix(path, "/kling") {
		relayMode = config.RelayModeKling
	}

	return relayMode
}
