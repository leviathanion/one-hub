package responses

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"one-api/common/jsonobject"
	"one-api/types"
)

// ObservedEvent contains only lifecycle facts. Raw delivery and billing
// evidence extraction remain independent users of the original payload.
type ObservedEvent struct {
	Type string

	HasSequence   bool
	Sequence      int64
	SequenceError error

	ResponseObject       bool
	Response             *types.OpenAIResponsesResponses
	ResponseErrorPresent bool
	TopLevelError        json.RawMessage
	TopLevelCode         json.RawMessage
	TopLevelMessage      json.RawMessage
}

func ObserveEventLifecycle(payload []byte) (ObservedEvent, error) {
	object, err := jsonobject.Parse(payload)
	if err != nil {
		return ObservedEvent{}, err
	}
	eventType, err := requiredRawString(object.Fields, "type")
	if err != nil {
		return ObservedEvent{}, err
	}
	observed := ObservedEvent{
		Type:            eventType,
		TopLevelError:   cloneRaw(object.Fields["error"]),
		TopLevelCode:    cloneRaw(object.Fields["code"]),
		TopLevelMessage: cloneRaw(object.Fields["message"]),
	}
	if rawSequence, ok := object.Fields["sequence_number"]; ok {
		observed.HasSequence = true
		observed.Sequence, observed.SequenceError = rawNonNegativeInteger(rawSequence)
	}

	rawResponse, responsePresent := object.Fields["response"]
	if !responsePresent || isRawNull(rawResponse) {
		return observed, nil
	}
	responseObject, err := jsonobject.Parse(rawResponse)
	if err != nil {
		return observed, nil
	}
	observed.ResponseObject = true
	if rawError, ok := responseObject.Fields["error"]; ok && !isRawNull(rawError) {
		observed.ResponseErrorPresent = true
	}

	// 容错投影独立保留资源身份和计费字段的原始证据。
	var response types.OpenAIResponsesResponses
	if err := response.DecodeCapturedProviderJSON(rawResponse); err != nil {
		return observed, err
	}
	observed.Response = &response
	return observed, nil
}

func requiredRawString(fields map[string]json.RawMessage, name string) (string, error) {
	if _, ok := fields[name]; !ok {
		return "", fmt.Errorf("%s is required", name)
	}
	value, err := optionalRawString(fields, name)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("%s must be a non-empty string", name)
	}
	return strings.TrimSpace(value), nil
}

func optionalRawString(fields map[string]json.RawMessage, name string) (string, error) {
	raw, ok := fields[name]
	if !ok || isRawNull(raw) {
		return "", nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", errors.New("must be a string")
	}
	return strings.TrimSpace(value), nil
}

func rawNonNegativeInteger(raw json.RawMessage) (int64, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return 0, err
	}
	number, ok := value.(json.Number)
	if !ok {
		return 0, errors.New("must be a non-negative integer")
	}
	parsed, err := strconv.ParseInt(number.String(), 10, 64)
	if err != nil || parsed < 0 {
		return 0, errors.New("must be a non-negative integer")
	}
	return parsed, nil
}

func isRawNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

func cloneRaw(raw json.RawMessage) json.RawMessage {
	return append(json.RawMessage(nil), raw...)
}
