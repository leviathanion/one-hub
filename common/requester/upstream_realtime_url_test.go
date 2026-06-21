package requester

import "testing"

func TestValidateUpstreamRealtimeURLPolicy(t *testing.T) {
	t.Run("https upgrades to wss", func(t *testing.T) {
		got, err := ValidateUpstreamRealtimeURL("https://api.openai.com/v1/realtime", UpstreamRealtimeURLPolicy{})
		if err != nil {
			t.Fatalf("expected https upstream to validate, got %v", err)
		}
		if got != "wss://api.openai.com/v1/realtime" {
			t.Fatalf("expected wss rewrite, got %q", got)
		}
	})

	t.Run("plaintext rejected by default", func(t *testing.T) {
		if _, err := ValidateUpstreamRealtimeURL("http://example.com/v1/realtime", UpstreamRealtimeURLPolicy{}); err == nil {
			t.Fatal("expected plaintext upstream to be rejected")
		}
		if _, err := ValidateUpstreamRealtimeURL("ws://example.com/v1/realtime", UpstreamRealtimeURLPolicy{}); err == nil {
			t.Fatal("expected plaintext websocket upstream to be rejected")
		}
	})

	t.Run("self hosted permits plaintext and private hosts", func(t *testing.T) {
		got, err := ValidateUpstreamRealtimeURL("http://127.0.0.1:8080/v1/realtime", UpstreamRealtimeURLPolicy{AllowSelfHosted: true})
		if err != nil {
			t.Fatalf("expected self-hosted plaintext upstream to validate, got %v", err)
		}
		if got != "ws://127.0.0.1:8080/v1/realtime" {
			t.Fatalf("expected ws rewrite, got %q", got)
		}
	})

	t.Run("self hosted still rejects metadata hosts", func(t *testing.T) {
		for _, rawURL := range []string{
			"http://169.254.169.254/latest/meta-data",
			"http://100.100.100.200/latest/meta-data",
			"http://[fd00:ec2::254]/latest/meta-data",
			"http://metadata.google.internal/computeMetadata/v1",
		} {
			if _, err := ValidateUpstreamRealtimeURL(rawURL, UpstreamRealtimeURLPolicy{AllowSelfHosted: true}); err == nil {
				t.Fatalf("expected self-hosted metadata upstream %s to be rejected", rawURL)
			}
		}
	})

	for _, rawURL := range []string{
		"https://127.0.0.1/v1/realtime",
		"https://localhost/v1/realtime",
		"https://ｌｏｃａｌｈｏｓｔ/v1/realtime",
		"https://10.0.0.12/v1/realtime",
		"https://172.16.0.1/v1/realtime",
		"https://192.168.1.2/v1/realtime",
		"https://169.254.169.254/latest/meta-data",
		"https://100.100.100.200/latest/meta-data",
		"https://[fd00:ec2::254]/latest/meta-data",
		"https://metadata.google.internal/computeMetadata/v1",
		"https://metadata.google.internal./computeMetadata/v1",
		"https://[fe80::1]/v1/realtime",
	} {
		t.Run("blocked "+rawURL, func(t *testing.T) {
			if _, err := ValidateUpstreamRealtimeURL(rawURL, UpstreamRealtimeURLPolicy{}); err == nil {
				t.Fatalf("expected %s to be rejected", rawURL)
			}
		})
	}
}

func TestValidateUpstreamRealtimeURLBlocksMetadataHostnameWithoutDNS(t *testing.T) {
	for _, policy := range []UpstreamRealtimeURLPolicy{
		{},
		{AllowSelfHosted: true},
		{ResolveHost: false},
	} {
		if _, err := ValidateUpstreamRealtimeURL("https://metadata.google.internal/computeMetadata/v1", policy); err == nil {
			t.Fatalf("expected metadata hostname to be rejected with policy %+v", policy)
		}
	}
}
