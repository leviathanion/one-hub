package wire

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"one-api/common/jsonobject"
	"one-api/common/responsesws"
)

type Metadata struct {
	Fields map[string]json.RawMessage
	Source Source
}

func MetadataFromResponsesBody(object *jsonobject.Object) (Metadata, error) {
	if object == nil {
		return Metadata{Source: SourceBodyMetadata}, nil
	}
	raw, ok := object.Fields["client_metadata"]
	if !ok || len(bytes.TrimSpace(raw)) == 0 {
		return Metadata{Source: SourceBodyMetadata}, nil
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return Metadata{}, reject("client_metadata", "must be a JSON object")
	}
	fields, err := parseMetadataObject(raw)
	if err != nil {
		return Metadata{}, err
	}
	return Metadata{Fields: fields, Source: SourceBodyMetadata}, nil
}

func MetadataFromResponsesFrame(frame *responsesws.RawResponsesCreateFrame) (Metadata, error) {
	if frame == nil || frame.Object == nil {
		return Metadata{Source: SourceFrameMetadata}, nil
	}
	raw, ok := frame.Object["client_metadata"]
	if !ok || len(bytes.TrimSpace(raw)) == 0 {
		return Metadata{Source: SourceFrameMetadata}, nil
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return Metadata{}, reject("client_metadata", "must be a JSON object")
	}
	fields, err := parseMetadataObject(raw)
	if err != nil {
		return Metadata{}, err
	}
	return Metadata{Fields: fields, Source: SourceFrameMetadata}, nil
}

func parseMetadataObject(raw json.RawMessage) (map[string]json.RawMessage, error) {
	if len(raw) > 64*1024 {
		return nil, reject("client_metadata", "serialized metadata exceeds 64 KiB")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	fields := make(map[string]json.RawMessage)
	if err := decoder.Decode(&fields); err != nil {
		return nil, reject("client_metadata", "must be a JSON object")
	}
	if fields == nil {
		return nil, reject("client_metadata", "must be a JSON object")
	}
	return cloneRawMap(fields), nil
}

func cloneRawMap(input map[string]json.RawMessage) map[string]json.RawMessage {
	if input == nil {
		return nil
	}
	out := make(map[string]json.RawMessage, len(input))
	for key, value := range input {
		out[key] = append(json.RawMessage(nil), value...)
	}
	return out
}

func (m Metadata) String(key string, valid func(string) bool) (string, FieldState, error) {
	if m.Fields == nil {
		return "", FieldMissing, nil
	}
	raw, ok := m.Fields[key]
	if !ok {
		return "", FieldMissing, nil
	}
	value, err := metadataStringValue(key, raw)
	if err != nil {
		return "", FieldInvalid, err
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return "", FieldEmpty, nil
	}
	if valid != nil && !valid(value) {
		return "", FieldInvalid, reject("client_metadata."+key, "metadata value is invalid")
	}
	return value, FieldPresent, nil
}

func metadataStringValue(key string, raw json.RawMessage) (string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return "", nil
	}
	if len(trimmed) > 0 && trimmed[0] == '"' {
		var value string
		if err := json.Unmarshal(trimmed, &value); err != nil {
			return "", err
		}
		return value, nil
	}
	switch key {
	case "x-codex-turn-metadata", "x-codex-turn-state":
		if len(trimmed) > 0 && trimmed[0] == '{' {
			return string(trimmed), nil
		}
	}
	return "", fmt.Errorf("metadata %s must be a string", key)
}

func validateMetadata(metadata Metadata) error {
	for _, spec := range fieldSpecs {
		if !spec.validateMetadata || spec.metadata == "" {
			continue
		}
		_, state, err := metadata.String(spec.metadata, spec.valid)
		if err != nil {
			return err
		}
		if state == FieldInvalid {
			return reject("client_metadata."+spec.metadata, "metadata value is invalid")
		}
	}
	return nil
}
