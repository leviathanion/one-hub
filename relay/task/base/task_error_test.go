package base

import (
	"net/http"
	"testing"

	"one-api/types"
)

func TestOpenAIErrorToTaskErrorPreservesSubmissionFacts(t *testing.T) {
	tests := []struct {
		name         string
		apiErr       *types.OpenAIErrorWithStatusCode
		notAttempted bool
		ambiguous    bool
		accepted     bool
		rejected     bool
	}{
		{name: "pre dispatch", apiErr: &types.OpenAIErrorWithStatusCode{StatusCode: http.StatusInternalServerError, UpstreamNotAttempted: true}, notAttempted: true},
		{name: "ambiguous transport", apiErr: &types.OpenAIErrorWithStatusCode{StatusCode: http.StatusBadGateway, UpstreamAmbiguous: true}, ambiguous: true},
		{name: "accepted malformed success", apiErr: &types.OpenAIErrorWithStatusCode{StatusCode: http.StatusInternalServerError, UpstreamAccepted: true}, accepted: true},
		{name: "definite provider rejection", apiErr: &types.OpenAIErrorWithStatusCode{StatusCode: http.StatusBadRequest}, rejected: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := OpenAIErrToTaskErr(test.apiErr)
			if got.UpstreamNotAttempted != test.notAttempted || got.UpstreamAmbiguous != test.ambiguous || got.UpstreamAccepted != test.accepted || got.ProviderRejected != test.rejected {
				t.Fatalf("submission facts=%+v", got)
			}
		})
	}
}
