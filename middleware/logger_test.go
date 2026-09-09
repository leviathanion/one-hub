package middleware

import "testing"

func TestQueryKeySummaryNeverIncludesValues(t *testing.T) {
	raw := "cursor=user-data&api_key=sk-secret&cursor=next"
	if got := queryKeySummary(raw); got != "api_key,cursor" {
		t.Fatalf("query summary=%q", got)
	}
	if got := queryKeySummary("bad=%zz"); got != "invalid" {
		t.Fatalf("malformed query summary=%q", got)
	}
}
