package midjourney

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"one-api/common/config"
	"one-api/common/requester"
	"one-api/internal/testutil/sqlitetest"
	"one-api/model"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// R06：真实 SQL owner 与 HTTP 查询组合，查询缺失不能释放预扣或制造终态。
func TestMissingOldMidjourneyTaskRemainsObservableUntilProviderTerminal(t *testing.T) {
	for _, status := range []string{"SUCCESS", "FAILURE"} {
		t.Run(status, func(t *testing.T) {
			body := `[]`
			db, channel, newOwner := midjourneyPollFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(body))
			}))
			owner := newOwner("old-task")
			poll := func() error {
				return updateTaskBatch(context.Background(), channel.Id, []string{"old-task"}, map[string]*model.Task{"old-task": owner})
			}
			if err := poll(); err != nil {
				t.Fatal(err)
			}
			var durable model.Task
			if err := db.First(&durable, owner.ID).Error; err != nil {
				t.Fatal(err)
			}
			if durable.ProviderState != model.TaskProviderStateAccepted || durable.Status != model.TaskStatusSubmitted || durable.Progress == 100 || durable.ChargedQuota != nil || durable.OwnerClosedAt != nil {
				t.Fatalf("missing result finalized accepted task: %+v", durable)
			}
			if delay := time.Until(time.Unix(durable.NextActionAt, 0)); delay <= 0 || delay > 30*time.Second {
				t.Fatalf("missing task was not rescheduled within 30 seconds: %s", delay)
			}
			assertMidjourneyPollBalances(t, db, 900)
			body = fmt.Sprintf(`[{"id":"old-task","status":%q,"progress":"100%%","imageUrl":"https://example.com/result.png","failReason":"provider reason"}]`, status)
			for range 2 {
				if err := poll(); err != nil {
					t.Fatal(err)
				}
				assertMidjourneyPollBalances(t, db, 1000)
			}
			if err := db.First(&durable, owner.ID).Error; err != nil {
				t.Fatal(err)
			}
			if durable.ProviderState != model.TaskProviderStateClosed || string(durable.Status) != status || durable.ChargedQuota == nil || *durable.ChargedQuota != 0 || durable.NextActionAt != 0 {
				t.Fatalf("provider terminal not persisted: %+v", durable)
			}
			if view := model.MidjourneyFromTask(&durable); view.ImageUrl != "https://example.com/result.png" {
				t.Fatalf("provider result missing: %+v", view)
			}
		})
	}
}

func TestMidjourneyPartialPollOnlyReschedulesMissingTask(t *testing.T) {
	db, channel, newOwner := midjourneyPollFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"id":"complete","status":"SUCCESS","progress":"100%"}]`))
	}))
	complete, missing := newOwner("complete"), newOwner("missing")
	if err := updateTaskBatch(context.Background(), channel.Id, []string{"complete", "missing"}, map[string]*model.Task{"complete": complete, "missing": missing}); err != nil {
		t.Fatal(err)
	}
	if complete.ProviderState != model.TaskProviderStateClosed || missing.ProviderState != model.TaskProviderStateAccepted || missing.NextActionAt <= time.Now().Unix() {
		t.Fatalf("partial result mixed task states: complete=%+v missing=%+v", complete, missing)
	}
	assertMidjourneyPollBalances(t, db, 900)
}

func TestMidjourneyQueryAndRescheduleErrorsPreservePollFence(t *testing.T) {
	for _, mode := range []string{"query-error", "reschedule-error"} {
		t.Run(mode, func(t *testing.T) {
			db, channel, newOwner := midjourneyPollFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if mode == "query-error" {
					http.Error(w, "unavailable", http.StatusServiceUnavailable)
					return
				}
				_, _ = w.Write([]byte(`[]`))
			}))
			owner := newOwner("retry-observation")
			if _, err := model.ClaimTaskPoll(context.Background(), owner, time.Now(), model.TaskPollFenceWindow); err != nil {
				t.Fatal(err)
			}
			fence := owner.NextActionAt
			if mode == "reschedule-error" {
				if err := db.Callback().Update().Before("gorm:update").Register("test:reschedule-error", func(tx *gorm.DB) {
					if tx.Statement.Table == "tasks" {
						tx.AddError(errors.New("reschedule unavailable"))
					}
				}); err != nil {
					t.Fatal(err)
				}
			}
			err := updateTaskBatch(context.Background(), channel.Id, []string{"retry-observation"}, map[string]*model.Task{"retry-observation": owner})
			if err == nil {
				t.Fatal("query/reschedule failure was swallowed")
			}
			var durable model.Task
			if err := db.First(&durable, owner.ID).Error; err != nil {
				t.Fatal(err)
			}
			if durable.ProviderState != model.TaskProviderStateAccepted || durable.NextActionAt != fence || durable.ChargedQuota != nil {
				t.Fatalf("failed observation lost durable reschedule: %+v", durable)
			}
			assertMidjourneyPollBalances(t, db, 900)
		})
	}
}

func midjourneyPollFixture(t *testing.T, handler http.Handler) (*gorm.DB, *model.Channel, func(string) *model.Task) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	db, err := gorm.Open(sqlite.Open(sqlitetest.MemoryDSN()), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := db.AutoMigrate(&model.Channel{}, &model.Task{}, &model.User{}, &model.Token{}, &model.UserGroup{}); err != nil {
		t.Fatal(err)
	}
	originalDB, originalClient := model.DB, requester.HTTPClient
	model.DB, requester.HTTPClient = db, server.Client()
	t.Cleanup(func() { model.DB, requester.HTTPClient = originalDB, originalClient })
	channel := &model.Channel{Id: 1, Type: config.ChannelTypeMidjourney, Name: "mj", Key: "owner-secret", Status: config.ChannelStatusEnabled, BaseURL: &server.URL, Other: `{}`}
	for _, row := range []any{
		channel,
		&model.User{Id: 1, Username: "poll-user", Password: "password123", AccessToken: "access", Quota: 1000, Status: config.UserStatusEnabled},
		&model.Token{Id: 1, UserId: 1, Key: "poll-token", Status: config.TokenStatusEnabled, ExpiredTime: -1, RemainQuota: 1000},
	} {
		if err := db.Session(&gorm.Session{SkipHooks: true}).Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	return db, channel, func(id string) *model.Task {
		t.Helper()
		owner := &model.Task{Platform: model.TaskPlatformMidjourney, UserId: 1, TokenID: 1, ChannelId: channel.Id, Status: model.TaskStatusSubmitted, ReservedQuota: 100, ProviderNamespace: "task-platform:midjourney", ProviderTaskScopeIncarnation: "provider-wide", RequestFingerprint: id}
		if _, err := model.CreateTaskBillingOwner(context.Background(), owner); err != nil {
			t.Fatal(err)
		}
		if _, err := model.ClaimTaskSubmission(context.Background(), owner, "claim-"+id); err != nil {
			t.Fatal(err)
		}
		if _, err := model.AcceptTaskSubmission(context.Background(), owner, id); err != nil {
			t.Fatal(err)
		}
		owner.CreatedAt = time.Now().Add(-2 * time.Hour).Unix()
		owner.NextActionAt = time.Now().Add(-time.Second).Unix()
		if err := db.Model(owner).Updates(map[string]any{"created_at": owner.CreatedAt, "next_action_at": owner.NextActionAt}).Error; err != nil {
			t.Fatal(err)
		}
		return owner
	}
}

func assertMidjourneyPollBalances(t *testing.T, db *gorm.DB, want int) {
	t.Helper()
	var user model.User
	var token model.Token
	if err := db.First(&user, 1).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.First(&token, 1).Error; err != nil {
		t.Fatal(err)
	}
	if user.Quota != want || token.RemainQuota != want {
		t.Fatalf("balances changed unexpectedly: user=%d token=%d want=%d", user.Quota, token.RemainQuota, want)
	}
}

func TestMidjourneyProgressUsesDisabledOrSoftDeletedOwnerIncarnation(t *testing.T) {
	for _, test := range []struct {
		name       string
		disable    bool
		softDelete bool
	}{
		{name: "disabled", disable: true},
		{name: "soft-deleted", softDelete: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("mj-api-secret") != "owner-secret" {
					t.Errorf("owner credential missing from poll")
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`[{"id":"mj-owner-1","status":"IN_PROGRESS","progress":"42%","promptEn":"ready"}]`))
			}))
			defer server.Close()

			db, err := gorm.Open(sqlite.Open(sqlitetest.MemoryDSN()), &gorm.Config{})
			if err != nil {
				t.Fatal(err)
			}
			if err := db.AutoMigrate(&model.Channel{}, &model.Task{}); err != nil {
				t.Fatal(err)
			}
			originalDB, originalClient := model.DB, requester.HTTPClient
			model.DB, requester.HTTPClient = db, server.Client()
			t.Cleanup(func() { model.DB, requester.HTTPClient = originalDB, originalClient })

			proxy := ""
			channel := &model.Channel{Id: 1, Type: config.ChannelTypeMidjourney, Name: "mj", Key: "owner-secret", Status: config.ChannelStatusEnabled, BaseURL: &server.URL, Proxy: &proxy, Other: `{}`}
			if err := db.Create(channel).Error; err != nil {
				t.Fatal(err)
			}
			if test.disable {
				if err := db.Model(&model.Channel{}).Where("id = ?", channel.Id).Update("status", config.ChannelStatusManuallyDisabled).Error; err != nil {
					t.Fatal(err)
				}
			}
			if test.softDelete {
				if err := db.Delete(&model.Channel{}, channel.Id).Error; err != nil {
					t.Fatal(err)
				}
			}

			owner := &model.Task{
				Platform: model.TaskPlatformMidjourney, UserId: 1, TokenID: 1, ChannelId: channel.Id,
				ProviderState: model.TaskProviderStateAccepted, ProviderNamespace: "task-platform:midjourney",
				ProviderTaskScopeIncarnation: "provider-wide", RequestFingerprint: "owner-progress",
				Status: model.TaskStatusSubmitted, Version: 2, NextActionAt: time.Now().Add(time.Minute).Unix(),
				CreatedAt: time.Now().Unix(), UpdatedAt: time.Now().Unix(),
			}
			model.SetTaskProviderID(owner, "mj-owner-1")
			if err := db.Create(owner).Error; err != nil {
				t.Fatal(err)
			}
			if err := updateTaskBatch(context.Background(), channel.Id, []string{"mj-owner-1"}, map[string]*model.Task{"mj-owner-1": owner}); err != nil {
				t.Fatalf("poll durable owner: %v", err)
			}
			var durable model.Task
			if err := db.First(&durable, owner.ID).Error; err != nil {
				t.Fatal(err)
			}
			if durable.Status != model.TaskStatusInProgress || durable.Progress != 42 || durable.ProviderState != model.TaskProviderStateAccepted {
				t.Fatalf("unexpected durable poll snapshot: %+v", durable)
			}
		})
	}
}
