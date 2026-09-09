package base

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"one-api/types"
)

const (
	MaxChatRemoteMediaItems      = 16
	MaxChatRemoteMediaItemBytes  = 10 << 20
	MaxChatRemoteMediaTotalBytes = 20 << 20
)

// RemoteMediaMode is owned by the selected provider adapter. Candidate
// evaluation only chooses a mode; it never performs network I/O.
type RemoteMediaMode uint8

const (
	RemoteMediaReject RemoteMediaMode = iota
	RemoteMediaPassURL
	RemoteMediaMaterialize
)

type ChatRemoteMediaSummary struct {
	Items      int
	RemoteURLs int
	DataURIs   int
}

func (s ChatRemoteMediaSummary) HasMedia() bool { return s.Items > 0 }

// RemoteMediaFetcher is bound by the caller to the original request context.
// Keeping context out of this interface prevents adapters from detaching work
// from cancellation or substituting a longer-lived context.
type RemoteMediaFetcher interface {
	Fetch(rawURL string) (mimeType string, body []byte, err error)
}

type ChatRemoteMediaMaterializer interface {
	MaterializeChatRemoteMedia(request *types.ChatCompletionRequest, fetcher RemoteMediaFetcher) error
}

// ChatRemoteMediaHistoryNormalizer 是可选的 provider 本地 hook，用于把公开
// 输出表示在共享媒体准入/物化边界前转成标准 image_url。hook 只能做本地处理，
// 网络访问归 RemoteMediaFetcher 所有。
type ChatRemoteMediaHistoryNormalizer interface {
	NormalizeChatRemoteMedia(request *types.ChatCompletionRequest) error
}

// ErrBase64PayloadTooLarge distinguishes a bounded-size rejection from a
// malformed base64 payload without allocating a decoded buffer first.
var ErrBase64PayloadTooLarge = errors.New("base64 payload exceeds limit")

// DecodeBase64Bounded validates the strict padded StdEncoding form and checks
// the exact decoded size before allocating the output buffer. Callers use the
// same owner for raw image data and data URI payloads.
func DecodeBase64Bounded(payload string, maxDecodedBytes int) ([]byte, error) {
	if maxDecodedBytes <= 0 {
		return nil, errors.New("base64 decoded size limit is invalid")
	}
	if payload == "" {
		return nil, errors.New("base64 payload is empty")
	}
	maxInt := int(^uint(0) >> 1)
	if maxDecodedBytes > (maxInt-2)/4*3 {
		return nil, errors.New("base64 decoded size limit is invalid")
	}
	maxEncodedBytes := ((maxDecodedBytes + 2) / 3) * 4
	if len(payload) > maxEncodedBytes {
		return nil, fmt.Errorf("%w: %d decoded bytes allowed", ErrBase64PayloadTooLarge, maxDecodedBytes)
	}
	if len(payload)%4 != 0 {
		return nil, errors.New("base64 payload has invalid length")
	}
	if strings.IndexFunc(payload, func(char rune) bool {
		return char == '\r' || char == '\n' || char == '\t' || char == ' '
	}) >= 0 {
		return nil, errors.New("base64 payload contains whitespace")
	}

	padding := 0
	if len(payload) >= 1 && payload[len(payload)-1] == '=' {
		padding++
	}
	if len(payload) >= 2 && payload[len(payload)-2] == '=' {
		padding++
	}
	for index, char := range []byte(payload) {
		if char == '=' {
			if index < len(payload)-padding {
				return nil, errors.New("base64 payload has invalid padding")
			}
			continue
		}
		if !isBase64StdEncodingByte(char) {
			return nil, errors.New("base64 payload contains invalid character")
		}
	}
	decodedLen := len(payload)/4*3 - padding
	if decodedLen < 0 || decodedLen > maxDecodedBytes {
		return nil, fmt.Errorf("%w: %d decoded bytes allowed", ErrBase64PayloadTooLarge, maxDecodedBytes)
	}
	decoded := make([]byte, decodedLen)
	count, err := base64.StdEncoding.Strict().Decode(decoded, []byte(payload))
	if err != nil {
		return nil, errors.New("base64 payload is invalid")
	}
	return decoded[:count], nil
}

func isBase64StdEncodingByte(char byte) bool {
	return char >= 'A' && char <= 'Z' || char >= 'a' && char <= 'z' || char >= '0' && char <= '9' || char == '+' || char == '/'
}

// SummarizeChatRemoteMedia performs the provider-independent, local-only part
// of admission. It deliberately does not decode data URIs or contact URLs so it
// is safe to run for every routing candidate.
func SummarizeChatRemoteMedia(request *types.ChatCompletionRequest) (ChatRemoteMediaSummary, error) {
	var summary ChatRemoteMediaSummary
	if request == nil {
		return summary, errors.New("chat request is required")
	}
	for messageIndex, message := range request.Messages {
		for partIndex, part := range message.ParseContent() {
			if strings.TrimSpace(part.Type) != types.ContentTypeImageURL {
				continue
			}
			if part.ImageURL == nil || strings.TrimSpace(part.ImageURL.URL) == "" {
				return summary, fmt.Errorf("messages[%d].content[%d].image_url.url is required", messageIndex, partIndex)
			}
			rawURL := strings.TrimSpace(part.ImageURL.URL)
			summary.Items++
			if summary.Items > MaxChatRemoteMediaItems {
				return summary, fmt.Errorf("chat request contains more than %d media items", MaxChatRemoteMediaItems)
			}
			if strings.HasPrefix(strings.ToLower(rawURL), "data:") {
				summary.DataURIs++
				continue
			}
			parsed, err := url.Parse(rawURL)
			if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
				return summary, fmt.Errorf("messages[%d].content[%d].image_url.url must use http, https, or a raster data URI", messageIndex, partIndex)
			}
			summary.RemoteURLs++
		}
	}
	return summary, nil
}

// ValidateChatRemoteMedia performs the selected-request local validation used
// by PassURL adapters. Remote URLs remain untouched and are never fetched.
func ValidateChatRemoteMedia(request *types.ChatCompletionRequest) error {
	parts, err := collectChatRemoteMedia(request)
	if err != nil {
		return err
	}
	total := 0
	for _, part := range parts {
		if !part.dataURI {
			continue
		}
		_, body, err := DecodeChatMediaDataURI(part.url)
		if err != nil {
			return fmt.Errorf("messages[%d].content[%d].image_url.url is invalid: %w", part.messageIndex, part.partIndex, err)
		}
		total += len(body)
		if total > MaxChatRemoteMediaTotalBytes {
			return fmt.Errorf("chat request media exceeds %d total bytes", MaxChatRemoteMediaTotalBytes)
		}
	}
	return nil
}

// MaterializeChatRemoteMedia replaces every remote image URL with a validated
// raster data URI. The helper is provider-neutral; provider adapters opt into
// it explicitly through ChatRemoteMediaMaterializer.
func MaterializeChatRemoteMedia(request *types.ChatCompletionRequest, fetcher RemoteMediaFetcher) error {
	if fetcher == nil {
		return errors.New("remote media fetcher is required")
	}
	parts, err := collectChatRemoteMedia(request)
	if err != nil {
		return err
	}

	// Validate every local item before the first network operation. A malformed
	// later data URI therefore cannot cause an earlier URL to be fetched.
	total := 0
	for _, part := range parts {
		if !part.dataURI {
			continue
		}
		_, body, err := DecodeChatMediaDataURI(part.url)
		if err != nil {
			return fmt.Errorf("messages[%d].content[%d].image_url.url is invalid: %w", part.messageIndex, part.partIndex, err)
		}
		total += len(body)
		if total > MaxChatRemoteMediaTotalBytes {
			return fmt.Errorf("chat request media exceeds %d total bytes", MaxChatRemoteMediaTotalBytes)
		}
	}

	parsedByMessage := make(map[int][]types.ChatMessagePart)
	for _, part := range parts {
		if part.dataURI {
			continue
		}
		mimeType, body, err := fetcher.Fetch(part.url)
		if err != nil {
			return fmt.Errorf("materialize messages[%d].content[%d].image_url.url: %w", part.messageIndex, part.partIndex, err)
		}
		mimeType, err = CanonicalRasterMIME(mimeType)
		if err != nil {
			return err
		}
		if len(body) > MaxChatRemoteMediaItemBytes {
			return fmt.Errorf("materialized media exceeds %d bytes", MaxChatRemoteMediaItemBytes)
		}
		total += len(body)
		if total > MaxChatRemoteMediaTotalBytes {
			return fmt.Errorf("chat request media exceeds %d total bytes", MaxChatRemoteMediaTotalBytes)
		}

		messageParts, ok := parsedByMessage[part.messageIndex]
		if !ok {
			messageParts = request.Messages[part.messageIndex].ParseContent()
		}
		messageParts[part.partIndex].ImageURL.URL = "data:" + mimeType + ";base64," + base64.StdEncoding.EncodeToString(body)
		parsedByMessage[part.messageIndex] = messageParts
	}
	for messageIndex, messageParts := range parsedByMessage {
		request.Messages[messageIndex].Content = messageParts
	}
	return nil
}

type chatRemoteMediaPart struct {
	messageIndex int
	partIndex    int
	url          string
	dataURI      bool
}

func collectChatRemoteMedia(request *types.ChatCompletionRequest) ([]chatRemoteMediaPart, error) {
	summary, err := SummarizeChatRemoteMedia(request)
	if err != nil {
		return nil, err
	}
	parts := make([]chatRemoteMediaPart, 0, summary.Items)
	for messageIndex, message := range request.Messages {
		for partIndex, part := range message.ParseContent() {
			if strings.TrimSpace(part.Type) != types.ContentTypeImageURL {
				continue
			}
			rawURL := strings.TrimSpace(part.ImageURL.URL)
			parts = append(parts, chatRemoteMediaPart{
				messageIndex: messageIndex,
				partIndex:    partIndex,
				url:          rawURL,
				dataURI:      strings.HasPrefix(strings.ToLower(rawURL), "data:"),
			})
		}
	}
	return parts, nil
}

func DecodeChatMediaDataURI(raw string) (string, []byte, error) {
	raw = strings.TrimSpace(raw)
	comma := strings.IndexByte(raw, ',')
	if comma <= len("data:") || !strings.EqualFold(raw[:len("data:")], "data:") {
		return "", nil, errors.New("data URI is malformed")
	}
	header := raw[len("data:"):comma]
	segments := strings.Split(header, ";")
	if len(segments) != 2 || !strings.EqualFold(segments[1], "base64") {
		return "", nil, errors.New("data URI must use base64 without extra parameters")
	}
	mimeType := normalizeRasterMIME(segments[0])
	if !supportedRasterMIME(mimeType) {
		return "", nil, fmt.Errorf("data URI MIME %q is not a supported raster image", segments[0])
	}
	payload := raw[comma+1:]
	if payload == "" || strings.IndexFunc(payload, func(r rune) bool {
		return r == '\r' || r == '\n' || r == '\t' || r == ' '
	}) >= 0 {
		return "", nil, errors.New("data URI base64 payload is empty or contains whitespace")
	}
	body, err := DecodeBase64Bounded(payload, MaxChatRemoteMediaItemBytes)
	if err != nil {
		if errors.Is(err, ErrBase64PayloadTooLarge) {
			return "", nil, fmt.Errorf("data URI exceeds %d decoded bytes", MaxChatRemoteMediaItemBytes)
		}
		return "", nil, errors.New("data URI contains invalid base64")
	}
	return mimeType, body, nil
}

func normalizeRasterMIME(mimeType string) string {
	mimeType = strings.ToLower(strings.TrimSpace(mimeType))
	if mimeType == "image/jpg" {
		return "image/jpeg"
	}
	return mimeType
}

// CanonicalRasterMIME is shared by provider-local materializers so the
// RemoteMediaFetcher interface cannot expand the accepted media types.
func CanonicalRasterMIME(mimeType string) (string, error) {
	canonical := normalizeRasterMIME(mimeType)
	if !supportedRasterMIME(canonical) {
		return "", fmt.Errorf("materialized media MIME %q is not supported", mimeType)
	}
	return canonical, nil
}

func supportedRasterMIME(mimeType string) bool {
	switch mimeType {
	case "image/gif", "image/jpeg", "image/png", "image/webp":
		return true
	default:
		return false
	}
}
