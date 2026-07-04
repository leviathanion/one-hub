package wire

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"

	"one-api/common/responsesws"
)

type FramePatchInput struct {
	Identity                  Identity
	Model                     string
	DefaultPreviousResponseID string
	ResponsesLite             bool
	Clock                     Clock
}

func PlanResponsesWSFrame(frame *responsesws.RawResponsesCreateFrame, in FramePatchInput) ([]byte, error) {
	if frame == nil {
		return nil, reject("response.create", "first response.create frame is required")
	}
	model := strings.TrimSpace(in.Model)
	if model == "" {
		return nil, reject("model", "model is required")
	}
	object := cloneRawMap(frame.Object)
	encodedModel, err := json.Marshal(model)
	if err != nil {
		return nil, err
	}
	object["model"] = encodedModel
	if _, exists := object["previous_response_id"]; !exists && strings.TrimSpace(in.DefaultPreviousResponseID) != "" {
		encodedPrevious, err := json.Marshal(strings.TrimSpace(in.DefaultPreviousResponseID))
		if err != nil {
			return nil, err
		}
		object["previous_response_id"] = encodedPrevious
	}

	metadata, err := MetadataFromResponsesFrame(frame)
	if err != nil {
		return nil, err
	}
	if err := validateMetadata(metadata); err != nil {
		return nil, err
	}
	if err := validateFrameMetadataIdentity(metadata, in.Identity); err != nil {
		return nil, err
	}
	metadataFields := cloneRawMap(metadata.Fields)
	if metadataFields == nil {
		metadataFields = make(map[string]json.RawMessage)
	}
	if err := setMetadataIfMissing(metadataFields, "session_id", in.Identity.SessionID); err != nil {
		return nil, err
	}
	if err := setMetadataIfMissing(metadataFields, "thread_id", in.Identity.ThreadID); err != nil {
		return nil, err
	}
	if err := setMetadataIfMissing(metadataFields, "x-codex-window-id", in.Identity.WindowID); err != nil {
		return nil, err
	}
	if err := setMetadataIfMissing(metadataFields, "x-codex-installation-id", in.Identity.InstallationID); err != nil {
		return nil, err
	}
	if in.ResponsesLite {
		if err := setMetadataIfMissing(metadataFields, "ws_request_header_x_openai_internal_codex_responses_lite", "true"); err != nil {
			return nil, err
		}
	}
	clock := in.Clock
	if clock == nil {
		clock = RealClock{}
	}
	metadataFields["x-codex-ws-stream-request-start-ms"], err = marshalRaw(strconv.FormatInt(clock.Now().UnixMilli(), 10))
	if err != nil {
		return nil, err
	}
	object["client_metadata"], err = marshalRaw(metadataFields)
	if err != nil {
		return nil, err
	}
	return json.Marshal(object)
}

func validateFrameMetadataIdentity(metadata Metadata, identity Identity) error {
	checks := []struct {
		key   string
		value string
		valid func(string) bool
	}{
		{key: "session_id", value: identity.SessionID, valid: validID},
		{key: "thread_id", value: identity.ThreadID, valid: validID},
		{key: "x-codex-window-id", value: identity.WindowID, valid: validID},
		{key: "x-codex-installation-id", value: identity.InstallationID, valid: validInstallationID},
	}
	for _, check := range checks {
		expected := strings.TrimSpace(check.value)
		value, state, err := metadata.String(check.key, check.valid)
		if err != nil {
			return err
		}
		if expected == "" {
			continue
		}
		if state != FieldMissing && (state != FieldPresent || value != expected) {
			return reject("client_metadata."+check.key, "metadata identity does not match open connection")
		}
	}
	return nil
}

func frameMetadataObject(object map[string]json.RawMessage) (map[string]json.RawMessage, error) {
	raw, ok := object["client_metadata"]
	if !ok || len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		if ok && bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return nil, reject("client_metadata", "client_metadata must be a JSON object")
		}
		return make(map[string]json.RawMessage), nil
	}
	if bytes.TrimSpace(raw)[0] != '{' {
		return nil, reject("client_metadata", "client_metadata must be a JSON object")
	}
	metadata := make(map[string]json.RawMessage)
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&metadata); err != nil {
		return nil, reject("client_metadata", "client_metadata must be a JSON object")
	}
	return cloneRawMap(metadata), nil
}

func setMetadataIfMissing(metadata map[string]json.RawMessage, key, value string) error {
	if metadata == nil || strings.TrimSpace(value) == "" {
		return nil
	}
	if _, exists := metadata[key]; exists {
		return nil
	}
	raw, err := marshalRaw(strings.TrimSpace(value))
	if err != nil {
		return err
	}
	metadata[key] = raw
	return nil
}

func marshalRaw(value any) (json.RawMessage, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return raw, nil
}
