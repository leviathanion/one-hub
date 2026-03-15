package codex

import (
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
