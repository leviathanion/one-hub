package channel

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"one-api/common/requester"
)

func TestSearxngFollowsBoundedReadOnlyRedirect(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"query":"one","number_of_results":1,"results":[{"url":"https://result.example","title":"result","content":"ok"}]}`))
	}))
	defer target.Close()

	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/search", http.StatusTemporaryRedirect)
	}))
	defer source.Close()

	original := requester.HTTPClient
	requester.HTTPClient = source.Client()
	t.Cleanup(func() { requester.HTTPClient = original })

	result, err := NewSearxng(source.URL + "/search?q={query}").Query("one")
	if err != nil {
		t.Fatalf("query through redirect: %v", err)
	}
	if len(result.Results) != 1 || result.Results[0].Title != "result" {
		t.Fatalf("unexpected result: %+v", result)
	}
}
