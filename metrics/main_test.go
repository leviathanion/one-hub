package metrics

import (
	"testing"

	"one-api/common/config"

	dto "github.com/prometheus/client_model/go"
)

func TestRequestBodyDecodedBytesBucketsCoverDefaultDecodeLimit(t *testing.T) {
	buckets := requestBodyDecodedBytesBuckets()
	if len(buckets) == 0 {
		t.Fatal("expected decoded-bytes histogram buckets to be configured")
	}

	topBucket := buckets[len(buckets)-1]
	if topBucket < float64(config.RequestBodyDecodeMaxDecodedBytes) {
		t.Fatalf("decoded-bytes histogram stops at %.0f bytes, below default decode limit %d", topBucket, config.RequestBodyDecodeMaxDecodedBytes)
	}
}

func TestRequestBodyDecodedBytesBucketsFollowCurrentDecodeLimit(t *testing.T) {
	originalLimit := config.RequestBodyDecodeMaxDecodedBytes
	t.Cleanup(func() {
		config.RequestBodyDecodeMaxDecodedBytes = originalLimit
	})

	config.RequestBodyDecodeMaxDecodedBytes = 1 << 20
	lowerBuckets := requestBodyDecodedBytesBuckets()
	lowerTopBucket := lowerBuckets[len(lowerBuckets)-1]
	if lowerTopBucket < float64(config.RequestBodyDecodeMaxDecodedBytes) {
		t.Fatalf("decoded-bytes histogram stops at %.0f bytes, below lowered decode limit %d", lowerTopBucket, config.RequestBodyDecodeMaxDecodedBytes)
	}

	config.RequestBodyDecodeMaxDecodedBytes = 128 << 20
	higherBuckets := requestBodyDecodedBytesBuckets()
	higherTopBucket := higherBuckets[len(higherBuckets)-1]
	if higherTopBucket < float64(config.RequestBodyDecodeMaxDecodedBytes) {
		t.Fatalf("decoded-bytes histogram stops at %.0f bytes, below raised decode limit %d", higherTopBucket, config.RequestBodyDecodeMaxDecodedBytes)
	}
	if higherTopBucket <= lowerTopBucket {
		t.Fatalf("expected raised decode limit to expand histogram coverage, got lower=%.0f higher=%.0f", lowerTopBucket, higherTopBucket)
	}
}

func TestRecordUsageObservedUnbilledIncrementsCounter(t *testing.T) {
	source := "input_audio_transcription"
	model := "metrics-test-model"
	counter := usageObservedUnbilled.WithLabelValues(source, model)
	before := readCounterValue(t, counter)

	RecordUsageObservedUnbilled(source, model)

	after := readCounterValue(t, counter)
	if after != before+1 {
		t.Fatalf("expected usage_observed_unbilled to increment by 1, before=%v after=%v", before, after)
	}
}

func readCounterValue(t *testing.T, metric interface{ Write(*dto.Metric) error }) float64 {
	t.Helper()
	var pb dto.Metric
	if err := metric.Write(&pb); err != nil {
		t.Fatalf("expected metric write to succeed: %v", err)
	}
	if pb.Counter == nil || pb.Counter.Value == nil {
		t.Fatal("expected counter metric value")
	}
	return *pb.Counter.Value
}
