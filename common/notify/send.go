package notify

import (
	"context"
	"fmt"
	"one-api/common/logger"
	"sync"
	"time"
)

// HTTPProfileNotification currently bounds each notification request at ten
// seconds. Keep the per-notifier context aligned with that existing profile
// while detaching it from an exhausted health-processing deadline.
const notificationSendTimeout = 10 * time.Second

func (n *Notify) Send(ctx context.Context, title, message string) {
	if ctx == nil {
		ctx = context.Background()
	}

	requestCtx := context.WithoutCancel(ctx)
	var waitGroup sync.WaitGroup
	for channelName, channel := range n.notifiers {
		if channel == nil {
			continue
		}
		sendCtx, cancel := context.WithTimeout(requestCtx, notificationSendTimeout)
		waitGroup.Add(1)
		go func(channelName string, channel Notifier, sendCtx context.Context, cancel context.CancelFunc) {
			defer waitGroup.Done()
			defer cancel()
			if err := channel.Send(sendCtx, title, message); err != nil {
				logger.LogError(sendCtx, fmt.Sprintf("%s err: %s", channelName, err.Error()))
			}
		}(channelName, channel, sendCtx, cancel)
	}
	waitGroup.Wait()
}

func Send(title, message string) {
	SendContext(context.Background(), title, message)
}

func SendContext(ctx context.Context, title, message string) {
	if ctx == nil {
		ctx = context.Background()
	}
	//lint:ignore SA1029 reason: 需要使用该类型作为错误处理
	ctx = context.WithValue(ctx, logger.RequestIdKey, "NotifyTask")

	notifyChannels.Send(ctx, title, message)
}
