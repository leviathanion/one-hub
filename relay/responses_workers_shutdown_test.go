package relay

import (
	"context"
	"github.com/gin-gonic/gin"
	"net/http/httptest"
	"one-api/common/responsesws"
	"one-api/types"
	"sync"
	"testing"
	"time"
)

func TestHandlerJoinIncludesFirstTurnProviderWork(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("GET", "/v1/responses", nil)
	frame, err := responsesws.ParseRawResponsesCreateFrame([]byte(`{"type":"response.create","model":"gpt-5","input":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	entered, release, workerDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	old := openAndPrimeResponsesWSSessionForActor
	openAndPrimeResponsesWSSessionForActor = func(context.Context, *gin.Context, *responsesws.RawResponsesCreateFrame, *types.OpenAIResponsesRequest, responsesWSOpenAdmission) (*responsesWSOpenResult, *types.OpenAIErrorWithStatusCode) {
		close(entered)
		<-release
		close(workerDone)
		return nil, nil
	}
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer func() { unblock(); <-workerDone; openAndPrimeResponsesWSSessionForActor = old }()
	actor := NewResponsesWSSessionActor(c)
	actor.ReserveFirstTurnOpening(frame)
	actor.Start()
	actor.startFirstTurnOpenWorker(actor.turns.opening.openingID, frame)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("open worker did not enter")
	}
	actor.markClientClosed(nil)
	actor.Post(ResponsesWSEventClientClosed{})
	select {
	case <-actor.Done():
	case <-time.After(time.Second):
		t.Fatal("actor not closed")
	}
	joined := make(chan struct{})
	go func() { actor.waitStartedGoroutines(); close(joined) }()
	select {
	case <-joined:
		t.Fatal("handler join abandoned provider work")
	case <-time.After(20 * time.Millisecond):
	}
	unblock()
	select {
	case <-joined:
	case <-time.After(time.Second):
		t.Fatal("handler could not join completed provider work")
	}
}

type blockedShutdownSendSession struct {
	responsesWSTestSession
	entered, release chan struct{}
}

func (s *blockedShutdownSendSession) SendClientWithResult(context.Context, responsesws.SendRequest) responsesws.ResponsesWSTransportSendResult {
	close(s.entered)
	<-s.release
	return responsesws.ResponsesWSTransportSendResult{Status: responsesws.ResponsesWSTransportSendAttempted}
}
func TestHandlerJoinIncludesInFlightSendWorker(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("GET", "/v1/responses", nil)
	actor := NewResponsesWSSessionActor(c)
	session := &blockedShutdownSendSession{entered: make(chan struct{}), release: make(chan struct{})}
	var once sync.Once
	unblock := func() { once.Do(func() { close(session.release) }) }
	defer unblock()
	actor.Start()
	actor.startSendWorker()
	actor.workers.sendCommands <- responsesWSSendCommand{Session: session, Frame: responsesws.NewTextFrame([]byte(`{"type":"response.create"}`))}
	select {
	case <-session.entered:
	case <-time.After(time.Second):
		t.Fatal("send worker did not enter provider")
	}
	actor.markClientClosed(nil)
	actor.Post(ResponsesWSEventClientClosed{})
	select {
	case <-actor.Done():
	case <-time.After(time.Second):
		t.Fatal("actor failed to close")
	}
	joined := make(chan struct{})
	go func() { actor.waitStartedGoroutines(); close(joined) }()
	select {
	case <-joined:
		t.Fatal("handler join abandoned in-flight send")
	case <-time.After(20 * time.Millisecond):
	}
	unblock()
	select {
	case <-joined:
	case <-time.After(time.Second):
		t.Fatal("send worker did not exit")
	}
}
