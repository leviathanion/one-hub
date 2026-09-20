package relay_util

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"one-api/common"
	"one-api/model"
)

// ResourceOwnerChannelKey records authorization-derived routing, never an
// administrator override. References in the same request must share a channel.
const ResourceOwnerChannelKey = "resource_owner_channel_id"

// ResourceReferencePolicy is shared by HTTP and realtime adapters. The adapter
// extracts protocol references; only this relay policy queries ownership.
type ResourceReferencePolicy struct{ Context *gin.Context }

func (p ResourceReferencePolicy) AuthorizeResourceReference(kind, id string) error {
	c := p.Context
	switch kind {
	case "file", "conversation", "chat_audio":
	default:
		return common.StringErrorWrapperLocal(kind+" requires an implemented account-scoped resource owner", "unsupported_capability", http.StatusBadRequest)
	}
	if c == nil || c.Request == nil || c.GetInt("id") <= 0 {
		return common.StringErrorWrapperLocal("resource reference is not authorized", "resource_not_found", http.StatusNotFound)
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	owner, err := model.GetResourceOwner(ctx, kind, id, c.GetInt("id"))
	if err != nil {
		if errors.Is(err, model.ErrResourceOwnerNotFound) {
			return common.StringErrorWrapperLocal("resource reference is not authorized", "resource_not_found", http.StatusNotFound)
		}
		return common.StringErrorWrapperLocal("resource ownership is unavailable", "resource_owner_unavailable", http.StatusServiceUnavailable)
	}
	for _, key := range []string{ResourceOwnerChannelKey, "channel_id", "responses_owner_channel_id"} {
		if channelID := c.GetInt(key); channelID > 0 && channelID != owner.ChannelID {
			return common.StringErrorWrapperLocal("resource references require the same channel", "resource_channel_conflict", http.StatusBadRequest)
		}
	}
	if channelID := c.GetInt("specific_channel_id"); channelID > 0 && !c.GetBool("specific_channel_id_ignore") && channelID != owner.ChannelID {
		return common.StringErrorWrapperLocal("resource reference conflicts with selected channel", "resource_channel_conflict", http.StatusBadRequest)
	}
	c.Set(ResourceOwnerChannelKey, owner.ChannelID)
	c.Set("specific_channel_id", owner.ChannelID)
	c.Set("specific_channel_id_ignore", false)
	return nil
}

func (p *realtimeWorkPolicy) AuthorizeResourceReference(kind, id string) error {
	return (ResourceReferencePolicy{Context: p.c}).AuthorizeResourceReference(kind, id)
}
