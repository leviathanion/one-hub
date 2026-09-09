package model

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"one-api/common/config"
	"one-api/internal/testutil/sqlitetest"

	"github.com/google/uuid"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func useTaskBillingRepositoryTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(sqlitetest.MemoryDSN()), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&User{}, &Token{}, &Channel{}, &Task{}, &UserGroup{}); err != nil {
		t.Fatal(err)
	}
	for id := 1; id <= 4; id++ {
		if err := db.Create(&User{Id: id, Username: fmt.Sprintf("user-%d", id), Password: "password123", AccessToken: fmt.Sprintf("access-%d", id), AffCode: fmt.Sprintf("aff-%d", id), Quota: 1000, Status: config.UserStatusEnabled}).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Session(&gorm.Session{SkipHooks: true}).Create(&Token{Id: id, UserId: id, Key: fmt.Sprintf("token-%d", id), Status: config.TokenStatusEnabled, ExpiredTime: -1, RemainQuota: 1000}).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Create(&Channel{Id: 1, Type: config.ChannelTypeKling, Name: "kling", Key: "key", Status: config.ChannelStatusEnabled}).Error; err != nil {
		t.Fatal(err)
	}
	originalDB := DB
	DB = db
	t.Cleanup(func() { DB = originalDB })
	return db
}

func createDurableTaskOwner(t *testing.T, userID int, scope, fingerprint string) *Task {
	t.Helper()
	task := &Task{
		Platform:                     TaskPlatformKling,
		UserId:                       userID,
		TokenID:                      userID,
		ChannelId:                    1,
		Status:                       TaskStatusSubmitted,
		ReservedQuota:                10,
		ProviderNamespace:            "task-platform:kling",
		ProviderTaskScopeIncarnation: scope,
		RequestFingerprint:           fingerprint,
	}
	result, err := CreateTaskBillingOwner(context.Background(), task)
	if err != nil || result.Outcome != BillingBalanceCommitted {
		t.Fatalf("create task owner: result=%+v err=%v", result, err)
	}
	return task
}

func claimDurableTaskOwner(t *testing.T, task *Task) {
	t.Helper()
	result, err := ClaimTaskSubmission(context.Background(), task, uuid.NewString())
	if err != nil || result.Outcome != TaskMutationApplied {
		t.Fatalf("claim task owner: result=%+v err=%v", result, err)
	}
}

func acceptDurableTaskOwner(t *testing.T, task *Task, providerTaskID string) {
	t.Helper()
	result, err := AcceptTaskSubmission(context.Background(), task, providerTaskID)
	if err != nil || result.Outcome != TaskMutationApplied {
		t.Fatalf("accept task owner: result=%+v err=%v", result, err)
	}
}

func TestTaskConfirmUsesCurrentUnlimitedQuotaPolicy(t *testing.T) {
	for _, test := range []struct {
		name             string
		unlimitedAtTry   bool
		unlimitedAtClose bool
		wantRemain       int
		wantUsed         int
	}{
		{name: "limited to unlimited", unlimitedAtTry: false, unlimitedAtClose: true, wantRemain: 1000, wantUsed: 0},
		{name: "unlimited to limited", unlimitedAtTry: true, unlimitedAtClose: false, wantRemain: 970, wantUsed: 30},
	} {
		t.Run(test.name, func(t *testing.T) {
			useTaskBillingRepositoryTestDB(t)
			if err := DB.Model(&Token{}).Where("id = ?", 1).Update("unlimited_quota", test.unlimitedAtTry).Error; err != nil {
				t.Fatal(err)
			}
			task := createDurableTaskOwner(t, 1, "provider-wide", "policy-transition")
			claimDurableTaskOwner(t, task)
			acceptDurableTaskOwner(t, task, "task-policy-transition")
			if err := DB.Model(&Token{}).Where("id = ?", 1).Update("unlimited_quota", test.unlimitedAtClose).Error; err != nil {
				t.Fatal(err)
			}
			task.Status = TaskStatusSuccess
			task.Progress = 100
			if _, err := FinalizeTaskBillingOwner(context.Background(), task, 30, "confirm"); err != nil {
				t.Fatal(err)
			}
			// 重复终态不能再次累计用户已消费额度。
			if _, err := FinalizeTaskBillingOwner(context.Background(), task, 30, "confirm"); err != nil {
				t.Fatal(err)
			}
			var user User
			var token Token
			if err := DB.First(&user, 1).Error; err != nil {
				t.Fatal(err)
			}
			if err := DB.First(&token, 1).Error; err != nil {
				t.Fatal(err)
			}
			if user.Quota != 970 || user.UsedQuota != 30 || token.RemainQuota != test.wantRemain || token.UsedQuota != test.wantUsed {
				t.Fatalf("balances user=(%d,%d) token=(%d,%d)", user.Quota, user.UsedQuota, token.RemainQuota, token.UsedQuota)
			}
		})
	}
}

func TestBoundTaskChildUsesDisabledOrSoftDeletedChannelIncarnation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		retire func(*testing.T, *gorm.DB)
	}{
		{
			name: "disabled",
			retire: func(t *testing.T, db *gorm.DB) {
				if err := db.Model(&Channel{}).Where("id = ?", 1).Update("status", config.ChannelStatusManuallyDisabled).Error; err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "soft-deleted",
			retire: func(t *testing.T, db *gorm.DB) {
				if err := db.Delete(&Channel{}, 1).Error; err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := useTaskBillingRepositoryTestDB(t)
			tc.retire(t, db)

			newTask := func(fingerprint string) *Task {
				return &Task{
					Platform:                     TaskPlatformKling,
					UserId:                       1,
					TokenID:                      1,
					ChannelId:                    1,
					Status:                       TaskStatusSubmitted,
					ReservedQuota:                10,
					ProviderNamespace:            "task-platform:kling",
					ProviderTaskScopeIncarnation: "provider-wide",
					RequestFingerprint:           fingerprint,
				}
			}

			ordinary := newTask("ordinary-" + tc.name)
			if result, err := CreateTaskBillingOwner(context.Background(), ordinary); err == nil || result.Outcome != BillingBalanceDefinitelyRolledBack {
				t.Fatalf("ordinary new task used retired channel: result=%+v err=%v", result, err)
			}

			bound := newTask("bound-" + tc.name)
			result, err := CreateTaskBillingOwnerForBoundChannel(context.Background(), bound)
			if err != nil || result.Outcome != BillingBalanceCommitted || bound.ID == 0 {
				t.Fatalf("owner-bound child rejected retired incarnation: task=%+v result=%+v err=%v", bound, result, err)
			}
		})
	}
}

func TestTaskSubmissionClaimIsStableAndCannotBeRegranted(t *testing.T) {
	useTaskBillingRepositoryTestDB(t)
	task := createDurableTaskOwner(t, 1, "provider-wide", "claim-stability")
	stale := *task
	claimID := uuid.NewString()
	result, err := ClaimTaskSubmission(context.Background(), task, claimID)
	if err != nil || result.Outcome != TaskMutationApplied || task.SubmissionClaimID != claimID {
		t.Fatalf("first claim: task=%+v result=%+v err=%v", task, result, err)
	}
	result, err = ClaimTaskSubmission(context.Background(), &stale, uuid.NewString())
	if result.Outcome != TaskMutationDefinitelyNotApplied || !errors.Is(err, ErrTaskBillingState) {
		t.Fatalf("second caller must not receive submission authority: result=%+v err=%v", result, err)
	}
	result, err = ClaimTaskSubmission(context.Background(), task, claimID)
	if err != nil || result.Outcome != TaskMutationApplied {
		t.Fatalf("same claim must be idempotently recognizable: result=%+v err=%v", result, err)
	}
}

func TestTaskOwnerSchemaRequiresIdentityAndProgressIndexes(t *testing.T) {
	db := useTaskBillingRepositoryTestDB(t)
	if err := CheckTaskOwnerSchema(context.Background()); err != nil {
		t.Fatalf("complete task owner schema should pass: %v", err)
	}
	if err := db.Migrator().DropIndex(&Task{}, "idx_tasks_progress"); err != nil {
		t.Fatalf("drop progress index: %v", err)
	}
	if err := CheckTaskOwnerSchema(context.Background()); err == nil {
		t.Fatal("missing progress index must fail readiness")
	}
}

func TestTaskProviderAndPublicIdentityConstraints(t *testing.T) {
	useTaskBillingRepositoryTestDB(t)
	first := createDurableTaskOwner(t, 1, "account-a", "identity-1")
	claimDurableTaskOwner(t, first)
	acceptDurableTaskOwner(t, first, "task-shared")

	physicalConflict := createDurableTaskOwner(t, 2, "account-a", "identity-2")
	claimDurableTaskOwner(t, physicalConflict)
	result, err := AcceptTaskSubmission(context.Background(), physicalConflict, "task-shared")
	if result.Outcome != TaskMutationDefinitelyNotApplied || !errors.Is(err, ErrTaskIdentity) {
		t.Fatalf("same provider identity must conflict across users: result=%+v err=%v", result, err)
	}

	publicConflict := createDurableTaskOwner(t, 1, "account-b", "identity-3")
	claimDurableTaskOwner(t, publicConflict)
	result, err = AcceptTaskSubmission(context.Background(), publicConflict, "task-shared")
	if result.Outcome != TaskMutationDefinitelyNotApplied || !errors.Is(err, ErrTaskIdentity) {
		t.Fatalf("same public identity must conflict across scopes: result=%+v err=%v", result, err)
	}

	independent := createDurableTaskOwner(t, 2, "account-b", "identity-4")
	claimDurableTaskOwner(t, independent)
	acceptDurableTaskOwner(t, independent, "task-shared")
}

func TestTaskProviderDedupReusesOnlySamePrincipalScopeAndFingerprint(t *testing.T) {
	useTaskBillingRepositoryTestDB(t)
	existing := createDurableTaskOwner(t, 1, "account-a", "same-request")
	claimDurableTaskOwner(t, existing)
	acceptDurableTaskOwner(t, existing, "task-deduplicated")

	provisional := createDurableTaskOwner(t, 1, "account-a", "same-request")
	provisionalOwnerID := provisional.OwnerID
	claimDurableTaskOwner(t, provisional)
	result, err := AcceptTaskSubmission(context.Background(), provisional, "task-deduplicated")
	if err != nil || result.Outcome != TaskMutationApplied || provisional.OwnerID != existing.OwnerID {
		t.Fatalf("same request dedup should reuse existing owner: task=%+v result=%+v err=%v", provisional, result, err)
	}
	closed, err := loadTaskOwnerForRecovery(context.Background(), provisionalOwnerID)
	if err != nil || closed.ProviderState != TaskProviderStateClosed || closed.Status != TaskStatusLocalFailure || closed.ChargedQuota == nil || *closed.ChargedQuota != 0 {
		t.Fatalf("provisional dedup owner must close and release reservation: task=%+v err=%v", closed, err)
	}

	differentFingerprint := createDurableTaskOwner(t, 1, "account-a", "different-request")
	claimDurableTaskOwner(t, differentFingerprint)
	result, err = AcceptTaskSubmission(context.Background(), differentFingerprint, "task-deduplicated")
	if result.Outcome != TaskMutationDefinitelyNotApplied || !errors.Is(err, ErrTaskIdentity) || differentFingerprint.ProviderState != TaskProviderStateClosed {
		t.Fatalf("different request must close as conflict: task=%+v result=%+v err=%v", differentFingerprint, result, err)
	}
}

func TestTaskPollVersionFencePreventsClosedOwnerRevival(t *testing.T) {
	useTaskBillingRepositoryTestDB(t)
	task := createDurableTaskOwner(t, 1, "provider-wide", "poll-fence")
	claimDurableTaskOwner(t, task)
	acceptDurableTaskOwner(t, task, "task-fence")
	stale := *task
	now := time.Now().Add(time.Second)
	claim, err := ClaimTaskPoll(context.Background(), task, now, TaskPollFenceWindow)
	if err != nil || claim.Outcome != TaskMutationApplied {
		t.Fatalf("claim poll: result=%+v err=%v", claim, err)
	}
	second, secondErr := ClaimTaskPoll(context.Background(), &stale, now, TaskPollFenceWindow)
	if second.Outcome != TaskMutationDefinitelyNotApplied || !errors.Is(secondErr, ErrTaskBillingState) {
		t.Fatalf("concurrent poll must lose version CAS: result=%+v err=%v", second, secondErr)
	}

	task.Status = TaskStatusInProgress
	if saved, saveErr := SaveTaskPollSnapshot(context.Background(), task, now.Add(TaskPollInterval)); saveErr != nil || saved.Outcome != TaskMutationApplied {
		t.Fatalf("save current poll snapshot: result=%+v err=%v", saved, saveErr)
	}
	task.Status = TaskStatusSuccess
	task.Progress = 100
	task.SubmitTime = 101
	task.StartTime = 202
	task.FinishTime = 303
	if _, err := FinalizeTaskBillingOwner(context.Background(), task, 0, "cancel"); err != nil {
		t.Fatalf("finalize current poll: %v", err)
	}

	stale.Status = TaskStatusInProgress
	if saved, saveErr := SaveTaskPollSnapshot(context.Background(), &stale, now.Add(2*TaskPollInterval)); saved.Outcome != TaskMutationDefinitelyNotApplied || !errors.Is(saveErr, ErrTaskBillingState) {
		t.Fatalf("late non-terminal result must not revive closed owner: result=%+v err=%v", saved, saveErr)
	}
	durable, err := loadTaskOwnerForRecovery(context.Background(), task.OwnerID)
	if err != nil || durable.ProviderState != TaskProviderStateClosed || durable.Status != TaskStatusSuccess || TaskProviderID(durable) != "task-fence" || durable.SubmitTime != 101 || durable.StartTime != 202 || durable.FinishTime != 303 {
		t.Fatalf("unexpected durable terminal owner: task=%+v err=%v", durable, err)
	}
}

func TestTaskProgressKeysetScansBeyondLegacyOldestLimit(t *testing.T) {
	useTaskBillingRepositoryTestDB(t)
	now := time.Now()
	for i := 0; i < 450; i++ {
		task := &Task{
			Platform: TaskPlatformKling, UserId: 1, TokenID: 1, ChannelId: 1,
			ProviderState: TaskProviderStateAccepted, ProviderNamespace: "task-platform:kling",
			ProviderTaskScopeIncarnation: "provider-wide", RequestFingerprint: fmt.Sprintf("keyset-%d", i),
			Status: TaskStatusSubmitted, NextActionAt: now.Add(-time.Minute).Unix(), CreatedAt: now.Unix(), UpdatedAt: now.Unix(),
		}
		SetTaskProviderID(task, fmt.Sprintf("task-keyset-%03d", i))
		if err := DB.Create(task).Error; err != nil {
			t.Fatalf("create keyset fixture %d: %v", i, err)
		}
	}
	var afterNext, afterID int64
	seen := 0
	for {
		page, err := ListDueTaskOwners(context.Background(), now, afterNext, afterID, TaskProgressPageSize)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			break
		}
		seen += len(page)
		last := page[len(page)-1]
		afterNext, afterID = last.NextActionAt, last.ID
	}
	if seen != 450 {
		t.Fatalf("keyset scan saw %d owners, want 450", seen)
	}
}

func TestTaskAcceptanceRecoveryRequiresRecordedAcceptance(t *testing.T) {
	useTaskBillingRepositoryTestDB(t)
	accepted := createDurableTaskOwner(t, 1, "provider-wide", "accept-closed")
	claimDurableTaskOwner(t, accepted)
	submissionSnapshot := *accepted
	acceptDurableTaskOwner(t, accepted, "task-accepted")
	accepted.Status = TaskStatusSuccess
	accepted.Progress = 100
	if _, err := FinalizeTaskBillingOwner(context.Background(), accepted, 0, "cancel"); err != nil {
		t.Fatal(err)
	}
	result, err := AcceptTaskSubmission(context.Background(), &submissionSnapshot, "task-accepted")
	if err != nil || result.Outcome != TaskMutationApplied || submissionSnapshot.ProviderState != TaskProviderStateClosed {
		t.Fatalf("accepted-then-closed must remain recognizable: task=%+v result=%+v err=%v", submissionSnapshot, result, err)
	}

	ambiguous := createDurableTaskOwner(t, 2, "provider-wide", "handle-only")
	claimDurableTaskOwner(t, ambiguous)
	SetTaskProviderID(ambiguous, "task-handle-only")
	if result, err := PreserveTaskSubmissionHandle(context.Background(), ambiguous, "task-handle-only"); err != nil || result.Outcome != TaskMutationApplied {
		t.Fatalf("preserve diagnostic handle: result=%+v err=%v", result, err)
	}
	ambiguousSnapshot := *ambiguous
	ambiguous.Status = TaskStatusUnknown
	ambiguous.Progress = 100
	if _, err := FinalizeTaskBillingOwner(context.Background(), ambiguous, 0, "cancel"); err != nil {
		t.Fatal(err)
	}
	result, err = AcceptTaskSubmission(context.Background(), &ambiguousSnapshot, "task-handle-only")
	if result.Outcome == TaskMutationApplied || err == nil {
		t.Fatalf("handle-only UNKNOWN closure must not prove acceptance: result=%+v err=%v", result, err)
	}
}
