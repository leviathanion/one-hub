package common

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"testing"

	"one-api/common/config"
	"one-api/types"

	"github.com/pkoukk/tiktoken-go"
)

func tokenTestImageDataURL(t *testing.T, width, height int) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, img); err != nil {
		t.Fatalf("encode test image: %v", err)
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(encoded.Bytes())
}

func TestUnknownModelUsesDefaultO200kEncoder(t *testing.T) {
	originalDisable := config.DisableTokenEncoders
	originalGPT35 := gpt35TokenEncoder
	originalGPT4o := gpt4oTokenEncoder
	originalMap := tokenEncoderMap
	config.DisableTokenEncoders = false
	gpt35TokenEncoder = &tiktoken.Tiktoken{}
	gpt4oTokenEncoder = &tiktoken.Tiktoken{}
	tokenEncoderMap = map[string]*tiktoken.Tiktoken{}
	t.Cleanup(func() {
		config.DisableTokenEncoders = originalDisable
		gpt35TokenEncoder = originalGPT35
		gpt4oTokenEncoder = originalGPT4o
		tokenEncoderMap = originalMap
	})

	for _, modelName := range []string{"gpt-5.6", "vendor-future-model"} {
		if got := GetTokenEncoder(modelName); got != gpt4oTokenEncoder {
			t.Fatalf("expected unknown model %s to use the generic o200k fallback", modelName)
		}
	}
}

func TestOpenAIImageAdmissionEstimateIsDetailAndModelNameAgnostic(t *testing.T) {
	imageURL := tokenTestImageDataURL(t, 1024, 1024)
	for _, detail := range []string{"high", "original", "auto", "", "future-detail"} {
		got, err := countOpenaiImageTokens(imageURL, detail, "future-openai-model")
		if err != nil {
			t.Fatalf("detail %q returned an error: %v", detail, err)
		}
		if got != 1024 {
			t.Fatalf("detail %q estimate=%d want conservative patch/tile maximum 1024", detail, got)
		}
	}
	low, err := countOpenaiImageTokens(imageURL, "low", "future-openai-model")
	if err != nil || low != patchGridLowImageTokenFloor {
		t.Fatalf("low detail estimate=%d err=%v", low, err)
	}
	fallback, err := countOpenaiImageTokens("not-an-image", "original", "future-openai-model")
	if err != nil || fallback != patchGridLowImageTokenFloor {
		t.Fatalf("unobservable image must use a small bounded admission floor, got=%d err=%v", fallback, err)
	}
}

func TestResponsesPDFAdmissionEstimateUsesWireDetailConservatively(t *testing.T) {
	pdfData := "data:application/pdf;base64," + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("p"), 100))
	for _, test := range []struct {
		detail string
		want   int
	}{
		{detail: "low", want: pdfLowVisualTokenFloor},
		{detail: "high", want: pdfHighVisualTokenFloor},
		{detail: "auto", want: pdfHighVisualTokenFloor},
		{detail: "", want: pdfHighVisualTokenFloor},
		{detail: "future-detail", want: pdfHighVisualTokenFloor},
	} {
		got := countResponsesFilePartTokens(pdfData, "", "document.pdf", test.detail)
		if got != test.want {
			t.Fatalf("detail %q estimate=%d want=%d", test.detail, got, test.want)
		}
	}
	if got := countResponsesFilePartTokens("", "https://example.com/document.pdf", "", "high"); got != pdfHighVisualTokenFloor {
		t.Fatalf("remote PDF without local size evidence must use the small visual floor, got %d", got)
	}
	if got := countResponsesFilePartTokens("plain text", "", "notes.txt", ""); got != 0 {
		t.Fatalf("non-PDF input must not be assigned PDF visual tokens, got %d", got)
	}
}

func TestCountTokenInputMessagesIncludesInlinePDFAdmission(t *testing.T) {
	originalDisable := config.DisableTokenEncoders
	config.DisableTokenEncoders = true
	t.Cleanup(func() { config.DisableTokenEncoders = originalDisable })

	pdfData := "data:application/pdf;base64," + base64.StdEncoding.EncodeToString([]byte("small pdf"))
	input := []any{map[string]any{
		"type": "message",
		"role": "user",
		"content": []any{map[string]any{
			"type": "input_file", "filename": "document.pdf", "file_data": pdfData, "detail": "auto",
		}},
	}}
	if got := CountTokenInputMessages(input, "any-model", config.PreCostDefault); got < pdfHighVisualTokenFloor {
		t.Fatalf("Responses PDF was omitted from admission estimate: %d", got)
	}
}

func legacyCountTokenInputMessagesFallback(t *testing.T, input any, model string, preCostType int) int {
	t.Helper()

	jsonStr, err := json.Marshal(input)
	if err != nil {
		t.Fatalf("marshal input: %v", err)
	}

	var messages []types.ChatCompletionMessage
	if err := json.Unmarshal(jsonStr, &messages); err != nil {
		t.Fatalf("unmarshal messages: %v", err)
	}

	return CountTokenMessages(messages, model, preCostType)
}

func TestCountTokenInputMessagesFastPathMatchesChatMessages(t *testing.T) {
	originalDisable := config.DisableTokenEncoders
	originalApproximate := config.ApproximateTokenEnabled
	config.DisableTokenEncoders = true
	config.ApproximateTokenEnabled = false
	t.Cleanup(func() {
		config.DisableTokenEncoders = originalDisable
		config.ApproximateTokenEnabled = originalApproximate
	})

	input := []any{
		map[string]any{
			"type": "message",
			"role": "user",
			"content": []any{
				map[string]any{
					"type": "input_text",
					"text": "hello from responses",
				},
			},
		},
		map[string]any{
			"type":      "function_call",
			"call_id":   "call_1",
			"name":      "lookup_weather",
			"arguments": `{"city":"shanghai"}`,
		},
		map[string]any{
			"type":    "function_call_output",
			"call_id": "call_1",
			"output":  "sunny",
		},
	}

	expectedMessages := []types.ChatCompletionMessage{
		{
			Role: "user",
			Content: []any{
				map[string]any{
					"type": "text",
					"text": "hello from responses",
				},
			},
		},
		{
			Role: types.ChatMessageRoleAssistant,
			ToolCalls: []*types.ChatCompletionToolCalls{
				{
					Id:   "call_1",
					Type: "function",
					Function: &types.ChatCompletionToolCallsFunction{
						Name:      "lookup_weather",
						Arguments: `{"city":"shanghai"}`,
					},
				},
			},
		},
		{
			Role:       types.ChatMessageRoleTool,
			ToolCallID: "call_1",
			Content:    "sunny",
		},
	}

	got := CountTokenInputMessages(input, "gpt-4o", config.PreCostDefault)
	want := CountTokenMessages(expectedMessages, "gpt-4o", config.PreCostDefault)

	if got != want {
		t.Fatalf("expected fast-path token count %d, got %d", want, got)
	}
}

func TestCountTokenInputMessagesFallsBackForUnsupportedResponsesItems(t *testing.T) {
	originalDisable := config.DisableTokenEncoders
	originalApproximate := config.ApproximateTokenEnabled
	config.DisableTokenEncoders = true
	config.ApproximateTokenEnabled = false
	t.Cleanup(func() {
		config.DisableTokenEncoders = originalDisable
		config.ApproximateTokenEnabled = originalApproximate
	})

	input := []any{
		map[string]any{
			"type": "message",
			"role": "user",
			"content": []any{
				map[string]any{
					"type": "input_text",
					"text": "hello from responses",
				},
			},
		},
		map[string]any{
			"type": types.InputTypeReasoning,
			"summary": []any{
				map[string]any{
					"type": types.ContentTypeSummaryText,
					"text": "internal reasoning summary",
				},
			},
		},
	}

	if _, ok := responsesInputToMessagesFast(input); ok {
		t.Fatal("expected mixed responses input to bypass fast path")
	}

	got := CountTokenInputMessages(input, "gpt-4o", config.PreCostDefault)
	want := legacyCountTokenInputMessagesFallback(t, input, "gpt-4o", config.PreCostDefault)

	if got != want {
		t.Fatalf("expected fallback token count %d, got %d", want, got)
	}
}
