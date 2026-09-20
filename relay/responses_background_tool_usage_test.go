package relay

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	commonresponses "one-api/common/responses"
	"one-api/model"
	"one-api/relay/relay_util"
	"one-api/types"
)

func TestBackgroundToolSnapshotsSettleOnce(t *testing.T) {
	r, _ := backgroundFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_tools_bg","status":"queued"}`)
	}, `{"model":"gpt-5","background":true,"input":"hello"}`)
	model.PricingInstance.Prices["gpt-5"] = &model.Price{Model: "gpt-5", Type: model.TokensPriceType, Input: 1, Output: 1}
	if apiErr, done := RelayHandler(r); apiErr != nil || !done {
		t.Fatalf("create err=%v done=%v", apiErr, done)
	}
	waitBackgroundObserver(t, r.c)
	owner, err := model.GetResponseOwner(context.Background(), "resp_tools_bg", 1)
	if err != nil || owner.TaskOwnerID == nil {
		t.Fatalf("owner=%+v err=%v", owner, err)
	}
	task, err := model.GetBackgroundResponseTask(context.Background(), *owner.TaskOwnerID)
	if err != nil {
		t.Fatal(err)
	}
	var response types.OpenAIResponsesResponses
	if err := json.Unmarshal([]byte(`{"id":"resp_tools_bg","status":"in_progress","model":"gpt-5","tools":[{"type":"web_search","search_context_size":"high"}],"output":[{"id":"ws_1","type":"web_search_call","status":"completed","action":{"type":"search"}}],"usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5}}`), &response); err != nil {
		t.Fatal(err)
	}
	key := types.BuildExtraBillingKey(types.APIToolTypeWebSearch, "high")
	for i := 0; i < 2; i++ {
		observeBackgroundTask(context.Background(), task, &response, nil)
		if got := backgroundTaskData(task).Evidence.restore().ExtraBilling[key].CallCount; got != 1 {
			t.Fatalf("snapshot %d tool count=%d", i, got)
		}
	}
	baseline := &types.Usage{}
	commonresponses.ApplyResponsesUsage(baseline, &response)
	quota, err := relay_util.NewPricedQuota(backgroundTaskContext(context.Background(), task, backgroundTaskData(task)), "gpt-5", 0)
	if err != nil {
		t.Fatal(err)
	}
	expected := quota.EvaluateProviderUsage(baseline)
	if !expected.Confirm || expected.FinalQuota <= 0 {
		t.Fatalf("invalid pricing fixture: %+v", expected)
	}
	response.Status = "completed"
	observeBackgroundTask(context.Background(), task, &response, nil)
	if task.ChargedQuota == nil || *task.ChargedQuota != expected.FinalQuota {
		t.Fatalf("charge=%v want=%d", task.ChargedQuota, expected.FinalQuota)
	}
	var before, after model.User
	if err := model.DB.First(&before, 1).Error; err != nil {
		t.Fatal(err)
	}
	observeBackgroundTask(context.Background(), task, &response, nil)
	if err := model.DB.First(&after, 1).Error; err != nil {
		t.Fatal(err)
	}
	if before.Quota != after.Quota || before.UsedQuota != after.UsedQuota {
		t.Fatalf("duplicate terminal changed balance: before=%+v after=%+v", before, after)
	}
}
