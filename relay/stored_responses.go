package relay

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"one-api/common"
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

const storedResponsesRequestTimeout = 10 * time.Minute

func StoredResponses(c *gin.Context) {
	responseID := c.Param("response_id")
	operation, ok := storedResponsesOperation(c.Request.Method, c.Request.URL.Path, c.Param("response_id"))
	if !ok || strings.TrimSpace(responseID) == "" {
		renderStoredResponsesError(c, storedResponsesNotFoundError())
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
		if owner == nil || owner.UserID != c.GetInt("id") {
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

	body, bodyErr := common.CacheRequestBody(c)
	if bodyErr != nil {
		renderStoredResponsesError(c, common.ErrorWrapperLocal(bodyErr, "invalid_request_body", http.StatusBadRequest))
		return
	}
	requestCtx, cancelRequest := context.WithTimeout(c.Request.Context(), storedResponsesRequestTimeout)
	defer cancelRequest()
	response, apiErr := storedProvider.RelayStoredResponse(requestCtx, providersBase.StoredResponsesRequest{
		Body:       body,
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
	if apiErr := responseStoredLifecycleClient(c, response, owner, providerResponsePolicyForChannel(channel, operation, true)); apiErr != nil {
		observeRelayProviderFailure(c, channel, apiErr)
		renderStoredResponsesError(c, apiErr)
		return
	}
	metrics.RecordProvider(c, response.StatusCode)
	recordZeroQuotaResponsesAudit(c, channelID, "responses lifecycle:"+string(operation))
}

// Match the operation after the entire opaque ID. An ID named input_items
// must not accidentally select the child operation.
func storedResponsesOperation(method, path, responseID string) (providersBase.Operation, bool) {
	target := "/responses/" + responseID
	if method == http.MethodPost && strings.HasSuffix(path, target+"/cancel") {
		return providersBase.OperationResponsesCancel, true
	}
	if method == http.MethodGet && strings.HasSuffix(path, target+"/input_items") {
		return providersBase.OperationResponsesInputItems, true
	}
	if strings.HasSuffix(path, target) {
		if method == http.MethodGet {
			return providersBase.OperationResponsesRetrieve, true
		}
		if method == http.MethodDelete {
			return providersBase.OperationResponsesDelete, true
		}
	}
	return "", false
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
	operation, _ := storedResponsesOperation(c.Request.Method, c.Request.URL.Path, c.Param("response_id"))
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
