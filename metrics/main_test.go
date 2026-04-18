package metrics

import (
	"testing"

	"one-api/common/config"
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
