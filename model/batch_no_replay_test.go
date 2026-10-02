package model

import (
	"context"
	"errors"
	"gorm.io/gorm"
	"testing"
	"time"
)

func TestShutdownConfirmedWriteWithLostAcknowledgementIsNotReplayed(t *testing.T) {
	db := setupBatchShutdownTest(t)
	lostAck := errors.New("simulated response lost after commit")
	if err := db.Callback().Update().After("gorm:commit_or_rollback_transaction").Register("test:lost_ack", func(tx *gorm.DB) {
		if tx.Statement.Table == "channels" && tx.Error == nil {
			tx.AddError(lostAck)
		}
	}); err != nil {
		t.Fatal(err)
	}
	if err := addNewRecord(BatchUpdateTypeChannelUsedQuota, 1, 7); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := flushBatchContext(ctx); !errors.Is(err, lostAck) {
		t.Fatalf("wanted lost acknowledgement, got %v", err)
	}
	assertBatchProjection(t, db, 0, 0, 7)
	db.Callback().Update().Remove("test:lost_ack")
	if err := StopBatchUpdater(ctx); err != nil {
		t.Fatal(err)
	}
	assertBatchProjection(t, db, 0, 0, 7)
}
