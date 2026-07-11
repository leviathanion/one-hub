package codex

import (
	"context"
	"strings"
	"testing"
	"time"

	"one-api/model"
)

func TestPrepareChannelForAutoRefreshNormalizesProxyTemplate(t *testing.T) {
	rawProxy := "http://proxy.example/%s"
	channel := &model.Channel{
		Id:    1,
		Key:   "secret-key",
		Proxy: &rawProxy,
	}

	prepared := prepareChannelForAutoRefresh(channel)
	if prepared == nil || prepared.Proxy == nil {
		t.Fatalf("expected prepared channel with proxy")
	}
	if *prepared.Proxy == rawProxy {
		t.Fatalf("expected proxy template to be normalized, got %q", *prepared.Proxy)
	}
	if *channel.Proxy != rawProxy {
		t.Fatalf("expected original channel proxy to remain unchanged, got %q", *channel.Proxy)
	}
}

func TestPrepareChannelForAutoRefreshGuardsNilProxy(t *testing.T) {
	channel := &model.Channel{
		Id:  1,
		Key: "secret-key",
	}

	prepared := prepareChannelForAutoRefresh(channel)
	if prepared == nil || prepared.Proxy == nil {
		t.Fatalf("expected prepared channel to have a non-nil proxy pointer")
	}
	if *prepared.Proxy != "" {
		t.Fatalf("expected empty proxy string, got %q", *prepared.Proxy)
	}
}

func TestRecordAutoRefreshResultPartialDoesNotAdvanceLastSuccess(t *testing.T) {
	autoRefreshStatusMu.Lock()
	autoRefreshStatus = AutoRefreshStatus{
		LastSuccessAt: 123,
		IntervalSec:   int64(AutoRefreshInterval / time.Second),
		LeadSec:       int64(AutoRefreshLead / time.Second),
	}
	autoRefreshStatusMu.Unlock()

	recordAutoRefreshResult(
		time.Now().Add(-2*time.Second),
		AutoRefreshSummary{Failed: 1},
		"partial",
		"channel 1: refresh failed",
		false,
	)

	status := GetAutoRefreshStatus()
	if status.LastSuccessAt != 123 {
		t.Fatalf("expected last success timestamp to remain unchanged, got %d", status.LastSuccessAt)
	}
	if status.LastResult != "partial" {
		t.Fatalf("expected last result to be partial, got %q", status.LastResult)
	}
	if status.LastError == "" {
		t.Fatalf("expected last error to be recorded")
	}
}

func TestRunScheduledMaintenanceRunsOAuthBeforeUsage(t *testing.T) {
	originalOAuth := runScheduledOAuthRefresh
	originalUsage := runScheduledUsageRefresh
	t.Cleanup(func() {
		runScheduledOAuthRefresh = originalOAuth
		runScheduledUsageRefresh = originalUsage
	})

	order := make([]string, 0, 2)
	parent := context.WithValue(context.Background(), struct{}{}, "maintenance")
	runScheduledOAuthRefresh = func(ctx context.Context) AutoRefreshSummary {
		if ctx != parent {
			t.Fatal("expected OAuth refresh to receive maintenance parent context")
		}
		order = append(order, "oauth")
		return AutoRefreshSummary{}
	}
	runScheduledUsageRefresh = func(ctx context.Context) UsageAutoRefreshSummary {
		if ctx != parent {
			t.Fatal("expected usage refresh to receive maintenance parent context")
		}
		order = append(order, "usage")
		return UsageAutoRefreshSummary{}
	}

	RunScheduledMaintenance(parent)
	if len(order) != 2 || order[0] != "oauth" || order[1] != "usage" {
		t.Fatalf("expected serial OAuth then usage maintenance, got %v", order)
	}
}

func TestOAuthRunnerCancellationReachesChannelLoader(t *testing.T) {
	originalLoadChannels := loadAutoRefreshChannels
	loaderCanceled := make(chan struct{})
	loadAutoRefreshChannels = func(ctx context.Context) ([]*model.Channel, error) {
		<-ctx.Done()
		close(loaderCanceled)
		return nil, ctx.Err()
	}
	t.Cleanup(func() { loadAutoRefreshChannels = originalLoadChannels })

	parent, cancel := context.WithCancel(context.Background())
	cancel()
	summary := RunAutoRefreshWithTimeout(parent)
	if summary.Failed != 1 {
		t.Fatalf("expected canceled loader to fail the round, got %+v", summary)
	}
	select {
	case <-loaderCanceled:
	default:
		t.Fatal("expected round cancellation to reach OAuth channel loader")
	}
}

func TestRefreshChannelsInBackgroundMarksCanceledDispatchPartial(t *testing.T) {
	autoRefreshStatusMu.Lock()
	autoRefreshStatus = AutoRefreshStatus{
		LastSuccessAt: 456,
		IntervalSec:   int64(AutoRefreshInterval / time.Second),
		LeadSec:       int64(AutoRefreshLead / time.Second),
	}
	autoRefreshStatusMu.Unlock()

	originalLoadChannels := loadAutoRefreshChannels
	loadAutoRefreshChannels = func(context.Context) ([]*model.Channel, error) {
		return []*model.Channel{
			{Id: 1, Key: `{"refresh_token":"refresh-1"}`},
			{Id: 2, Key: `{"refresh_token":"refresh-2"}`},
		}, nil
	}
	t.Cleanup(func() {
		loadAutoRefreshChannels = originalLoadChannels
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	summary := RefreshChannelsInBackground(ctx)
	if summary.Failed != 2 {
		t.Fatalf("expected canceled dispatch to fail both channels, got %+v", summary)
	}

	status := GetAutoRefreshStatus()
	if status.LastResult != "partial" {
		t.Fatalf("expected partial result, got %q", status.LastResult)
	}
	if status.LastSuccessAt != 456 {
		t.Fatalf("expected last success timestamp to remain unchanged, got %d", status.LastSuccessAt)
	}
	if !strings.Contains(status.LastError, "context canceled") || !strings.Contains(status.LastError, "skipped 2") {
		t.Fatalf("expected cancellation details in last error, got %q", status.LastError)
	}
}
