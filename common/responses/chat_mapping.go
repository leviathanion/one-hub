package responses

import (
	"net/http"
	"strings"

	"one-api/common"
	"one-api/types"
)

// ChatTerminalError rejects Responses terminal states that cannot be truthfully
// represented as a successful Chat completion.
func ChatTerminalError(response *types.OpenAIResponsesResponses) *types.OpenAIErrorWithStatusCode {
	if response == nil {
		return common.StringErrorWrapper("provider Responses terminal is missing", "invalid_provider_response", http.StatusBadGateway)
	}
	status := strings.ToLower(strings.TrimSpace(response.Status))
	if status == types.ResponseStatusCompleted {
		return nil
	}
	if status == types.ResponseStatusIncomplete && response.IncompleteDetail != nil && strings.EqualFold(strings.TrimSpace(response.IncompleteDetail.Reason), "max_output_tokens") {
		return nil
	}
	if status != types.ResponseStatusFailed && status != types.ResponseStatusIncomplete {
		return nil
	}
	apiErr := common.StringErrorWrapper("provider response failed", "upstream_error", http.StatusBadGateway)
	if response.Error != nil {
		apiErr.OpenAIError = *response.Error
		if strings.TrimSpace(apiErr.Message) == "" {
			apiErr.Message = "provider response failed"
		}
	}
	if status == types.ResponseStatusIncomplete && response.IncompleteDetail != nil && strings.TrimSpace(response.IncompleteDetail.Reason) != "" {
		apiErr.Code = "response_incomplete"
		apiErr.Param = response.IncompleteDetail.Reason
	}
	apiErr.UpstreamAccepted = true
	return apiErr
}
