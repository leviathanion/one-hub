package model

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"one-api/common/config"

	"gorm.io/gorm"
)

func setupI003CredentialDatabase(t *testing.T, db *gorm.DB) {
	t.Helper()
	if err := db.AutoMigrate(&Channel{}); err != nil {
		t.Fatal(err)
	}
	oldDB := DB
	oldChooser := snapshotTestChannelGroup(t)
	DB = db
	restoreTestChannelGroup(t, testChannelGroupSnapshot{})
	t.Cleanup(func() { DB = oldDB; restoreTestChannelGroup(t, oldChooser) })
}

func seedI003UnresolvedFence(t *testing.T, channelID int) (CredentialRotationTicket, CredentialRecoverySnapshot) {
	t.Helper()
	insertCredentialRotationChannel(t, channelID, `{"access_token":"test-old-access","refresh_token":"test-old-refresh","account_id":"account-a"}`)
	ticket := CredentialRotationTicket{ChannelID: channelID, ExpectedRevision: 0, AttemptID: "Unresolved-Refresh"}
	if outcome, err := ClaimCredentialRotation(context.Background(), ticket, time.Now()); err != nil || outcome != CredentialRotationClaimAcquired {
		t.Fatalf("建立未决刷新 fence: outcome=%v err=%v", outcome, err)
	}
	return ticket, CredentialRecoverySnapshot{ChannelID: channelID, AccountID: "account-a", ExpectedRevision: 0, ExpectedFence: ticket.AttemptID}
}

func TestFixI003_RecoverySupersedesLateRefreshAcrossDatabases(t *testing.T) {
	forEachTestDatabase(t, func(t *testing.T, db *gorm.DB) {
		setupI003CredentialDatabase(t, db)
		lateTicket, recovery := seedI003UnresolvedFence(t, 33001)
		peer := lateTicket
		peer.AttemptID = "ordinary-refresh-cannot-recover"
		if outcome, err := ClaimCredentialRotation(context.Background(), peer, time.Now()); err != nil || outcome != CredentialRotationClaimBusy {
			t.Fatalf("普通刷新不得抢占未决 fence: outcome=%v err=%v", outcome, err)
		}
		newKey := `{"access_token":"test-authorized-access","refresh_token":"test-authorized-refresh","account_id":"account-a"}`
		if err := RecoverChannelCredentialWithContext(context.Background(), recovery, newKey); err != nil {
			t.Fatalf("已验证同账号的新授权必须恢复原渠道: %v", err)
		}
		if outcome, err := CommitCredentialRotation(context.Background(), lateTicket, "test-late-refresh"); err != nil || outcome != CredentialRotationCommitSuperseded {
			t.Fatalf("旧刷新迟到不得覆盖恢复结果: outcome=%v err=%v", outcome, err)
		}
		if canceled, err := CancelCredentialRotationBeforeDispatch(context.Background(), lateTicket); err != nil || canceled {
			t.Fatalf("旧 ticket 不再拥有恢复后的 fence: canceled=%v err=%v", canceled, err)
		}
		after, err := LoadCredentialRotationSnapshot(context.Background(), lateTicket.ChannelID)
		if err != nil || after.Key != newKey || after.Revision != 1 || after.Fence != nil || after.StartedAt != nil || after.Deleted {
			t.Fatalf("恢复字段必须原子生效: revision=%d fenced=%v started=%v deleted=%v err=%v", after.Revision, after.Fence != nil, after.StartedAt != nil, after.Deleted, err)
		}
		if err := RecoverChannelCredentialWithContext(context.Background(), recovery, newKey); !errors.Is(err, ErrChannelCredentialConflict) {
			t.Fatalf("已经消费的恢复快照不得重放: %v", err)
		}
		if ChannelGroup.publishGeneration.Load() == 0 {
			t.Fatal("成功恢复未失效已发布渠道")
		}
	})
}

func TestFixI003_RecoveryRejectsChangedSnapshotAcrossDatabases(t *testing.T) {
	forEachTestDatabase(t, func(t *testing.T, db *gorm.DB) {
		setupI003CredentialDatabase(t, db)
		for index, scenario := range []string{"revision", "fence", "fence-case", "type", "account", "deleted", "new-account", "empty-fence"} {
			t.Run(scenario, func(t *testing.T) {
				_, recovery := seedI003UnresolvedFence(t, 33100+index)
				newKey := `{"access_token":"test-authorized","account_id":"account-a"}`
				update := map[string]any{}
				switch scenario {
				case "revision":
					update["credential_revision"] = 1
				case "fence":
					update["credential_refresh_fence"] = "another-attempt"
				case "fence-case":
					update["credential_refresh_fence"] = "unresolved-refresh"
				case "type":
					update["type"] = config.ChannelTypeOpenAI
				case "account":
					update["key"] = `{"access_token":"test-other-account","account_id":"account-b"}`
				case "deleted":
					update["deleted_at"] = time.Now()
				case "new-account":
					newKey = `{"access_token":"test-other-account","account_id":"account-b"}`
				case "empty-fence":
					recovery.ExpectedFence = ""
				}
				if len(update) > 0 {
					if err := db.Model(&Channel{}).Where("id = ?", recovery.ChannelID).Updates(update).Error; err != nil {
						t.Fatal(err)
					}
				}
				before, err := LoadCredentialRotationSnapshot(context.Background(), recovery.ChannelID)
				if err != nil {
					t.Fatal(err)
				}
				if err := RecoverChannelCredentialWithContext(context.Background(), recovery, newKey); !errors.Is(err, ErrChannelCredentialConflict) {
					t.Fatalf("过期或错误身份快照必须拒绝: %v", err)
				}
				after, err := LoadCredentialRotationSnapshot(context.Background(), recovery.ChannelID)
				if err != nil || after.Key != before.Key || after.Revision != before.Revision || after.Fence == nil || *after.Fence != *before.Fence || after.StartedAt == nil || *after.StartedAt != *before.StartedAt || after.Deleted != before.Deleted || after.Type != before.Type {
					t.Fatalf("失败恢复修改了凭据或保护状态: channel=%d err=%v", recovery.ChannelID, err)
				}
			})
		}
	})
}

func TestFixI003_ConcurrentRecoveryHasOneWinnerAcrossDatabases(t *testing.T) {
	forEachTestDatabase(t, func(t *testing.T, db *gorm.DB) {
		setupI003CredentialDatabase(t, db)
		_, recovery := seedI003UnresolvedFence(t, 33200)
		start := make(chan struct{})
		type result struct {
			key string
			err error
		}
		results := make(chan result, 2)
		var wg sync.WaitGroup
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				key := fmt.Sprintf(`{"access_token":"test-winner-%d","account_id":"account-a"}`, i)
				<-start
				results <- result{key, RecoverChannelCredentialWithContext(context.Background(), recovery, key)}
			}(i)
		}
		close(start)
		wg.Wait()
		close(results)
		winner, successes := "", 0
		for result := range results {
			if result.err == nil {
				winner = result.key
				successes++
			}
		}
		after, err := LoadCredentialRotationSnapshot(context.Background(), recovery.ChannelID)
		if err != nil || successes != 1 || after.Key != winner || after.Revision != 1 || after.Fence != nil || after.StartedAt != nil {
			t.Fatalf("并发恢复必须恰有一方生效: successes=%d revision=%d fenced=%v err=%v", successes, after.Revision, after.Fence != nil, err)
		}
	})
}

func TestFixI003_RecoveryWriteFailureKeepsFence(t *testing.T) {
	useTestChannelDB(t)
	_, recovery := seedI003UnresolvedFence(t, 33300)
	if err := DB.Callback().Update().Before("gorm:update").Register("i003:write_failure", func(tx *gorm.DB) { tx.AddError(errors.New("injected recovery SQL failure")) }); err != nil {
		t.Fatal(err)
	}
	defer DB.Callback().Update().Remove("i003:write_failure")
	before, err := LoadCredentialRotationSnapshot(context.Background(), recovery.ChannelID)
	if err != nil {
		t.Fatal(err)
	}
	if err := RecoverChannelCredentialWithContext(context.Background(), recovery, `{"access_token":"test-authorized","account_id":"account-a"}`); err == nil {
		t.Fatal("SQL 写入失败仍报告恢复成功")
	}
	after, err := LoadCredentialRotationSnapshot(context.Background(), recovery.ChannelID)
	if err != nil || after.Key != before.Key || after.Revision != before.Revision || after.Fence == nil || *after.Fence != *before.Fence || after.StartedAt == nil || *after.StartedAt != *before.StartedAt {
		t.Fatalf("失败恢复不能提前清除 fence: channel=%d err=%v", recovery.ChannelID, err)
	}
}
