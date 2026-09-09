package model

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"one-api/common/cache"
	"one-api/common/config"
	commonredis "one-api/common/redis"
	"one-api/internal/testutil/sqlitetest"

	"github.com/redis/go-redis/v9"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

type issue031TraceKey struct{}

type issue031ObservedContexts struct {
	lookup            atomic.Value
	insert            atomic.Value
	lookupDeadline    atomic.Value
	insertDeadline    atomic.Value
	insertErrAtCreate atomic.Bool
}

func (o *issue031ObservedContexts) lookupContext() context.Context {
	value := o.lookup.Load()
	if value == nil {
		return nil
	}
	return value.(context.Context)
}

func (o *issue031ObservedContexts) insertContext() context.Context {
	value := o.insert.Load()
	if value == nil {
		return nil
	}
	return value.(context.Context)
}

func (o *issue031ObservedContexts) lookupDeadlineValue() (time.Time, bool) {
	value := o.lookupDeadline.Load()
	if value == nil {
		return time.Time{}, false
	}
	return value.(time.Time), true
}

func (o *issue031ObservedContexts) insertDeadlineValue() (time.Time, bool) {
	value := o.insertDeadline.Load()
	if value == nil {
		return time.Time{}, false
	}
	return value.(time.Time), true
}

func useIssue031DB(t *testing.T) *gorm.DB {
	t.Helper()

	db, err := gorm.Open(sqlite.Open(sqlitetest.MemoryDSN()), &gorm.Config{})
	if err != nil {
		t.Fatalf("打开消费日志测试数据库失败：%v", err)
	}
	if err := db.AutoMigrate(&User{}, &Token{}, &Log{}); err != nil {
		t.Fatalf("迁移消费日志测试表失败：%v", err)
	}

	oldDB := DB
	oldRedisEnabled := config.RedisEnabled
	oldBatchUpdateEnabled := config.BatchUpdateEnabled
	DB = db
	config.RedisEnabled = false
	config.BatchUpdateEnabled = false
	t.Cleanup(func() {
		DB = oldDB
		config.RedisEnabled = oldRedisEnabled
		config.BatchUpdateEnabled = oldBatchUpdateEnabled
	})
	return db
}

func issue031EnableLogging(t *testing.T, batch bool) {
	t.Helper()
	oldLogConsumeEnabled := config.LogConsumeEnabled
	oldBatchUpdateEnabled := config.BatchUpdateEnabled
	config.LogConsumeEnabled = true
	config.BatchUpdateEnabled = batch
	t.Cleanup(func() {
		config.LogConsumeEnabled = oldLogConsumeEnabled
		config.BatchUpdateEnabled = oldBatchUpdateEnabled
	})
}

func insertIssue031User(t *testing.T, db *gorm.DB) {
	t.Helper()
	if err := db.Create(&User{
		Id:          1,
		Username:    "issue031-user",
		Password:    "password123",
		AccessToken: "issue031-access-token",
		Quota:       1000,
		Group:       "default",
		Status:      config.UserStatusEnabled,
		Role:        config.RoleCommonUser,
		CreatedTime: 1,
	}).Error; err != nil {
		t.Fatalf("写入消费日志用户夹具失败：%v", err)
	}
	if err := db.Session(&gorm.Session{SkipHooks: true}).Create(&Token{
		Id:             1,
		UserId:         1,
		Key:            "issue031-token-key",
		Status:         config.TokenStatusEnabled,
		Name:           "issue031-token",
		ExpiredTime:    -1,
		RemainQuota:    1000,
		UnlimitedQuota: false,
		Group:          "default",
	}).Error; err != nil {
		t.Fatalf("写入消费日志令牌夹具失败：%v", err)
	}
}

func debitIssue031Quota(t *testing.T) {
	t.Helper()
	result, err := ApplyBillingReserve(context.Background(), 1, 1, 250)
	if err != nil || result.Outcome != BillingBalanceCommitted || !result.TokenQuotaApplied {
		t.Fatalf("建立已扣款 250 的真实资金状态失败：result=%+v err=%v", result, err)
	}
	var user User
	if err := DB.Unscoped().First(&user, 1).Error; err != nil {
		t.Fatalf("读取已扣款用户余额失败：%v", err)
	}
	var token Token
	if err := DB.Unscoped().First(&token, 1).Error; err != nil {
		t.Fatalf("读取已扣款令牌余额失败：%v", err)
	}
	if user.Quota != 750 || token.RemainQuota != 750 || token.UsedQuota != 250 {
		t.Fatalf("已扣款资金状态不正确：user.quota=%d token.remain=%d token.used=%d", user.Quota, token.RemainQuota, token.UsedQuota)
	}
}

func observeIssue031SQL(t *testing.T, db *gorm.DB, observed *issue031ObservedContexts, lookupDelay time.Duration, lookupErr error) {
	t.Helper()

	if err := db.Callback().Query().Before("gorm:query").Register("issue031_observe_username_lookup", func(tx *gorm.DB) {
		if tx.Statement.Schema == nil || tx.Statement.Schema.Table != "users" {
			return
		}
		observed.lookup.Store(tx.Statement.Context)
		if deadline, ok := tx.Statement.Context.Deadline(); ok {
			observed.lookupDeadline.Store(deadline)
		}
		if lookupErr != nil && lookupDelay <= 0 {
			tx.AddError(lookupErr)
			return
		}
		if lookupDelay <= 0 {
			return
		}
		timer := time.NewTimer(lookupDelay)
		defer timer.Stop()
		select {
		case <-timer.C:
			if lookupErr != nil {
				tx.AddError(lookupErr)
			}
		case <-tx.Statement.Context.Done():
			tx.AddError(tx.Statement.Context.Err())
		}
	}); err != nil {
		t.Fatalf("注册用户名查询观察器失败：%v", err)
	}
	if err := db.Callback().Create().Before("gorm:create").Register("issue031_observe_log_insert", func(tx *gorm.DB) {
		if tx.Statement.Schema != nil && tx.Statement.Schema.Table == "logs" {
			observed.insert.Store(tx.Statement.Context)
			if deadline, ok := tx.Statement.Context.Deadline(); ok {
				observed.insertDeadline.Store(deadline)
			}
			if tx.Statement.Context.Err() != nil {
				observed.insertErrAtCreate.Store(true)
			}
		}
	}); err != nil {
		t.Fatalf("注册日志写入观察器失败：%v", err)
	}
}

func readIssue031Log(t *testing.T, db *gorm.DB, content string) Log {
	t.Helper()
	var count int64
	if err := db.Model(&Log{}).Where("content = ?", content).Count(&count).Error; err != nil {
		t.Fatalf("统计消费日志失败：%v", err)
	}
	if count != 1 {
		t.Fatalf("期望恰好一条消费日志，实际 %d 条", count)
	}
	var log Log
	if err := db.Where("content = ?", content).First(&log).Error; err != nil {
		t.Fatalf("读取消费日志失败：%v", err)
	}
	return log
}

func assertIssue031ContextDeadline(t *testing.T, ctx context.Context, want time.Duration, label string) {
	t.Helper()
	if ctx == nil {
		t.Fatalf("没有观察到%s context", label)
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatalf("%s context 没有 deadline", label)
	}
	remaining := time.Until(deadline)
	if remaining > want {
		t.Fatalf("%s deadline 剩余时间异常：%s，期望不超过 %s", label, remaining, want)
	}
}

func assertIssue031InsertBudget(t *testing.T, observed *issue031ObservedContexts, lookupTimedOut bool) {
	t.Helper()
	lookupDeadline, lookupOK := observed.lookupDeadlineValue()
	insertDeadline, insertOK := observed.insertDeadlineValue()
	if !lookupOK || !insertOK {
		t.Fatalf("未同时观察到用户名查询和日志写入 deadline：lookup=%v insert=%v", lookupOK, insertOK)
	}
	if observed.insertErrAtCreate.Load() {
		t.Fatal("日志 INSERT 开始时 context 已经结束")
	}
	if lookupTimedOut {
		minimum := lookupDeadline.Add(consumeLogWriteTimeout - 100*time.Millisecond)
		if insertDeadline.Before(minimum) {
			t.Fatalf("用户名查询超时后未获得独立完整 INSERT 预算：lookup deadline=%s insert deadline=%s", lookupDeadline, insertDeadline)
		}
		return
	}
	if remaining := time.Until(insertDeadline); remaining < consumeLogWriteTimeout-100*time.Millisecond {
		t.Fatalf("日志 INSERT context 剩余预算过短：%s，期望接近 %s", remaining, consumeLogWriteTimeout)
	}
}

func assertIssue031AuditFields(t *testing.T, log Log, wantUsername string) {
	t.Helper()
	if log.UserId != 1 || log.Username != wantUsername || log.ModelName != "issue031-model" || log.TokenName != "issue031-token" || log.Quota != 250 || log.ChannelId != 31 || log.PromptTokens != 11 || log.CompletionTokens != 12 || log.CacheTokens != 13 || log.CacheReadTokens != 14 || log.CacheWriteTokens != 15 || log.RequestTime != 16 || log.IsStream || log.SourceIp != "127.0.0.31" {
		t.Fatalf("消费日志审计字段不完整：%+v", log)
	}
}

func recordIssue031Log(ctx context.Context) {
	RecordConsumeLog(ctx, 1, 31, 11, 12, 13, 14, 15, "issue031-model", "issue031-token", 250, "issue031-audit", 16, false, map[string]any{"case": "issue031"}, "127.0.0.31")
}

func TestIssue031SlowRedisDoesNotDelayOrReplaceSQLUsernameLookup(t *testing.T) {
	db := useIssue031DB(t)
	insertIssue031User(t, db)
	issue031EnableLogging(t, false)
	debitIssue031Quota(t)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("创建无响应 Redis 夹具失败：%v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	oldRedisEnabled, oldRedis := config.RedisEnabled, commonredis.RDB
	client := redis.NewClient(&redis.Options{
		Addr:            listener.Addr().String(),
		DialTimeout:     50 * time.Millisecond,
		ReadTimeout:     300 * time.Millisecond,
		WriteTimeout:    300 * time.Millisecond,
		MaxRetries:      0,
		Protocol:        2,
		DisableIdentity: true,
	})
	var getCount atomic.Int32
	client.AddHook(issue031RedisGetCounter{getCount: &getCount})
	commonredis.RDB = client
	config.RedisEnabled = true
	cache.InitCacheManager()
	t.Cleanup(func() {
		_ = client.Close()
		config.RedisEnabled = oldRedisEnabled
		commonredis.RDB = oldRedis
		cache.InitCacheManager()
	})

	started := time.Now()
	recordIssue031Log(context.Background())
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("Redis 无响应不应耗尽日志路径：耗时 %s", elapsed)
	}
	if got := getCount.Load(); got != 0 {
		t.Fatalf("消费日志路径不应发 Redis GET，实际 %d 次", got)
	}
	log := readIssue031Log(t, db, "issue031-audit")
	assertIssue031AuditFields(t, log, "issue031-user")
}

func TestIssue031UsernameLookupFailureKeepsIndependentLogWriteBudget(t *testing.T) {
	cases := []struct {
		name         string
		lookupDelay  time.Duration
		lookupErr    error
		wantUsername string
	}{
		{name: "healthy", wantUsername: "issue031-user"},
		{name: "deadline", lookupDelay: consumeLogUsernameLookupTimeout + 50*time.Millisecond},
		{name: "failure", lookupErr: errors.New("issue031 planned username failure")},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := useIssue031DB(t)
			insertIssue031User(t, db)
			issue031EnableLogging(t, false)
			debitIssue031Quota(t)
			var observed issue031ObservedContexts
			observeIssue031SQL(t, db, &observed, tc.lookupDelay, tc.lookupErr)

			traceCtx := context.WithValue(context.Background(), issue031TraceKey{}, "issue031-trace")
			started := time.Now()
			recordIssue031Log(traceCtx)
			if elapsed := time.Since(started); elapsed > 2*time.Second {
				t.Fatalf("用户名查询失败不应阻塞独立日志写入：耗时 %s", elapsed)
			}

			lookupCtx := observed.lookupContext()
			assertIssue031ContextDeadline(t, lookupCtx, consumeLogUsernameLookupTimeout, "用户名查询")
			if got := lookupCtx.Value(issue031TraceKey{}); got != "issue031-trace" {
				t.Fatalf("用户名查询丢失请求追踪值：%v", got)
			}
			lookupErr := lookupCtx.Err()
			if tc.lookupDelay <= 0 && !errors.Is(lookupErr, context.Canceled) {
				t.Fatalf("用户名查询 context 未在查询后取消：%v", lookupCtx.Err())
			}
			if tc.lookupDelay > 0 && !errors.Is(lookupErr, context.DeadlineExceeded) && !errors.Is(lookupErr, context.Canceled) {
				t.Fatalf("用户名查询 context 未在期限或查询后取消：%v", lookupErr)
			}

			insertCtx := observed.insertContext()
			assertIssue031ContextDeadline(t, insertCtx, consumeLogWriteTimeout, "日志写入")
			if got := insertCtx.Value(issue031TraceKey{}); got != "issue031-trace" {
				t.Fatalf("日志写入丢失请求追踪值：%v", got)
			}
			if !errors.Is(insertCtx.Err(), context.Canceled) {
				t.Fatalf("日志写入 context 未在函数返回后取消：%v", insertCtx.Err())
			}
			assertIssue031InsertBudget(t, &observed, tc.lookupDelay > 0)

			log := readIssue031Log(t, db, "issue031-audit")
			assertIssue031AuditFields(t, log, tc.wantUsername)
		})
	}
}

func TestIssue031CanceledRequestStillWritesAuditableLog(t *testing.T) {
	db := useIssue031DB(t)
	insertIssue031User(t, db)
	issue031EnableLogging(t, false)
	debitIssue031Quota(t)
	var observed issue031ObservedContexts
	observeIssue031SQL(t, db, &observed, 0, nil)

	requestCtx, cancel := context.WithCancel(context.WithValue(context.Background(), issue031TraceKey{}, "issue031-canceled-trace"))
	cancel()
	recordIssue031Log(requestCtx)

	if lookupCtx := observed.lookupContext(); lookupCtx == nil || lookupCtx.Err() != context.Canceled {
		t.Fatalf("预取消请求的用户名查询 context 未独立取消：%v", lookupCtx)
	}
	if insertCtx := observed.insertContext(); insertCtx == nil || insertCtx.Err() != context.Canceled {
		t.Fatalf("预取消请求的日志写入 context 未独立取消：%v", insertCtx)
	}
	assertIssue031InsertBudget(t, &observed, false)
	log := readIssue031Log(t, db, "issue031-audit")
	assertIssue031AuditFields(t, log, "issue031-user")
}

func TestIssue031BatchModeOnlyQueuesAndExistingFlushWritesLog(t *testing.T) {
	db := useIssue031DB(t)
	insertIssue031User(t, db)
	issue031EnableLogging(t, true)
	debitIssue031Quota(t)

	batchLogLock.Lock()
	previousBatchLogs := batchLogStore
	batchLogStore = nil
	batchLogLock.Unlock()
	t.Cleanup(func() {
		batchLogLock.Lock()
		batchLogStore = previousBatchLogs
		batchLogLock.Unlock()
	})

	recordIssue031Log(context.Background())
	var queued *Log
	batchLogLock.Lock()
	if len(batchLogStore) == 1 {
		queued = batchLogStore[0]
	}
	batchLogLock.Unlock()
	if queued == nil {
		t.Fatalf("BatchUpdateEnabled 下应仅入队一条消费日志")
	}
	assertIssue031AuditFields(t, *queued, "issue031-user")

	var count int64
	if err := db.Model(&Log{}).Where("content = ?", "issue031-audit").Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("flush 前不应同步写入日志，已有 %d 条", count)
	}
	flushBatchLogs()
	log := readIssue031Log(t, db, "issue031-audit")
	assertIssue031AuditFields(t, log, "issue031-user")
}

type issue031RedisGetCounter struct {
	getCount *atomic.Int32
}

func (h issue031RedisGetCounter) DialHook(next redis.DialHook) redis.DialHook {
	return next
}

func (h issue031RedisGetCounter) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if strings.EqualFold(cmd.Name(), "get") {
			h.getCount.Add(1)
		}
		return next(ctx, cmd)
	}
}

func (h issue031RedisGetCounter) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		for _, cmd := range cmds {
			if strings.EqualFold(cmd.Name(), "get") {
				h.getCount.Add(1)
			}
		}
		return next(ctx, cmds)
	}
}
