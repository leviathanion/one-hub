package wxpay

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func TestCertificateRefreshSerializesDownloads(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls, active atomic.Int32
		release := make(chan struct{})
		r := startCertificateRefresh(context.Background(), func(context.Context) error {
			if active.Add(1) != 1 {
				t.Error("证书下载并发执行")
			}
			defer active.Add(-1)
			calls.Add(1)
			<-release
			return nil
		}, func(err error) { t.Error(err) })
		defer r.close()
		synctest.Wait()
		time.Sleep(certificateRefreshInterval)
		synctest.Wait()
		if calls.Load() != 1 {
			t.Fatal("没有按周期触发下载")
		}
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := r.refresh(context.Background()); err != nil {
				t.Error(err)
			}
		}()
		if calls.Load() != 1 {
			t.Fatal("刷新未等待正在执行的下载")
		}
		close(release)
		wg.Wait()
		if calls.Load() != 2 {
			t.Fatal("串行刷新未完成")
		}
	})
}

func TestCertificateRefreshCancelJoinsConcurrentClose(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cancelled, release := make(chan struct{}), make(chan struct{})
		var calls atomic.Int32
		r := startCertificateRefresh(context.Background(), func(ctx context.Context) error {
			calls.Add(1)
			<-ctx.Done()
			close(cancelled)
			<-release
			return ctx.Err()
		}, func(err error) { t.Errorf("退役取消不应报告刷新故障: %v", err) })
		synctest.Wait()
		time.Sleep(certificateRefreshInterval)
		synctest.Wait()
		var wg sync.WaitGroup
		var closed atomic.Int32
		for i := 0; i < 16; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				r.close()
				closed.Add(1)
			}()
		}
		<-cancelled
		synctest.Wait()
		if closed.Load() != 0 {
			t.Fatal("下载尚未退出，关闭已提前返回")
		}
		close(release)
		wg.Wait()
		r.close()
		time.Sleep(2 * certificateRefreshInterval)
		if calls.Load() != 1 || closed.Load() != 16 {
			t.Fatal("关闭后仍执行刷新，或并发关闭没有完成")
		}
	})
}

func TestCertificateRefreshTimeoutAndNextScheduledAttempt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls, reports atomic.Int32
		r := startCertificateRefresh(context.Background(), func(ctx context.Context) error {
			calls.Add(1)
			<-ctx.Done()
			return ctx.Err()
		}, func(err error) {
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("下载未受超时控制: %v", err)
			}
			reports.Add(1)
		})
		defer r.close()
		synctest.Wait()
		time.Sleep(certificateRefreshInterval + certificateDownloadTimeout)
		synctest.Wait()
		if calls.Load() != 1 || reports.Load() != 1 {
			t.Fatal("下载超时未退出或未报告")
		}
		time.Sleep(certificateRefreshInterval - certificateDownloadTimeout)
		synctest.Wait()
		if calls.Load() != 2 {
			t.Fatal("超时后没有在下一周期继续刷新")
		}
	})
}
