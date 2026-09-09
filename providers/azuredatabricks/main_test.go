package azuredatabricks

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"one-api/types"
)

func TestConvertResponsePublishesUnaryProviderUsage(t *testing.T) {
	provider := &AzureDatabricksProvider{}
	provider.Usage = &types.Usage{}
	response, err := provider.convertResponse(&http.Response{Body: io.NopCloser(strings.NewReader(`{
		"id":"db-1","model":"databricks-actual","choices":[],
		"usage":{"prompt_tokens":7,"completion_tokens":0,"total_tokens":7}
	}`))})
	if err != nil {
		t.Fatalf("convertResponse returned error: %v", err)
	}
	if response.Usage == nil || !provider.Usage.HasProviderUsage() || provider.Usage.CompletionTokens != 0 {
		t.Fatalf("unary provider usage was not published: response=%+v settlement=%+v", response.Usage, provider.Usage)
	}
	if provider.Usage.ResponseModel != "databricks-actual" {
		t.Fatalf("actual model was not attributed: %+v", provider.Usage)
	}
}

func TestConvertResponseDoesNotAuthorizePartialUnaryUsage(t *testing.T) {
	provider := &AzureDatabricksProvider{}
	provider.Usage = &types.Usage{}
	_, err := provider.convertResponse(&http.Response{Body: io.NopCloser(strings.NewReader(`{
		"id":"db-2","model":"databricks-actual","choices":[],"usage":{"total_tokens":7}
	}`))})
	if err != nil {
		t.Fatalf("convertResponse returned error: %v", err)
	}
	if provider.Usage.HasProviderUsage() {
		t.Fatalf("partial Databricks usage became priceable: %+v", provider.Usage)
	}
}
