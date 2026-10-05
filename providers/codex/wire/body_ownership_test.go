package wire

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"

	"one-api/common/jsonobject"
	commonresponses "one-api/common/responses"
)

func TestBodyPlannersDoNotMutateSourceObject(t *testing.T) {
	for _, tc := range []struct {
		name string
		plan func(*jsonobject.Object) ([]byte, error)
	}{
		{"create", func(object *jsonobject.Object) ([]byte, error) {
			return PlanResponsesCreateBody(object, CreateBodyInput{
				Model: "gpt-5-codex", Stream: true,
				PromptCache: &commonresponses.PromptCacheDecision{Key: "pc-policy"},
			})
		}},
		{"compact", func(object *jsonobject.Object) ([]byte, error) {
			return PlanResponsesCompactBody(object, "gpt-5-codex", &commonresponses.PromptCacheDecision{Key: "pc-policy"})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := []byte(`{"model":"gpt-5","input":[{"type":"input_file","file_id":" file-owned "}],"INPUT":"opaque extension","conversation":"conv-owned","Conversation":"opaque extension","stream":false,"store":true,"reasoning":{"effort":"medium"},"include":["output_text.annotations"],"client_metadata":{"future":true},"future":{"n":12345678901234567890}}`)
			object, err := jsonobject.Parse(raw)
			if err != nil {
				t.Fatal(err)
			}
			fieldsBefore := make(map[string]string, len(object.Fields))
			for key, value := range object.Fields {
				fieldsBefore[key] = string(value)
			}
			orderBefore := append([]string(nil), object.Order...)
			body, err := tc.plan(object)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(object.Raw, raw) || !reflect.DeepEqual(object.Order, orderBefore) || len(object.Fields) != len(fieldsBefore) {
				t.Fatal("planner changed the source Raw, Order or field set")
			}
			for key, value := range object.Fields {
				if string(value) != fieldsBefore[key] {
					t.Fatalf("planner changed source field %q", key)
				}
			}
			var planned map[string]json.RawMessage
			if err := json.Unmarshal(body, &planned); err != nil {
				t.Fatal(err)
			}
			for _, key := range []string{"input", "INPUT", "conversation", "Conversation", "future"} {
				if string(planned[key]) != fieldsBefore[key] {
					t.Fatalf("planner changed unowned field %q", key)
				}
			}
			// A returned body can outlive its parsed source without sharing bytes.
			bodyBefore := bytes.Clone(body)
			object.Fields["future"][0] = '['
			object.Raw[0] = '['
			if !bytes.Equal(body, bodyBefore) {
				t.Fatal("planned body aliases the source object")
			}
		})
	}
}

func TestCreateBodyPlannerFailureDoesNotMutateSourceObject(t *testing.T) {
	raw := []byte(`{"model":"gpt-5","reasoning":{"effort":"medium"},"include":"future-union"}`)
	object, err := jsonobject.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	before := object.Clone()
	if _, err := PlanResponsesCreateBody(object, CreateBodyInput{Model: "gpt-5-codex", Stream: true}); err == nil {
		t.Fatal("expected include conversion error after the model/stream patches")
	}
	if !reflect.DeepEqual(object, before) {
		t.Fatal("failed planner changed source object")
	}
}
