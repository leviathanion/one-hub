package requester

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
)

type egressRedirectCookieJar struct {
	beforeSend func()
}

func (j egressRedirectCookieJar) Cookies(*url.URL) []*http.Cookie   { j.beforeSend(); return nil }
func (egressRedirectCookieJar) SetCookies(*url.URL, []*http.Cookie) {}

func TestEgressRedirectKeepsExitUntilFinalBodyCloses(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		calls.Add(1)
		if req.URL.Path != "/next" {
			http.Redirect(w, req, "/next", http.StatusFound)
			return
		}
		w.Header().Set("X-Redirect-Complete", "true")
		_, _ = io.WriteString(w, "redirect complete")
	}))
	defer upstream.Close()
	pool := setupEgressPoolClient(t, newProviderHTTPTransport(), 1)
	var sends int
	var competitorRejected bool
	HTTPClient.Jar = egressRedirectCookieJar{beforeSend: func() {
		sends++
		if sends != 2 {
			return
		}
		// net/http consults the cookie jar after closing the redirect body
		// and before sending the next hop. A different exit must not acquire
		// this Do's only slot during that interval.
		if got := egressTestReferences(pool); got != 1 {
			t.Fatalf("redirect released its exit between hops: references=%d", got)
		}
		resp, err := sendEgressTestRequest(t, "http://other-exit.invalid:8080", upstream.URL)
		if resp != nil {
			_ = resp.Body.Close()
		}
		competitorRejected = errors.Is(err, errProviderTransportCapacity)
		if !competitorRejected {
			t.Fatalf("competing exit should be rejected before dial: %v", err)
		}
	}}
	r := NewHTTPRequester("", nil).ForHTTPProfile(HTTPProfileObservationGET)
	req, err := r.NewRequest(http.MethodGet, upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := r.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if !competitorRejected || sends != 2 || calls.Load() != 2 || resp.StatusCode != http.StatusOK || resp.Header.Get("X-Redirect-Complete") != "true" {
		t.Fatalf("redirect failed: rejected=%t sends=%d calls=%d status=%d", competitorRejected, sends, calls.Load(), resp.StatusCode)
	}
	if got := egressTestReferences(pool); got != 1 {
		t.Fatalf("final open body lost its exit: references=%d", got)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if got := egressTestReferences(pool); got != 0 {
		t.Fatalf("final close did not release exit: references=%d", got)
	}
}
