package relay

import (
	"github.com/gin-gonic/gin"
	"io"
	"net/http"
	"net/http/httptest"
	"one-api/common/config"
	"one-api/common/providerresponse"
	"one-api/common/requestctx"
	"one-api/common/requester"
	commonresponses "one-api/common/responses"
	"one-api/model"
	"one-api/providers/openai"
	"one-api/types"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

func TestProviderRedactionFailedEventDiagnostics(t *testing.T) {
	for _, raw := range []string{
		`{"type":"response.failed","response":{"id":"resp_test","status":"failed","error":{"message":"bad input","code":"invalid_request_error"}},"debug":{"access_token":"body-owned"}}`,
		`{"type":"response.failed","response":{"id":"resp_test","status":"failed","Error":{"message":"organization org-private exhausted","code":"insufficient_quota","account_id":"acct-secret"}}}`,
	} {
		t.Run(raw, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			stream := &fakeRelayStream{dataChan: make(chan string, 1), errChan: make(chan error)}
			stream.dataChan <- "data: " + raw + "\n\n"
			close(stream.dataChan)
			close(stream.errChan)
			_, apiErr := responseNativeResponsesStreamClient(c, stream, commonresponses.NewStreamObserver())
			body := recorder.Body.String()
			if body == "" {
				t.Fatalf("fixture not delivered: %+v", apiErr)
			}
			if strings.Contains(body, "acct-secret") || strings.Contains(body, "org-private") {
				t.Fatalf("provider diagnostic leaked: %s", body)
			}
		})
	}
}
func TestProviderRedactionAudioJSONBoundary(t *testing.T) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", nil)
	requestctx.SetProviderCredentials(c, []string{"provider-secret-12345"})
	err := responseCustom(c, &types.AudioResponseWrapper{Headers: map[string]string{"Content-Type": "application/json", "X-Request-Id": "provider-secret-12345"}, Body: []byte(`{"text":"provider-secret-12345","account_id":"acct-secret"}`)}, providerresponse.OperationAudioTranscription)
	if err != nil {
		t.Fatal(err)
	}
	if recorder.Body.String() != `{"text":"provider-secret-12345","account_id":"acct-secret"}` || recorder.Header().Get("X-Request-Id") != "provider-secret-12345" {
		t.Fatalf("audio JSON bypass: header=%v body=%s", recorder.Header(), recorder.Body.String())
	}
}

func TestProviderRedactionRealOpenAITranscriptionJSON(t *testing.T) {
	const secret = "provider-secret-12345"
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+secret {
			t.Errorf("actual auth=%q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Request-Id", secret)
		io.WriteString(w, `{"text":"provider-secret-12345","account_id":"acct-secret","usage":{"type":"duration","seconds":9}}`)
	}))
	defer upstream.Close()
	oldClient := requester.HTTPClient
	requester.HTTPClient = upstream.Client()
	defer func() { requester.HTTPClient = oldClient }()
	c := issue047RelayTranscriptionContext(t, false)
	proxy := ""
	p := openai.CreateOpenAIProvider(&model.Channel{Type: config.ChannelTypeOpenAI, Key: secret, Proxy: &proxy}, upstream.URL)
	p.SetContext(c)
	p.SetUsage(&types.Usage{})
	r := NewRelayTranscriptions(c)
	if err := r.setRequest(); err != nil {
		t.Fatal(err)
	}
	r.provider = p
	r.modelName = issue047RelayModel
	if err, _ := r.send(); err != nil {
		t.Fatal(err)
	}
	if len(requestctx.ProviderCredentials(c)) == 0 {
		t.Fatal("snapshot absent")
	}
	usage := p.GetUsage()
	if calls.Load() != 1 || usage.ProviderOperationUnits == nil || *usage.ProviderOperationUnits != 1 || usage.ExtraUsageUnits[config.UsageExtraInputAudioTranscription] != 9 {
		t.Fatalf("脱敏丢失计费证据或重新请求上游: calls=%d usage=%+v", calls.Load(), usage)
	}
	body := recorderBody(c)
	if body != `{"text":"provider-secret-12345","account_id":"acct-secret","usage":{"type":"duration","seconds":9}}` || c.Writer.Header().Get("X-Request-Id") != secret {
		t.Fatalf("actual OpenAI transcription JSON leak: header=%v body=%s", c.Writer.Header(), body)
	}
}

func TestProviderRedactionAudioBodyContracts(t *testing.T) {
	const secret = "provider-secret-12345"
	for _, test := range []struct {
		name, raw, contentType string
	}{
		{name: "JSON with no content type", raw: `{"text":"provider-secret-12345","account_id":"private","future":9007199254740993}`},
		{name: "safe JSON", raw: `{ "text":"token=example", "future": [null,1e30] }`, contentType: "application/json"},
		{name: "VTT", raw: "WEBVTT\n\n00:00:01.000 --> 00:00:02.000\nprovider-secret-12345 account_id=example\n", contentType: "text/vtt"},
		{name: "safe text", raw: "  account_id=example\n", contentType: "text/plain"},
		{name: "duplicate JSON", raw: `{"text":"one","text":"two"}`, contentType: "application/json"},
		{name: "truncated JSON", raw: `{"text":`, contentType: "application/json"},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", nil)
			requestctx.SetProviderCredentials(c, []string{secret})
			response := &types.AudioResponseWrapper{Body: []byte(test.raw), Headers: map[string]string{"Content-Type": test.contentType, "X-Request-Id": secret, "Digest": "original", "Content-Length": strconv.Itoa(len(test.raw))}}
			apiErr := responseCustom(c, response, providerresponse.OperationAudioTranscription)
			if apiErr != nil || recorder.Body.String() != test.raw || recorder.Header().Get("X-Request-Id") != secret || recorder.Header().Get("Digest") != "original" || recorder.Header().Get("Content-Length") != strconv.Itoa(len(test.raw)) {
				t.Fatalf("正文或表示头被脱敏改变: %q %v err=%v", recorder.Body.String(), recorder.Header(), apiErr)
			}
			if string(response.Body) != test.raw || response.Headers["Digest"] != "original" {
				t.Fatal("交付修改了原始观察副本")
			}
		})
	}
}
