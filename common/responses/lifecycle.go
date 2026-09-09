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

	ResponsePresent      bool
	ResponseObject       bool
	Response             *types.OpenAIResponsesResponses
	ResponseFieldError   error
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
	observed.ResponsePresent = responsePresent
	if !responsePresent || isRawNull(rawResponse) {
		return observed, nil
	}
	responseObject, err := jsonobject.Parse(rawResponse)
	if err != nil {
		return observed, nil
	}
	observed.ResponseObject = true
	response := &types.OpenAIResponsesResponses{}
	var fieldErrors []error
	if response.ID, err = optionalRawString(responseObject.Fields, "id"); err != nil {
		fieldErrors = append(fieldErrors, fmt.Errorf("response.id: %w", err))
	}
	if response.Status, err = optionalRawString(responseObject.Fields, "status"); err != nil {
		fieldErrors = append(fieldErrors, fmt.Errorf("response.status: %w", err))
	}
	if response.Model, err = optionalRawString(responseObject.Fields, "model"); err != nil {
		fieldErrors = append(fieldErrors, fmt.Errorf("response.model: %w", err))
	}
	if response.ServiceTier, err = optionalRawString(responseObject.Fields, "service_tier"); err != nil {
		fieldErrors = append(fieldErrors, fmt.Errorf("response.service_tier: %w", err))
	}
	if rawError, ok := responseObject.Fields["error"]; ok && !isRawNull(rawError) {
		observed.ResponseErrorPresent = true
		var responseError types.OpenAIError
		if err := json.Unmarshal(rawError, &responseError); err == nil {
			response.Error = &responseError
		}
	}

	// A full decode enriches output and valid usage, but failure in a
	// non-lifecycle field must not erase the lifecycle facts above.
	var full types.OpenAIResponsesResponses
	if err := json.Unmarshal(rawResponse, &full); err == nil {
		response = &full
	}
	observed.Response = response
	observed.ResponseFieldError = errors.Join(fieldErrors...)
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
