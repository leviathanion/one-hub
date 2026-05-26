package logger

import (
	"context"
	"testing"
)

func TestNilLoggerFallbackPreservesLogHistory(t *testing.T) {
	originalLogger := Logger
	originalEntries := logHistory.GetLatestEntries(0)
	Logger = nil
	t.Cleanup(func() {
		Logger = originalLogger
		logHistory.mutex.Lock()
		logHistory.entries = originalEntries
		logHistory.mutex.Unlock()
	})

	logHistory.mutex.Lock()
	logHistory.entries = nil
	logHistory.mutex.Unlock()

	SysLog("system started")
	SysError("system failed")
	LogWarn(context.WithValue(context.Background(), RequestIdKey, "req-1"), "request warning")
	LogError(nil, "nil context error")

	entries := logHistory.GetLatestEntries(10)
	if len(entries) != 4 {
		t.Fatalf("expected nil logger fallback to preserve log history, got %d entries", len(entries))
	}
	if entries[0].Level != loggerINFO || entries[1].Level != loggerError || entries[2].Level != loggerWarn || entries[3].Level != loggerError {
		t.Fatalf("unexpected log levels: %+v", entries)
	}
	if entries[2].Message != "req-1 | request warning \n" {
		t.Fatalf("expected request id to be preserved, got %q", entries[2].Message)
	}
	if entries[3].Message != "unknown | nil context error \n" {
		t.Fatalf("expected nil context to use unknown request id, got %q", entries[3].Message)
	}
}
