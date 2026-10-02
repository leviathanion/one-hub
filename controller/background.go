package controller

import (
	"context"
	"one-api/internal/lifecycle"
)

// Admitted admin requests can launch probes and import warmups that outlive HTTP.
// Close after request admission/drain, cancel new work and join existing writes.
var backgroundBusiness = &lifecycle.Group{}
var backgroundContext, cancelBackground = context.WithCancel(context.Background())

func StopBackgroundBusiness(ctx context.Context) error {
	backgroundBusiness.Close()
	cancelBackground()
	return backgroundBusiness.Wait(ctx)
}
