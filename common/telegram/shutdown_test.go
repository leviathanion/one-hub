package telegram

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PaulSonOfLars/gotgbot/v2"
	"github.com/PaulSonOfLars/gotgbot/v2/ext"
	"github.com/PaulSonOfLars/gotgbot/v2/ext/handlers"
)

const shutdownUpdate = `{"update_id":1,"message":{"message_id":1,"date":1,"from":{"id":42,"is_bot":false,"first_name":"test"},"chat":{"id":42,"type":"private"},"text":"test"}}`

func shutdownUpdater(t *testing.T, handler func(*gotgbot.Bot, *ext.Context) error) *ext.Updater {
	t.Helper()
	bot, err := gotgbot.NewBot("123:fixture", &gotgbot.BotOpts{DisableTokenCheck: true})
	if err != nil {
		t.Fatal(err)
	}
	dispatcher := ext.NewDispatcher(nil)
	dispatcher.AddHandler(handlers.NewMessage(nil, handler))
	drain := &drainingDispatcher{Dispatcher: dispatcher, readerDone: make(chan struct{})}
	updater := ext.NewUpdater(drain, nil)
	if err := updater.AddWebhook(bot, "/test", nil); err != nil {
		t.Fatal(err)
	}
	drain.readerExpected = true
	updater.GetHandlerFunc("/")(httptest.NewRecorder(), httptest.NewRequest("POST", "/test", strings.NewReader(shutdownUpdate)))
	return updater
}
func TestTelegramShutdownWaitsForAdmittedCommand(t *testing.T) {
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	updater := shutdownUpdater(t, func(*gotgbot.Bot, *ext.Context) error { close(entered); <-release; close(done); return nil })
	old := TGupdater
	TGupdater = updater
	t.Cleanup(func() { TGupdater = old })
	<-entered
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := StopTelegramBot(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("unfinished command reported done: %v", err)
	}
	unblock()
	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := StopTelegramBot(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	default:
		t.Fatal("command not joined")
	}
}
func TestTelegramShutdownJoinsDispatcherReaderBeforeHandlers(t *testing.T) {
	for i := 0; i < 1000; i++ {
		var complete atomic.Bool
		updater := shutdownUpdater(t, func(*gotgbot.Bot, *ext.Context) error { complete.Store(true); return nil })
		if err := updater.Stop(); err != nil {
			t.Fatal(err)
		}
		if !complete.Load() {
			t.Fatalf("accepted update lost at stop, iteration %d", i)
		}
	}
}
func TestTelegramFailedStartupHasNoReaderToJoin(t *testing.T) {
	d := &drainingDispatcher{Dispatcher: ext.NewDispatcher(nil), readerDone: make(chan struct{})}
	done := make(chan struct{})
	go func() { d.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("waited for reader after failed startup")
	}
}
