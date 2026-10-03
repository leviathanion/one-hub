package telegram

import (
	"context"
	"encoding/json"
	"github.com/PaulSonOfLars/gotgbot/v2"
	"github.com/PaulSonOfLars/gotgbot/v2/ext"
	"sync"
)

var stopOnce sync.Once
var stopDone = make(chan struct{})
var stopErr error

// StopTelegramBot stops polling/webhook admission and waits for admitted command
// handlers, including balance mutations. Startup globals remain immutable while
// HTTP handlers finish; updater.Stop owns the bot and dispatcher lifecycle.
func StopTelegramBot(ctx context.Context) error {
	stopOnce.Do(func() {
		updater := TGupdater
		go func() {
			defer close(stopDone)
			if updater != nil {
				stopErr = updater.Stop()
			}
		}()
	})
	select {
	case <-stopDone:
		return stopErr
	default:
	}
	select {
	case <-stopDone:
		return stopErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

// gotgbot rc32 joins handler goroutines but not the reader that registers them.
// Stop must first join that reader after Updater has closed its update channel.
// This app starts one bot during synchronous startup; failed starts have no reader.
type drainingDispatcher struct {
	*ext.Dispatcher
	readerDone     chan struct{}
	readerExpected bool // set by startup before the runtime is exposed to shutdown
}

func (d *drainingDispatcher) Start(bot *gotgbot.Bot, updates <-chan json.RawMessage) {
	defer close(d.readerDone)
	d.Dispatcher.Start(bot, updates)
}
func (d *drainingDispatcher) Stop() {
	if d.readerExpected {
		<-d.readerDone
	}
	d.Dispatcher.Stop()
}
