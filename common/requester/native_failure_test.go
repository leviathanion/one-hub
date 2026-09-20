package requester

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"one-api/common/providerresponse"
	"one-api/types"
)

func TestCheckedRawFailuresPreserveAmbiguousSubmission(t *testing.T) {
	for _, status := range []int{400, 408, 429, 500, 502, 503} {
		for _, native := range []bool{false, true} {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				w.Write([]byte(`{"error":{"message":"upstream rejected","type":"provider_error"}}`))
			}))
			old := HTTPClient
			HTTPClient = server.Client()
			requester := NewHTTPRequester("", func(*http.Response) *types.OpenAIError {
				return &types.OpenAIError{Message: "upstream rejected", Type: "provider_error"}
			})
			req, _ := http.NewRequest(http.MethodPost, server.URL, nil)
			var apiErr *types.OpenAIErrorWithStatusCode
			if native {
				_, apiErr = requester.SendRequestRawCheckedNativeDialect(req, providerresponse.OperationUnknown)
			} else {
				_, apiErr = requester.SendRequestRawCheckedPreservingRedirect(req, providerresponse.OperationUnknown)
			}
			HTTPClient = old
			server.Close()
			if apiErr == nil || apiErr.UpstreamAmbiguous != (status == 408 || status >= 500) {
				t.Fatalf("native=%t status=%d err=%+v", native, status, apiErr)
			}
		}
	}
}
