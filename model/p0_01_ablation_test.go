package model

import (
	"context"
	"database/sql"
	"errors"
	"gorm.io/gorm"
	"testing"
)

func TestP001PlanNoChangeConvergesStalePublisher(t *testing.T) {
	db, pricing := setupVersionedPricingTest(t)
	if err := pricing.Init(); err != nil {
		t.Fatal(err)
	}
	source := []*Price{{Model: "ablation", Type: TokensPriceType, Input: 3, Output: 4}}
	if err := db.Create(&source).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := BumpPublicationVersionCAS(context.Background(), db, PublicationOwnerPrice, 1); err != nil {
		t.Fatal(err)
	}
	preview, err := PreviewPriceChange(context.Background(), source, PriceUpdateModeAdd)
	if err != nil {
		t.Fatal(err)
	}
	version, err := ApplyPriceChange(context.Background(), pricing, source, PriceUpdateModeAdd, preview.BaseVersion, preview.Digest)
	if err != nil {
		t.Fatal(err)
	}
	head, err := ReadPublicationVersion(context.Background(), db, PublicationOwnerPrice)
	if err != nil {
		t.Fatal(err)
	}
	if version != 2 || head != 2 || pricing.PublishedVersion() != 2 {
		t.Fatalf("no-change convergence: result=%d head=%d published=%d", version, head, pricing.PublishedVersion())
	}
	if _, ok := pricing.FindExactPrice("ablation"); !ok {
		t.Fatal("no-change failed to converge catalog")
	}
}

func TestP001FenceRunsBeforeMutation(t *testing.T) {
	_, pricing := setupVersionedPricingTest(t)
	called := false
	_, err := pricing.executePriceMutation(context.Background(), 2, func(*gorm.DB) error { called = true; return errPriceMutationNoChange })
	if !errors.Is(err, ErrPublicationVersionConflict) || called {
		t.Fatalf("stale callback ran=%v error=%v", called, err)
	}
}

func TestP001PublicationFailurePreservesCommittedSuccess(t *testing.T) {
	db, pricing := setupVersionedPricingTest(t)
	if err := pricing.Init(); err != nil {
		t.Fatal(err)
	}
	source := []*Price{{Model: "ablation", Type: TokensPriceType, Input: 3, Output: 4}}
	preview, err := PreviewPriceChange(context.Background(), source, PriceUpdateModeAdd)
	if err != nil {
		t.Fatal(err)
	}
	queries := 0
	if err := db.Callback().Query().Before("gorm:query").Register("ablation:fail_publication", func(tx *gorm.DB) {
		if tx.Statement.Table == "prices" {
			queries++
			if queries > 2 {
				tx.AddError(errors.New("publication read unavailable"))
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
	version, err := ApplyPriceChange(context.Background(), pricing, source, PriceUpdateModeAdd, preview.BaseVersion, preview.Digest)
	if err != nil || version != 2 {
		t.Fatalf("committed result version=%d err=%v", version, err)
	}
	head, err := ReadPublicationVersion(context.Background(), db, PublicationOwnerPrice)
	if err != nil || head != 2 {
		t.Fatalf("head=%d err=%v", head, err)
	}
	if err := db.Callback().Query().Remove("ablation:fail_publication"); err != nil {
		t.Fatal(err)
	}
	var count int64
	if err := db.Model(&Price{}).Where("model = ?", "ablation").Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("count=%d err=%v", count, err)
	}
	if pricing.PublishedVersion() != 1 || !pricing.IsDegraded() {
		t.Fatalf("failed publish leaked snapshot or degradation: version=%d degraded=%v", pricing.PublishedVersion(), pricing.IsDegraded())
	}
}

type p001AckLossPool struct{ *sql.DB }

func (p p001AckLossPool) BeginTx(ctx context.Context, opts *sql.TxOptions) (gorm.ConnPool, error) {
	tx, err := p.DB.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &p001AckLossTx{tx}, nil
}

type p001AckLossTx struct{ *sql.Tx }

func (tx p001AckLossTx) Commit() error {
	if err := tx.Tx.Commit(); err != nil {
		return err
	}
	return errors.New("commit acknowledgement lost")
}
func TestP001PlanConfirmsLostCommitAcknowledgement(t *testing.T) {
	db, pricing := setupVersionedPricingTest(t)
	if err := pricing.Init(); err != nil {
		t.Fatal(err)
	}
	source := []*Price{{Model: "ablation", Type: TokensPriceType, Input: 3, Output: 4}}
	preview, err := PreviewPriceChange(context.Background(), source, PriceUpdateModeAdd)
	if err != nil {
		t.Fatal(err)
	}
	pool, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	wrapper := p001AckLossPool{pool}
	db.Config.ConnPool = wrapper
	db.Statement.ConnPool = wrapper
	version, err := ApplyPriceChange(context.Background(), pricing, source, PriceUpdateModeAdd, preview.BaseVersion, preview.Digest)
	if err != nil || version != 2 || pricing.PublishedVersion() != 2 {
		t.Fatalf("lost ack unresolved version=%d published=%d err=%v", version, pricing.PublishedVersion(), err)
	}
	var count int64
	if err := db.Model(&Price{}).Where("model = ?", "ablation").Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("count=%d err=%v", count, err)
	}
}
