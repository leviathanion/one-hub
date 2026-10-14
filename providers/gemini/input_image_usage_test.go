package gemini

import (
	"bytes"
	"slices"
	"testing"

	"one-api/common/config"
	"one-api/types"
)

func TestNativeInputImageRequirementsObserveToolAttachments(t *testing.T) {
	for _, test := range []struct {
		name, part string
		image      bool
	}{
		{"inline image", `{"inlineData":{"mimeType":"image/png","data":"aW1hZ2U="}}`, true},
		{"file image", `{"fileData":{"mimeType":"image/png","fileUri":"gs://bucket/image"}}`, true},
		{"tool inline image", `{"functionResponse":{"name":"inspect","response":{},"parts":[{"inlineData":{"mimeType":" IMAGE/PNG ","data":"aW1hZ2U="}}]}}`, true},
		{"tool file image", `{"functionResponse":{"name":"inspect","response":{},"parts":[{"fileData":{"mimeType":"image/png","fileUri":"gs://bucket/image"}}]}}`, true},
		{"tool text", `{"functionResponse":{"name":"inspect","response":{"result":"done"}}}`, false},
		{"tool audio", `{"functionResponse":{"name":"inspect","response":{},"parts":[{"inlineData":{"mimeType":"audio/wav","data":"YXVkaW8="}}]}}`, false},
		{"tool business JSON", `{"functionResponse":{"name":"inspect","response":{"parts":[{"inlineData":{"mimeType":"image/png","data":"aW1hZ2U="}}]}}}`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw := []byte(`{"contents":[{"role":"user","parts":[` + test.part + `]}],"future":{"union":[null,900719925474099312345]}}`)
			request, err := ParseNativeRequest(raw, "model", false)
			if err != nil {
				t.Fatal(err)
			}
			usage := &types.Usage{}
			request.ApplyUsageRequirements(usage)
			if got := slices.Contains(usage.RequiredTokenExtraKeys, config.UsageExtraInputImageTokens); got != test.image {
				t.Fatalf("输入图片用量要求 = %t, 期望 %t: %+v", got, test.image, usage)
			}
			if !bytes.Equal(request.Raw, raw) {
				t.Fatalf("用量观察改写原生请求: %s", request.Raw)
			}
		})
	}
}
