package responses

import (
	"one-api/types"
	"testing"
)

func TestStreamObserverPreservesOpaqueResponseIdentity(t *testing.T) {
	observer := NewStreamObserver()
	var actual string
	observer.SetResponseIDObserver(func(id string) { actual = id })
	if err := observer.ObserveEvent(`data: {"type":"response.created","response":{"id":" Resp Raw "}}`); err != nil {
		t.Fatal(err)
	}
	if actual != " Resp Raw " {
		t.Fatalf("ownership callback got alias: %q", actual)
	}
	if err := observer.ObserveEvent(`data: {"type":"response.completed","response":{"id":"Resp Raw"}}`); err == nil {
		t.Fatal("different raw ID was accepted as the observed response")
	}
}
func TestToolBillingOpaqueIDsDoNotAlias(t *testing.T) {
	first, _, err := responsesToolBillingIdentity(" Item ", &types.ResponsesOutput{ID: " Item "}, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := responsesToolBillingIdentity("Item", &types.ResponsesOutput{ID: "Item"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if first.id == second.id {
		t.Fatal("tool billing identities were normalized")
	}
	if _, _, err := responsesToolBillingIdentity("Item", &types.ResponsesOutput{ID: " Item "}, nil); err == nil {
		t.Fatal("conflicting raw item IDs were merged")
	}
}
