package relay

import (
	"fmt"
	"testing"
)

func TestResponsesWSAttributionConflictDoesNotChargeTokens(t *testing.T) {
	for _, test := range []struct {
		name     string
		created  string
		terminal string
		conflict bool
	}{
		{name: "model_conflict", created: `"model":"gpt-other"`, terminal: `"model":"gpt-5"`, conflict: true},
		{name: "tier_conflict", created: `"model":"gpt-5","service_tier":"flex"`, terminal: `"model":"gpt-5","service_tier":"priority"`, conflict: true},
		{name: "identical", created: `"model":"gpt-5"`, terminal: `"model":"gpt-5"`},
		{name: "terminal_omits_model", created: `"model":"gpt-5"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			actor, _, conn := newSteeringTestActor(t, 1000)
			attempt := actor.turns.active.attempt
			steeringProviderFrame(actor, fmt.Sprintf(`{"type":"response.created","response":{"id":"resp_parent",%s}}`, test.created))
			fields := test.terminal
			if fields != "" {
				fields += ","
			}
			terminal := fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp_parent",%s"usage":{"input_tokens":5,"output_tokens":2,"total_tokens":7}},"future":9007199254740993}`, fields)
			steeringProviderFrame(actor, terminal)
			if got, _ := conn.lastWrite.Load().(string); got != terminal {
				t.Fatalf("归属观察改变了原帧：%q", got)
			}
			if attempt.TerminalUsage == nil || attempt.TerminalUsage.AttributionConflict != test.conflict {
				t.Fatalf("归属冲突未正确保留：%+v", attempt.TerminalUsage)
			}
			wantBalance := 993
			if test.conflict {
				wantBalance = 1000
			}
			user, token := readResponsesWSQuotaFixture(t)
			if user.Quota != wantBalance || token.RemainQuota != wantBalance {
				t.Fatalf("归属冲突影响结算：user=%d token=%d want=%d", user.Quota, token.RemainQuota, wantBalance)
			}
			steeringProviderFrame(actor, terminal)
			user, token = readResponsesWSQuotaFixture(t)
			if user.Quota != wantBalance || token.RemainQuota != wantBalance {
				t.Fatal("重复终态产生了第二次余额动作")
			}
		})
	}
}
