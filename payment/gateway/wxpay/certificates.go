package wxpay

import (
	"context"
	"sync"
	"time"
)

const certificateRefreshInterval = 6 * time.Hour
const certificateDownloadTimeout = 15 * time.Second

// certificateRefresh 只拥有本凭据版本的刷新任务；下载、缓存和验签仍由 SDK 负责。
type certificateRefresh struct {
	cancel   context.CancelFunc
	done     chan struct{}
	mu       sync.Mutex
	download func(context.Context) error
}

func startCertificateRefresh(ctx context.Context, download func(context.Context) error, report func(error)) *certificateRefresh {
	ctx, cancel := context.WithCancel(ctx)
	r := &certificateRefresh{cancel: cancel, done: make(chan struct{}), download: download}
	go func() {
		defer close(r.done)
		ticker := time.NewTicker(certificateRefreshInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := r.refresh(ctx); err != nil && ctx.Err() == nil {
					report(err)
				}
			}
		}
	}()
	return r
}

func (r *certificateRefresh) refresh(ctx context.Context) error {
	// SDK 会在刷新时替换内部 client，整个下载过程必须串行，不能只锁证书缓存。
	r.mu.Lock()
	defer r.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, certificateDownloadTimeout)
	defer cancel()
	return r.download(ctx)
}

func (r *certificateRefresh) close() {
	r.cancel()
	// 不持有刷新锁或资源池锁等待；重复关闭也必须等到同一个任务实际退出。
	<-r.done
}
