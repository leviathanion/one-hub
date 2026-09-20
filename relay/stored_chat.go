package relay

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"one-api/common"
	"one-api/common/logger"
	"one-api/model"
	providersBase "one-api/providers/base"
	"one-api/types"
)

func parseStoredChatHTTPOperation(method, escapedPath string) (resourceHTTPOperation, bool) {
	op := resourceHTTPOperation{kind: "stored_chat"}
	const root = "/v1/chat/completions"
	if escapedPath == root {
		op.topList = true
		return op, method == http.MethodGet
	}
	if !strings.HasPrefix(escapedPath, root+"/") {
		return op, false
	}
	parts := strings.Split(strings.TrimPrefix(escapedPath, root+"/"), "/")
	id, err := url.PathUnescape(parts[0])
	if err != nil || id == "" || id == "." || id == ".." || strings.ContainsAny(id, "/\\\x00") {
		return op, false
	}
	op.id = id
	if len(parts) == 1 {
		op.observeDelete = method == http.MethodDelete
		return op, method == http.MethodGet || method == http.MethodPost || method == http.MethodDelete
	}
	return op, len(parts) == 2 && parts[1] == "messages" && method == http.MethodGet
}

func (r *relayChat) prepareStoredChatOwnership() (func(), *types.OpenAIErrorWithStatusCode) {
	if r.chatRequest.Store == nil || !*r.chatRequest.Store {
		return func() {}, nil
	}
	provider, ok := r.provider.(providersBase.StoredChatSupport)
	if !ok || !provider.SupportsStoredChat() || chatModelRequiresResponses(r.modelName) {
		return nil, common.StringErrorWrapperLocal("selected channel cannot preserve Stored Chat lifecycle", "unsupported_capability", http.StatusBadRequest)
	}
	channel := r.provider.GetChannel()
	namespace, scope, err := model.ConservativeResponseOwnerIdentity(channel)
	if err != nil {
		return nil, common.ErrorWrapperLocal(err, "resource_owner_unavailable", http.StatusServiceUnavailable)
	}
	ctx := r.c.Request.Context()
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(resourceRequestTimeout)
	}
	reservation, err := model.ReserveResourceOwner(ctx, model.ResourceOwnerReservation{Kind: "stored_chat", UserID: r.c.GetInt("id"), TokenID: r.c.GetInt("token_id"), ChannelID: channel.Id, ProviderNamespace: namespace, ProviderScope: scope, SubmitDeadline: deadline})
	if err != nil {
		if errors.Is(err, model.ErrResourceOwnerCapacity) {
			return nil, common.ErrorWrapperLocal(err, "resource_capacity_exceeded", http.StatusTooManyRequests)
		}
		return nil, common.ErrorWrapperLocal(err, "resource_owner_unavailable", http.StatusServiceUnavailable)
	}
	var mu sync.Mutex
	boundID := ""
	provider.SetStoredChatOwnerCommit(func(id string) error {
		mu.Lock()
		defer mu.Unlock()
		if boundID == id {
			return nil
		}
		commitCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if _, err := model.BindResourceOwner(commitCtx, reservation.ID, r.c.GetInt("id"), id); err != nil {
			return err
		}
		boundID = id
		return nil
	})
	return func() {
		provider.SetStoredChatOwnerCommit(nil)
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := model.ReleaseResourceOwnerReservation(cleanupCtx, reservation.ID, r.c.GetInt("id")); err != nil {
			logger.LogError(cleanupCtx, "stored Chat reservation cleanup failed: "+err.Error())
		}
	}, nil
}
