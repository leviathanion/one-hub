package base

import (
	"bytes"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"one-api/types"
)

type recordingRemoteMediaFetcher struct {
	bodies map[string][]byte
	calls  map[string]int
	err    error
}

func (f *recordingRemoteMediaFetcher) Fetch(rawURL string) (string, []byte, error) {
	if f.calls == nil {
		f.calls = make(map[string]int)
	}
	f.calls[rawURL]++
	if f.err != nil {
		return "", nil, f.err
	}
	return "image/png", append([]byte(nil), f.bodies[rawURL]...), nil
}

func chatMediaRequest(urls ...string) *types.ChatCompletionRequest {
	parts := make([]types.ChatMessagePart, 0, len(urls))
	for _, rawURL := range urls {
		parts = append(parts, types.ChatMessagePart{
			Type:     types.ContentTypeImageURL,
			ImageURL: &types.ChatMessageImageURL{URL: rawURL},
		})
	}
	return &types.ChatCompletionRequest{Messages: []types.ChatCompletionMessage{{Role: types.ChatMessageRoleUser, Content: parts}}}
}

func TestSummarizeChatRemoteMediaRejectsUnsupportedLocatorAndCount(t *testing.T) {
	if _, err := SummarizeChatRemoteMedia(chatMediaRequest("ftp://example.com/a.png")); err == nil {
		t.Fatal("expected non-http remote media to fail local admission")
	}
	urls := make([]string, MaxChatRemoteMediaItems+1)
	for index := range urls {
		urls[index] = "https://example.com/a.png"
	}
	if _, err := SummarizeChatRemoteMedia(chatMediaRequest(urls...)); err == nil || !strings.Contains(err.Error(), "more than 16") {
		t.Fatalf("expected media count failure, got %v", err)
	}
}

func TestMaterializeChatRemoteMediaFetchesEachRemoteExactlyOnce(t *testing.T) {
	local := "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("local"))
	request := chatMediaRequest("https://example.com/a.png", local, "https://example.com/b.png")
	fetcher := &recordingRemoteMediaFetcher{bodies: map[string][]byte{
		"https://example.com/a.png": []byte("first"),
		"https://example.com/b.png": []byte("second"),
	}}
	if err := MaterializeChatRemoteMedia(request, fetcher); err != nil {
		t.Fatal(err)
	}
	if fetcher.calls["https://example.com/a.png"] != 1 || fetcher.calls["https://example.com/b.png"] != 1 || len(fetcher.calls) != 2 {
		t.Fatalf("unexpected fetches: %#v", fetcher.calls)
	}
	parts := request.Messages[0].ParseContent()
	for index, part := range parts {
		if part.ImageURL == nil || !strings.HasPrefix(part.ImageURL.URL, "data:image/png;base64,") {
			t.Fatalf("part %d was not a data URI: %#v", index, part)
		}
	}
	if parts[1].ImageURL.URL != local {
		t.Fatalf("local data URI changed: %q", parts[1].ImageURL.URL)
	}
}

func TestMaterializeChatRemoteMediaValidatesDataBeforeNetwork(t *testing.T) {
	request := chatMediaRequest("https://example.com/a.png", "data:image/png;base64,not-valid!")
	fetcher := &recordingRemoteMediaFetcher{bodies: map[string][]byte{"https://example.com/a.png": []byte("image")}}
	if err := MaterializeChatRemoteMedia(request, fetcher); err == nil {
		t.Fatal("expected invalid data URI to fail")
	}
	if len(fetcher.calls) != 0 {
		t.Fatalf("network started before local validation: %#v", fetcher.calls)
	}
}

func TestValidateChatRemoteMediaNeverFetchesAndEnforcesRasterDataURI(t *testing.T) {
	request := chatMediaRequest("https://example.com/a.png", "data:image/png;base64,"+base64.StdEncoding.EncodeToString([]byte("ok")))
	if err := ValidateChatRemoteMedia(request); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{
		"data:application/pdf;base64," + base64.StdEncoding.EncodeToString([]byte("pdf")),
		"data:image/png,AAAA",
		"data:image/png;base64,AA A=",
	} {
		if err := ValidateChatRemoteMedia(chatMediaRequest(raw)); err == nil {
			t.Fatalf("expected %q to fail", raw)
		}
	}
}

func TestDecodeBase64BoundedPreflightsSizeAndMatchesDataURI(t *testing.T) {
	for _, test := range []struct {
		name  string
		input string
		limit int
		want  string
	}{
		{name: "exact decoded boundary", input: "YWJj", limit: 3, want: "abc"},
		{name: "padded one byte", input: "YQ==", limit: 1, want: "a"},
		{name: "padded two bytes", input: "YWI=", limit: 2, want: "ab"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := DecodeBase64Bounded(test.input, test.limit)
			if err != nil || string(got) != test.want {
				t.Fatalf("bounded base64=%q err=%v, want %q", got, err, test.want)
			}
		})
	}

	large := strings.Repeat("A", 8)
	if _, err := DecodeBase64Bounded(large, 3); !errors.Is(err, ErrBase64PayloadTooLarge) {
		t.Fatalf("large encoded payload error=%v, want bounded-size rejection", err)
	}
	largeDataURI := strings.Repeat("A", ((MaxChatRemoteMediaItemBytes+2)/3)*4+4)
	if _, _, err := DecodeChatMediaDataURI("data:image/png;base64," + largeDataURI); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("large data URI error=%v, want bounded-size rejection", err)
	}

	for _, payload := range []string{"YQ=", "YQ===", "AA=A", "YQ=\n="} {
		if _, err := DecodeBase64Bounded(payload, 3); err == nil {
			t.Fatalf("raw payload %q unexpectedly accepted", payload)
		}
		if _, _, err := DecodeChatMediaDataURI("data:image/png;base64," + payload); err == nil {
			t.Fatalf("data URI payload %q unexpectedly accepted", payload)
		}
	}

	raw, err := DecodeBase64Bounded("YQ==", 1)
	if err != nil {
		t.Fatal(err)
	}
	_, fromURI, err := DecodeChatMediaDataURI("data:image/png;base64,YQ==")
	if err != nil || !bytes.Equal(raw, fromURI) {
		t.Fatalf("raw/data URI bounded decode differs: raw=%q uri=%q err=%v", raw, fromURI, err)
	}
}

func TestMaterializeChatRemoteMediaEnforcesPerItemAndTotalBudgets(t *testing.T) {
	oversized := bytes.Repeat([]byte{'x'}, MaxChatRemoteMediaItemBytes+1)
	fetcher := &recordingRemoteMediaFetcher{bodies: map[string][]byte{"https://example.com/large.png": oversized}}
	if err := MaterializeChatRemoteMedia(chatMediaRequest("https://example.com/large.png"), fetcher); err == nil {
		t.Fatal("expected per-item budget failure")
	}

	chunk := bytes.Repeat([]byte{'x'}, 7<<20)
	fetcher = &recordingRemoteMediaFetcher{bodies: map[string][]byte{
		"https://example.com/a.png": chunk,
		"https://example.com/b.png": chunk,
		"https://example.com/c.png": chunk,
	}}
	err := MaterializeChatRemoteMedia(chatMediaRequest(
		"https://example.com/a.png",
		"https://example.com/b.png",
		"https://example.com/c.png",
	), fetcher)
	if err == nil || !strings.Contains(err.Error(), "total bytes") {
		t.Fatalf("expected aggregate budget failure, got %v", err)
	}
}

func TestMaterializeChatRemoteMediaPropagatesFetcherFailure(t *testing.T) {
	want := errors.New("blocked target")
	fetcher := &recordingRemoteMediaFetcher{err: want}
	err := MaterializeChatRemoteMedia(chatMediaRequest("https://example.com/a.png"), fetcher)
	if !errors.Is(err, want) {
		t.Fatalf("expected fetch failure, got %v", err)
	}
}
