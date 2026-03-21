package main

import "testing"

func TestParseMetricsSnapshotAndDiff(t *testing.T) {
	beforeRaw := []byte(`
# HELP ttft_ms Time to first token in milliseconds.
# TYPE ttft_ms histogram
ttft_ms_bucket{path="/v1/chat/completions",transform_mode="native_chat",le="10"} 2
ttft_ms_bucket{path="/v1/chat/completions",transform_mode="native_chat",le="20"} 4
ttft_ms_bucket{path="/v1/chat/completions",transform_mode="native_chat",le="50"} 4
ttft_ms_sum{path="/v1/chat/completions",transform_mode="native_chat"} 60
ttft_ms_count{path="/v1/chat/completions",transform_mode="native_chat"} 4
`)
	afterRaw := []byte(`
# HELP ttft_ms Time to first token in milliseconds.
# TYPE ttft_ms histogram
ttft_ms_bucket{path="/v1/chat/completions",transform_mode="native_chat",le="10"} 3
ttft_ms_bucket{path="/v1/chat/completions",transform_mode="native_chat",le="20"} 6
ttft_ms_bucket{path="/v1/chat/completions",transform_mode="native_chat",le="50"} 8
ttft_ms_sum{path="/v1/chat/completions",transform_mode="native_chat"} 130
ttft_ms_count{path="/v1/chat/completions",transform_mode="native_chat"} 8
`)

	before, err := parseMetricsSnapshot(beforeRaw)
	if err != nil {
		t.Fatalf("parse before snapshot failed: %v", err)
	}
	after, err := parseMetricsSnapshot(afterRaw)
	if err != nil {
		t.Fatalf("parse after snapshot failed: %v", err)
	}

	reports := diffSnapshots(before, after)
	if len(reports) != 1 {
		t.Fatalf("expected one report, got %d", len(reports))
	}

	report := reports[0]
	if report.metric != "ttft_ms" {
		t.Fatalf("unexpected metric: %s", report.metric)
	}
	if report.labels["path"] != "/v1/chat/completions" {
		t.Fatalf("unexpected path label: %v", report.labels)
	}
	if report.count != 4 {
		t.Fatalf("expected count delta 4, got %.0f", report.count)
	}
	if report.avg != 17.5 {
		t.Fatalf("expected avg 17.5, got %.2f", report.avg)
	}
	if report.p50 != 20 || report.p95 != 50 || report.p99 != 50 {
		t.Fatalf("unexpected quantiles: p50=%.2f p95=%.2f p99=%.2f", report.p50, report.p95, report.p99)
	}
}

func TestParseLabelSet(t *testing.T) {
	labels, err := parseLabelSet(`path="/v1/chat/completions",transform_mode="native_chat",le="20"`)
	if err != nil {
		t.Fatalf("parse label set failed: %v", err)
	}
	if labels["path"] != "/v1/chat/completions" || labels["transform_mode"] != "native_chat" || labels["le"] != "20" {
		t.Fatalf("unexpected labels: %#v", labels)
	}
}
