package relay

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"one-api/common/providerresponse"

	"github.com/gin-gonic/gin"
)

func TestAudioSSEProtocolsPreserveEventsAndRequireOwnTerminal(t *testing.T) {
	for _, test := range []struct {
		name      string
		protocol  audioSSEProtocol
		operation providerresponse.Operation
		deltaType string
		doneType  string
	}{
		{name: "speech", protocol: audioSSESpeech, operation: providerresponse.OperationBinaryDownload, deltaType: "speech.audio.delta", doneType: "speech.audio.done"},
		{name: "transcription", protocol: audioSSETranscription, operation: providerresponse.OperationAudioTranscription, deltaType: "transcript.text.delta", doneType: "transcript.text.done"},
	} {
		t.Run(test.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/audio", nil)
			wire := "event: " + test.deltaType + "\r\ndata: {\"type\":\"" + test.deltaType + "\",\"future\":{\"kept\":true}}\r\n\r\n" +
				"event: " + test.doneType + "\r\ndata: {\"type\":\"" + test.doneType + "\"}\r\n\r\n"
			response := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(wire))}
			_, apiErr := responseAudioSSEClient(ctx, response, test.protocol, test.operation)
			if apiErr != nil {
				t.Fatalf("valid %s SSE failed: %+v", test.name, apiErr)
			}
			if recorder.Body.String() != wire {
				t.Fatalf("%s SSE wire changed:\nwant %q\n got %q", test.name, wire, recorder.Body.String())
			}
		})
	}
}

func TestAudioSSEProviderErrorIsSanitizedAndRenderedOnce(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/audio/speech", nil)
	wire := "event: error\ndata: {\"type\":\"error\",\"error\":{\"message\":\"organization org-secret rejected\",\"code\":\"upstream_failed\"},\"account_id\":\"acct-secret\"}\n\n"
	response := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(wire))}
	_, apiErr := responseAudioSSEClient(ctx, response, audioSSESpeech, providerresponse.OperationBinaryDownload)
	if apiErr == nil || !apiErr.UpstreamAccepted {
		t.Fatalf("provider audio error was treated as success: %+v", apiErr)
	}
	body := recorder.Body.String()
	if strings.Count(body, "event: error") != 1 || !ctx.GetBool(streamErrorAlreadyRenderedContextKey) {
		t.Fatalf("provider audio error was not rendered exactly once: %q", body)
	}
	for _, secret := range []string{"org-secret"} {
		if strings.Contains(body, secret) {
			t.Fatalf("audio SSE leaked %q: %q", secret, body)
		}
	}
}

func TestAudioSSEMissingTerminalGetsProtocolErrorBoundary(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", nil)
	wire := "event: transcript.text.delta\ndata: {\"type\":\"transcript.text.delta\",\"delta\":\"partial\"}\n\n"
	response := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(wire))}
	_, apiErr := responseAudioSSEClient(ctx, response, audioSSETranscription, providerresponse.OperationAudioTranscription)
	if apiErr == nil {
		t.Fatal("truncated transcription stream was accepted")
	}
	body := recorder.Body.String()
	if !strings.Contains(body, wire+"event: error\ndata:") || strings.Count(body, "event: error") != 1 {
		t.Fatalf("protocol error framing invalid: %q", body)
	}
}
