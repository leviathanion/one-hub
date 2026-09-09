package cache

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"one-api/common/config"
	commonredis "one-api/common/redis"

	"github.com/coocood/freecache"
	cacheM "github.com/eko/gocache/lib/v4/cache"
	"github.com/eko/gocache/lib/v4/marshaler"
	"github.com/eko/gocache/lib/v4/store"
	freecacheStore "github.com/eko/gocache/store/freecache/v4"
	"github.com/redis/go-redis/v9"
)

type consumeTestValue struct {
	Verifier string
	Target   int
}

func assertOneCacheConsumer(t *testing.T, key string) {
	t.Helper()
	want := consumeTestValue{Verifier: "local-oauth-test-verifier", Target: 42}
	if err := SetCache(key, want, time.Minute); err != nil {
		t.Fatal(err)
	}
	var successes atomic.Int32
	var workers sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 48; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			got, err := ConsumeCache[consumeTestValue](key)
			if err == nil {
				successes.Add(1)
				if got != want {
					t.Errorf("消费内容错误：%+v", got)
				}
				return
			}
			if !errors.Is(err, CacheNotFound) || got != (consumeTestValue{}) {
				t.Errorf("重复消费必须只有 miss 且不泄露值：got=%+v err=%v", got, err)
			}
		}()
	}
	close(start)
	workers.Wait()
	if successes.Load() != 1 {
		t.Fatalf("同 key 并发成功次数 = %d，期望 1", successes.Load())
	}
	if got, err := GetCache[consumeTestValue](key); !errors.Is(err, CacheNotFound) || got != (consumeTestValue{}) {
		t.Fatalf("消费后普通读取仍能获得值：%+v %v", got, err)
	}
}

func TestConsumeCacheLocalConcurrent(t *testing.T) {
	useTestCacheManager(t)
	assertOneCacheConsumer(t, "i003-local-concurrent")
}

type consumeTestTimer struct{ now atomic.Uint32 }

func (timer *consumeTestTimer) Now() uint32 { return timer.now.Load() }

func TestConsumeCacheLocalMissingExpiredAndCanceled(t *testing.T) {
	useTestCacheManager(t)
	timer := &consumeTestTimer{}
	timer.now.Store(100)
	cacheClient = cacheM.New[any](freecacheStore.NewFreecache(freecache.NewCacheCustomTimer(localCacheCapacityBytes, timer)))
	kvCache = marshaler.New(cacheClient)
	if got, err := ConsumeCache[string]("missing"); !errors.Is(err, CacheNotFound) || got != "" {
		t.Fatalf("不存在的 key：%q %v", got, err)
	}
	if err := SetCache("expired", "secret", time.Second); err != nil {
		t.Fatal(err)
	}
	timer.now.Store(102)
	if got, err := ConsumeCache[string]("expired"); !errors.Is(err, CacheNotFound) || got != "" {
		t.Fatalf("过期的 key：%q %v", got, err)
	}
	if err := SetCache("canceled", "secret", time.Minute); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got, err := ConsumeCacheContext[string](ctx, "canceled"); !errors.Is(err, context.Canceled) || got != "" {
		t.Fatalf("取消的消费返回了值：%q %v", got, err)
	}
	if got, err := ConsumeCacheContext[string](nil, "canceled"); err != nil || got != "secret" {
		t.Fatalf("消费前取消不应删除值：%q %v", got, err)
	}
}

type consumeFailingDeleteStore struct{ store.StoreInterface }

var errConsumeDelete = errors.New("本地删除失败")

func (s consumeFailingDeleteStore) Delete(context.Context, any) error { return errConsumeDelete }

func TestConsumeCacheLocalErrorsDoNotReturnPayload(t *testing.T) {
	useTestCacheManager(t)
	if err := SetCache("delete-failure", "secret", time.Minute); err != nil {
		t.Fatal(err)
	}
	backend := cacheClient.GetCodec().GetStore()
	cacheClient = cacheM.New[any](consumeFailingDeleteStore{backend})
	kvCache = marshaler.New(cacheClient)
	if got, err := ConsumeCache[string]("delete-failure"); !errors.Is(err, errConsumeDelete) || got != "" {
		t.Fatalf("删除失败泄露了 payload：%q %v", got, err)
	}
	if got, err := GetCache[string]("delete-failure"); err != nil || got != "secret" {
		t.Fatalf("消费失败不应改动普通读取语义：%q %v", got, err)
	}
	cacheClient = cacheM.New[any](backend)
	kvCache = marshaler.New(cacheClient)
	if err := cacheClient.Set(context.Background(), "malformed", []byte{0xa3, 'b'}); err != nil {
		t.Fatal(err)
	}
	if got, err := ConsumeCache[string]("malformed"); err == nil || got != "" {
		t.Fatalf("解码失败泄露了 payload：%q %v", got, err)
	}
	if _, err := ConsumeCache[string]("malformed"); !errors.Is(err, CacheNotFound) {
		t.Fatalf("畸形的已消费状态仍可重用：%v", err)
	}
}

func useConsumeRealRedis(t *testing.T) *redis.Client {
	t.Helper()
	url := os.Getenv("ONEHUB_TEST_REDIS_URL")
	if url == "" {
		t.Skip("真实 Redis 用例需要显式 ONEHUB_TEST_REDIS_URL")
	}
	options, err := redis.ParseURL(url)
	if err != nil {
		t.Fatal(err)
	}
	host, _, err := net.SplitHostPort(options.Addr)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		t.Fatal("真实 Redis 用例只接受显式 loopback 测试实例")
	}
	client := redis.NewClient(options)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		client.Close()
		t.Fatal(err)
	}
	oldEnabled, oldRedis := config.RedisEnabled, commonredis.RDB
	oldClient, oldKV := cacheClient, kvCache
	config.RedisEnabled, commonredis.RDB = true, client
	InitCacheManager()
	t.Cleanup(func() {
		config.RedisEnabled, commonredis.RDB = oldEnabled, oldRedis
		cacheClient, kvCache = oldClient, oldKV
		client.Close()
	})
	return client
}

func consumeRedisKey(t *testing.T, client *redis.Client) string {
	t.Helper()
	key := fmt.Sprintf("onehub:i003:consume:%s:%d", t.Name(), time.Now().UnixNano())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := client.Del(ctx, key).Err(); err != nil {
			t.Errorf("清理本次独立 Redis key：%v", err)
		}
	})
	return key
}

func TestConsumeCacheRealRedis(t *testing.T) {
	client := useConsumeRealRedis(t)
	t.Run("concurrent", func(t *testing.T) {
		assertOneCacheConsumer(t, consumeRedisKey(t, client))
	})
	t.Run("missing_expired", func(t *testing.T) {
		key := consumeRedisKey(t, client)
		if got, err := ConsumeCache[string](key); !errors.Is(err, CacheNotFound) || got != "" {
			t.Fatalf("不存在的 key：%q %v", got, err)
		}
		if err := SetCache(key, "secret", time.Millisecond); err != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
		if got, err := ConsumeCache[string](key); !errors.Is(err, CacheNotFound) || got != "" {
			t.Fatalf("过期的 key：%q %v", got, err)
		}
	})
	t.Run("read_error", func(t *testing.T) {
		key := consumeRedisKey(t, client)
		if err := client.LPush(context.Background(), key, "secret").Err(); err != nil {
			t.Fatal(err)
		}
		if got, err := ConsumeCache[string](key); err == nil || got != "" {
			t.Fatalf("Redis GET 类型错误泄露了 payload：%q %v", got, err)
		}
	})
	t.Run("lost_reply_is_not_retried", func(t *testing.T) {
		key := consumeRedisKey(t, client)
		if err := SetCache(key, "secret", time.Minute); err != nil {
			t.Fatal(err)
		}
		var sent atomic.Int32
		options := *client.Options()
		options.Dialer = func(ctx context.Context, network, addr string) (net.Conn, error) {
			conn, err := (&net.Dialer{}).DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			return &consumeLostReplyConn{Conn: conn, sent: &sent}, nil
		}
		failingClient := redis.NewClient(&options)
		defer failingClient.Close()
		commonredis.RDB = failingClient
		defer func() { commonredis.RDB = client }()
		if got, err := ConsumeCache[string](key); err == nil || errors.Is(err, CacheNotFound) || got != "" {
			t.Fatalf("丢失回复必须保留消费失败且不泄露值：%q %v", got, err)
		}
		if sent.Load() != 1 {
			t.Fatalf("歧义消费实际发送次数 = %d，期望 1", sent.Load())
		}
		if err := client.Get(context.Background(), key).Err(); !errors.Is(err, redis.Nil) {
			t.Fatalf("服务端应已完成消费：%v", err)
		}
	})
}

type consumeLostReplyConn struct {
	net.Conn
	sent      *atomic.Int32
	dropReply atomic.Bool
}

func (c *consumeLostReplyConn) Write(payload []byte) (int, error) {
	if bytes.Contains(payload, []byte(consumeCacheScript)) {
		c.sent.Add(1)
		c.dropReply.Store(true)
	}
	return c.Conn.Write(payload)
}

func (c *consumeLostReplyConn) Read(payload []byte) (int, error) {
	n, err := c.Conn.Read(payload)
	if n > 0 && c.dropReply.Swap(false) {
		return 0, io.ErrUnexpectedEOF
	}
	return n, err
}
