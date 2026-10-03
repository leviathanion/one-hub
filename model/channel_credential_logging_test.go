package model

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"one-api/common/config"
	"one-api/common/credentials"
	"strings"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// Credential operations must never log token values, including failed SQL.
type credentialSQLLogCapture struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *credentialSQLLogCapture) Printf(format string, args ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	fmt.Fprintf(&c.buf, format, args...)
}

func (c *credentialSQLLogCapture) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

func credentialLogFixture(entry, phase string) (oldKey, newKey, oldAccess, oldRefresh, newAccess, newRefresh string) {
	oldAccess = fmt.Sprintf("credential-log-%s-old-access", entry)
	oldRefresh = fmt.Sprintf("credential-log-%s-old-refresh", entry)
	newAccess = fmt.Sprintf("credential-log-%s-%s-new-access", entry, phase)
	newRefresh = fmt.Sprintf("credential-log-%s-%s-new-refresh", entry, phase)
	oldKey = fmt.Sprintf(`{"access_token":%q,"refresh_token":%q,"account_id":"account-a"}`, oldAccess, oldRefresh)
	newKey = fmt.Sprintf(`{"access_token":%q,"refresh_token":%q,"account_id":"account-a"}`, newAccess, newRefresh)
	return
}

// failureHook 注入错误的时机：none 不注入；after 在 gorm:update 回调完成、SQL
// 已构建并执行之后把错误加入事务。注意这是 callback 层注入，不是驱动拒绝执行：
// 普通 Commit 的语句可能已提交并由 Commit 按重载证据归类为 AlreadyApplied，
// Recover 保守返回写错误，不报告恢复成功。它使 Error Trace 分支携带完整已构建 SQL，用于
// 验证该分支的日志安全。
type failureHook string

const (
	hookNone  failureHook = "none"
	hookAfter failureHook = "after"
)

// runCredentialSQLLogWindow 先建立独立渠道 fixture，再在指定 gorm 日志窗口内执行
// 一次凭据写入口。wantErr=false 且 entry 为 replace_commit、hook 为 after 时，
// 提交语句已执行、Commit 按重载证据归类为 AlreadyApplied 并成功返回（该窗口
// 仍验证了 Error Trace 分支对已构建凭据 SQL 的抑制）。
func runCredentialSQLLogWindow(t *testing.T, db *gorm.DB, level gormlogger.LogLevel, slow time.Duration, entry, phase string, channelID int, hook failureHook, wantErr bool) {
	t.Helper()
	oldKey, newKey, oldAccess, oldRefresh, newAccess, newRefresh := credentialLogFixture(entry, phase)
	// 渠道 fixture 必须先于捕获窗口完成：fixture INSERT 本身带旧凭据，属于
	// 测试准备数据，不属于被验证的写入口。
	insertCredentialRotationChannel(t, channelID, oldKey)
	var ticket credentials.Ticket
	if entry != "compare_and_set" {
		ticket = credentials.Ticket{Type: config.ChannelTypeCodex, ChannelID: channelID, ExpectedVersion: 0, AttemptID: fmt.Sprintf("credential-log-fence-%d", channelID)}
		if outcome, err := testRotation().Claim(context.Background(), ticket, time.Now()); err != nil || outcome != credentials.ClaimAcquired {
			t.Fatalf("建立未决 fence: outcome=%v err=%v", outcome, err)
		}
	}

	capture := &credentialSQLLogCapture{}
	captureLogger := gormlogger.New(capture, gormlogger.Config{LogLevel: level, SlowThreshold: slow, Colorful: false})
	originalLogger := db.Logger
	db.Logger = captureLogger
	defer func() { db.Logger = originalLogger }()

	callbackName := fmt.Sprintf("credential_log_failure_%s_%s_%d", entry, phase, channelID)
	registerFailure := func() {
		if err := db.Callback().Update().After("gorm:update").Register(callbackName, func(tx *gorm.DB) {
			tx.AddError(errors.New("injected credential SQL failure"))
		}); err != nil {
			t.Fatal(err)
		}
	}
	removeFailure := func() {
		if err := db.Callback().Update().Remove(callbackName); err != nil {
			t.Fatal(err)
		}
	}

	// 正向对照：非 Silent 语句在同一窗口必然打印带参数的 SQL（错误窗口用
	// 执行后注入制造真实 Error Trace），证明捕获面有效。
	controlMarker := fmt.Sprintf("credential-log-control-%s-%s-%d", entry, phase, channelID)
	if level == gormlogger.Error {
		registerFailure()
	}
	control := db.Model(&Channel{}).Where("id = ?", channelID).Update("name", controlMarker)
	if level == gormlogger.Error {
		removeFailure()
		if control.Error == nil {
			t.Fatal("Error 级正向对照未按预期失败")
		}
	} else if control.Error != nil {
		t.Fatalf("正向对照语句失败: %v", control.Error)
	}

	writeOnce := func() error {
		switch entry {
		case "recover":
			row, err := testRotation().Store.Load(context.Background(), channelID)
			if err != nil {
				return err
			}
			return testRotation().Recover(context.Background(), row, ticket.AttemptID, newKey)
		case "replace_commit":
			outcome, err := testRotation().Commit(context.Background(), ticket, newKey)
			if err == nil && outcome != credentials.CommitApplied && outcome != credentials.CommitAlreadyApplied {
				return credentials.ErrConflict
			}
			return err
		case "compare_and_set":
			row, err := testRotation().Store.Load(context.Background(), channelID)
			if err != nil {
				return err
			}
			updated, err := testRotation().Store.CompareAndSwap(context.Background(), row, row.BizData, &newKey)
			if err != nil {
				return err
			}
			if !updated {
				return credentials.ErrConflict
			}
			return nil
		default:
			t.Fatalf("未知受测入口 %q", entry)
			return nil
		}
	}
	if hook == hookAfter {
		registerFailure()
	}
	runErr := writeOnce()
	if hook == hookAfter {
		removeFailure()
	}
	if wantErr && runErr == nil {
		t.Fatal("注入 SQL 失败后受测入口仍成功")
	}
	if !wantErr && runErr != nil {
		t.Fatalf("受测写入入口失败: %v", runErr)
	}

	logged := capture.String()
	if !bytes.Contains([]byte(logged), []byte(controlMarker)) {
		t.Fatalf("日志捕获未生效（正向对照缺失）：level=%v slow=%v logged=%q", level, slow, logged)
	}
	secrets := []string{oldAccess, oldRefresh, newAccess, newRefresh, `"access_token":`, `"refresh_token":`}
	var leaked []string
	for _, secret := range secrets {
		if strings.Contains(logged, secret) {
			leaked = append(leaked, secret)
		}
	}
	if len(leaked) > 0 {
		t.Errorf("凭据 token/JSON 泄漏到 SQL 日志：level=%v slow=%v leaked=%v", level, slow, leaked)
	}
}

func TestChannelCredentialSQLNeverLogsTokensAcrossDatabases(t *testing.T) {
	forEachTestDatabase(t, func(t *testing.T, db *gorm.DB) {
		setupCredentialDatabase(t, db)
		type window struct {
			entry, phase string
			channelID    int
			level        gormlogger.LogLevel
			slow         time.Duration
			hook         failureHook
			wantErr      bool
		}
		windows := []window{
			// Debug/Info 等价窗口：成功写入不得输出任何凭据 SQL。
			{"recover", "debug", 33900, gormlogger.Info, 200 * time.Millisecond, hookNone, false},
			{"replace_commit", "debug", 33910, gormlogger.Info, 200 * time.Millisecond, hookNone, false},
			{"compare_and_set", "debug", 33920, gormlogger.Info, 200 * time.Millisecond, hookNone, false},
			// Error Trace 窗口：gorm:update 回调完成后注入错误（非驱动拒绝执行），
			// Error Trace 分支携带已构建 SQL，凭据仍不得出现；Recover 由外层事务
			// 回滚而报错，CompareAndSet 上报错。
			{"recover", "error", 33901, gormlogger.Error, 200 * time.Millisecond, hookAfter, true},
			{"compare_and_set", "error", 33921, gormlogger.Error, 200 * time.Millisecond, hookAfter, true},
			// 同上窗口：Commit 的语句在注入点已执行/提交，Commit 按重载证据归类为
			// AlreadyApplied 并返回成功；该窗口验证已构建凭据 SQL 在 Error Trace
			// 分支仍不出现。
			{"replace_commit", "error_applied", 33912, gormlogger.Error, 200 * time.Millisecond, hookAfter, false},
			// 慢 Trace 窗口：极小阈值使成功语句进入慢日志分支，仍不得输出凭据 SQL。
			{"recover", "slow", 33902, gormlogger.Warn, time.Nanosecond, hookNone, false},
			{"replace_commit", "slow", 33913, gormlogger.Warn, time.Nanosecond, hookNone, false},
			{"compare_and_set", "slow", 33922, gormlogger.Warn, time.Nanosecond, hookNone, false},
		}
		for _, w := range windows {
			t.Run(w.entry+"_"+w.phase, func(t *testing.T) {
				runCredentialSQLLogWindow(t, db, w.level, w.slow, w.entry, w.phase, w.channelID, w.hook, w.wantErr)
			})
		}
	})
}
