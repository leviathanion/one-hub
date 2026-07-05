package config

import "testing"

func TestRetryStatusCodesPolicyDefault(t *testing.T) {
	original := RetryStatusCodes
	t.Cleanup(func() {
		if err := SetRetryStatusCodes(original); err != nil {
			t.Fatalf("restore retry status codes: %v", err)
		}
	})

	if err := SetRetryStatusCodes(DefaultRetryStatusCodes); err != nil {
		t.Fatalf("expected default retry status codes to parse, got %v", err)
	}
	for _, status := range []int{307, 401, 402, 403, 408, 429, 500, 502, 503, 504} {
		if !RetryStatusCodeIsRetryable(status) {
			t.Fatalf("expected default status %d to be retryable", status)
		}
	}
	for _, status := range []int{400, 404, 499, 501, 524, 599} {
		if RetryStatusCodeIsRetryable(status) {
			t.Fatalf("expected default status %d to be non-retryable", status)
		}
	}
}

func TestRetryStatusCodesPolicyCanonicalizesExactAndFamilyTokens(t *testing.T) {
	original := RetryStatusCodes
	t.Cleanup(func() {
		if err := SetRetryStatusCodes(original); err != nil {
			t.Fatalf("restore retry status codes: %v", err)
		}
	})

	if err := SetRetryStatusCodes(" 503, 401\n5xx;401，502、403 "); err != nil {
		t.Fatalf("expected retry status code policy to parse, got %v", err)
	}
	if RetryStatusCodes != "401,403,5xx" {
		t.Fatalf("expected canonical retry status codes, got %q", RetryStatusCodes)
	}
	if !RetryStatusCodeIsRetryable(401) || !RetryStatusCodeIsRetryable(599) {
		t.Fatal("expected exact and family status codes to match")
	}
	if RetryStatusCodeIsRetryable(429) {
		t.Fatal("expected omitted status code to remain non-retryable")
	}
}

func TestRetryStatusCodesPolicyAllowsEmptyList(t *testing.T) {
	original := RetryStatusCodes
	t.Cleanup(func() {
		if err := SetRetryStatusCodes(original); err != nil {
			t.Fatalf("restore retry status codes: %v", err)
		}
	})

	if err := SetRetryStatusCodes(" "); err != nil {
		t.Fatalf("expected empty retry status code policy to parse, got %v", err)
	}
	if RetryStatusCodes != "" {
		t.Fatalf("expected empty retry status code policy, got %q", RetryStatusCodes)
	}
	if RetryStatusCodeIsRetryable(500) {
		t.Fatal("expected empty retry status code policy to disable status retries")
	}
}

func TestRetryStatusCodesPolicyRejectsInvalidTokens(t *testing.T) {
	for _, value := range []string{"99", "600", "abc", "0xx", "6xx", "50x"} {
		if err := ValidateRetryStatusCodes(value); err == nil {
			t.Fatalf("expected retry status code policy %q to fail validation", value)
		}
	}
}
