package relay_util

import (
	"fmt"
	"testing"

	commonresponses "one-api/common/responses"
	"one-api/providers/openai"
	"one-api/types"
)

func TestResponsesMalformedPriceDimensionsDoNotUseDefaults(t *testing.T) {
	for _, fields := range []string{
		`"model":"tiered-model","service_tier":{"name":"flex"}`,
		`"model":{"name":"different-model"},"service_tier":"default"`,
	} {
		t.Run(fields, func(t *testing.T) {
			quota := newTieredQuotaForTest(t)
			payload := fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp_price",%s,"status":"completed","usage":{"input_tokens":100,"output_tokens":10,"total_tokens":110}}}`, fields)
			event, ok := commonresponses.ParseStreamUsageEvent([]byte(payload))
			if !ok || event.Response == nil || event.Response.Usage == nil {
				t.Fatal("independent usage was discarded")
			}
			event.Response.Usage.MarkProviderReported()
			usage := &types.Usage{}
			commonresponses.ApplyResponsesUsage(usage, event.Response)
			decision := quota.EvaluateProviderUsage(usage)
			if decision.Confirm || decision.FinalQuota != 0 || componentStatuses(decision.Components)["tokens"] != PriceComponentConflictingEvidence {
				t.Fatalf("uninterpretable dimension became billable: %+v", decision)
			}
			usage.SetProviderExtraBilling(types.APIToolTypeWebSearch, "", 1)
			decision = quota.EvaluateProviderUsage(usage)
			if !decision.Confirm || decision.FinalQuota <= 0 || componentStatuses(decision.Components)["tokens"] != PriceComponentConflictingEvidence {
				t.Fatalf("independent search component was discarded: %+v", decision)
			}
		})
	}
}

func TestResponsesStreamAttributionConflictKeepsIndependentToolBilling(t *testing.T) {
	for _, mode := range []string{"created_then_conflict", "terminal_then_conflict", "identical_terminal", "terminal_omits_dimensions", "terminal_then_tool"} {
		t.Run(mode, func(t *testing.T) {
			quota := newTieredQuotaForTest(t)
			usage := &types.Usage{}
			handler := &openai.OpenAIResponsesStreamHandler{Usage: usage}
			observe := func(payload string) {
				t.Helper()
				if err := handler.ObserveResponsesEvent("data: " + payload + "\n\n"); err != nil {
					t.Fatal(err)
				}
			}
			observe(`{"type":"response.created","response":{"id":"resp_price","model":"tiered-model","service_tier":"flex","tools":[{"type":"web_search"}]}}`)
			tool := `{"type":"response.output_item.done","item":{"id":"search_price","type":"web_search_call","status":"completed","action":{"type":"search"}}}`
			if mode != "terminal_then_tool" {
				observe(tool)
			}
			terminal := func(tier string, omit bool) string {
				fields := fmt.Sprintf(`"model":"tiered-model","service_tier":%q,`, tier)
				if omit {
					fields = ""
				}
				return fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp_price",%s"usage":{"input_tokens":100,"output_tokens":10,"total_tokens":110}}}`, fields)
			}
			tier := "flex"
			if mode == "created_then_conflict" {
				tier = "priority"
			}
			observe(terminal(tier, mode == "terminal_omits_dimensions"))
			if mode == "terminal_then_conflict" {
				observe(terminal("priority", false))
			} else if mode == "identical_terminal" {
				observe(terminal("flex", false))
			}
			// 同一已完成工具的迟到事件仍观察，但不能重复计费。
			observe(tool)
			observe(tool)
			key := types.BuildExtraBillingKey(types.APIToolTypeWebSearch, "")
			if usage.ExtraBilling[key].CallCount != 1 {
				t.Fatalf("独立工具用量丢失或重复：%+v", usage.ExtraBilling)
			}
			decision := quota.EvaluateProviderUsage(usage)
			statuses := componentStatuses(decision.Components)
			wantStatus := PriceComponentPriceable
			if mode == "created_then_conflict" || mode == "terminal_then_conflict" {
				wantStatus = PriceComponentConflictingEvidence
			}
			if statuses["tokens"] != wantStatus || statuses["unit:"+key] != PriceComponentPriceable || !decision.Confirm {
				t.Fatalf("token 冲突影响了独立服务，或被默认值掩盖：%+v", decision)
			}
			for _, component := range decision.Components {
				if component.Name == "tokens" && (wantStatus == PriceComponentConflictingEvidence && component.Charge != 0 || wantStatus == PriceComponentPriceable && component.Charge != 200) {
					t.Fatalf("累计 token 快照计价错误：%+v", component)
				}
			}
		})
	}
}
