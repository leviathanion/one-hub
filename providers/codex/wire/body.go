package wire

import (
	"bytes"
	"encoding/json"
	"strings"

	"one-api/common/jsonobject"
	commonresponses "one-api/common/responses"
	"one-api/types"
)

type CreateBodyInput struct {
	Model       string
	Stream      bool
	PromptCache *commonresponses.PromptCacheDecision
}

func PlanResponsesCreateBody(object *jsonobject.Object, in CreateBodyInput) ([]byte, error) {
	if object == nil {
		return nil, reject("body", "request body is required")
	}
	if strings.TrimSpace(in.Model) == "" {
		return nil, reject("model", "model is required")
	}
	if err := validateCreateBody(object); err != nil {
		return nil, err
	}

	out := object.Clone()
	if err := out.SetJSON("model", strings.TrimSpace(in.Model)); err != nil {
		return nil, err
	}
	if err := out.SetJSON("stream", in.Stream); err != nil {
		return nil, err
	}
	if err := out.SetJSON("store", false); err != nil {
		return nil, err
	}
	if err := applyPromptCacheDecision(out, in.PromptCache); err != nil {
		return nil, err
	}
	if _, ok := out.Fields["reasoning"]; ok {
		if err := appendInclude(out, "reasoning.encrypted_content"); err != nil {
			return nil, err
		}
	}
	return out.MarshalJSON()
}

func validateCreateBody(object *jsonobject.Object) error {
	if object == nil {
		return reject("body", "request body is required")
	}
	if _, hasTemperature := object.Fields["temperature"]; hasTemperature {
		if _, hasTopP := object.Fields["top_p"]; hasTopP {
			return reject("temperature", "temperature and top_p cannot both be present")
		}
	}
	if _, ok := object.Fields["context_management"]; ok {
		return reject("context_management", "context_management is not supported by Codex Official upstream")
	}
	if _, ok := object.Fields["truncation"]; ok {
		return reject("truncation", "truncation is not supported by Codex Official upstream")
	}
	if raw, ok := object.Fields["client_metadata"]; ok {
		trimmed := bytes.TrimSpace(raw)
		if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) || trimmed[0] != '{' {
			return reject("client_metadata", "client_metadata must be a JSON object")
		}
	}
	if raw, ok := object.Fields["prompt_cache_key"]; ok {
		key, err := promptCacheKeyFromRaw(raw)
		if err != nil {
			return err
		}
		if !validPromptCacheKey(key) {
			return reject("prompt_cache_key", "prompt_cache_key is invalid")
		}
	}
	return nil
}

func promptCacheKeyFromRaw(raw json.RawMessage) (string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return "", reject("prompt_cache_key", "prompt_cache_key must be a non-empty string")
	}
	var key string
	if err := json.Unmarshal(trimmed, &key); err != nil {
		return "", reject("prompt_cache_key", "prompt_cache_key must be a string")
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return "", reject("prompt_cache_key", "prompt_cache_key must be a non-empty string")
	}
	return key, nil
}

func appendInclude(object *jsonobject.Object, value string) error {
	raw, ok := object.Fields["include"]
	if !ok || len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return object.SetJSON("include", []string{value})
	}
	var items []string
	if err := json.Unmarshal(raw, &items); err != nil {
		return reject("include", "include must be a string array")
	}
	has := false
	out := make([]string, 0, len(items)+1)
	for _, item := range items {
		if item == value {
			has = true
		}
		out = append(out, item)
	}
	if !has {
		out = append(out, value)
	}
	return object.SetJSON("include", out)
}

func applyPromptCacheDecision(object *jsonobject.Object, decision *commonresponses.PromptCacheDecision) error {
	if object == nil {
		return reject("body", "request body is required")
	}
	if _, exists := object.Fields["prompt_cache_key"]; exists {
		return nil
	}
	key := ""
	if decision != nil {
		key = strings.TrimSpace(decision.Key)
	}
	if key == "" {
		return nil
	}
	if !validPromptCacheKey(key) {
		return reject("prompt_cache_key", "prompt_cache_key is invalid")
	}
	return object.SetJSON("prompt_cache_key", key)
}

func PlanResponsesCompactBody(object *jsonobject.Object, request types.OpenAIResponsesRequest, model string, promptCache *commonresponses.PromptCacheDecision) ([]byte, error) {
	if object == nil {
		return nil, reject("body", "request body is required")
	}
	if err := validateCreateBody(object); err != nil {
		return nil, err
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return nil, reject("model", "model is required")
	}

	body := make(map[string]any, 6)
	body["model"] = model
	if request.Input != nil {
		body["input"] = request.Input
	}
	if strings.TrimSpace(request.Instructions) != "" {
		body["instructions"] = request.Instructions
	}
	if strings.TrimSpace(request.PreviousResponseID) != "" {
		body["previous_response_id"] = request.PreviousResponseID
	}
	if key := strings.TrimSpace(request.PromptCacheKey); key != "" {
		if !validPromptCacheKey(key) {
			return nil, reject("prompt_cache_key", "prompt_cache_key is invalid")
		}
		body["prompt_cache_key"] = key
	} else if promptCache != nil && strings.TrimSpace(promptCache.Key) != "" {
		key := strings.TrimSpace(promptCache.Key)
		if !validPromptCacheKey(key) {
			return nil, reject("prompt_cache_key", "prompt_cache_key is invalid")
		}
		body["prompt_cache_key"] = key
	}
	if strings.TrimSpace(request.PromptCacheRetention) != "" {
		body["prompt_cache_retention"] = request.PromptCacheRetention
	}
	return json.Marshal(body)
}
