package responsesws

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	commonresponses "one-api/common/responses"
	"one-api/types"
	"strings"
)

// RawResponsesCreateFrame preserves the original response.create envelope and
// a typed projection for relay-side admission and provider rewrite.
type RawResponsesCreateFrame struct {
	Raw               json.RawMessage
	Object            map[string]json.RawMessage
	Projection        types.OpenAIResponsesRequest
	EventID           string
	MultiAgentEnabled bool
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
	return parseRawResponsesCreateFrame(raw, false)
}

// ParseRawResponsesCreateFrameOwned transfers ownership of raw to the returned
// frame. Callers must not mutate raw after a successful call.
func ParseRawResponsesCreateFrameOwned(raw []byte) (*RawResponsesCreateFrame, error) {
	return parseRawResponsesCreateFrame(raw, true)
}

func parseRawResponsesCreateFrame(raw []byte, owned bool) (*RawResponsesCreateFrame, error) {
	object, err := decodeTopLevelObjectNoDuplicateKeys(raw)
	if err != nil {
		return nil, err
	}

	eventType := rawStringField(object, "type")
	if eventType != "response.create" {
		return nil, fmt.Errorf("unsupported responses websocket event type %q", eventType)
	}

	projection, err := commonresponses.ProjectRawRequest(raw)
	if err != nil {
		return nil, fmt.Errorf("decode response.create envelope: %w", err)
	}
	if strings.TrimSpace(projection.Model) == "" {
		projection.Model = rawStringField(object, "model")
	}
	if strings.TrimSpace(projection.Model) == "" {
		return nil, errors.New("response.create model is required")
	}
	multiAgentEnabled, err := commonresponses.ProjectMultiAgentEnabled(object["multi_agent"])
	if err != nil {
		return nil, err
	}

	retainedRaw := bytes.TrimSpace(raw)
	if !owned {
		retainedRaw = append([]byte(nil), retainedRaw...)
	}
	return &RawResponsesCreateFrame{
		Raw:               json.RawMessage(retainedRaw),
		Object:            object,
		Projection:        projection,
		EventID:           rawStringField(object, "event_id"),
		MultiAgentEnabled: multiAgentEnabled,
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
