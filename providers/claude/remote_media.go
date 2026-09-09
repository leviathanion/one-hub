package claude

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"one-api/providers/base"
)

// NativeRemoteMediaSummary describes only URL source blocks that one-hub may
// need to adapt. Existing base64 sources and unknown future source variants
// remain provider-owned wire semantics.
type NativeRemoteMediaSummary struct {
	Items        int
	ImageURLs    int
	DocumentURLs int
	DataURIs     int
	InvalidURLs  int
}

func (s NativeRemoteMediaSummary) HasURLSources() bool { return s.Items > 0 }

// SummarizeNativeRemoteMedia is local-only and tolerant. Direct Claude relays
// use the summary without turning one-hub into an Anthropic request validator;
// adapters that need inline bytes call ValidateMaterializableNativeRemoteMedia.
func SummarizeNativeRemoteMedia(request *ClaudeRequest) NativeRemoteMediaSummary {
	var summary NativeRemoteMediaSummary
	if request == nil {
		return summary
	}
	for index := range request.Messages {
		walkNativeClaudeContent(request.Messages[index].Content, fmt.Sprintf("messages[%d].content", index), func(ref nativeClaudeURLSource) {
			summary.Items++
			if ref.kind == "document" {
				summary.DocumentURLs++
			} else {
				summary.ImageURLs++
			}
			if strings.TrimSpace(ref.rawURL) == "" {
				summary.InvalidURLs++
			} else if strings.HasPrefix(strings.ToLower(strings.TrimSpace(ref.rawURL)), "data:") {
				summary.DataURIs++
			}
		})
	}
	return summary
}

// ValidateMaterializableNativeRemoteMedia checks only the explicit adapter
// boundary used by Bedrock/Vertex. Their Anthropic-compatible surfaces do not
// accept URL sources, while the current SafeFetcher intentionally supports
// raster images only, so document URL sources are not representable.
func ValidateMaterializableNativeRemoteMedia(request *ClaudeRequest, summary NativeRemoteMediaSummary) error {
	if request == nil {
		return errors.New("Claude request is required")
	}
	if summary.Items > base.MaxChatRemoteMediaItems {
		return fmt.Errorf("Claude request contains more than %d URL media items", base.MaxChatRemoteMediaItems)
	}
	if summary.DocumentURLs > 0 {
		return errors.New("document URL sources cannot be represented by this Claude adapter")
	}
	if summary.InvalidURLs > 0 {
		return errors.New("Claude image URL source is empty")
	}
	var validationErr error
	for index := range request.Messages {
		walkNativeClaudeContent(request.Messages[index].Content, fmt.Sprintf("messages[%d].content", index), func(ref nativeClaudeURLSource) {
			if validationErr != nil || ref.kind != "image" {
				return
			}
			for field := range ref.source {
				if field != "type" && field != "url" {
					validationErr = fmt.Errorf("%s.source.%s cannot be represented by this Claude adapter", ref.path, field)
					return
				}
			}
			rawURL := strings.TrimSpace(ref.rawURL)
			if strings.HasPrefix(strings.ToLower(rawURL), "data:") {
				return
			}
			parsed, err := url.Parse(rawURL)
			if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
				validationErr = fmt.Errorf("%s.source.url must use http, https, or a raster data URI", ref.path)
			}
		})
	}
	return validationErr
}

// MaterializeNativeRemoteMedia deep-copies the typed cross-protocol request and
// replaces every image URL source with a validated base64 source. The direct
// Claude exact-wire request body is never modified.
func MaterializeNativeRemoteMedia(request *ClaudeRequest, fetcher base.RemoteMediaFetcher) (*ClaudeRequest, error) {
	if request == nil {
		return nil, errors.New("Claude request is required")
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("copy Claude request: %w", err)
	}
	prepared := &ClaudeRequest{}
	if err := json.Unmarshal(encoded, prepared); err != nil {
		return nil, fmt.Errorf("copy Claude request: %w", err)
	}
	summary := SummarizeNativeRemoteMedia(prepared)
	if err := ValidateMaterializableNativeRemoteMedia(prepared, summary); err != nil {
		return nil, err
	}

	refs := collectNativeClaudeURLSources(prepared)
	total := 0
	// Decode all local values before the first network operation.
	for _, ref := range refs {
		if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(ref.rawURL)), "data:") {
			continue
		}
		mimeType, body, err := base.DecodeChatMediaDataURI(ref.rawURL)
		if err != nil {
			return nil, fmt.Errorf("%s.source.url is invalid: %w", ref.path, err)
		}
		total += len(body)
		if total > base.MaxChatRemoteMediaTotalBytes {
			return nil, fmt.Errorf("Claude request media exceeds %d total bytes", base.MaxChatRemoteMediaTotalBytes)
		}
		mimeType, err = base.CanonicalRasterMIME(mimeType)
		if err != nil {
			return nil, fmt.Errorf("%s.source.url: %w", ref.path, err)
		}
		setNativeClaudeBase64Source(ref, mimeType, body)
	}

	for _, ref := range refs {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(ref.rawURL)), "data:") {
			continue
		}
		if fetcher == nil {
			return nil, errors.New("remote media fetcher is required")
		}
		mimeType, body, err := fetcher.Fetch(strings.TrimSpace(ref.rawURL))
		if err != nil {
			return nil, fmt.Errorf("materialize %s.source.url: %w", ref.path, err)
		}
		if len(body) > base.MaxChatRemoteMediaItemBytes {
			return nil, fmt.Errorf("materialized media exceeds %d bytes", base.MaxChatRemoteMediaItemBytes)
		}
		total += len(body)
		if total > base.MaxChatRemoteMediaTotalBytes {
			return nil, fmt.Errorf("Claude request media exceeds %d total bytes", base.MaxChatRemoteMediaTotalBytes)
		}
		mimeType, err = base.CanonicalRasterMIME(mimeType)
		if err != nil {
			return nil, fmt.Errorf("%s.source.url: %w", ref.path, err)
		}
		setNativeClaudeBase64Source(ref, mimeType, body)
	}
	return prepared, nil
}

type nativeClaudeURLSource struct {
	kind   string
	path   string
	rawURL string
	block  map[string]any
	source map[string]any
}

func collectNativeClaudeURLSources(request *ClaudeRequest) []nativeClaudeURLSource {
	if request == nil {
		return nil
	}
	refs := make([]nativeClaudeURLSource, 0)
	for index := range request.Messages {
		walkNativeClaudeContent(request.Messages[index].Content, fmt.Sprintf("messages[%d].content", index), func(ref nativeClaudeURLSource) {
			refs = append(refs, ref)
		})
	}
	return refs
}

func walkNativeClaudeContent(content any, path string, visit func(nativeClaudeURLSource)) {
	blocks, ok := content.([]any)
	if !ok {
		return
	}
	for index, rawBlock := range blocks {
		block, ok := rawBlock.(map[string]any)
		if !ok {
			continue
		}
		blockPath := fmt.Sprintf("%s[%d]", path, index)
		blockType, _ := block["type"].(string)
		switch strings.TrimSpace(blockType) {
		case "image", "document":
			source, ok := block["source"].(map[string]any)
			if !ok {
				continue
			}
			sourceType, _ := source["type"].(string)
			if strings.TrimSpace(sourceType) != "url" {
				continue
			}
			rawURL, _ := source["url"].(string)
			visit(nativeClaudeURLSource{kind: strings.TrimSpace(blockType), path: blockPath, rawURL: rawURL, block: block, source: source})
		case ContentTypeToolResult:
			walkNativeClaudeContent(block["content"], blockPath+".content", visit)
		}
	}
}

func setNativeClaudeBase64Source(ref nativeClaudeURLSource, mimeType string, body []byte) {
	ref.block["source"] = map[string]any{
		"type":       "base64",
		"media_type": mimeType,
		"data":       base64.StdEncoding.EncodeToString(body),
	}
}
