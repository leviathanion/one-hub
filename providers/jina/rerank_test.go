package jina

import (
	"encoding/json"
	"testing"

	"one-api/types"
)

func TestJinaTotalTokensAreRerankTokenBasis(t *testing.T) {
	var providerUsage types.Usage
	if err := json.Unmarshal([]byte(`{"total_tokens":13}`), &providerUsage); err != nil {
		t.Fatalf("decode Jina usage: %v", err)
	}
	target := &types.Usage{}
	applyJinaRerankUsage(target, &providerUsage)
	if !target.HasProviderUsage() || target.PromptTokens != 13 || target.CompletionTokens != 0 {
		t.Fatalf("Jina total_tokens did not become the rerank basis: %+v", target)
	}

	var missing types.Usage
	if err := json.Unmarshal([]byte(`{}`), &missing); err != nil {
		t.Fatal(err)
	}
	target = &types.Usage{}
	applyJinaRerankUsage(target, &missing)
	if target.HasProviderUsage() {
		t.Fatalf("missing Jina total_tokens became rerank evidence: %+v", target)
	}

	var zero types.Usage
	if err := json.Unmarshal([]byte(`{"total_tokens":0}`), &zero); err != nil {
		t.Fatal(err)
	}
	applyJinaRerankUsage(target, &zero)
	if !target.HasProviderUsage() || target.TotalTokens != 0 {
		t.Fatalf("present zero Jina total_tokens was not authorized: %+v", target)
	}
}
