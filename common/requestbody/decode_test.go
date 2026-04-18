package requestbody

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func TestDecodeBodyZstdSuccess(t *testing.T) {
	plain := []byte(`{"model":"gpt-5","input":"hello"}`)
	compressed := mustCompressZstd(t, plain)

	wireBody, decodedBody, meta, err := DecodeBody(bytes.NewReader(compressed), "zstd", Limits{
		MaxWireBytes:          1 << 20,
		MaxDecodedBytes:       1 << 20,
		MaxDecoderWindowBytes: 1 << 20,
		MaxExpansionRatio:     64,
		MaxEncodingLayers:     2,
	})
	if err != nil {
		t.Fatalf("expected zstd body to decode, got %v", err)
	}
	if string(wireBody) != string(compressed) {
		t.Fatal("expected wire body to preserve the compressed payload")
	}
	if string(decodedBody) != string(plain) {
		t.Fatalf("unexpected decoded body: %s", decodedBody)
	}
	if meta == nil || meta.DecodedBytes != len(plain) || meta.WireBytes != len(compressed) {
		t.Fatalf("expected decode meta to be populated, got %+v", meta)
	}
	if len(meta.ContentEncodings) != 1 || meta.ContentEncodings[0] != "zstd" {
		t.Fatalf("unexpected content encodings in meta: %+v", meta)
	}
}

func TestDecodeBodyRejectsUnsupportedEncoding(t *testing.T) {
	_, _, _, err := DecodeBody(strings.NewReader("payload"), "gzip", Limits{
		MaxWireBytes:      1024,
		MaxDecodedBytes:   1024,
		MaxExpansionRatio: 64,
		MaxEncodingLayers: 2,
	})
	if err == nil {
		t.Fatal("expected unsupported encoding to fail")
	}

	decodeErr, ok := err.(*DecodeError)
	if !ok {
		t.Fatalf("expected DecodeError, got %T", err)
	}
	if decodeErr.Kind != ErrorKindUnsupportedEncoding {
		t.Fatalf("expected unsupported encoding error kind, got %s", decodeErr.Kind)
	}
}

func TestDecodeBodyRejectsInvalidPlansBeforeReadingBody(t *testing.T) {
	tests := []struct {
		name      string
		header    string
		maxLayers int
		wantKind  ErrorKind
	}{
		{name: "unsupported", header: "gzip", maxLayers: 2, wantKind: ErrorKindUnsupportedEncoding},
		{name: "invalid_header", header: "zstd,,gzip", maxLayers: 2, wantKind: ErrorKindInvalidEncoding},
		{name: "too_many_layers", header: "zstd, zstd", maxLayers: 1, wantKind: ErrorKindBodyTooLarge},
	}

	for _, tc := range tests {
		reader := &trackingReader{}
		_, _, _, err := DecodeBody(reader, tc.header, Limits{
			MaxWireBytes:      1024,
			MaxDecodedBytes:   1024,
			MaxExpansionRatio: 64,
			MaxEncodingLayers: tc.maxLayers,
		})
		if err == nil {
			t.Fatalf("%s: expected decode plan to fail", tc.name)
		}

		decodeErr, ok := err.(*DecodeError)
		if !ok {
			t.Fatalf("%s: expected DecodeError, got %T", tc.name, err)
		}
		if decodeErr.Kind != tc.wantKind {
			t.Fatalf("%s: expected error kind %s, got %s", tc.name, tc.wantKind, decodeErr.Kind)
		}
		if reader.readCalls != 0 {
			t.Fatalf("%s: expected invalid decode plan to fail before reading body, got %d reads", tc.name, reader.readCalls)
		}
	}
}

func TestDecodeBodyRejectsInvalidHeader(t *testing.T) {
	_, _, _, err := DecodeBody(strings.NewReader("payload"), "zstd,,gzip", Limits{
		MaxWireBytes:      1024,
		MaxDecodedBytes:   1024,
		MaxExpansionRatio: 64,
		MaxEncodingLayers: 2,
	})
	if err == nil {
		t.Fatal("expected invalid encoding header to fail")
	}

	decodeErr, ok := err.(*DecodeError)
	if !ok {
		t.Fatalf("expected DecodeError, got %T", err)
	}
	if decodeErr.Kind != ErrorKindInvalidEncoding {
		t.Fatalf("expected invalid encoding error kind, got %s", decodeErr.Kind)
	}
}

func TestDecodeBodyRejectsTooManyLayers(t *testing.T) {
	_, _, _, err := DecodeBody(strings.NewReader("payload"), "identity, zstd, zstd", Limits{
		MaxWireBytes:      1024,
		MaxDecodedBytes:   1024,
		MaxExpansionRatio: 64,
		MaxEncodingLayers: 1,
	})
	if err == nil {
		t.Fatal("expected too many layers to fail")
	}

	decodeErr, ok := err.(*DecodeError)
	if !ok {
		t.Fatalf("expected DecodeError, got %T", err)
	}
	if decodeErr.Kind != ErrorKindBodyTooLarge {
		t.Fatalf("expected body too large error kind, got %s", decodeErr.Kind)
	}
}

func TestDecodeBodyRejectsDecodedSizeLimit(t *testing.T) {
	plain := bytes.Repeat([]byte("a"), 2048)
	compressed := mustCompressZstd(t, plain)

	_, _, _, err := DecodeBody(bytes.NewReader(compressed), "zstd", Limits{
		MaxWireBytes:      4096,
		MaxDecodedBytes:   512,
		MaxExpansionRatio: 64,
		MaxEncodingLayers: 2,
	})
	if err == nil {
		t.Fatal("expected decoded size limit to fail")
	}

	decodeErr, ok := err.(*DecodeError)
	if !ok {
		t.Fatalf("expected DecodeError, got %T", err)
	}
	if decodeErr.Kind != ErrorKindBodyTooLarge {
		t.Fatalf("expected body too large error kind, got %s", decodeErr.Kind)
	}
}

func TestDecodeBodyRejectsWireSizeLimit(t *testing.T) {
	plain := []byte(`{"model":"gpt-5","input":"small decoded payload"}`)
	compressed := mustCompressZstd(t, plain)

	_, _, _, err := DecodeBody(bytes.NewReader(compressed), "zstd", Limits{
		MaxWireBytes:      int64(len(compressed) - 1),
		MaxDecodedBytes:   1 << 20,
		MaxExpansionRatio: 64,
		MaxEncodingLayers: 2,
	})
	if err == nil {
		t.Fatal("expected wire size limit to fail")
	}

	decodeErr, ok := err.(*DecodeError)
	if !ok {
		t.Fatalf("expected DecodeError, got %T", err)
	}
	if decodeErr.Kind != ErrorKindBodyTooLarge {
		t.Fatalf("expected body too large error kind, got %s", decodeErr.Kind)
	}
}

func TestDecodeBodyRejectsExpansionRatioLimit(t *testing.T) {
	plain := bytes.Repeat([]byte("b"), 4096)
	compressed := mustCompressZstd(t, plain)

	_, _, _, err := DecodeBody(bytes.NewReader(compressed), "zstd", Limits{
		MaxWireBytes:      4096,
		MaxDecodedBytes:   8192,
		MaxExpansionRatio: 2,
		MaxEncodingLayers: 2,
	})
	if err == nil {
		t.Fatal("expected expansion ratio limit to fail")
	}

	decodeErr, ok := err.(*DecodeError)
	if !ok {
		t.Fatalf("expected DecodeError, got %T", err)
	}
	if decodeErr.Kind != ErrorKindBodyTooLarge {
		t.Fatalf("expected body too large error kind, got %s", decodeErr.Kind)
	}
}

func TestDecodeBodyRejectsMultiLayerExpansionRatioLimit(t *testing.T) {
	plain := bytes.Repeat([]byte("d"), 32<<10)
	compressedTwice := mustCompressZstd(t, mustCompressZstd(t, plain))

	_, _, _, err := DecodeBody(bytes.NewReader(compressedTwice), "zstd, zstd", Limits{
		MaxWireBytes:      1 << 20,
		MaxDecodedBytes:   1 << 20,
		MaxExpansionRatio: 64,
		MaxEncodingLayers: 2,
	})
	if err == nil {
		t.Fatal("expected multi-layer expansion ratio limit to fail")
	}

	decodeErr, ok := err.(*DecodeError)
	if !ok {
		t.Fatalf("expected DecodeError, got %T", err)
	}
	if decodeErr.Kind != ErrorKindBodyTooLarge {
		t.Fatalf("expected body too large error kind, got %s", decodeErr.Kind)
	}
	if !strings.Contains(decodeErr.Message, "expansion ratio") {
		t.Fatalf("expected expansion ratio error message, got %q", decodeErr.Message)
	}
}

func TestDecodeBodyRejectsZstdWindowBeyondConfiguredLimit(t *testing.T) {
	plain := bytes.Repeat([]byte("c"), 4096)
	compressed := mustCompressZstdWithOptions(t, plain, zstd.WithWindowSize(1<<20), zstd.WithSingleSegment(false))

	_, _, _, err := DecodeBody(bytes.NewReader(compressed), "zstd", Limits{
		MaxWireBytes:          int64(len(compressed) + 1),
		MaxDecodedBytes:       64 << 10,
		MaxDecoderWindowBytes: 64 << 10,
		MaxExpansionRatio:     64,
		MaxEncodingLayers:     2,
	})
	if err == nil {
		t.Fatal("expected oversized zstd window to fail")
	}

	decodeErr, ok := err.(*DecodeError)
	if !ok {
		t.Fatalf("expected DecodeError, got %T", err)
	}
	if decodeErr.Kind != ErrorKindBodyTooLarge {
		t.Fatalf("expected body too large error kind, got %s", decodeErr.Kind)
	}
}

func TestDecodeBodyAllowsConfiguredWindowAboveDecodedLimit(t *testing.T) {
	plain := bytes.Repeat([]byte("c"), 4096)
	compressed := mustCompressZstdWithOptions(t, plain, zstd.WithWindowSize(1<<20), zstd.WithSingleSegment(false))

	_, decodedBody, _, err := DecodeBody(bytes.NewReader(compressed), "zstd", Limits{
		MaxWireBytes:          int64(len(compressed) + 1),
		MaxDecodedBytes:       64 << 10,
		MaxDecoderWindowBytes: 1 << 20,
		MaxExpansionRatio:     512,
		MaxEncodingLayers:     2,
	})
	if err != nil {
		t.Fatalf("expected configured zstd window to decode, got %v", err)
	}
	if !bytes.Equal(decodedBody, plain) {
		t.Fatalf("unexpected decoded body length: got %d want %d", len(decodedBody), len(plain))
	}
}

func TestDecodeBodyStillRejectsDecodedSizeWhenWindowBudgetIsHigher(t *testing.T) {
	plain := bytes.Repeat([]byte("e"), 96<<10)
	compressed := mustCompressZstdWithOptions(t, plain, zstd.WithWindowSize(1<<20), zstd.WithSingleSegment(false))

	_, _, _, err := DecodeBody(bytes.NewReader(compressed), "zstd", Limits{
		MaxWireBytes:          int64(len(compressed) + 1),
		MaxDecodedBytes:       64 << 10,
		MaxDecoderWindowBytes: 1 << 20,
		MaxExpansionRatio:     4096,
		MaxEncodingLayers:     2,
	})
	if err == nil {
		t.Fatal("expected decoded size limit to fail even with a larger window budget")
	}

	decodeErr, ok := err.(*DecodeError)
	if !ok {
		t.Fatalf("expected DecodeError, got %T", err)
	}
	if decodeErr.Kind != ErrorKindBodyTooLarge {
		t.Fatalf("expected body too large error kind, got %s", decodeErr.Kind)
	}
	if !strings.Contains(decodeErr.Message, "size limit") {
		t.Fatalf("expected decoded size error message, got %q", decodeErr.Message)
	}
}

func TestNormalizeContentEncodingLabel(t *testing.T) {
	tests := []struct {
		name      string
		header    string
		maxLayers int
		want      string
	}{
		{name: "empty", header: "", want: "none"},
		{name: "identity", header: "identity", want: "identity"},
		{name: "supported", header: "ZSTD", want: "zstd"},
		{name: "chain", header: "identity, zstd", maxLayers: 2, want: "zstd"},
		{name: "unsupported", header: "gzip", want: "unsupported"},
		{name: "invalid", header: "zstd,,gzip", want: "invalid"},
		{name: "too_many_layers", header: "zstd, zstd, zstd", maxLayers: 2, want: "too_many_layers"},
	}

	for _, tc := range tests {
		if got := NormalizeContentEncodingLabel(tc.header, tc.maxLayers); got != tc.want {
			t.Fatalf("%s: expected %q, got %q", tc.name, tc.want, got)
		}
	}
}

func mustCompressZstd(t *testing.T, plain []byte) []byte {
	t.Helper()

	var buf bytes.Buffer
	writer, err := zstd.NewWriter(&buf)
	if err != nil {
		t.Fatalf("failed to create zstd writer: %v", err)
	}
	if _, err = writer.Write(plain); err != nil {
		t.Fatalf("failed to write zstd payload: %v", err)
	}
	if err = writer.Close(); err != nil {
		t.Fatalf("failed to close zstd writer: %v", err)
	}
	return buf.Bytes()
}

func mustCompressZstdWithOptions(t *testing.T, plain []byte, opts ...zstd.EOption) []byte {
	t.Helper()

	encoder, err := zstd.NewWriter(nil, opts...)
	if err != nil {
		t.Fatalf("failed to create zstd encoder: %v", err)
	}
	return encoder.EncodeAll(plain, nil)
}

type trackingReader struct {
	readCalls int
}

func (r *trackingReader) Read(_ []byte) (int, error) {
	r.readCalls++
	return 0, io.EOF
}
