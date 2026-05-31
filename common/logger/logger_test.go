package logger

import (
	"context"
	"testing"

	"github.com/spf13/viper"
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

func TestDebugLogsRespectConfiguredLevelForHistory(t *testing.T) {
	originalLogger := Logger
	originalLogLevel := viper.GetString("log_level")
	originalEntries := logHistory.GetLatestEntries(0)
	Logger = nil
	t.Cleanup(func() {
		Logger = originalLogger
		viper.Set("log_level", originalLogLevel)
		logHistory.mutex.Lock()
		logHistory.entries = originalEntries
		logHistory.mutex.Unlock()
	})

	logHistory.mutex.Lock()
	logHistory.entries = nil
	logHistory.mutex.Unlock()

	viper.Set("log_level", "info")
	SysDebug("hidden system debug")
	LogDebug(context.WithValue(context.Background(), RequestIdKey, "req-debug"), "hidden request debug")
	if entries := logHistory.GetLatestEntries(10); len(entries) != 0 {
		t.Fatalf("expected info level to hide debug entries from log history, got %+v", entries)
	}

	viper.Set("log_level", "debug")
	SysDebug("visible system debug")
	LogDebug(context.WithValue(context.Background(), RequestIdKey, "req-debug"), "visible request debug")
	entries := logHistory.GetLatestEntries(10)
	if len(entries) != 2 {
		t.Fatalf("expected debug level to preserve debug entries in log history, got %d: %+v", len(entries), entries)
	}
	if entries[0].Level != loggerDEBUG || entries[1].Level != loggerDEBUG {
		t.Fatalf("expected debug entries, got %+v", entries)
	}
}
