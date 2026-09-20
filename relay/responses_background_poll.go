package relay

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"time"

	"one-api/common/logger"
	"one-api/model"
	providersBase "one-api/providers/base"
	"one-api/types"
)

const backgroundTrackingWindow = 24 * time.Hour
const backgroundPollBodyLimit = 16 << 20

func backgroundResponsesURL() *url.URL { return &url.URL{Path: "/v1/responses"} }

type BackgroundResponsesProgressor struct{}

func (*BackgroundResponsesProgressor) UpdateTaskStatus(ctx context.Context, _ map[int][]string, tasks map[string]*model.Task) error {
	for _, task := range tasks {
		pollCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := pollBackgroundResponse(pollCtx, task)
		cancel()
		if err != nil && !errors.Is(err, model.ErrTaskBillingState) {
			logger.LogError(ctx, "background response poll: "+err.Error())
		}
	}
	return nil
}
func pollBackgroundResponse(ctx context.Context, task *model.Task) error {
	if task == nil || task.ProviderState != model.TaskProviderStateAccepted {
		return nil
	}
	data := backgroundTaskData(task)
	acceptedAt := task.SubmitTime
	if task.AcceptanceRecordedAt != nil {
		acceptedAt = *task.AcceptanceRecordedAt
	}
	if time.Now().Unix()-acceptedAt >= int64(backgroundTrackingWindow/time.Second) {
		task.Status = model.TaskStatusUnknown
		task.FailReason = "local background tracking window expired"
		return finalizeBackgroundTask(ctx, task, data)
	}
	c := backgroundTaskContext(ctx, task, data)
	channel, err := fetchOwnerChannelById(ctx, task.ChannelId)
	if err != nil {
		return rescheduleBackgroundFailure(ctx, task, data)
	}
	provider, _, err := prepareProviderForChannel(c, "", channel)
	if err != nil {
		return rescheduleBackgroundFailure(ctx, task, data)
	}
	stored, ok := provider.(providersBase.StoredResponsesInterface)
	if !ok {
		return rescheduleBackgroundFailure(ctx, task, data)
	}
	response, apiErr := stored.RelayStoredResponse(ctx, providersBase.StoredResponsesRequest{Operation: providersBase.OperationResponsesRetrieve, ResponseID: model.TaskProviderID(task)})
	if apiErr != nil {
		if apiErr.StatusCode == http.StatusNotFound || apiErr.StatusCode == http.StatusGone {
			task.Status = model.TaskStatusUnknown
			task.FailReason = "upstream response is no longer retrievable"
			return finalizeBackgroundTask(ctx, task, data)
		}
		return rescheduleBackgroundFailure(ctx, task, data)
	}
	if response == nil {
		return rescheduleBackgroundFailure(ctx, task, data)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, backgroundPollBodyLimit+1))
	if err != nil || len(raw) > backgroundPollBodyLimit {
		return rescheduleBackgroundFailure(ctx, task, data)
	}
	if response.StatusCode == http.StatusNotFound || response.StatusCode == http.StatusGone {
		task.Status = model.TaskStatusUnknown
		task.FailReason = "upstream response is no longer retrievable"
		return finalizeBackgroundTask(ctx, task, data)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return rescheduleBackgroundFailure(ctx, task, data)
	}
	var observed types.OpenAIResponsesResponses
	if err = observed.DecodeCapturedProviderJSON(raw); err != nil || observed.ID != model.TaskProviderID(task) {
		return rescheduleBackgroundFailure(ctx, task, data)
	}
	observeBackgroundTask(ctx, task, &observed, nil)
	return nil
}
func rescheduleBackgroundFailure(ctx context.Context, task *model.Task, data backgroundResponseData) error {
	data.Failures++
	if data.Failures > 3 {
		data.Failures = 3
	}
	delay := time.Duration(1<<uint(data.Failures-1)) * model.TaskPollInterval
	encoded, err := encodeBackgroundTaskData(data)
	if err != nil {
		return err
	}
	task.Data = encoded
	_, err = model.SaveBackgroundResponseEvidence(ctx, task, time.Now().Add(delay))
	return err
}
