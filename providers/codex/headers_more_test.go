package codex

import "testing"

func TestCodexHeaderBagAdditionalBranches(t *testing.T) {
	if normalizeCodexHeaderKey("  X-Test  ") != "x-test" {
		t.Fatal("expected codex header key normalization to trim and lowercase")
	}

	var nilBag *codexHeaderBag
	nilBag.Set("x-test", "1")
	nilBag.SetIfAbsent("x-test", "1")
	nilBag.Delete("x-test")
	if nilBag.Get("x-test") != "" || nilBag.Has("x-test") || nilBag.Map() != nil {
		t.Fatal("expected nil codex header bag helpers to stay no-op")
	}

	bag := newCodexHeaderBag()
	bag.Set("  ", "value")
	if len(bag.Map()) != 0 {
		t.Fatalf("expected blank codex header keys to be ignored, got %+v", bag.Map())
	}

	bag.Set("X-Test", "1")
	bag.SetIfAbsent("X-Test", "2")
	if got := bag.Get("x-test"); got != "1" {
		t.Fatalf("expected SetIfAbsent not to replace existing header values, got %q", got)
	}

	bag.Delete("x-test")
	if bag.Has("x-test") {
		t.Fatalf("expected Delete to remove codex headers, got %+v", bag.Map())
	}
}
