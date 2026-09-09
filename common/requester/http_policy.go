package requester

import (
	"net/http"
	"strings"
	"time"
)

// HTTPProfile selects one code-owned I/O policy. Business acknowledgement
// parsing and admission limits remain with the caller/adapter.
type HTTPProfile string

const (
	HTTPProfileWorkAction     HTTPProfile = "work_action"
	HTTPProfileObservationGET HTTPProfile = "observation_get"
	HTTPProfileLongStream     HTTPProfile = "long_stream"
	HTTPProfileNotification   HTTPProfile = "notification"
)

const (
	longStreamResponseHeaderTimeout = 30 * time.Second
	longStreamBodyIdleTimeout       = 2 * time.Minute
	longStreamMaxLifetime           = time.Hour
)

type redirectMode uint8

const (
	redirectReject redirectMode = iota
	redirectReadOnly
)

type httpPolicy struct {
	redirect              redirectMode
	maxRedirects          int
	overrideTimeout       bool
	overallTimeout        time.Duration
	responseHeaderTimeout time.Duration
	bodyIdleTimeout       time.Duration
	maxLifetime           time.Duration
}

func policyForHTTPProfile(profile HTTPProfile) httpPolicy {
	switch profile {
	case HTTPProfileObservationGET:
		return httpPolicy{
			redirect:        redirectReadOnly,
			maxRedirects:    3,
			overrideTimeout: true,
			overallTimeout:  30 * time.Second,
		}
	case HTTPProfileLongStream:
		return httpPolicy{
			redirect:              redirectReject,
			overrideTimeout:       true,
			responseHeaderTimeout: longStreamResponseHeaderTimeout,
			bodyIdleTimeout:       longStreamBodyIdleTimeout,
			maxLifetime:           longStreamMaxLifetime,
		}
	case HTTPProfileNotification:
		return httpPolicy{
			redirect:        redirectReject,
			overrideTimeout: true,
			overallTimeout:  10 * time.Second,
		}
	default:
		return httpPolicy{redirect: redirectReject}
	}
}

func checkRedirectForPolicy(policy httpPolicy) func(*http.Request, []*http.Request) error {
	if policy.redirect != redirectReadOnly {
		return providerRedirectPolicy
	}
	return func(next *http.Request, via []*http.Request) error {
		if next == nil || len(via) == 0 || len(via) > policy.maxRedirects {
			return http.ErrUseLastResponse
		}
		previous := via[len(via)-1]
		if previous == nil || previous.URL == nil || next.URL == nil {
			return http.ErrUseLastResponse
		}
		if previous.Method != http.MethodGet && previous.Method != http.MethodHead {
			return http.ErrUseLastResponse
		}
		if next.Method != http.MethodGet && next.Method != http.MethodHead {
			return http.ErrUseLastResponse
		}
		if next.URL.User != nil || (previous.URL.Scheme == "https" && next.URL.Scheme != "https") {
			return http.ErrUseLastResponse
		}
		if !sameHTTPAuthority(previous, next) {
			stripRedirectCredentials(next)
		}
		return nil
	}
}

func sameHTTPAuthority(first, second *http.Request) bool {
	if first == nil || second == nil || first.URL == nil || second.URL == nil {
		return false
	}
	return strings.EqualFold(first.URL.Scheme, second.URL.Scheme) &&
		strings.EqualFold(first.URL.Host, second.URL.Host)
}

func stripRedirectCredentials(req *http.Request) {
	if req == nil {
		return
	}
	for _, name := range []string{
		"Authorization",
		"Cookie",
		"Proxy-Authorization",
		"X-Api-Key",
		"Api-Key",
	} {
		req.Header.Del(name)
	}
}
