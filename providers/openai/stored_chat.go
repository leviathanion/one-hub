package openai

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"one-api/common"
	"one-api/common/config"
	"one-api/model"
	"one-api/types"
)

func supportsStoredChatChannel(channel *model.Channel) bool {
	if channel == nil {
		return false
	}
	switch channel.Type {
	case config.ChannelTypeOpenAI, config.ChannelTypeAzureV1, config.ChannelTypeCustom:
		return true
	default:
		return false
	}
}

func (p *OpenAIProvider) SupportsStoredChat() bool {
	return p != nil && supportsStoredChatChannel(p.Channel) && !p.StreamEscapeJSON
}

func (p *OpenAIProvider) SetStoredChatOwnerCommit(commit func(string) error) {
	p.storedChatOwnerCommit = commit
}

// The actual body, after all overlays, determines whether an owner is needed.
// An overlay cannot silently enable storage without the relay's prior reserve.
func (p *OpenAIProvider) chatResourceRequestCommit(req *http.Request) (func([]byte) error, *types.OpenAIErrorWithStatusCode) {
	raw, err := requestBodyCopy(req)
	if err != nil {
		return nil, common.ErrorWrapperLocal(err, "read_request_body_failed", http.StatusInternalServerError)
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return nil, nil
	}
	var store, stream bool
	_ = json.Unmarshal(fields["store"], &store)
	_ = json.Unmarshal(fields["stream"], &stream)
	if store && (!p.SupportsStoredChat() || p.storedChatOwnerCommit == nil) {
		return nil, common.StringErrorWrapperLocal("stored Chat requires a native channel and resource capacity reservation", "unsupported_capability", http.StatusBadRequest)
	}
	slots := chatAudioSlots(fields)
	if slots > 0 {
		if (!p.ProviderRawJSONReplay && !p.usesNativeOpenAIWire()) || p.StreamEscapeJSON || p.prepareChatAudioOwner == nil || p.commitChatAudioOwner == nil {
			return nil, common.StringErrorWrapperLocal("Chat audio output requires native resource ownership support", "unsupported_capability", http.StatusBadRequest)
		}
		if err := p.prepareChatAudioOwner(slots); err != nil {
			var apiErr *types.OpenAIErrorWithStatusCode
			if errors.As(err, &apiErr) {
				return nil, apiErr
			}
			return nil, common.ErrorWrapperLocal(errors.New("Chat audio resource capacity is unavailable"), "resource_owner_unavailable", http.StatusServiceUnavailable)
		}
	}
	audioCommit := p.commitChatAudioOwner
	if !store && audioCommit == nil {
		return nil, nil
	}
	commit := p.storedChatOwnerCommit
	return func(raw []byte) error {
		// One projection serves both resource families without rewriting the frame.
		var envelope map[string]json.RawMessage
		if json.Unmarshal(raw, &envelope) != nil {
			return nil
		}
		if e := envelope["error"]; len(e) > 0 && string(e) != "null" {
			return nil
		}
		var id, object string
		_ = json.Unmarshal(envelope["id"], &id)
		_ = json.Unmarshal(envelope["object"], &object)
		if stream && object != "chat.completion.chunk" && (object != "" || len(envelope["choices"]) == 0) {
			return nil
		}
		ownershipFailure := func() error {
			apiErr := common.ErrorWrapperLocal(errors.New("Chat resource ownership could not be committed"), "resource_owner_commit_failed", http.StatusServiceUnavailable)
			apiErr.UpstreamAccepted = true
			return apiErr
		}
		if store && strings.TrimSpace(id) != "" {
			if err := commit(id); err != nil {
				return ownershipFailure()
			}
		}
		if audioCommit != nil {
			if facts := chatAudioFacts(envelope["choices"]); len(facts) > 0 {
				if err := audioCommit(facts); err != nil {
					return ownershipFailure()
				}
			}
		}
		return nil
	}, nil
}
