package codex

import (
	"testing"

	"one-api/types"
)

func TestClassifyCodexSupplierTerminalOwnsPrivateAliases(t *testing.T) {
	cases := []struct {
		name      string
		eventType string
		response  *types.OpenAIResponsesResponses
		kind      codexSupplierTerminalKind
	}{
		{name: "sparse supplier done", eventType: types.EventTypeResponseDone, response: &types.OpenAIResponsesResponses{}, kind: codexSupplierCompletedTerminal},
		{name: "responses completed supplier alias", eventType: "response.completed", response: &types.OpenAIResponsesResponses{Status: types.ResponseStatusCompleted}, kind: codexSupplierCompletedTerminal},
		{name: "responses failed supplier alias", eventType: "response.failed", response: &types.OpenAIResponsesResponses{Status: types.ResponseStatusFailed}, kind: codexSupplierFailedTerminal},
		{name: "responses incomplete supplier alias", eventType: "response.incomplete", response: &types.OpenAIResponsesResponses{Status: types.ResponseStatusIncomplete}, kind: codexSupplierFailedTerminal},
		{name: "supplier cancelled alias", eventType: "response.cancelled", response: &types.OpenAIResponsesResponses{Status: types.ResponseStatusCancelled}, kind: codexSupplierCancelledTerminal},
		{name: "supplier terminal status fallback", eventType: "response.updated", response: &types.OpenAIResponsesResponses{Status: types.ResponseStatusCancelled}, kind: codexSupplierCancelledTerminal},
		{name: "nonterminal supplier event", eventType: "response.updated", response: &types.OpenAIResponsesResponses{Status: types.ResponseStatusInProgress}, kind: codexSupplierNonTerminal},
		{name: "uncorrelated top-level error", eventType: types.EventTypeError, response: nil, kind: codexSupplierNonTerminal},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyCodexSupplierTerminal(tc.eventType, tc.response, false)
			if got != tc.kind || got.isTerminal() != (tc.kind != codexSupplierNonTerminal) {
				t.Fatalf("classifyCodexSupplierTerminal() = %v, want %v", got, tc.kind)
			}
		})
	}
}

func TestInterpretCodexSupplierCancellationDoesNotFabricatePublicTerminal(t *testing.T) {
	evidence, ok := interpretCodexSupplierTerminal(
		"response.cancelled",
		&types.OpenAIResponsesResponses{Status: types.ResponseStatusCancelled},
	)
	if !ok {
		t.Fatal("expected supplier cancellation to be terminal")
	}
	if evidence.kind != codexSupplierCancelledTerminal || evidence.publicEventType != "" || evidence.responseStatus != "" {
		t.Fatalf("supplier cancellation must remain unrepresentable: %+v", evidence)
	}
}

func TestCodexSupplierTopLevelErrorScopeRequiresMatchingIdentity(t *testing.T) {
	if !codexSupplierErrorIsConnectionScoped(true, "", "resp_1") {
		t.Fatal("unidentified top-level error must fail-close the connection")
	}
	if !codexSupplierErrorIsConnectionScoped(true, "resp_other", "resp_1") {
		t.Fatal("mismatched top-level error must fail-close the connection")
	}
	if codexSupplierErrorIsConnectionScoped(true, "resp_1", "resp_1") {
		t.Fatal("matching top-level error must stay scoped to its turn")
	}
	if codexSupplierErrorIsConnectionScoped(false, "", "") {
		t.Fatal("ordinary provider event became a connection error")
	}
}
