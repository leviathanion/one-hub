package requester

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestHTTPProfilesKeepWorkActionsNoRedirectAndBoundAuxiliaryCalls(t *testing.T) {
	original := HTTPClient
	HTTPClient = &http.Client{Timeout: 2 * time.Minute}
	t.Cleanup(func() { HTTPClient = original })

	workClient, apiErr := providerHTTPClient(HTTPProfileWorkAction, false)
	if apiErr != nil {
		t.Fatalf("work client: %v", apiErr)
	}
	initial, _ := http.NewRequest(http.MethodPost, "https://provider.example/v1/responses", nil)
	next, _ := http.NewRequest(http.MethodPost, "https://provider.example/v1/other", nil)
	if err := workClient.CheckRedirect(next, []*http.Request{initial}); !errors.Is(err, http.ErrUseLastResponse) {
		t.Fatalf("work action followed redirect: %v", err)
	}
	if workClient.Timeout != 2*time.Minute {
		t.Fatalf("work action should preserve configured relay timeout, got %s", workClient.Timeout)
	}

	notificationClient, apiErr := providerHTTPClient(HTTPProfileNotification, false)
	if apiErr != nil {
		t.Fatalf("notification client: %v", apiErr)
	}
	if notificationClient.Timeout != 10*time.Second {
		t.Fatalf("notification timeout=%s, want 10s", notificationClient.Timeout)
	}

	streamClient, apiErr := providerHTTPClient(HTTPProfileLongStream, false)
	if apiErr != nil {
		t.Fatalf("long stream client: %v", apiErr)
	}
	if streamClient.Timeout != 0 {
		t.Fatalf("long stream inherited global overall timeout: %s", streamClient.Timeout)
	}
}

func TestHTTPRequesterLongStreamViewDoesNotInheritBaseOverallTimeout(t *testing.T) {
	original := HTTPClient
	HTTPClient = &http.Client{
		Timeout: 10 * time.Millisecond,
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			select {
			case <-time.After(30 * time.Millisecond):
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
					Request:    req,
				}, nil
			case <-req.Context().Done():
				return nil, req.Context().Err()
			}
		}),
	}
	t.Cleanup(func() { HTTPClient = original })

	base := NewHTTPRequester("", nil)
	baseRequest, _ := base.NewRequest(http.MethodPost, "https://provider.example/v1/responses", base.WithBody([]byte("{}")))
	if response, apiErr := base.SendRequestRaw(baseRequest); response != nil || apiErr == nil {
		t.Fatalf("base WorkAction unexpectedly escaped its overall timeout: response=%v err=%+v", response, apiErr)
	}

	stream := base.ForHTTPProfile(HTTPProfileLongStream)
	streamRequest, _ := stream.NewRequest(http.MethodPost, "https://provider.example/v1/responses", stream.WithBody([]byte("{}")))
	response, apiErr := stream.SendRequestRaw(streamRequest)
	if apiErr != nil || response == nil || response.StatusCode != http.StatusOK {
		t.Fatalf("long stream inherited base overall timeout: response=%v err=%+v", response, apiErr)
	}
	_ = response.Body.Close()
	if base.profile != HTTPProfileWorkAction {
		t.Fatalf("long stream send mutated base requester profile: %q", base.profile)
	}
}

func TestLongStreamProfileBoundsResponseHeaderWait(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		select {
		case <-req.Context().Done():
		case <-time.After(time.Second):
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	policy := policyForHTTPProfile(HTTPProfileLongStream)
	policy.responseHeaderTimeout = 20 * time.Millisecond
	policy.maxLifetime = time.Second
	req, _ := http.NewRequest(http.MethodPost, server.URL, nil)
	started := time.Now()
	transport, ok := server.Client().Transport.(*http.Transport)
	if !ok {
		t.Fatalf("server transport=%T, want *http.Transport", server.Client().Transport)
	}
	transports := newProviderHTTPTransportSet(transport)
	resp, err, wroteRequest := doHTTPRequest(server.Client(), req, policy, transports.noKeepAlive)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err == nil {
		t.Fatal("long stream accepted an unbounded response-header wait")
	}
	if !wroteRequest {
		t.Fatal("response-header timeout fired before the request was written")
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("response-header timeout took %s", elapsed)
	}
}

func TestLongStreamProfileBoundsIdleResponseBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-req.Context().Done()
	}))
	defer server.Close()

	policy := policyForHTTPProfile(HTTPProfileLongStream)
	policy.responseHeaderTimeout = time.Second
	policy.bodyIdleTimeout = 20 * time.Millisecond
	policy.maxLifetime = time.Second
	req, _ := http.NewRequest(http.MethodGet, server.URL, nil)
	resp, err, _ := doHTTPRequest(server.Client(), req, policy)
	if err != nil {
		t.Fatalf("open idle response: %v", err)
	}
	defer resp.Body.Close()

	started := time.Now()
	if _, err := resp.Body.Read(make([]byte, 1)); err == nil {
		t.Fatal("idle response body read unexpectedly succeeded")
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("idle body timeout took %s", elapsed)
	}
}

func TestLongStreamProfileBoundsActiveResponseLifetime(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		flusher := w.(http.Flusher)
		w.WriteHeader(http.StatusOK)
		flusher.Flush()
		ticker := time.NewTicker(5 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-req.Context().Done():
				return
			case <-ticker.C:
				_, _ = w.Write([]byte("x"))
				flusher.Flush()
			}
		}
	}))
	defer server.Close()

	policy := policyForHTTPProfile(HTTPProfileLongStream)
	policy.responseHeaderTimeout = time.Second
	policy.bodyIdleTimeout = 200 * time.Millisecond
	policy.maxLifetime = 30 * time.Millisecond
	req, _ := http.NewRequest(http.MethodGet, server.URL, nil)
	resp, err, _ := doHTTPRequest(server.Client(), req, policy)
	if err != nil {
		t.Fatalf("open active response: %v", err)
	}
	defer resp.Body.Close()

	started := time.Now()
	if _, err := io.Copy(io.Discard, resp.Body); err == nil {
		t.Fatal("active response body outlived its maximum lifetime")
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("maximum lifetime took %s", elapsed)
	}
}

func TestHTTPRequesterConcurrentProfileViewsDoNotMutateBase(t *testing.T) {
	base := NewHTTPRequester("", nil)
	longStream := base.ForHTTPProfile(HTTPProfileLongStream)
	if longStream == nil || longStream == base {
		t.Fatal("expected an independent requester profile view")
	}
	if base.profile != HTTPProfileWorkAction || longStream.profile != HTTPProfileLongStream {
		t.Fatalf("profile view mutated its base: base=%q stream=%q", base.profile, longStream.profile)
	}

	const workers = 32
	var wg sync.WaitGroup
	wg.Add(workers)
	for index := 0; index < workers; index++ {
		go func(index int) {
			defer wg.Done()
			profile := HTTPProfileWorkAction
			if index%2 == 0 {
				profile = HTTPProfileLongStream
			}
			view := base.ForHTTPProfile(profile)
			if view == nil || view.profile != profile || view.Context == nil {
				t.Errorf("invalid requester view: %+v", view)
			}
		}(index)
	}
	wg.Wait()
	if base.profile != HTTPProfileWorkAction {
		t.Fatalf("concurrent views mutated base profile: %q", base.profile)
	}
}

func TestObservationRedirectPolicyOnlyFollowsCredentialFreeReads(t *testing.T) {
	policy := policyForHTTPProfile(HTTPProfileObservationGET)
	check := checkRedirectForPolicy(policy)

	previous, _ := http.NewRequest(http.MethodGet, "http://search.example/query?q=one", nil)
	next, _ := http.NewRequest(http.MethodGet, "https://canonical.example/query?q=one", nil)
	next.Header.Set("Authorization", "Bearer secret")
	next.Header.Set("Cookie", "session=secret")
	next.Header.Set("X-Api-Key", "secret")
	if err := check(next, []*http.Request{previous}); err != nil {
		t.Fatalf("safe observation redirect rejected: %v", err)
	}
	for _, name := range []string{"Authorization", "Cookie", "X-Api-Key"} {
		if got := next.Header.Get(name); got != "" {
			t.Fatalf("cross-authority redirect retained %s=%q", name, got)
		}
	}

	post, _ := http.NewRequest(http.MethodPost, "https://canonical.example/query", nil)
	if err := check(post, []*http.Request{previous}); !errors.Is(err, http.ErrUseLastResponse) {
		t.Fatalf("observation profile followed non-read redirect: %v", err)
	}

	downgradeFrom, _ := http.NewRequest(http.MethodGet, "https://search.example/query", nil)
	downgradeTo, _ := http.NewRequest(http.MethodGet, "http://search.example/query", nil)
	if err := check(downgradeTo, []*http.Request{downgradeFrom}); !errors.Is(err, http.ErrUseLastResponse) {
		t.Fatalf("observation profile followed HTTPS downgrade: %v", err)
	}
}
