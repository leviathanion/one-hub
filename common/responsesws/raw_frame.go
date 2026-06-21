package responsesws

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"one-api/types"
	"strings"
)

// RawResponsesCreateFrame preserves the original response.create envelope and
// a typed projection for relay-side admission and provider rewrite.
type RawResponsesCreateFrame struct {
	Raw        json.RawMessage
	Object     map[string]json.RawMessage
	Projection types.OpenAIResponsesRequest
	EventID    string
}

// ProviderEventEnvelope is the minimal parsed shape of a provider event.
type ProviderEventEnvelope struct {
	Type    string
	EventID string
	Object  map[string]json.RawMessage
}

// ClientEventEnvelope is the minimal parsed shape of a client event.
type ClientEventEnvelope struct {
	Type    string
	EventID string
	Object  map[string]json.RawMessage
}

var ErrInvalidClientEventPayload = errors.New("responses websocket client event payload is invalid")
var ErrInvalidProviderEventPayload = errors.New("responses websocket provider event payload is invalid")

func ParseRawResponsesCreateFrame(raw []byte) (*RawResponsesCreateFrame, error) {
	object, err := decodeTopLevelObjectNoDuplicateKeys(raw)
	if err != nil {
		return nil, err
	}

	eventType := rawStringField(object, "type")
	if eventType != "response.create" {
		return nil, fmt.Errorf("unsupported responses websocket event type %q", eventType)
	}

	var projection types.OpenAIResponsesRequest
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&projection); err != nil {
		return nil, fmt.Errorf("decode response.create projection: %w", err)
	}
	if strings.TrimSpace(projection.Model) == "" {
		projection.Model = rawStringField(object, "model")
	}
	if strings.TrimSpace(projection.Model) == "" {
		return nil, errors.New("response.create model is required")
	}

	return &RawResponsesCreateFrame{
		Raw:        append(json.RawMessage(nil), bytes.TrimSpace(raw)...),
		Object:     cloneRawObject(object),
		Projection: projection,
		EventID:    rawStringField(object, "event_id"),
	}, nil
}

func (f *RawResponsesCreateFrame) CloneForModel(model string) ([]byte, error) {
	if f == nil {
		return nil, errors.New("responses create frame is required")
	}
	trimmedModel := strings.TrimSpace(model)
	if trimmedModel == "" {
		return nil, errors.New("response.create model is required")
	}
	if rawStringField(f.Object, "model") == trimmedModel {
		return append([]byte(nil), f.Raw...), nil
	}
	object := cloneRawObject(f.Object)
	encodedModel, err := json.Marshal(trimmedModel)
	if err != nil {
		return nil, err
	}
	object["model"] = encodedModel
	return json.Marshal(object)
}

func (f *RawResponsesCreateFrame) CloneWithDefaultPreviousResponseID(previousResponseID string) ([]byte, error) {
	if f == nil {
		return nil, errors.New("responses create frame is required")
	}
	trimmedPreviousResponseID := strings.TrimSpace(previousResponseID)
	if _, exists := f.Object["previous_response_id"]; trimmedPreviousResponseID == "" || exists {
		return append([]byte(nil), f.Raw...), nil
	}
	object := cloneRawObject(f.Object)
	encodedPreviousResponseID, err := json.Marshal(trimmedPreviousResponseID)
	if err != nil {
		return nil, err
	}
	object["previous_response_id"] = encodedPreviousResponseID
	return json.Marshal(object)
}

func ValidateProviderEventPayload(raw []byte) error {
	_, err := ParseProviderEventEnvelope(raw)
	return err
}

func ValidateClientEventPayload(raw []byte) error {
	_, err := ParseClientEventEnvelope(raw)
	return err
}

func ParseClientEventEnvelope(raw []byte) (*ClientEventEnvelope, error) {
	object, err := decodeTopLevelObjectNoDuplicateKeys(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidClientEventPayload, err)
	}
	eventType := rawStringField(object, "type")
	if eventType == "" {
		return nil, fmt.Errorf("%w: top-level type must be a non-empty string", ErrInvalidClientEventPayload)
	}
	return &ClientEventEnvelope{
		Type:    eventType,
		EventID: rawStringField(object, "event_id"),
		Object:  cloneRawObject(object),
	}, nil
}

func ParseProviderEventEnvelope(raw []byte) (*ProviderEventEnvelope, error) {
	object, err := decodeTopLevelObjectNoDuplicateKeys(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidProviderEventPayload, err)
	}
	eventType := rawStringField(object, "type")
	if eventType == "" {
		return nil, fmt.Errorf("%w: top-level type must be a non-empty string", ErrInvalidProviderEventPayload)
	}
	return &ProviderEventEnvelope{
		Type:    eventType,
		EventID: rawStringField(object, "event_id"),
		Object:  cloneRawObject(object),
	}, nil
}

func BuildResponsesHTTPBridgeBody(object map[string]json.RawMessage, model string, previousResponseID string) (map[string]json.RawMessage, error) {
	if object == nil {
		return nil, errors.New("responses websocket bridge request is required")
	}
	trimmedModel := strings.TrimSpace(model)
	if trimmedModel == "" {
		return nil, errors.New("response.create model is required")
	}

	body := cloneRawObject(object)
	// The bridge converts a ResponsesWS client event into an HTTP /responses
	// body. Keep future create-body fields raw, but do not forward WebSocket
	// envelope fields into the HTTP endpoint.
	for _, key := range []string{"type", "event_id"} {
		delete(body, key)
	}
	if err := validateResponsesHTTPBridgeTransportFields(body); err != nil {
		return nil, err
	}
	delete(body, "background")

	encodedModel, err := json.Marshal(trimmedModel)
	if err != nil {
		return nil, err
	}
	body["model"] = encodedModel
	if _, exists := body["previous_response_id"]; !exists {
		trimmedPreviousResponseID := strings.TrimSpace(previousResponseID)
		if trimmedPreviousResponseID != "" {
			encodedPreviousResponseID, err := json.Marshal(trimmedPreviousResponseID)
			if err != nil {
				return nil, err
			}
			body["previous_response_id"] = encodedPreviousResponseID
		}
	}
	body["stream"] = json.RawMessage("true")
	return body, nil
}

func NormalizeResponsesHTTPBridgeRequestMap(requestMap map[string]interface{}) error {
	if requestMap == nil {
		return nil
	}
	// The HTTP bridge contract is enforced after channel custom_parameter
	// merging too. This is deliberately a final boundary check: provider-specific
	// customization stays available, but it cannot turn the bridge request into a
	// non-streaming/background Responses call that the WS relay cannot consume.
	for _, key := range []string{"type", "event_id"} {
		delete(requestMap, key)
	}
	if raw, ok := requestMap["background"]; ok {
		if raw == nil {
			delete(requestMap, "background")
		} else if background, ok := raw.(bool); ok {
			if background {
				return unsupportedResponsesWSBridgeFieldError("background")
			}
			delete(requestMap, "background")
		} else {
			return unsupportedResponsesWSBridgeFieldError("background")
		}
	}
	if raw, ok := requestMap["stream"]; ok {
		if raw != nil {
			stream, ok := raw.(bool)
			if !ok || !stream {
				return unsupportedResponsesWSBridgeFieldError("stream")
			}
		}
	}
	requestMap["stream"] = true
	return nil
}

func validateResponsesHTTPBridgeTransportFields(body map[string]json.RawMessage) error {
	if body == nil {
		return nil
	}
	if raw, ok := body["stream"]; ok && len(raw) > 0 && strings.TrimSpace(string(raw)) != "null" {
		var stream bool
		if err := json.Unmarshal(raw, &stream); err != nil {
			return err
		}
		if !stream {
			return unsupportedResponsesWSBridgeFieldError("stream")
		}
	}
	if raw, ok := body["background"]; ok && len(raw) > 0 && strings.TrimSpace(string(raw)) != "null" {
		var background bool
		if err := json.Unmarshal(raw, &background); err != nil {
			return err
		}
		if background {
			return unsupportedResponsesWSBridgeFieldError("background")
		}
	}
	return nil
}

func unsupportedResponsesWSBridgeFieldError(field string) error {
	err := fmt.Errorf("responses websocket HTTP bridge does not support %s", field)
	payload, marshalErr := json.Marshal(struct {
		Type   string `json:"type"`
		Status int    `json:"status"`
		Error  struct {
			Type    string `json:"type"`
			Code    string `json:"code"`
			Message string `json:"message"`
			Param   string `json:"param"`
		} `json:"error"`
	}{
		Type:   "error",
		Status: http.StatusBadRequest,
		Error: struct {
			Type    string `json:"type"`
			Code    string `json:"code"`
			Message string `json:"message"`
			Param   string `json:"param"`
		}{
			Type:    "invalid_request_error",
			Code:    "unsupported_responses_ws_bridge_field",
			Message: "field is not supported by Responses websocket HTTP bridge",
			Param:   field,
		},
	})
	if marshalErr != nil {
		return err
	}
	return NewClientPayloadError(err, payload)
}

func decodeTopLevelObjectNoDuplicateKeys(raw []byte) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()

	token, err := decoder.Token()
	if err != nil {
		return nil, fmt.Errorf("decode json object: %w", err)
	}
	delim, ok := token.(json.Delim)
	if !ok || delim != '{' {
		return nil, errors.New("top-level JSON value must be an object")
	}

	object := map[string]json.RawMessage{}
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return nil, fmt.Errorf("decode object key: %w", err)
		}
		key, ok := keyToken.(string)
		if !ok {
			return nil, errors.New("object key must be a string")
		}
		if _, exists := object[key]; exists {
			return nil, fmt.Errorf("duplicate top-level key %q", key)
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, fmt.Errorf("decode object value %q: %w", key, err)
		}
		object[key] = append(json.RawMessage(nil), value...)
	}

	token, err = decoder.Token()
	if err != nil {
		return nil, fmt.Errorf("decode object end: %w", err)
	}
	delim, ok = token.(json.Delim)
	if !ok || delim != '}' {
		return nil, errors.New("invalid top-level JSON object")
	}
	if decoder.More() {
		return nil, errors.New("unexpected trailing JSON data")
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err == nil {
		return nil, errors.New("unexpected trailing JSON data")
	} else if !errors.Is(err, io.EOF) {
		return nil, errors.New("unexpected trailing JSON data")
	}
	return object, nil
}

func cloneRawObject(object map[string]json.RawMessage) map[string]json.RawMessage {
	cloned := make(map[string]json.RawMessage, len(object))
	for key, value := range object {
		cloned[key] = append(json.RawMessage(nil), value...)
	}
	return cloned
}

func rawStringField(object map[string]json.RawMessage, key string) string {
	var value string
	if raw, ok := object[key]; ok {
		_ = json.Unmarshal(raw, &value)
	}
	return strings.TrimSpace(value)
}
