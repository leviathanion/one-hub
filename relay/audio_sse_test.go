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

func TestAudioSSEProtocolsPreserveEvents(t *testing.T) {
	for _, test := range []struct {
		name      string
		operation providerresponse.Operation
		deltaType string
		doneType  string
	}{
		{name: "speech", operation: providerresponse.OperationBinaryDownload, deltaType: "speech.audio.delta", doneType: "speech.audio.done"},
		{name: "transcription", operation: providerresponse.OperationAudioTranscription, deltaType: "transcript.text.delta", doneType: "transcript.text.done"},
	} {
		t.Run(test.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/audio", nil)
			wire := "event: " + test.deltaType + "\r\ndata: {\"type\":\"" + test.deltaType + "\",\"future\":{\"kept\":true}}\r\n\r\n" +
				"event: " + test.doneType + "\r\ndata: {\"type\":\"" + test.doneType + "\"}\r\n\r\n"
			response := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(wire))}
			_, apiErr := responseNativeSSEClient(ctx, response, test.operation)
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
	_, apiErr := responseNativeSSEClient(ctx, response, providerresponse.OperationBinaryDownload)
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

func TestAudioSSECleanEOFDoesNotRequireBusinessTerminal(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", nil)
	wire := "event: transcript.text.delta\ndata: {\"type\":\"transcript.text.delta\",\"delta\":\"partial\"}\n\n"
	response := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(wire))}
	_, apiErr := responseNativeSSEClient(ctx, response, providerresponse.OperationAudioTranscription)
	if apiErr != nil {
		t.Fatalf("clean EOF changed into a business error: %+v", apiErr)
	}
	if body := recorder.Body.String(); body != wire {
		t.Fatalf("clean EOF changed raw stream: %q", body)
	}
}

func TestAudioSSEBusinessMarkersDoNotStopRawDelivery(t *testing.T) {
	for _, marker := range []string{
		"event: speech.audio.done\ndata: {\"type\":\"speech.audio.done\"}\n\n",
		"event: error\ndata: {\"type\":\"error\",\"error\":{\"message\":\"rejected\"}}\n\n",
	} {
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/audio/speech", nil)
		wire := marker + ": future comment\r\nid: 42\r\nretry: 100\r\nevent: extension\r\ndata: {\"future\":true}\r\n\r\n"
		response := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(wire))}
		_, apiErr := responseNativeSSEClient(ctx, response, providerresponse.OperationBinaryDownload)
		if strings.HasPrefix(marker, "event: error") != (apiErr != nil) {
			t.Fatalf("business observation error=%+v", apiErr)
		}
		if recorder.Body.String() != wire {
			t.Fatalf("marker truncated or changed stream: %q", recorder.Body.String())
		}
	}
}

type audioSSEFailingReader struct{ wire *strings.Reader }

func (r *audioSSEFailingReader) Read(p []byte) (int, error) {
	n, err := r.wire.Read(p)
	if err == io.EOF {
		return n, io.ErrUnexpectedEOF
	}
	return n, err
}

func TestAudioSSEDoneDoesNotMaskTransportFailure(t *testing.T) {
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/audio/speech", nil)
	wire := "event: speech.audio.done\ndata: {\"type\":\"speech.audio.done\"}\n\n"
	response := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(&audioSSEFailingReader{wire: strings.NewReader(wire)})}
	_, apiErr := responseNativeSSEClient(ctx, response, providerresponse.OperationBinaryDownload)
	if apiErr == nil || !apiErr.UpstreamAccepted {
		t.Fatalf("done masked real transport failure: %+v", apiErr)
	}
	if !strings.HasPrefix(recorder.Body.String(), wire) {
		t.Fatalf("completed prefix missing: %q", recorder.Body.String())
	}
}

func TestAudioSSECapacityFailureRemainsLocalTransportBoundary(t *testing.T) {
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/audio/speech", nil)
	response := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: " + strings.Repeat("x", nativeSSEMaxEventBytes+1) + "\n\n"))}
	_, apiErr := responseNativeSSEClient(ctx, response, providerresponse.OperationBinaryDownload)
	if apiErr == nil || !apiErr.UpstreamAccepted {
		t.Fatalf("unbounded audio event accepted: %+v", apiErr)
	}
	if strings.Contains(recorder.Body.String(), "xxxxx") {
		t.Fatal("oversized unframed bytes were delivered")
	}
}

func TestNativeMediaSSECRLinesPreserveWireAndObserveUsage(t *testing.T) {
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/audio/speech", nil)
	wire := ": note\revent: speech.audio.done\rdata: {\"type\":\"speech.audio.done\",\"usage\":\rdata: {\"output_tokens\":5}}\r\revent: future\rdata: extension\r\r"
	response := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(wire))}
	observed := ""
	_, apiErr := responseNativeSSEClient(ctx, response, providerresponse.OperationBinaryDownload, func(payload []byte) {
		if strings.Contains(string(payload), "usage") {
			observed = string(payload)
		}
	})
	if apiErr != nil || recorder.Body.String() != wire {
		t.Fatalf("CR stream changed: %q err=%+v", recorder.Body.String(), apiErr)
	}
	if observed != "{\"type\":\"speech.audio.done\",\"usage\":\n{\"output_tokens\":5}}" {
		t.Fatalf("CR multiline evidence=%q", observed)
	}
}
