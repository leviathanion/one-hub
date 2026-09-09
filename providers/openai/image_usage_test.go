package openai

import (
	"encoding/json"
	"testing"

	"one-api/common/config"
	"one-api/types"
)

func TestOpenAIImageUsagePreservesAllTokenBuckets(t *testing.T) {
	var response types.ImageResponse
	if err := json.Unmarshal([]byte(`{
		"model":"gpt-image-actual",
		"data":[{"b64_json":"image"}],
		"usage":{
			"input_tokens":10,"output_tokens":5,"total_tokens":15,
			"input_tokens_details":{"text_tokens":6,"image_tokens":4},
			"output_tokens_details":{"text_tokens":1,"image_tokens":4}
		}
	}`), &response); err != nil {
		t.Fatalf("decode image response: %v", err)
	}
	target := &types.Usage{}
	ApplyImageEvidence(target, &response)
	if !target.HasProviderUsage() || target.ResponseModel != "gpt-image-actual" {
		t.Fatalf("image token usage was not authorized: %+v", target)
	}
	extra := target.GetExtraTokens()
	for key, want := range map[string]int{
		config.UsageExtraInputTextTokens:   6,
		config.UsageExtraInputImageTokens:  4,
		config.UsageExtraOutputTextTokens:  1,
		config.UsageExtraOutputImageTokens: 4,
	} {
		if extra[key] != want {
			t.Fatalf("image token bucket %s=%d want %d: %+v", key, extra[key], want, target)
		}
	}
	if target.ProviderOperationUnits == nil || *target.ProviderOperationUnits != 1 {
		t.Fatalf("image output count was not recorded: %+v", target)
	}
}

func TestOpenAIImageUsageRejectsMissingOutputDetails(t *testing.T) {
	var response types.ImageResponse
	if err := json.Unmarshal([]byte(`{
		"data":[{"b64_json":"image"}],
		"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15,"input_tokens_details":{"text_tokens":6,"image_tokens":4}}
	}`), &response); err != nil {
		t.Fatalf("decode image response: %v", err)
	}
	target := &types.Usage{}
	ApplyImageEvidence(target, &response)
	if target.HasProviderUsage() {
		t.Fatalf("partial image token usage became priceable: %+v", target)
	}
}

func TestExactImageDataCountRequiresAnExplicitArray(t *testing.T) {
	for _, test := range []struct {
		name      string
		payload   string
		wantCount *int
	}{
		{name: "missing", payload: `{}`},
		{name: "null", payload: `{"data":null}`},
		{name: "future union", payload: `{"data":{"future":true}}`},
		{name: "empty array", payload: `{"data":[]}`, wantCount: intPointer(0)},
		{name: "two items", payload: `{"data":[{"future":true},"variant"]}`, wantCount: intPointer(2)},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := &OpenAIProviderImageResponse{}
			if err := response.DecodeCapturedProviderJSON([]byte(test.payload)); err != nil {
				t.Fatal(err)
			}
			target := &types.Usage{}
			applyImageEvidence(target, &response.ImageResponse, response.providerDataCount)
			if test.wantCount == nil {
				if target.ProviderOperationUnits != nil {
					t.Fatalf("unconfirmed data became zero units: %+v", target)
				}
				return
			}
			if target.ProviderOperationUnits == nil || *target.ProviderOperationUnits != *test.wantCount {
				t.Fatalf("count=%v want=%d", target.ProviderOperationUnits, *test.wantCount)
			}
		})
	}
}

func intPointer(value int) *int { return &value }

func TestTypedImageDataCountDistinguishesMissingFromZero(t *testing.T) {
	for _, raw := range []string{`{}`, `{"data":null}`, `{"data":[]}`, `{"data":[{"url":"image"}]}`} {
		t.Run(raw, func(t *testing.T) {
			var response OpenAIProviderImageResponse
			if err := json.Unmarshal([]byte(raw), &response); err != nil {
				t.Fatal(err)
			}
			target := &types.Usage{}
			ApplyImageEvidence(target, &response.ImageResponse)
			for _, count := range []*int{target.ProviderOperationUnits, response.confirmedDataCount(false)} {
				if response.Data == nil {
					if count != nil {
						t.Fatalf("missing data fabricated confirmed zero: %d", *count)
					}
				} else if count == nil || *count != len(response.Data) {
					t.Fatalf("explicit count lost: %v", count)
				}
			}
		})
	}
}
