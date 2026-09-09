package base

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"one-api/model"
)

func TestCommonRequestHeadersUsesImmutableIdentityWithBusinessHeaders(t *testing.T) {
	raw := `{"openai-organization":" org-a ","OPENAI-PROJECT":"project-a","x-business-tag":"new"}`
	provider := &BaseProvider{Channel: &model.Channel{ModelHeaders: &raw}}
	headers := map[string]string{"OPENAI-ORGANIZATION": "untrusted-org", "openai-project": "untrusted-project"}
	provider.CommonRequestHeaders(headers)
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("OpenAI-Organization") != "org-a" || request.Header.Get("OpenAI-Project") != "project-a" || request.Header.Get("X-Business-Tag") != "new" {
			t.Errorf("unexpected provider headers: %+v", request.Header)
		}
		if len(request.Header.Values("OpenAI-Organization")) != 1 || len(request.Header.Values("OpenAI-Project")) != 1 {
			t.Errorf("duplicate provider identity headers: %+v", request.Header)
		}
	}))
	defer upstream.Close()
	request, _ := http.NewRequest(http.MethodGet, upstream.URL, nil)
	for name, value := range headers {
		request.Header.Add(name, value)
	}
	response, err := upstream.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
}
