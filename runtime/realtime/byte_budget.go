package realtime

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
)

var ErrByteBudgetExceeded = errors.New("realtime byte budget exceeded")

// ByteBudget owns the retained payload bytes of one queue stage. Credits are
// stage-local: a handoff acquires the destination credit before releasing the
// source credit.
type ByteBudget struct {
	mu      sync.Mutex
	limit   int64
	used    int64
	changed chan struct{}
}

type ByteCredit struct {
	budget   *ByteBudget
	bytes    int64
	released atomic.Bool
}

func NewByteBudget(limit int64) *ByteBudget {
	if limit < 0 {
		limit = 0
	}
	return &ByteBudget{limit: limit, changed: make(chan struct{})}
}

func (b *ByteBudget) TryAcquire(bytes int) (*ByteCredit, bool) {
	if b == nil || bytes < 0 {
		return nil, false
	}
	want := int64(bytes)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.limit <= 0 || want > b.limit-b.used {
		return nil, false
	}
	b.used += want
	return &ByteCredit{budget: b, bytes: want}, true
}

func (b *ByteBudget) Acquire(ctx context.Context, bytes int) (*ByteCredit, error) {
	if b == nil || bytes < 0 {
		return nil, ErrByteBudgetExceeded
	}
	if ctx == nil {
		ctx = context.Background()
	}
	want := int64(bytes)
	for {
		b.mu.Lock()
		if b.limit <= 0 || want > b.limit {
			b.mu.Unlock()
			return nil, ErrByteBudgetExceeded
		}
		if want <= b.limit-b.used {
			b.used += want
			b.mu.Unlock()
			return &ByteCredit{budget: b, bytes: want}, nil
		}
		changed := b.changed
		b.mu.Unlock()

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-changed:
		}
	}
}

func (b *ByteBudget) Used() int64 {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.used
}

func (b *ByteBudget) Limit() int64 {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.limit
}

func (c *ByteCredit) Release() {
	if c == nil || !c.released.CompareAndSwap(false, true) || c.budget == nil || c.bytes <= 0 {
		return
	}
	c.budget.mu.Lock()
	c.budget.used -= c.bytes
	close(c.budget.changed)
	c.budget.changed = make(chan struct{})
	c.budget.mu.Unlock()
}
