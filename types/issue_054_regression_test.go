package types

import (
	"encoding/json"
	"testing"
)

func TestIssue054UsageEventOperationEvidenceSurvivesTrustedProjections(t *testing.T) {
	event := &UsageEvent{}
	event.MarkProviderOperationUnits(1)

	cloned := event.Clone()
	if cloned == nil || cloned.ProviderOperationUnits == nil || *cloned.ProviderOperationUnits != 1 {
		t.Fatalf("operation evidence was lost during clone: %+v", cloned)
	}
	*cloned.ProviderOperationUnits = 2
	if event.ProviderOperationUnits == nil || *event.ProviderOperationUnits != 1 {
		t.Fatalf("clone shares operation evidence pointer: original=%+v clone=%+v", event, cloned)
	}

	merged := &UsageEvent{}
	merged.Merge(event)
	if merged.ProviderOperationUnits == nil || *merged.ProviderOperationUnits != 1 {
		t.Fatalf("operation evidence was lost during merge: %+v", merged)
	}
	usage := event.ToChatUsage()
	if usage == nil || usage.ProviderOperationUnits == nil || *usage.ProviderOperationUnits != 1 {
		t.Fatalf("operation evidence was lost converting to Usage: %+v", usage)
	}
	*usage.ProviderOperationUnits = 3
	if *event.ProviderOperationUnits != 1 {
		t.Fatalf("Usage conversion shares operation evidence pointer: event=%+v usage=%+v", event, usage)
	}

	wire, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(wire, &fields); err != nil {
		t.Fatal(err)
	}
	if _, present := fields["provider_operation_units"]; present {
		t.Fatalf("internal operation evidence escaped into provider/client wire: %s", wire)
	}
}
