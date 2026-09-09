package responses

import (
	"encoding/json"
	"testing"
)

func TestProjectMultiAgentEnabled(t *testing.T) {
	for _, tc := range []struct {
		name    string
		raw     string
		want    bool
		wantErr bool
	}{
		{name: "enabled with future fields", raw: `{"enabled":true,"max_concurrent_subagents":3,"future":{"keep":true}}`, want: true},
		{name: "disabled", raw: `{"enabled":false}`},
		{name: "omitted enabled", raw: `{"max_concurrent_subagents":3}`},
		{name: "legacy boolean stays provider owned", raw: `true`},
		{name: "wrong enabled type stays provider owned", raw: `{"enabled":"true"}`},
		{name: "duplicate enabled follows raw JSON projection", raw: `{"enabled":true,"enabled":false}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ProjectMultiAgentEnabled(json.RawMessage(tc.raw))
			if got != tc.want || (err != nil) != tc.wantErr {
				t.Fatalf("ProjectMultiAgentEnabled(%s)=(%v,%v), want (%v,error=%v)", tc.raw, got, err, tc.want, tc.wantErr)
			}
		})
	}
}
