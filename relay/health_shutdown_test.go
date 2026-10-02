package relay

import (
	"context"
	"errors"
	"github.com/gin-gonic/gin"
	"net/http/httptest"
	"one-api/internal/lifecycle"
	"one-api/model"
	"one-api/types"
	"sync"
	"testing"
	"time"
)

func TestShutdownJoinsDetachedProviderHealth(t *testing.T) {
	oldProcess, oldObservers := processChannelRelayErrorFunc, backgroundBusiness
	backgroundBusiness = &lifecycle.Group{}
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	processChannelRelayErrorFunc = func(context.Context, int, string, *types.OpenAIErrorWithStatusCode, int) {
		close(entered)
		<-release
		close(done)
	}
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer func() {
		unblock()
		<-done
		processChannelRelayErrorFunc = oldProcess
		backgroundBusiness = oldObservers
	}()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/chat/completions", nil)
	observeRelayProviderFailure(c, &model.Channel{Id: 1}, &types.OpenAIErrorWithStatusCode{StatusCode: 500})
	<-entered
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := StopBackgroundBusiness(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("unfinished health falsely completed: %v", err)
	}
	unblock()
	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := StopBackgroundBusiness(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	default:
		t.Fatal("health not joined")
	}
}
