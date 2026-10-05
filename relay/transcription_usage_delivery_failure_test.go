package relay

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"one-api/common"
	"one-api/common/config"
	"one-api/common/groupctx"
	"one-api/common/requester"
	"one-api/model"
	"one-api/providers/openai"
	relayUtil "one-api/relay/relay_util"
	"one-api/types"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

const transcriptionDeliveryTranscriptionModel = "issue050-transcription-model"

const transcriptionDeliveryTranscriptionDoneWire = "event: transcript.text.done\ndata: {\"type\":\"transcript.text.done\",\"text\":\"ok\",\"model\":\"issue050-transcription-model\",\"usage\":{\"type\":\"tokens\",\"input_tokens\":8,\"output_tokens\":2,\"total_tokens\":10,\"input_token_details\":{\"text_tokens\":8,\"audio_tokens\":0}}}\n\n"

func TestTranscriptionUsageSurvivesWriterFailureAndReachesSQL(t *testing.T) {
	for _, test := range []struct {
		name       string
		wire       string
		failAfter  int
		wantError  bool
		wantUsage  bool
		wantCharge int
	}{
		{name: "writer closes after complete terminal", wire: transcriptionDeliveryTranscriptionDoneWire, failAfter: 1, wantError: true, wantUsage: true, wantCharge: 10},
		{name: "normal delivery", wire: transcriptionDeliveryTranscriptionDoneWire, wantUsage: true, wantCharge: 10},
		{name: "invalid total", wire: transcriptionDeliveryInvalidTranscriptionWire(), wantUsage: false},
		{name: "error terminal", wire: transcriptionDeliveryErrorTranscriptionWire(), wantError: true, wantUsage: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			setupTranscriptionDeliveryBilling(t)
			var upstreamCalls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				upstreamCalls.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, test.wire)
			}))
			t.Cleanup(server.Close)
			previousClient := requester.HTTPClient
			requester.HTTPClient = server.Client()
			t.Cleanup(func() { requester.HTTPClient = previousClient })

			ctx, _ := transcriptionDeliveryTranscriptionContext(t)
			if test.failAfter > 0 {
				ctx.Writer = &transcriptionDeliveryFailingWriter{ResponseWriter: ctx.Writer, failAfter: test.failAfter}
			}
			proxy := ""
			provider := openai.CreateOpenAIProvider(&model.Channel{Plugin: model.NewCustomEndpointPlugin(), Type: config.ChannelTypeCustom, Key: "issue050-key", Proxy: &proxy}, server.URL)
			provider.SetContext(ctx)
			relay := &relayTranscriptions{
				relayBase: relayBase{c: ctx, provider: provider, modelName: transcriptionDeliveryTranscriptionModel},
				request:   types.AudioRequest{Model: transcriptionDeliveryTranscriptionModel, Stream: true},
			}

			apiErr, _ := RelayHandler(relay)
			if (apiErr != nil) != test.wantError {
				t.Fatalf("transcription relay error=%+v wantError=%v", apiErr, test.wantError)
			}
			if upstreamCalls.Load() != 1 {
				t.Fatalf("expected one upstream request, got %d", upstreamCalls.Load())
			}
			usage := provider.GetUsage()
			if usage.HasProviderUsage() != test.wantUsage {
				t.Fatalf("provider usage evidence=%v want=%v: %+v", usage.HasProviderUsage(), test.wantUsage, usage)
			}
			if test.wantUsage && (usage.PromptTokens != 8 || usage.CompletionTokens != 2 || usage.TotalTokens != 10 || usage.ResponseModel != transcriptionDeliveryTranscriptionModel || usage.ExtraTokens[config.UsageExtraInputTextTokens] != 8 || usage.ExtraTokens[config.UsageExtraInputAudio] != 0) {
				t.Fatalf("transcription usage snapshot mismatch: %+v", usage)
			}
			assertTranscriptionDeliverySQL(t, usage, test.wantCharge, test.wantUsage)
		})
	}
}

func TestCompleteTerminalIsObservedBeforeSendDataAfterClientClose(t *testing.T) {
	setupTranscriptionDeliveryBilling(t)
	var upstreamCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, transcriptionDeliveryTranscriptionDoneWire)
	}))
	t.Cleanup(server.Close)
	previousClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() { requester.HTTPClient = previousClient })

	requestContext, cancelRequest := context.WithCancel(context.Background())
	ctx, _ := transcriptionDeliveryTranscriptionContext(t)
	ctx.Request = ctx.Request.WithContext(requestContext)
	proxy := ""
	provider := &transcriptionDeliveryTranscriptionProvider{
		OpenAIProvider: openai.CreateOpenAIProvider(&model.Channel{Plugin: model.NewCustomEndpointPlugin(), Type: config.ChannelTypeCustom, Key: "issue050-close-key", Proxy: &proxy}, server.URL),
	}
	provider.SetContext(ctx)
	provider.SetUsage(&types.Usage{})
	attempt, err := relayUtil.NewAttemptQuota(ctx, transcriptionDeliveryTranscriptionModel, 0, relayUtil.BillingAttemptSpec{LogProtocol: relayUtil.LogProtocolHTTPStream})
	if err != nil {
		t.Fatalf("create transcription attempt: %v", err)
	}
	if err := attempt.ApplyReserve(ctx.Request.Context()); err != nil {
		t.Fatalf("reserve transcription quota: %v", err)
	}
	if err := attempt.ClaimSubmission(); err != nil {
		t.Fatalf("claim transcription submission: %v", err)
	}
	response, apiErr := provider.CreateTranscriptions(&types.AudioRequest{Model: transcriptionDeliveryTranscriptionModel, Stream: true})
	if apiErr != nil || response == nil || response.Stream == nil {
		t.Fatalf("create streaming transcription response: response=%+v err=%+v", response, apiErr)
	}
	frameComplete := make(chan []byte, 1)
	releaseFrame := make(chan struct{})
	var releaseFrameOnce sync.Once
	release := func() { releaseFrameOnce.Do(func() { close(releaseFrame) }) }
	readerClosed := make(chan struct{})
	response.Stream.Body = &transcriptionDeliveryCloseProbeBody{ReadCloser: response.Stream.Body, closed: readerClosed}
	handler := newNativeSSEHandler(response.ObserveProviderEvent)
	terminalFrameSeen := atomic.Bool{}
	handleReturned := make(chan struct{})
	var handleReturnedOnce sync.Once
	handler.afterFrame = func(event []byte) {
		terminalFrameSeen.Store(true)
		frameComplete <- event
		<-releaseFrame
	}
	stream, apiErr := requester.RequestNoTrimStreamWithEmitterOptions[string](nil, response.Stream, func(rawLine *[]byte, emitter requester.StreamEmitter[string]) {
		handler.Handle(rawLine, emitter)
		if terminalFrameSeen.Load() {
			handleReturnedOnce.Do(func() { close(handleReturned) })
		}
	}, requester.StreamReadOptions{MaxLineBytes: nativeSSEMaxEventBytes, RequireProtocolTerminal: true})
	if apiErr != nil {
		t.Fatalf("create transcription SSE reader: %+v", apiErr)
	}
	stream.Recv()
	t.Cleanup(func() {
		cancelRequest()
		stream.Close()
		release()
		select {
		case <-handleReturned:
		case <-time.After(2 * time.Second):
			t.Errorf("terminal handler did not return during cleanup")
		}
		requester.CloseAndDrainStream(stream)
	})
	select {
	case event := <-frameComplete:
		eventName, payloadType, _ := audioSSEFacts(event)
		if eventName != "transcript.text.done" || payloadType != "transcript.text.done" || !bytes.HasSuffix(event, []byte("\n\n")) {
			t.Fatalf("post-framer hook did not receive a complete terminal event: %q", event)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("framer did not accept the complete terminal event")
	}
	cancelRequest()
	stream.Close()
	select {
	case <-readerClosed:
	case <-time.After(2 * time.Second):
		t.Fatal("stream.Close did not close the upstream reader before release")
	}
	release()
	select {
	case <-handleReturned:
	case <-time.After(2 * time.Second):
		t.Fatal("terminal handler did not return after the consumer close")
	}
	requester.CloseAndDrainStream(stream)
	if upstreamCalls.Load() != 1 {
		t.Fatalf("expected one upstream request, got %d", upstreamCalls.Load())
	}
	usage := provider.GetUsage()
	if !usage.HasProviderUsage() || usage.PromptTokens != 8 || usage.CompletionTokens != 2 || usage.TotalTokens != 10 {
		t.Fatalf("complete terminal usage was lost before SendData: %+v", usage)
	}
	result, err := attempt.CloseFromProviderResult(ctx.Request.Context(), usage, true)
	if err != nil || !result.Confirmed || result.ChargedQuota != 10 {
		t.Fatalf("settle transcription usage after reader drain: result=%+v err=%v", result, err)
	}
	assertTranscriptionDeliverySQL(t, usage, 10, true)
}

func TestCancellationBeforeTerminalStopsFutureUsageRead(t *testing.T) {
	setupTranscriptionDeliveryBilling(t)
	var upstreamCalls atomic.Int32
	var terminalBlankSent atomic.Bool
	relayDone := make(chan struct{})
	serverDone := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		defer close(serverDone)
		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: transcript.text.done\ndata: {\"type\":\"transcript.text.done\",\"text\":\"ok\",\"model\":\"issue050-transcription-model\",\"usage\":{\"type\":\"tokens\",\"input_tokens\":8,\"output_tokens\":2,\"total_tokens\":10}}\n")
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		select {
		case <-relayDone:
			return
		case <-time.After(2 * time.Second):
			terminalBlankSent.Store(true)
			_, _ = io.WriteString(w, "\n")
		}
	}))
	t.Cleanup(server.Close)
	previousClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() { requester.HTTPClient = previousClient })

	requestContext, cancelRequest := context.WithCancel(context.Background())
	ctx, _ := transcriptionDeliveryTranscriptionContext(t)
	ctx.Request = ctx.Request.WithContext(requestContext)
	terminalDataRead := make(chan struct{})
	releaseBlankRead := make(chan struct{})
	var releaseBlankReadOnce sync.Once
	release := func() { releaseBlankReadOnce.Do(func() { close(releaseBlankRead) }) }
	readerClosed := make(chan struct{})
	streamDone := make(chan struct{})
	t.Cleanup(func() {
		cancelRequest()
		release()
		select {
		case <-streamDone:
		case <-time.After(2 * time.Second):
			t.Errorf("relay goroutine did not stop during cleanup")
		}
		select {
		case <-serverDone:
		case <-time.After(2 * time.Second):
			t.Errorf("upstream goroutine did not stop during cleanup")
		}
	})
	proxy := ""
	baseProvider := openai.CreateOpenAIProvider(&model.Channel{Plugin: model.NewCustomEndpointPlugin(), Type: config.ChannelTypeCustom, Key: "issue050-before-terminal-key", Proxy: &proxy}, server.URL)
	provider := &transcriptionDeliveryTranscriptionProvider{
		OpenAIProvider: baseProvider,
		wrapStream: func(stream *http.Response) {
			originalBody := stream.Body
			gateBody := &transcriptionDeliveryTerminalDataGateBody{
				ReadCloser: originalBody,
				reader:     bufio.NewReader(originalBody),
				dataRead:   terminalDataRead,
				closed:     readerClosed,
				beforeBlankRead: func() {
					// The terminal data line has been returned and processed by the
					// framer before the reader asks for its still-unsent blank line.
					cancelRequest()
					<-releaseBlankRead
				},
			}
			stream.Body = gateBody
		},
	}
	provider.SetContext(ctx)
	relay := &relayTranscriptions{
		relayBase: relayBase{c: ctx, provider: provider, modelName: transcriptionDeliveryTranscriptionModel},
		request:   types.AudioRequest{Model: transcriptionDeliveryTranscriptionModel, Stream: true},
	}
	resultCh := make(chan *types.OpenAIErrorWithStatusCode, 1)
	go func() {
		apiErr, _ := RelayHandler(relay)
		resultCh <- apiErr
		close(relayDone)
		close(streamDone)
	}()
	select {
	case <-terminalDataRead:
	case <-time.After(2 * time.Second):
		t.Fatal("terminal data line was not read before the blank line")
	}
	select {
	case <-readerClosed:
	case <-time.After(2 * time.Second):
		t.Fatal("CloseAndDrain did not close the upstream reader before blank release")
	}
	release()
	select {
	case <-resultCh:
	case <-time.After(2 * time.Second):
		t.Fatal("relay did not stop after pre-terminal cancellation")
	}
	select {
	case <-serverDone:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream gate did not observe relay completion")
	}
	if upstreamCalls.Load() != 1 {
		t.Fatalf("expected one upstream request, got %d", upstreamCalls.Load())
	}
	if terminalBlankSent.Load() {
		t.Fatal("upstream sent the terminal blank after pre-terminal cancellation")
	}
	if provider.GetUsage().HasProviderUsage() {
		t.Fatalf("pre-terminal cancellation became billable: %+v", provider.GetUsage())
	}
	assertTranscriptionDeliverySQL(t, provider.GetUsage(), 0, false)
}

func TestTranscriptionCloseIsIdempotent(t *testing.T) {
	setupTranscriptionDeliveryBilling(t)
	ctx, _ := transcriptionDeliveryTranscriptionContext(t)
	usage := &types.Usage{PromptTokens: 8, CompletionTokens: 2, TotalTokens: 10, ResponseModel: transcriptionDeliveryTranscriptionModel}
	usage.MarkProviderReported()
	attempt, err := relayUtil.NewAttemptQuota(ctx, transcriptionDeliveryTranscriptionModel, 0, relayUtil.BillingAttemptSpec{LogProtocol: relayUtil.LogProtocolHTTPStream})
	if err != nil {
		t.Fatalf("create transcription attempt: %v", err)
	}
	if err := attempt.ApplyReserve(ctx.Request.Context()); err != nil {
		t.Fatalf("reserve transcription quota: %v", err)
	}
	if err := attempt.ClaimSubmission(); err != nil {
		t.Fatalf("claim transcription submission: %v", err)
	}
	first, err := attempt.CloseFromProviderResult(ctx.Request.Context(), usage, true)
	if err != nil || !first.Confirmed || first.ChargedQuota != 10 {
		t.Fatalf("first transcription settlement failed: result=%+v err=%v", first, err)
	}
	second, err := attempt.CloseFromProviderResult(ctx.Request.Context(), usage, true)
	if err != nil || !reflect.DeepEqual(first, second) {
		t.Fatalf("duplicate Close changed transcription settlement: first=%+v second=%+v err=%v", first, second, err)
	}
	assertTranscriptionDeliverySQL(t, usage, 10, true)
}

func transcriptionDeliveryInvalidTranscriptionWire() string {
	return "event: transcript.text.done\ndata: {\"type\":\"transcript.text.done\",\"model\":\"issue050-transcription-model\",\"usage\":{\"type\":\"tokens\",\"input_tokens\":8,\"output_tokens\":2,\"total_tokens\":99,\"input_token_details\":{\"text_tokens\":8,\"audio_tokens\":0}}}\n\n"
}

func transcriptionDeliveryErrorTranscriptionWire() string {
	return "event: error\ndata: {\"type\":\"transcript.text.done\",\"model\":\"issue050-transcription-model\",\"usage\":{\"type\":\"tokens\",\"input_tokens\":8,\"output_tokens\":2,\"total_tokens\":10},\"error\":{\"type\":\"provider_error\",\"message\":\"transcription failed\"}}\n\n"
}

func transcriptionDeliveryTranscriptionContext(t *testing.T) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("model", transcriptionDeliveryTranscriptionModel); err != nil {
		t.Fatalf("write transcription model field: %v", err)
	}
	if err := writer.WriteField("stream", "true"); err != nil {
		t.Fatalf("write transcription stream field: %v", err)
	}
	file, err := writer.CreateFormFile("file", "audio.wav")
	if err != nil {
		t.Fatalf("create transcription file field: %v", err)
	}
	if _, err := file.Write([]byte("wave-bytes")); err != nil {
		t.Fatalf("write transcription audio: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close transcription multipart: %v", err)
	}
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", bytes.NewReader(body.Bytes())).WithContext(context.Background())
	ctx.Request.Header.Set("Content-Type", writer.FormDataContentType())
	ctx.Request.ContentLength = int64(body.Len())
	if _, err := common.CacheRequestBody(ctx); err != nil {
		t.Fatalf("cache transcription multipart: %v", err)
	}
	ctx.Set("id", 1)
	ctx.Set("token_id", 1)
	ctx.Set("group_ratio", 1.0)
	groupctx.SetRoutingGroup(ctx, "default", groupctx.RoutingGroupSourceUserGroup)
	return ctx, recorder
}

type transcriptionDeliveryFailingWriter struct {
	gin.ResponseWriter
	failAfter int
	writes    int
	onFailure func()
}

type transcriptionDeliveryTranscriptionProvider struct {
	*openai.OpenAIProvider
	wrapStream func(*http.Response)
}

func (p *transcriptionDeliveryTranscriptionProvider) CreateTranscriptions(request *types.AudioRequest) (*types.AudioResponseWrapper, *types.OpenAIErrorWithStatusCode) {
	response, apiErr := p.OpenAIProvider.CreateTranscriptions(request)
	if response != nil && response.Stream != nil && p.wrapStream != nil {
		p.wrapStream(response.Stream)
	}
	return response, apiErr
}

type transcriptionDeliveryCloseProbeBody struct {
	io.ReadCloser
	closed chan struct{}
	once   sync.Once
}

func (b *transcriptionDeliveryCloseProbeBody) Close() error {
	if b == nil {
		return nil
	}
	b.once.Do(func() {
		if b.closed != nil {
			close(b.closed)
		}
	})
	if b.ReadCloser == nil {
		return nil
	}
	return b.ReadCloser.Close()
}

type transcriptionDeliveryTerminalDataGateBody struct {
	io.ReadCloser
	reader          *bufio.Reader
	dataRead        chan struct{}
	closed          chan struct{}
	beforeBlankRead func()
	pending         []byte
	pendingErr      error
	pendingData     bool
	dataLineSeen    bool
	dataReadOnce    sync.Once
	beforeBlankOnce sync.Once
	closeOnce       sync.Once
}

func (b *transcriptionDeliveryTerminalDataGateBody) Read(p []byte) (int, error) {
	if len(b.pending) == 0 {
		if b.dataLineSeen && b.beforeBlankRead != nil {
			b.beforeBlankOnce.Do(b.beforeBlankRead)
		}
		line, err := b.reader.ReadBytes('\n')
		if len(line) == 0 {
			return 0, err
		}
		b.pending = line
		b.pendingErr = err
		b.pendingData = bytes.HasPrefix(bytes.TrimSpace(line), []byte("data:"))
	}
	n := copy(p, b.pending)
	b.pending = b.pending[n:]
	if len(b.pending) > 0 {
		return n, nil
	}
	if b.pendingData {
		b.pendingData = false
		b.dataLineSeen = true
		b.dataReadOnce.Do(func() {
			if b.dataRead != nil {
				close(b.dataRead)
			}
		})
	}
	err := b.pendingErr
	b.pendingErr = nil
	return n, err
}

func (b *transcriptionDeliveryTerminalDataGateBody) Close() error {
	if b == nil {
		return nil
	}
	b.closeOnce.Do(func() {
		if b.closed != nil {
			close(b.closed)
		}
	})
	if b.ReadCloser == nil {
		return nil
	}
	return b.ReadCloser.Close()
}

func (w *transcriptionDeliveryFailingWriter) Write(payload []byte) (int, error) {
	w.writes++
	if w.failAfter > 0 && w.writes >= w.failAfter {
		if w.onFailure != nil {
			w.onFailure()
		}
		return 0, errors.New("issue050 downstream writer closed")
	}
	return w.ResponseWriter.Write(payload)
}

func (w *transcriptionDeliveryFailingWriter) WriteString(payload string) (int, error) {
	return w.Write([]byte(payload))
}

func setupTranscriptionDeliveryBilling(t *testing.T) {
	t.Helper()
	setupRelayTestDB(t, &model.User{}, &model.Token{}, &model.Log{})
	if err := model.DB.Create(&model.User{
		Id: 1, Username: "issue050-user", Password: "password123", AccessToken: "issue050-access",
		Quota: 100000, Group: "default", Status: config.UserStatusEnabled, Role: config.RoleCommonUser,
	}).Error; err != nil {
		t.Fatalf("create transcription billing user: %v", err)
	}
	if err := model.DB.Session(&gorm.Session{SkipHooks: true}).Create(&model.Token{
		Id: 1, UserId: 1, Key: "issue050-token", Name: "issue050-token", Status: config.TokenStatusEnabled,
		ExpiredTime: -1, RemainQuota: 100000, Group: "default",
	}).Error; err != nil {
		t.Fatalf("create transcription billing token: %v", err)
	}
	originalPricing := model.PricingInstance
	originalBatch := config.BatchUpdateEnabled
	originalRedis := config.RedisEnabled
	originalReserve := config.PreConsumedQuota
	originalLog := config.LogConsumeEnabled
	model.PricingInstance = &model.Pricing{Prices: map[string]*model.Price{
		transcriptionDeliveryTranscriptionModel: {Model: transcriptionDeliveryTranscriptionModel, Type: model.TokensPriceType, Input: 1, Output: 1},
	}}
	config.BatchUpdateEnabled = false
	config.RedisEnabled = false
	config.PreConsumedQuota = 50
	config.LogConsumeEnabled = true
	t.Cleanup(func() {
		model.PricingInstance = originalPricing
		config.BatchUpdateEnabled = originalBatch
		config.RedisEnabled = originalRedis
		config.PreConsumedQuota = originalReserve
		config.LogConsumeEnabled = originalLog
	})
}

func assertTranscriptionDeliverySQL(t *testing.T, usage *types.Usage, charge int, confirmed bool) {
	t.Helper()
	var user model.User
	if err := model.DB.First(&user, 1).Error; err != nil {
		t.Fatalf("read transcription settled user: %v", err)
	}
	var token model.Token
	if err := model.DB.First(&token, 1).Error; err != nil {
		t.Fatalf("read transcription settled token: %v", err)
	}
	if user.Quota != 100000-charge || user.UsedQuota != charge || token.RemainQuota != 100000-charge || token.UsedQuota != charge {
		t.Fatalf("transcription SQL balances mismatch: user=%+v token=%+v charge=%d usage=%+v", user, token, charge, usage)
	}
	var logs []model.Log
	if err := model.DB.Where("type = ?", model.LogTypeConsume).Find(&logs).Error; err != nil {
		t.Fatalf("read transcription consume logs: %v", err)
	}
	wantLogs := 0
	if confirmed {
		wantLogs = 1
	}
	if len(logs) != wantLogs {
		t.Fatalf("transcription consume log count=%d want=%d logs=%+v", len(logs), wantLogs, logs)
	}
	if confirmed && (logs[0].Quota != charge || logs[0].PromptTokens != 8 || logs[0].CompletionTokens != 2 || logs[0].ModelName != transcriptionDeliveryTranscriptionModel || !logs[0].IsStream) {
		t.Fatalf("transcription consume log mismatch: %+v", logs[0])
	}
}
