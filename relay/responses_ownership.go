package relay

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"one-api/model"
	runtimeaffinity "one-api/runtime/channelaffinity"
	"one-api/types"

	"github.com/gin-gonic/gin"
)

const (
	responsesEphemeralProofTTL        = time.Hour
	responsesOwnerChannelIDContextKey = "responses_owner_channel_id"
)

// Lifecycle storage runs on websocket and HTTP request paths. Bound each
// operation independently so one stalled DB/Redis dependency cannot retain an
// actor or lifecycle request indefinitely.
var responsesLifecycleIOTimeout = 5 * time.Second

type responsesOwnershipError struct {
	apiErr *types.OpenAIErrorWithStatusCode
}

func (e *responsesOwnershipError) Error() string {
	if e == nil || e.apiErr == nil {
		return "responses ownership error"
	}
	return e.apiErr.Message
}

func newResponsesOwnershipError(status int, code, param, message string) error {
	return &responsesOwnershipError{apiErr: &types.OpenAIErrorWithStatusCode{
		OpenAIError: types.OpenAIError{
			Message: message,
			Type:    "invalid_request_error",
			Param:   param,
			Code:    code,
		},
		StatusCode: status,
		LocalError: true,
	}}
}

func responsesOwnershipAPIError(err error) *types.OpenAIErrorWithStatusCode {
	var ownershipErr *responsesOwnershipError
	if !errors.As(err, &ownershipErr) || ownershipErr == nil {
		return nil
	}
	return ownershipErr.apiErr
}

func responseOwnerRequestContext(c *gin.Context) context.Context {
	if c != nil && c.Request != nil {
		return c.Request.Context()
	}
	return context.Background()
}

func boundedResponsesLifecycleContext(parent context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	return context.WithTimeout(parent, responsesLifecycleIOTimeout)
}

func responseRequiresDurableOwner(request *types.OpenAIResponsesRequest) bool {
	return request != nil && (request.Store == nil || *request.Store)
}

type responsesContinuationRoute struct {
	Strict    bool
	ChannelID int
}

func prepareResponsesContinuationOwnership(c *gin.Context, request *types.OpenAIResponsesRequest) (responsesContinuationRoute, error) {
	if c == nil || request == nil {
		return responsesContinuationRoute{}, nil
	}
	responseID := strings.TrimSpace(request.PreviousResponseID)
	if responseID == "" {
		return responsesContinuationRoute{}, nil
	}
	userID := c.GetInt("id")
	operationCtx, cancel := boundedResponsesLifecycleContext(responseOwnerRequestContext(c))
	defer cancel()
	owner, err := model.GetResponseOwner(operationCtx, responseID, userID)
	if err == nil {
		if owner == nil || owner.State != model.ResponseOwnerStateActive || owner.UserID != userID {
			return responsesContinuationRoute{}, invalidPreviousResponseOwnerError()
		}
		if pin := explicitChannelPinID(c); pin > 0 && pin != owner.ChannelID {
			return responsesContinuationRoute{}, invalidPreviousResponseOwnerError()
		}
		c.Set("specific_channel_id", owner.ChannelID)
		c.Set("specific_channel_id_ignore", false)
		c.Set(responsesOwnerChannelIDContextKey, owner.ChannelID)
		return responsesContinuationRoute{Strict: true, ChannelID: owner.ChannelID}, nil
	}
	if !errors.Is(err, model.ErrResponseOwnerNotFound) {
		return responsesContinuationRoute{}, newResponsesOwnershipError(http.StatusServiceUnavailable, "responses_owner_store_unavailable", "previous_response_id", "stored response ownership is temporarily unavailable")
	}
	// The new request's store flag controls the new response, not whether the
	// previous response was ephemeral. A user-scoped proof therefore remains
	// valid when the continuation changes its own persistence choice.
	if channelID, ok := lookupResponsesEphemeralProof(c, responseID); ok {
		if pin := explicitChannelPinID(c); pin > 0 {
			if pin != channelID {
				return responsesContinuationRoute{}, invalidPreviousResponseOwnerError()
			}
			return responsesContinuationRoute{ChannelID: pin}, nil
		}
		setPreferredChannelFromAffinity(c, channelID)
		return responsesContinuationRoute{ChannelID: channelID}, nil
	}
	if pin := explicitChannelPinID(c); pin > 0 {
		return responsesContinuationRoute{Strict: responseRequiresDurableOwner(request), ChannelID: pin}, nil
	}
	return responsesContinuationRoute{}, invalidPreviousResponseOwnerError()
}

func responseOwnerChannelID(c *gin.Context) int {
	if c == nil {
		return 0
	}
	return c.GetInt(responsesOwnerChannelIDContextKey)
}

func invalidPreviousResponseOwnerError() error {
	return &responsesOwnershipError{apiErr: previousResponseNotFoundAPIError()}
}

func previousResponseNotFoundAPIError() *types.OpenAIErrorWithStatusCode {
	return &types.OpenAIErrorWithStatusCode{
		OpenAIError: types.OpenAIError{
			Message: "previous response was not found",
			Type:    "invalid_request_error",
			Param:   "previous_response_id",
			Code:    "previous_response_not_found",
		},
		StatusCode: http.StatusBadRequest,
		LocalError: true,
	}
}

func persistStoredResponseOwner(c *gin.Context, responseID string, channelID int) *types.OpenAIErrorWithStatusCode {
	providerNamespace, responseScope := "", "provider-wide"
	var err error
	channelType := c.GetInt("channel_type")
	if channelType > 0 {
		providerNamespace = fmt.Sprintf("channel-type:%d", channelType)
	} else {
		operationCtx, cancel := boundedResponsesLifecycleContext(context.WithoutCancel(responseOwnerRequestContext(c)))
		channel, loadErr := model.GetChannelIncarnationByID(operationCtx, channelID)
		cancel()
		if loadErr != nil {
			err = fmt.Errorf("load response owner channel incarnation: %w", loadErr)
		} else {
			providerNamespace, responseScope, err = model.ConservativeResponseOwnerIdentity(channel)
		}
	}
	var owner *model.ResponseOwner
	if err == nil {
		owner, err = model.NewResponseOwner(responseID, c.GetInt("id"), c.GetInt("token_id"), channelID, time.Now(), providerNamespace, responseScope)
	}
	if err == nil {
		operationCtx, cancel := boundedResponsesLifecycleContext(context.WithoutCancel(responseOwnerRequestContext(c)))
		defer cancel()
		err = model.CreateResponseOwner(operationCtx, owner)
	}
	if err == nil {
		return nil
	}
	code := "responses_owner_persist_failed"
	if errors.Is(err, model.ErrResponseOwnerConflict) {
		code = "responses_owner_conflict"
	}
	return &types.OpenAIErrorWithStatusCode{
		OpenAIError: types.OpenAIError{
			Message: "stored response ownership could not be committed",
			Type:    "server_error",
			Code:    code,
		},
		StatusCode: http.StatusServiceUnavailable,
		LocalError: true,
	}
}

func responsesEphemeralProofKey(c *gin.Context, responseID string) string {
	userID := 0
	if c != nil {
		userID = c.GetInt("id")
	}
	responseID = strings.TrimSpace(responseID)
	if userID <= 0 || responseID == "" {
		return ""
	}
	digest := sha256.Sum256([]byte(responseID))
	return fmt.Sprintf("responses:ephemeral:user:%d:id:%s", userID, hex.EncodeToString(digest[:16]))
}

func recordResponsesEphemeralProof(c *gin.Context, responseID string, channelID int) {
	key := responsesEphemeralProofKey(c, responseID)
	if key == "" || channelID <= 0 {
		return
	}
	channelAffinityManager().SetRecord(key, runtimeaffinity.Record{ChannelID: channelID}, responsesEphemeralProofTTL)
}

func lookupResponsesEphemeralProof(c *gin.Context, responseID string) (int, bool) {
	key := responsesEphemeralProofKey(c, responseID)
	if key == "" {
		return 0, false
	}
	record, ok := channelAffinityManager().Get(key)
	return record.ChannelID, ok && record.ChannelID > 0
}

func clearResponsesEphemeralProof(c *gin.Context, responseID string, ownerChannelID int) {
	key := responsesEphemeralProofKey(c, responseID)
	if key == "" || ownerChannelID <= 0 {
		return
	}
	manager := channelAffinityManager()
	record, ok := manager.Get(key)
	if ok && record.ChannelID == ownerChannelID {
		manager.Delete(key)
	}
}
