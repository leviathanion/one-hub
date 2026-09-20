package openai

import (
	"encoding/json"
	"net/http"
	"one-api/common"
	"one-api/common/config"
	"one-api/common/requestctx"
	commonresponses "one-api/common/responses"
	"one-api/types"
)

// Authorize the actual body after channel overlays; inspecting the initial DTO
// alone would allow a custom parameter to inject an unowned voice or audio ID.
func (p *OpenAIProvider) authorizeMediaRequestBody(req *http.Request, relayMode int) *types.OpenAIErrorWithStatusCode {
	operation := ""
	switch relayMode {
	case config.RelayModeAudioSpeech:
		operation = "speech"
	case config.RelayModeChatCompletions:
		operation = "chat"
	case config.RelayModeImagesEdits, config.RelayModeImagesGenerations, config.RelayModeImagesVariations:
		operation = "images"
	default:
		return nil
	}
	raw, err := requestBodyCopy(req)
	if err != nil {
		return common.ErrorWrapperLocal(err, "read_request_body_failed", http.StatusInternalServerError)
	}
	var body map[string]any
	if json.Unmarshal(raw, &body) != nil {
		return nil
	}
	refs := commonresponses.ProtocolResourceReferences(body, operation)
	for _, ref := range refs {
		if err := requestctx.AuthorizeResourceReference(p.Context, ref.Kind, ref.ID); err != nil {
			return common.ErrorWrapperLocal(err, "unsupported_capability", http.StatusBadRequest)
		}
	}
	return nil
}
