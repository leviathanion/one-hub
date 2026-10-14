package base

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"

	"one-api/common/providerresponse"
	"one-api/model"
)

func TestResponsesCustomParametersRejectLifecycleChanges(t *testing.T) {
	for _, tc := range []struct {
		name   string
		body   string
		params string
		field  string
	}{
		{"delete previous response", `{"previous_response_id":"resp_a"}`, `{"pre_add":true,"remove_params":["previous_response_id"]}`, "previous_response_id"},
		{"inject previous response", `{}`, `{"pre_add":true,"previous_response_id":"resp_a"}`, "previous_response_id"},
		{"delete conversation", `{"conversation":{"id":"conv_a"}}`, `{"pre_add":true,"remove_params":["conversation"]}`, "conversation"},
		{"inject conversation", `{}`, `{"conversation":"conv_a"}`, "conversation"},
		{"change nested conversation", `{"conversation":{"id":"conv_a"}}`, `{"remove_params":["conversation.id"]}`, "conversation"},
		{"delete store", `{"store":false}`, `{"pre_add":true,"remove_params":["store"]}`, "store"},
		{"inject store", `{}`, `{"store":false}`, "store"},
		{"change null presence", `{"store":null}`, `{"remove_params":["store"]}`, "store"},
		{"inject background", `{}`, `{"background":true}`, "background"},
		{"change prompt", `{"prompt":{"id":"prompt_a"}}`, `{"overwrite":true,"prompt":{"id":"prompt_b"}}`, "prompt"},
		{"per model lifecycle", `{"store":false}`, `{"per_model":true,"pre_add":true,"mapped-model":{"remove_params":["store"]}}`, "store"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fields := responsesParameterFields(t, tc.body)
			before, _ := json.Marshal(fields)
			channel := &model.Channel{CustomParameter: &tc.params}
			err := ValidateResponsesCustomParameterCompatibility(channel, fields, "mapped-model", providerresponse.OperationResponsesCreate, true)
			var capabilityError *RequestCapabilityError
			if !errors.As(err, &capabilityError) || capabilityError.Param != tc.field {
				t.Fatalf("expected lifecycle conflict for %s, got %v", tc.field, err)
			}
			after, _ := json.Marshal(fields)
			if !bytes.Equal(before, after) {
				t.Fatalf("assessment changed canonical fields: before=%s after=%s", before, after)
			}
		})
	}
}

func TestResponsesCustomParametersAllowOrdinaryTuning(t *testing.T) {
	params := `{"pre_add":true,"overwrite":true,"store":false,"temperature":0.4}`
	channel := &model.Channel{CustomParameter: &params}
	fields := responsesParameterFields(t, `{"store":false,"future":9007199254740993}`)
	if err := ValidateResponsesCustomParameterCompatibility(channel, fields, "model", providerresponse.OperationResponsesCreate, true); err != nil {
		t.Fatal(err)
	}
	custom, err := channel.GetCustomParameterMap()
	if err != nil {
		t.Fatal(err)
	}
	body := map[string]interface{}{"store": false, "future": json.Number("9007199254740993")}
	result, field, changed, err := ApplyResponsesCustomParameters(body, custom, "model", false, true)
	if err != nil || changed || field != "" || result["store"] != false || result["temperature"] != 0.4 || result["future"] != json.Number("9007199254740993") {
		t.Fatalf("ordinary parameters changed lifecycle or numbers: result=%v field=%s changed=%v err=%v", result, field, changed, err)
	}
}

func TestResponsesCustomParametersRespectPreAddStage(t *testing.T) {
	params := `{"pre_add":true,"remove_params":["previous_response_id"]}`
	channel := &model.Channel{CustomParameter: &params}
	fields := responsesParameterFields(t, `{"previous_response_id":"resp_a"}`)
	if err := ValidateResponsesCustomParameterCompatibility(channel, fields, "model", providerresponse.OperationResponsesCreate, false); err != nil {
		t.Fatalf("already-applied pre_add was evaluated again: %v", err)
	}
	if err := ValidateResponsesCustomParameterCompatibility(channel, fields, "model", providerresponse.OperationResponsesCreate, true); err == nil {
		t.Fatal("pre_add stage did not freeze previous_response_id")
	}
}

func TestResponsesInputTokensPreservesItsLifecycleBoundary(t *testing.T) {
	params := `{"store":false,"background":true,"conversation":"conv_a"}`
	channel := &model.Channel{CustomParameter: &params}
	fields := responsesParameterFields(t, `{"previous_response_id":"resp_a"}`)
	if err := ValidateResponsesCustomParameterCompatibility(channel, fields, "model", providerresponse.OperationResponsesInputTokens, true); err != nil {
		t.Fatalf("input_tokens unexpectedly adopted generation lifecycle restrictions: %v", err)
	}
	params = `{"remove_params":["previous_response_id"]}`
	channel = &model.Channel{CustomParameter: &params}
	var capabilityError *RequestCapabilityError
	err := ValidateResponsesCustomParameterCompatibility(channel, fields, "model", providerresponse.OperationResponsesInputTokens, true)
	if !errors.As(err, &capabilityError) || capabilityError.Param != "previous_response_id" {
		t.Fatalf("input_tokens changed owner route: %v", err)
	}
}

func TestResponsesCustomParametersRetainResourceReferenceCheck(t *testing.T) {
	params := `{"tools":[{"type":"file_search","vector_store_ids":["vs_a"]}]}`
	channel := &model.Channel{CustomParameter: &params}
	var capabilityError *RequestCapabilityError
	err := ValidateResponsesCustomParameterCompatibility(channel, responsesParameterFields(t, `{}`), "model", providerresponse.OperationResponsesCreate, true)
	if !errors.As(err, &capabilityError) || capabilityError.Param != "vector_store_ids" {
		t.Fatalf("unexpected injected resource reference result: %v", err)
	}
}

func responsesParameterFields(t *testing.T, raw string) map[string]json.RawMessage {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &fields); err != nil {
		t.Fatal(err)
	}
	return fields
}
