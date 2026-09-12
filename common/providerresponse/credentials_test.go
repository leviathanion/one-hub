package providerresponse

import (
	"net/http"
	"strings"
	"testing"
)

func TestRequestCredentialsAndHeaderBoundary(t *testing.T) {
	req, err := http.NewRequest("GET", "https://provider.example/?key=query-secret", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.SetBasicAuth("user", "basic-secret")
	credentials := RequestCredentials(req)
	joined := strings.Join(credentials, " ")
	if !strings.Contains(joined, "basic-secret") || !strings.Contains(joined, "query-secret") {
		t.Fatal("认证值未完整捕获")
	}
	headers := http.Header{"X-Request-Id": {"query-secret"}, "Retry-After": {"3"}, "Set-Cookie": {"private=1"}}
	safe := FilterCredentialHeaders(Filter(headers, Policy{}), credentials...)
	if safe.Get("X-Request-Id") != "" || safe.Get("Set-Cookie") != "" || safe.Get("Retry-After") != "3" {
		t.Fatalf("响应头脱敏错误: %v", safe)
	}
	if headers.Get("X-Request-Id") != "query-secret" {
		t.Fatal("输入响应头被修改")
	}
}

func TestProviderHandshakeCredentialSources(t *testing.T) {
	req, _ := http.NewRequest("GET", "https://provider.example/?authorization=signed-query", nil)
	req.Header.Set("X-Amz-Security-Token", "temporary-session")
	req.Header.Set("Mj-Api-Secret", "mj-secret")
	values := strings.Join(RequestCredentials(req), " ")
	for _, want := range []string{"signed-query", "temporary-session", "mj-secret"} {
		if !strings.Contains(values, want) {
			t.Fatalf("实际认证遗漏 %q", want)
		}
	}
}
