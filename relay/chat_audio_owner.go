package relay

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"one-api/common"
	"one-api/common/logger"
	"one-api/model"
	providersBase "one-api/providers/base"
	"one-api/types"
)

const chatAudioOwnerMaxSlots = 64

func (r *relayChat) prepareChatAudioOwnership() (func(), *types.OpenAIErrorWithStatusCode) {
	provider, ok := r.provider.(providersBase.ChatAudioOwnerSupport)
	if !ok {
		return func() {}, nil
	}
	ctx := r.c.Request.Context()
	userID, tokenID := r.c.GetInt("id"), r.c.GetInt("token_id")
	channel := r.provider.GetChannel()
	var mu sync.Mutex
	var reservations []*model.ResourceOwner
	bound := make(map[string]*model.ResourceOwner)
	choiceIDs := make(map[int]string)
	closed := false
	reserve := func(count int) error {
		if count < 1 {
			return nil
		}
		if count > chatAudioOwnerMaxSlots {
			return common.StringErrorWrapperLocal("Chat audio output exceeds local resource capacity", "resource_capacity_exceeded", http.StatusTooManyRequests)
		}
		if len(reservations) >= count {
			return nil
		}
		namespace, scope, err := model.ConservativeResponseOwnerIdentity(channel)
		if err != nil {
			return err
		}
		deadline, ok := ctx.Deadline()
		if !ok {
			deadline = time.Now().Add(resourceRequestTimeout)
		}
		specs := make([]model.ResourceOwnerReservation, count-len(reservations))
		for i := range specs {
			specs[i] = model.ResourceOwnerReservation{Kind: "chat_audio", UserID: userID, TokenID: tokenID, ChannelID: channel.Id, ProviderNamespace: namespace, ProviderScope: scope, SubmitDeadline: deadline}
		}
		owners, err := model.NewResourceOwnerRepository(model.DB).ReserveMany(ctx, specs)
		if errors.Is(err, model.ErrResourceOwnerCapacity) {
			return common.StringErrorWrapperLocal("Chat audio resource capacity is exhausted", "resource_capacity_exceeded", http.StatusTooManyRequests)
		}
		if err != nil {
			return err
		}
		reservations = append(reservations, owners...)
		return nil
	}
	provider.SetChatAudioOwnerPolicy(func(count int) error {
		mu.Lock()
		defer mu.Unlock()
		if closed {
			return errors.New("Chat audio observation is closed")
		}
		return reserve(count)
	}, func(facts []providersBase.ChatAudioResourceFact) error {
		mu.Lock()
		defer mu.Unlock()
		if closed {
			return errors.New("Chat audio observation is closed")
		}
		for _, fact := range facts {
			id := fact.ID
			if id == "" && fact.Index != nil {
				id = choiceIDs[*fact.Index]
			}
			if id == "" {
				continue
			}
			owner := bound[id]
			if owner == nil {
				// A provider may emit an unexpected audio result; ordinary text requests
				// reserve nothing until a real resource first appears at delivery.
				if len(reservations) == 0 {
					if err := reserve(1); err != nil {
						return err
					}
				}
				if len(bound) >= len(reservations) {
					return model.ErrResourceOwnerCapacity
				}
				reservation := reservations[len(bound)]
				commitCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
				var err error
				owner, err = model.BindResourceOwner(commitCtx, reservation.ID, userID, id)
				cancel()
				if err != nil {
					return err
				}
				bound[id] = owner
			}
			if fact.Index != nil && len(choiceIDs) < chatAudioOwnerMaxSlots {
				choiceIDs[*fact.Index] = id
			}
			if fact.ExpiresAt != nil {
				// Keep only bounded optional metadata; no SQL observation may
				// hold the original audio frame behind the delivery barrier.
				observed := time.Unix(*fact.ExpiresAt, 0)
				owner.UpstreamExpiresAt = &observed
			}
		}
		return nil
	})
	return func() {
		provider.SetChatAudioOwnerPolicy(nil, nil)
		mu.Lock()
		defer mu.Unlock()
		if closed {
			return
		}
		closed = true
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		for _, reservation := range reservations {
			if err := model.ReleaseResourceOwnerReservation(cleanupCtx, reservation.ID, userID); err != nil {
				logger.LogError(cleanupCtx, "Chat audio reservation cleanup failed")
			}
		}
		for _, owner := range bound {
			if owner.UpstreamExpiresAt != nil {
				if err := model.DB.WithContext(cleanupCtx).Model(&model.ResourceOwner{}).Where("id = ? AND user_id = ?", owner.ID, userID).Update("upstream_expires_at", owner.UpstreamExpiresAt).Error; err != nil {
					logger.LogError(cleanupCtx, "Chat audio expiry observation unavailable")
				}
			}
		}
	}, nil
}
