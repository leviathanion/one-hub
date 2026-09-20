package relay

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"gorm.io/gorm"
	"one-api/common/config"
	"one-api/model"
	"one-api/types"
)

func taskProjectionFixture(t *testing.T, family string) (*model.Task, func(*model.Task) error, int) {
	t.Helper()
	var task *model.Task
	initial := 100000
	raw := []byte(`{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10,"prompt_tokens_details":{"cached_tokens":2}}`)
	usage := &types.Usage{}
	if err := json.Unmarshal(raw, usage); err != nil {
		t.Fatal(err)
	}
	usage.MarkProviderReported()
	var finalize func(*model.Task) error
	if family == model.TaskPlatformOpenAIResponsesBackground {
		r, _ := backgroundFixture(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"resp_projection","status":"queued"}`)
		}, `{"model":"gpt-5","background":true,"store":false,"input":"hello"}`)
		setBatchTestPrice(t, 1, 1)
		if apiErr, _ := RelayHandler(r); apiErr != nil {
			t.Fatal(apiErr)
		}
		waitBackgroundObserver(t, r.c)
		owner, err := model.GetResponseOwner(context.Background(), "resp_projection", 1)
		if err != nil {
			t.Fatal(err)
		}
		task, err = model.GetBackgroundResponseTask(context.Background(), *owner.TaskOwnerID)
		if err != nil {
			t.Fatal(err)
		}
		data := backgroundTaskData(task)
		data.Evidence = captureBackgroundUsage("resp_projection", usage)
		finalize = func(candidate *model.Task) error {
			candidate.Status = model.TaskStatusSuccess
			ownUsage := &types.Usage{}
			_ = json.Unmarshal(raw, ownUsage)
			ownUsage.MarkProviderReported()
			ownData := data
			ownData.Evidence = captureBackgroundUsage("resp_projection", ownUsage)
			return finalizeBackgroundTask(context.Background(), candidate, ownData)
		}
	} else {
		initial = 1000000
		_, makeContext := batchFixture(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if r.URL.Path == "/v1/files/file-input/content" {
				_, _ = io.WriteString(w, "{\"custom_id\":\"a\",\"method\":\"POST\",\"url\":\"/v1/chat/completions\",\"body\":{\"model\":\"gpt-5\",\"store\":false,\"messages\":[]}}\n")
				return
			}
			_, _ = io.WriteString(w, `{"id":"batch-projection","status":"validating"}`)
		})
		c, rec := makeContext(http.MethodPost, "/v1/batches", strings.NewReader(`{"input_file_id":"file-input","endpoint":"/v1/chat/completions"}`))
		BatchRelay(c)
		if rec.Code != 200 {
			t.Fatalf("batch create status=%d body=%s", rec.Code, rec.Body.String())
		}
		task = batchTestTask(t)
		var data openAIBatchData
		if err := json.Unmarshal(task.Data, &data); err != nil {
			t.Fatal(err)
		}
		finalize = func(candidate *model.Task) error {
			candidate.Status = model.TaskStatusSuccess
			ownUsage := &types.Usage{}
			_ = json.Unmarshal(raw, ownUsage)
			ownUsage.MarkProviderReported()
			observed := map[string]*batchObservedUsage{"a": {usage: ownUsage, digest: sha256.Sum256(raw)}}
			return finalizeOpenAIBatch(context.Background(), candidate, data, observed)
		}
	}
	previousLog := config.LogConsumeEnabled
	previousBatch := config.BatchUpdateEnabled
	config.LogConsumeEnabled = true
	config.BatchUpdateEnabled = false
	t.Cleanup(func() { config.LogConsumeEnabled = previousLog; config.BatchUpdateEnabled = previousBatch })
	return task, finalize, initial
}

func assertTaskProjection(t *testing.T, task *model.Task, initial int) {
	t.Helper()
	var user model.User
	var channel model.Channel
	var logs []model.Log
	if err := model.DB.First(&user, task.UserId).Error; err != nil {
		t.Fatal(err)
	}
	if err := model.DB.First(&channel, task.ChannelId).Error; err != nil {
		t.Fatal(err)
	}
	if err := model.DB.Where("type = ? AND content = ?", model.LogTypeConsume, "async task: "+task.Platform).Find(&logs).Error; err != nil {
		t.Fatal(err)
	}
	if task.ChargedQuota == nil || *task.ChargedQuota <= 0 {
		t.Fatalf("not charged: %+v", task)
	}
	amount := int(*task.ChargedQuota)
	if user.Quota != initial-amount || user.UsedQuota != amount || user.RequestCount != 1 || channel.UsedQuota != int64(amount) {
		t.Fatalf("projection/balance mismatch: amount=%d user=%+v channel=%+v", amount, user, channel)
	}
	if len(logs) != 1 || logs[0].Quota != amount || logs[0].PromptTokens != 7 || logs[0].CompletionTokens != 3 || logs[0].CacheTokens != 2 || logs[0].TokenName != "token-alpha" {
		t.Fatalf("logs=%+v amount=%d", logs, amount)
	}
	if logs[0].Metadata.Data()["task_owner_id"] != task.OwnerID {
		t.Fatalf("log lost task identity: %+v", logs[0])
	}
}

func TestAsyncTaskFirstCloseProjectsOnceForBothFamilies(t *testing.T) {
	for _, family := range []string{model.TaskPlatformOpenAIResponsesBackground, model.TaskPlatformOpenAIBatch} {
		t.Run(family, func(t *testing.T) {
			task, finalize, initial := taskProjectionFixture(t, family)
			stale := *task
			if err := finalize(task); err != nil {
				t.Fatal(err)
			}
			if err := finalize(&stale); err != nil {
				t.Fatal(err)
			}
			if err := finalize(task); err != nil {
				t.Fatal(err)
			}
			assertTaskProjection(t, task, initial)
		})
	}
}

func TestAsyncTaskConcurrentClosesProjectOnlyWinner(t *testing.T) {
	for _, family := range []string{model.TaskPlatformOpenAIResponsesBackground, model.TaskPlatformOpenAIBatch} {
		t.Run(family, func(t *testing.T) {
			task, finalize, initial := taskProjectionFixture(t, family)
			// A single SQLite connection faithfully serializes the SQL row lock while
			// the callers still independently attempt the same owner/version.
			sqlDB, err := model.DB.DB()
			if err != nil {
				t.Fatal(err)
			}
			sqlDB.SetMaxOpenConns(1)
			a, b := *task, *task
			var wg sync.WaitGroup
			wg.Add(2)
			results := make(chan error, 2)
			go func() { defer wg.Done(); results <- finalize(&a) }()
			go func() { defer wg.Done(); results <- finalize(&b) }()
			wg.Wait()
			close(results)
			for err := range results {
				if err != nil {
					t.Fatal(err)
				}
			}
			assertTaskProjection(t, &a, initial)
		})
	}
}

func TestAsyncTaskProjectionFailureNeverReopensBalanceOrRetriesProjection(t *testing.T) {
	for _, family := range []string{model.TaskPlatformOpenAIResponsesBackground, model.TaskPlatformOpenAIBatch} {
		t.Run(family, func(t *testing.T) {
			task, finalize, initial := taskProjectionFixture(t, family)
			injected := errors.New("projection unavailable")
			if err := model.DB.Callback().Create().Before("gorm:create").Register("task_projection:log_failure", func(tx *gorm.DB) {
				if tx.Statement.Table == "logs" {
					tx.AddError(injected)
				}
			}); err != nil {
				t.Fatal(err)
			}
			if err := model.DB.Callback().Update().Before("gorm:update").Register("task_projection:counter_failure", func(tx *gorm.DB) {
				if tx.Statement.Table == "channels" {
					tx.AddError(injected)
				}
				if fields, ok := tx.Statement.Dest.(map[string]interface{}); ok && tx.Statement.Table == "users" {
					if _, present := fields["request_count"]; present {
						tx.AddError(injected)
					}
				}
			}); err != nil {
				t.Fatal(err)
			}
			if err := finalize(task); err != nil {
				t.Fatalf("projection rolled back settlement: %v", err)
			}
			model.DB.Callback().Create().Remove("task_projection:log_failure")
			model.DB.Callback().Update().Remove("task_projection:counter_failure")
			if err := finalize(task); err != nil {
				t.Fatal(err)
			}
			var user model.User
			var channel model.Channel
			var count int64
			model.DB.First(&user, task.UserId)
			model.DB.First(&channel, task.ChannelId)
			model.DB.Model(&model.Log{}).Where("content = ?", "async task: "+family).Count(&count)
			if task.ProviderState != model.TaskProviderStateClosed || task.ChargedQuota == nil || user.Quota != initial-int(*task.ChargedQuota) {
				t.Fatalf("billing truth lost: task=%+v user=%+v", task, user)
			}
			if count != 0 || user.RequestCount != 0 || channel.UsedQuota != 0 {
				t.Fatalf("projection unexpectedly retried: logs=%d requests=%d channel=%d", count, user.RequestCount, channel.UsedQuota)
			}
		})
	}
}
