package relay_util

import (
	"testing"

	"one-api/types"
)

func TestGetDefaultExtraServicePriceSupportsAdditionalServiceTypes(t *testing.T) {
	if got := getDefaultExtraServicePrice(types.APIToolTypeFileSearch, "gpt-5", ""); got != defaultExtraServicePrices.FileSearch {
		t.Fatalf("expected file search default price %v, got %v", defaultExtraServicePrices.FileSearch, got)
	}
	if got := getDefaultExtraServicePrice(types.APIToolTypeCodeInterpreter, "gpt-5", ""); got != defaultExtraServicePrices.CodeInterpreter {
		t.Fatalf("expected code interpreter default price %v, got %v", defaultExtraServicePrices.CodeInterpreter, got)
	}
}
