package relay

import (
	"context"
	"one-api/internal/lifecycle"
)

// Tracks only request-derived asynchronous database work. Call after HTTP
// handlers are joined so no accepted request can register another observer.
var backgroundBusiness = &lifecycle.Group{}

func StopBackgroundBusiness(ctx context.Context) error {
	backgroundBusiness.Close()
	return backgroundBusiness.Wait(ctx)
}
