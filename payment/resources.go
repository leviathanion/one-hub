package payment

import (
	"context"
	"errors"
	"one-api/common/logger"
	"one-api/payment/types"
	"sync"
)

// 每个 revision 拥有一组 SDK 资源；短期句柄结束后才同步退役后台任务。
type resourceKey struct {
	id       int
	revision int64
	profile  string
}
type boundResource struct {
	client   GatewayClient
	cancel   context.CancelFunc
	ready    chan struct{}
	done     chan struct{}
	err      error
	refs     int
	draining bool
	closing  bool
}
type ResourcePool struct {
	mu      sync.Mutex
	ctx     context.Context
	cancel  context.CancelFunc
	entries map[resourceKey]*boundResource
	all     map[*boundResource]struct{}
	latest  map[int]int64
	closed  bool
}
type GatewayHandle struct {
	Client GatewayClient
	pool   *ResourcePool
	entry  *boundResource
	once   sync.Once
}

func NewResourcePool(ctx context.Context) *ResourcePool {
	ctx, cancel := context.WithCancel(ctx)
	return &ResourcePool{ctx: ctx, cancel: cancel, entries: make(map[resourceKey]*boundResource), all: make(map[*boundResource]struct{}), latest: make(map[int]int64)}
}

var Resources = NewResourcePool(context.Background())

func (p *ResourcePool) Acquire(ctx context.Context, snapshot types.GatewaySnapshot) (*GatewayHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	key := resourceKey{snapshot.GatewayID, snapshot.CredentialRevision, snapshot.Identity.ProtocolProfile}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, errors.New("支付资源已关闭")
	}
	if p.latest[key.id] > key.revision {
		p.mu.Unlock()
		return nil, errors.New("支付凭证已轮换，请重新读取")
	}
	entry := p.entries[key]
	if entry == nil {
		serviceCtx, cancel := context.WithCancel(p.ctx)
		entry = &boundResource{cancel: cancel, ready: make(chan struct{}), done: make(chan struct{})}
		p.entries[key] = entry
		p.all[entry] = struct{}{}
		p.mu.Unlock()
		client, err := newBoundClient(serviceCtx, snapshot)
		p.mu.Lock()
		entry.client = client
		entry.err = err
		if err != nil {
			entry.draining = true
			if p.entries[key] == entry {
				delete(p.entries, key)
			}
		}
		close(entry.ready)
		p.mu.Unlock()
		if err != nil {
			p.retire(entry)
			return nil, err
		}
	} else {
		p.mu.Unlock()
		select {
		case <-entry.ready:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	p.mu.Lock()
	if entry.err != nil || entry.draining || p.closed || p.latest[key.id] > key.revision {
		err := entry.err
		if err == nil {
			err = errors.New("支付凭证已轮换，请重新读取")
		}
		p.mu.Unlock()
		p.retire(entry)
		return nil, err
	}
	entry.refs++
	p.latest[key.id] = key.revision
	var old []*boundResource
	for k, e := range p.entries {
		if k.id == key.id && k != key && k.revision < key.revision {
			e.draining = true
			delete(p.entries, k)
			old = append(old, e)
		}
	}
	p.mu.Unlock()
	for _, e := range old {
		p.retire(e)
	}
	return &GatewayHandle{Client: entry.client, pool: p, entry: entry}, nil
}
func (h *GatewayHandle) Release() {
	if h == nil {
		return
	}
	h.once.Do(func() { h.pool.mu.Lock(); h.entry.refs--; h.pool.mu.Unlock(); h.pool.retire(h.entry) })
}
func (p *ResourcePool) retire(entry *boundResource) {
	p.mu.Lock()
	if !entry.draining || entry.refs != 0 || entry.closing {
		p.mu.Unlock()
		return
	}
	select {
	case <-entry.ready:
	default:
		p.mu.Unlock()
		return
	}
	entry.closing = true
	p.mu.Unlock()
	entry.cancel()
	if closer, ok := entry.client.(GatewayResourceCloser); ok {
		if err := closer.CloseResources(); err != nil {
			logger.SysError("支付资源退役失败: " + err.Error())
		}
	}
	close(entry.done)
	p.mu.Lock()
	delete(p.all, entry)
	p.mu.Unlock()
}
func (p *ResourcePool) Publish(snapshot types.GatewaySnapshot, client GatewayClient, cancel context.CancelFunc) error {
	key := resourceKey{snapshot.GatewayID, snapshot.CredentialRevision, snapshot.Identity.ProtocolProfile}
	ready := make(chan struct{})
	close(ready)
	entry := &boundResource{client: client, cancel: cancel, ready: ready, done: make(chan struct{})}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		entry.draining = true
		p.retire(entry)
		return errors.New("支付资源已关闭")
	}
	var old []*boundResource
	for k, e := range p.entries {
		if k.id == key.id {
			if k.revision > key.revision {
				p.mu.Unlock()
				entry.draining = true
				p.retire(entry)
				return errors.New("支付资源版本冲突")
			}
			e.draining = true
			delete(p.entries, k)
			old = append(old, e)
		}
	}
	p.entries[key] = entry
	p.all[entry] = struct{}{}
	p.latest[key.id] = key.revision
	p.mu.Unlock()
	for _, e := range old {
		p.retire(e)
	}
	return nil
}
func (p *ResourcePool) Close() {
	p.mu.Lock()
	p.closed = true
	entries := make([]*boundResource, 0, len(p.all))
	for entry := range p.all {
		entry.draining = true
		entries = append(entries, entry)
	}
	p.entries = make(map[resourceKey]*boundResource)
	p.mu.Unlock()
	for _, entry := range entries {
		<-entry.ready
		p.retire(entry)
		<-entry.done
	}
	p.cancel()
}
