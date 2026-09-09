package relay

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"one-api/common/logger"
	"one-api/common/providerresponse"
	"one-api/common/requestctx"
	"one-api/common/utils"
	"one-api/metrics"
	"one-api/middleware"
	"one-api/model"
	providersBase "one-api/providers/base"
	"one-api/types"

	"github.com/gin-gonic/gin"
)

func StoredResponses(c *gin.Context) {
	responseID := strings.TrimSpace(c.Param("response_id"))
	operation, ok := storedResponsesOperation(c.Request.Method, c.Request.URL.Path)
	if !ok || responseID == "" {
		renderStoredResponsesError(c, storedResponsesNotFoundError())
		return
	}
	if err := validateStoredResponsesQuery(operation, c.Request.URL.Query()); err != nil {
		renderStoredResponsesError(c, capabilityGateAPIError(err))
		return
	}

	if apiErr := middleware.ValidateCurrentPrincipal(c); apiErr != nil {
		renderStoredResponsesError(c, apiErr)
		return
	}

	operationCtx, cancel := boundedResponsesLifecycleContext(c.Request.Context())
	defer cancel()
	owner, err := model.GetResponseOwner(operationCtx, responseID, c.GetInt("id"))
	channelID := 0
	if err == nil {
		if owner == nil || owner.UserID != c.GetInt("id") || owner.State != model.ResponseOwnerStateActive {
			renderStoredResponsesError(c, storedResponsesNotFoundError())
			return
		}
		channelID = owner.ChannelID
	} else if errors.Is(err, model.ErrResponseOwnerNotFound) && explicitChannelPinID(c) > 0 {
		channelID = explicitChannelPinID(c)
	} else if errors.Is(err, model.ErrResponseOwnerNotFound) {
		renderStoredResponsesError(c, storedResponsesNotFoundError())
		return
	} else {
		renderStoredResponsesError(c, storedResponsesUnavailableError())
		return
	}

	channel, err := fetchOwnerChannelById(operationCtx, channelID)
	if err != nil {
		renderStoredResponsesError(c, storedResponsesUnavailableError())
		return
	}
	if err := requireAdapterOperationSupport(operation, true)(channel); err != nil {
		renderStoredResponsesError(c, capabilityGateAPIError(err))
		return
	}
	provider, _, err := prepareProviderForChannel(c, "", channel)
	if err != nil {
		renderStoredResponsesError(c, storedResponsesUnavailableError())
		return
	}
	storedProvider, ok := provider.(providersBase.StoredResponsesInterface)
	if !ok {
		renderStoredResponsesError(c, storedResponsesUnavailableError())
		return
	}

	response, apiErr := storedProvider.RelayStoredResponse(operationCtx, providersBase.StoredResponsesRequest{
		Operation:  operation,
		ResponseID: responseID,
		RawQuery:   c.Request.URL.RawQuery,
		Headers:    requestctx.NewHeaderSnapshot(c.Request.Header),
	})
	if apiErr != nil {
		observeRelayProviderFailure(c, channel, apiErr)
		renderStoredResponsesError(c, apiErr)
		return
	}
	if response == nil {
		renderStoredResponsesError(c, storedResponsesUnavailableError())
		return
	}
	if operation == providersBase.OperationResponsesDelete && response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusMultipleChoices && owner != nil {
		tombstoneCtx, cancelTombstone := boundedResponsesLifecycleContext(context.WithoutCancel(c.Request.Context()))
		err := model.TombstoneResponseOwner(tombstoneCtx, responseID, c.GetInt("id"))
		if err != nil {
			// The provider deletion is already committed. Returning a local 503
			// would falsely invite a retry even though the resource no longer
			// exists; retain the provider's success and surface the stale local
			// tombstone as an operator-visible consistency error.
			logger.LogError(tombstoneCtx, fmt.Sprintf("failed to tombstone deleted stored response %s: %v", responseID, err))
		}
		cancelTombstone()
	}
	if apiErr := responseMultipart(c, response, providerResponsePolicyForChannel(channel, operation, true)); apiErr != nil {
		observeRelayProviderFailure(c, channel, apiErr)
		renderStoredResponsesError(c, apiErr)
		return
	}
	metrics.RecordProvider(c, response.StatusCode)
	recordZeroQuotaResponsesAudit(c, channelID, "responses lifecycle:"+string(operation))
}

func storedResponsesOperation(method, path string) (providersBase.Operation, bool) {
	if method == http.MethodGet && strings.HasSuffix(path, "/input_items") {
		return providersBase.OperationResponsesInputItems, true
	}
	if method == http.MethodGet {
		return providersBase.OperationResponsesRetrieve, true
	}
	if method == http.MethodDelete {
		return providersBase.OperationResponsesDelete, true
	}
	return "", false
}

func validateStoredResponsesQuery(operation providersBase.Operation, query url.Values) error {
	if operation != providersBase.OperationResponsesRetrieve {
		return nil
	}
	for _, value := range query["stream"] {
		if !strings.EqualFold(strings.TrimSpace(value), "false") {
			return newCapabilityGateError("stream", "streaming stored response retrieval is not supported")
		}
	}
	if query.Has("starting_after") {
		return newCapabilityGateError("starting_after", "stored response stream resumption is not supported")
	}
	return nil
}

func storedResponsesNotFoundError() *types.OpenAIErrorWithStatusCode {
	return &types.OpenAIErrorWithStatusCode{
		OpenAIError: types.OpenAIError{Message: "response not found", Type: "invalid_request_error", Code: "response_not_found"},
		StatusCode:  http.StatusNotFound,
		LocalError:  true,
	}
}

func storedResponsesUnavailableError() *types.OpenAIErrorWithStatusCode {
	return &types.OpenAIErrorWithStatusCode{
		OpenAIError: types.OpenAIError{Message: "stored response routing is temporarily unavailable", Type: "server_error", Code: "responses_owner_store_unavailable"},
		StatusCode:  http.StatusServiceUnavailable,
		LocalError:  true,
	}
}

func renderStoredResponsesError(c *gin.Context, apiErr *types.OpenAIErrorWithStatusCode) {
	if apiErr == nil {
		apiErr = storedResponsesUnavailableError()
	}
	operation, _ := storedResponsesOperation(c.Request.Method, c.Request.URL.Path)
	if replayProviderRawResponse(c, apiErr, providerresponse.Policy{
		Operation:        operation,
		DataPath:         providerresponse.DataPathExactWire,
		BodyUnmodified:   true,
		PreserveRedirect: true,
	}) {
		return
	}
	relay := &relayBase{c: c}
	relay.HandleJsonError(apiErr)
}

func recordZeroQuotaResponsesAudit(c *gin.Context, channelID int, content string) {
	if c == nil {
		return
	}
	requestTime := 0
	if startedAt := c.GetTime("requestStartTime"); !startedAt.IsZero() {
		requestTime = int(time.Since(startedAt).Milliseconds())
	}
	metadata := utils.AppendUserAgentMetadata(map[string]any{"quota_free": true}, c.Request.UserAgent())
	model.RecordConsumeLog(c.Request.Context(), c.GetInt("id"), channelID, 0, 0, 0, 0, 0, "", c.GetString("token_name"), 0, content, requestTime, false, metadata, c.ClientIP())
}
