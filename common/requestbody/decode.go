package requestbody

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/klauspost/compress/zstd"
)

type ErrorKind string

const (
	ErrorKindUnsupportedEncoding ErrorKind = "unsupported_encoding"
	ErrorKindInvalidEncoding     ErrorKind = "invalid_encoding"
	ErrorKindBodyTooLarge        ErrorKind = "body_too_large"
)

type DecodeError struct {
	Kind    ErrorKind
	Message string
}

func (e *DecodeError) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

func (e *DecodeError) StatusCode() int {
	if e == nil {
		return http.StatusBadRequest
	}
	switch e.Kind {
	case ErrorKindUnsupportedEncoding:
		return http.StatusUnsupportedMediaType
	case ErrorKindBodyTooLarge:
		return http.StatusRequestEntityTooLarge
	default:
		return http.StatusBadRequest
	}
}

type Limits struct {
	MaxWireBytes      int64
	MaxDecodedBytes   int64
	MaxExpansionRatio int64
	MaxEncodingLayers int
}

type DecodeMeta struct {
	ContentEncodings []string `json:"content_encodings,omitempty"`
	WireBytes        int      `json:"wire_bytes"`
	DecodedBytes     int      `json:"decoded_bytes"`
	ExpansionRatio   float64  `json:"expansion_ratio"`
}

type Decoder interface {
	Name() string
	Decode(input []byte, budget decodeBudget) ([]byte, error)
}

type decodePlan struct {
	encodings []string
}

var decoderRegistry = map[string]Decoder{
	"zstd": zstdDecoder{},
}

type zstdDecoder struct{}

func (zstdDecoder) Name() string {
	return "zstd"
}

func (zstdDecoder) Decode(input []byte, budget decodeBudget) ([]byte, error) {
	decodeWindowBudget := budget.maxDecodedBytes
	if decodeWindowBudget < zstd.MinWindowSize {
		// Trade-off: the library requires MaxWindow >= 1KiB, but we still pin
		// MaxMemory to the exact effective budget so valid oversized frames fail
		// as 413 while malformed payloads still surface as 400 invalid input.
		decodeWindowBudget = zstd.MinWindowSize
	}

	reader, err := zstd.NewReader(
		bytes.NewReader(input),
		zstd.WithDecoderConcurrency(1),
		zstd.WithDecoderLowmem(true),
		zstd.WithDecoderMaxMemory(uint64(budget.maxDecodedBytes)),
		zstd.WithDecoderMaxWindow(uint64(decodeWindowBudget)),
	)
	if err != nil {
		if decodeErr := mapZstdDecodeError(err, budget); decodeErr != nil {
			return nil, decodeErr
		}
		return nil, fmt.Errorf("create zstd reader: %w", err)
	}
	defer reader.Close()

	return readDecodedBytes(reader, budget)
}

// DecodeBody normalizes transport-level request encodings before the rest of the
// application inspects the payload. Callers should treat the returned decoded
// body as the canonical request body and keep the wire body only for diagnostics.
func DecodeBody(reader io.Reader, contentEncoding string, limits Limits) ([]byte, []byte, *DecodeMeta, error) {
	plan, err := prepareDecodePlan(contentEncoding, limits.MaxEncodingLayers)
	if err != nil {
		return nil, nil, nil, err
	}

	wireBody, err := readWireBody(reader, limits.MaxWireBytes)
	if err != nil {
		return nil, nil, nil, err
	}
	if len(plan.encodings) == 0 {
		meta := buildDecodeMeta(nil, wireBody, wireBody)
		return wireBody, wireBody, meta, nil
	}

	chainBudget := buildChainDecodeBudget(int64(len(wireBody)), limits)
	current := wireBody
	for i := len(plan.encodings) - 1; i >= 0; i-- {
		encoding := plan.encodings[i]
		decoder := decoderRegistry[encoding]
		current, err = decoder.Decode(current, chainBudget)
		if err != nil {
			if decodeErr, ok := err.(*DecodeError); ok {
				return wireBody, nil, nil, decodeErr
			}
			return wireBody, nil, nil, &DecodeError{
				Kind:    ErrorKindInvalidEncoding,
				Message: fmt.Sprintf("failed to decode request body encoded with %s", encoding),
			}
		}
	}

	meta := buildDecodeMeta(plan.encodings, wireBody, current)
	return wireBody, current, meta, nil
}

// NormalizeContentEncodingLabel collapses user-provided Content-Encoding values
// into a bounded label space for metrics. Supported chains keep their canonical
// order while invalid, unsupported, or overlong values are bucketed, avoiding
// unbounded metric cardinality from arbitrary header input.
func NormalizeContentEncodingLabel(value string, maxLayers int) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "none"
	}

	encodings, err := parseContentEncodings(trimmed, 0)
	if err != nil {
		return "invalid"
	}
	if len(encodings) == 0 {
		return "identity"
	}
	if maxLayers > 0 && len(encodings) > maxLayers {
		return "too_many_layers"
	}

	normalized := make([]string, 0, len(encodings))
	for _, encoding := range encodings {
		if _, ok := decoderRegistry[encoding]; !ok {
			return "unsupported"
		}
		normalized = append(normalized, encoding)
	}
	return strings.Join(normalized, "+")
}

func prepareDecodePlan(contentEncoding string, maxLayers int) (*decodePlan, error) {
	encodings, err := parseContentEncodings(contentEncoding, maxLayers)
	if err != nil {
		return nil, err
	}
	for _, encoding := range encodings {
		if _, ok := decoderRegistry[encoding]; !ok {
			return nil, &DecodeError{
				Kind:    ErrorKindUnsupportedEncoding,
				Message: fmt.Sprintf("unsupported content encoding %q", encoding),
			}
		}
	}
	return &decodePlan{
		encodings: encodings,
	}, nil
}

func readWireBody(reader io.Reader, maxWireBytes int64) ([]byte, error) {
	if reader == nil {
		return []byte{}, nil
	}

	limit := maxWireBytes
	if limit <= 0 {
		limit = 64 << 20
	}

	limitedReader := &io.LimitedReader{R: reader, N: limit + 1}
	body, err := io.ReadAll(limitedReader)
	if err != nil {
		return nil, &DecodeError{
			Kind:    ErrorKindInvalidEncoding,
			Message: "failed to read encoded request body",
		}
	}
	if int64(len(body)) > limit {
		return nil, &DecodeError{
			Kind:    ErrorKindBodyTooLarge,
			Message: "encoded request body exceeds the configured size limit",
		}
	}
	return body, nil
}

type decodeBudget struct {
	maxDecodedBytes    int64
	limitedByExpansion bool
}

func readDecodedBytes(reader io.Reader, budget decodeBudget) ([]byte, error) {
	limitedReader := &io.LimitedReader{R: reader, N: budget.maxDecodedBytes + 1}
	output, err := io.ReadAll(limitedReader)
	if err != nil {
		if decodeErr := mapZstdDecodeError(err, budget); decodeErr != nil {
			return nil, decodeErr
		}
		return nil, &DecodeError{
			Kind:    ErrorKindInvalidEncoding,
			Message: "failed to decode request body",
		}
	}
	if int64(len(output)) > budget.maxDecodedBytes {
		if budget.limitedByExpansion {
			return nil, &DecodeError{
				Kind:    ErrorKindBodyTooLarge,
				Message: "decoded request body exceeds the configured expansion ratio limit",
			}
		}
		return nil, &DecodeError{
			Kind:    ErrorKindBodyTooLarge,
			Message: "decoded request body exceeds the configured size limit",
		}
	}

	return output, nil
}

func parseContentEncodings(value string, maxLayers int) ([]string, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return nil, nil
	}

	parts := strings.Split(trimmed, ",")
	encodings := make([]string, 0, len(parts))
	for _, part := range parts {
		token := strings.ToLower(strings.TrimSpace(part))
		if token == "" {
			return nil, &DecodeError{
				Kind:    ErrorKindInvalidEncoding,
				Message: "invalid content encoding header",
			}
		}
		if token == "identity" {
			continue
		}
		encodings = append(encodings, token)
	}

	if maxLayers > 0 && len(encodings) > maxLayers {
		return nil, &DecodeError{
			Kind:    ErrorKindBodyTooLarge,
			Message: "content encoding chain exceeds the configured layer limit",
		}
	}

	return encodings, nil
}

func buildDecodeMeta(encodings []string, wireBody []byte, decodedBody []byte) *DecodeMeta {
	meta := &DecodeMeta{
		ContentEncodings: append([]string(nil), encodings...),
		WireBytes:        len(wireBody),
		DecodedBytes:     len(decodedBody),
	}
	if len(wireBody) > 0 {
		meta.ExpansionRatio = float64(len(decodedBody)) / float64(len(wireBody))
	}
	return meta
}

func configuredDecodedLimit(limits Limits) int64 {
	if limits.MaxDecodedBytes > 0 {
		return limits.MaxDecodedBytes
	}
	return 64 << 20
}

func configuredExpansionLimit(inputBytes int64, limits Limits) (int64, bool) {
	if limits.MaxExpansionRatio <= 0 || inputBytes <= 0 {
		return 0, false
	}
	return saturatingMultiply(inputBytes, limits.MaxExpansionRatio), true
}

func buildChainDecodeBudget(wireBytes int64, limits Limits) decodeBudget {
	// Trade-off: every decode layer shares a single budget derived from the
	// original wire body. That can reject rare chains where an intermediate blob
	// is larger than the final cleartext, but it keeps CPU/memory bounded and
	// prevents multi-layer encodings from resetting the expansion allowance.
	decodeBudget := decodeBudget{
		maxDecodedBytes: configuredDecodedLimit(limits),
	}
	if expansionLimit, ok := configuredExpansionLimit(wireBytes, limits); ok && expansionLimit <= decodeBudget.maxDecodedBytes {
		decodeBudget.maxDecodedBytes = expansionLimit
		decodeBudget.limitedByExpansion = true
	}
	return decodeBudget
}

func mapZstdDecodeError(err error, budget decodeBudget) *DecodeError {
	switch {
	case errors.Is(err, zstd.ErrWindowSizeExceeded):
		return &DecodeError{
			Kind:    ErrorKindBodyTooLarge,
			Message: "encoded request body requires a decoder window larger than the configured limit",
		}
	case errors.Is(err, zstd.ErrDecoderSizeExceeded), errors.Is(err, zstd.ErrFrameSizeExceeded):
		message := "decoded request body exceeds the configured size limit"
		if budget.limitedByExpansion {
			message = "decoded request body exceeds the configured expansion ratio limit"
		}
		return &DecodeError{
			Kind:    ErrorKindBodyTooLarge,
			Message: message,
		}
	default:
		return nil
	}
}

func saturatingMultiply(left, right int64) int64 {
	if left <= 0 || right <= 0 {
		return 0
	}
	if left > (1<<63-1)/right {
		return 1<<63 - 1
	}
	return left * right
}
