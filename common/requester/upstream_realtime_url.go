package requester

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/idna"
)

const upstreamRealtimeURLLookupTimeout = 2 * time.Second

var (
	ErrUpstreamRealtimeURLInvalid       = errors.New("upstream realtime websocket url is invalid")
	ErrUpstreamRealtimeURLUnsupported   = errors.New("unsupported upstream realtime websocket url scheme")
	ErrUpstreamRealtimeURLRequiresWSS   = errors.New("upstream realtime websocket requires wss")
	ErrUpstreamRealtimeURLHostBlocked   = errors.New("upstream realtime websocket host is not allowed")
	ErrUpstreamRealtimeURLResolveFailed = errors.New("upstream realtime websocket host could not be resolved")

	ErrUpstreamResponsesHTTPURLInvalid       = errors.New("upstream responses http url is invalid")
	ErrUpstreamResponsesHTTPURLUnsupported   = errors.New("unsupported upstream responses http url scheme")
	ErrUpstreamResponsesHTTPURLRequiresHTTPS = errors.New("upstream responses http bridge requires https")
	ErrUpstreamResponsesHTTPURLHostBlocked   = errors.New("upstream responses http bridge host is not allowed")
	ErrUpstreamResponsesHTTPURLResolveFailed = errors.New("upstream responses http bridge host could not be resolved")
)

type UpstreamRealtimeURLPolicy struct {
	// AllowSelfHosted permits explicit self-hosted upstreams to use ws/http and
	// private hosts. This is intentionally opt-in because realtime websocket
	// requests carry bearer credentials over a long-lived connection.
	AllowSelfHosted bool
	// ResolveHost enables DNS resolution and resolved-IP blocking. Callers using
	// an explicit proxy should leave this false because the proxy owns DNS
	// resolution; URL-layer checks still reject literal private hosts.
	ResolveHost bool
}

type UpstreamRealtimeURLValidationError struct {
	Err        error
	StatusCode int
}

func (e *UpstreamRealtimeURLValidationError) Error() string {
	if e == nil || e.Err == nil {
		return ErrUpstreamRealtimeURLInvalid.Error()
	}
	return e.Err.Error()
}

func (e *UpstreamRealtimeURLValidationError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func UpstreamRealtimeURLStatusCode(err error) int {
	var validationErr *UpstreamRealtimeURLValidationError
	if errors.As(err, &validationErr) && validationErr != nil && validationErr.StatusCode > 0 {
		return validationErr.StatusCode
	}
	return http.StatusBadRequest
}

func UpstreamResponsesHTTPURLStatusCode(err error) int {
	var validationErr *UpstreamResponsesHTTPURLValidationError
	if errors.As(err, &validationErr) && validationErr != nil && validationErr.StatusCode > 0 {
		return validationErr.StatusCode
	}
	return http.StatusBadRequest
}

type UpstreamResponsesHTTPURLValidationError struct {
	Err        error
	StatusCode int
}

func (e *UpstreamResponsesHTTPURLValidationError) Error() string {
	if e == nil || e.Err == nil {
		return ErrUpstreamResponsesHTTPURLInvalid.Error()
	}
	return e.Err.Error()
}

func (e *UpstreamResponsesHTTPURLValidationError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func ValidateUpstreamRealtimeURL(rawURL string, policy UpstreamRealtimeURLPolicy) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsed == nil || parsed.Hostname() == "" {
		return "", upstreamRealtimeURLValidationError(ErrUpstreamRealtimeURLInvalid, http.StatusInternalServerError)
	}

	switch strings.ToLower(parsed.Scheme) {
	case "https":
		parsed.Scheme = "wss"
	case "http":
		if !policy.AllowSelfHosted {
			return "", upstreamRealtimeURLValidationError(ErrUpstreamRealtimeURLRequiresWSS, http.StatusBadRequest)
		}
		parsed.Scheme = "ws"
	case "wss":
	case "ws":
		if !policy.AllowSelfHosted {
			return "", upstreamRealtimeURLValidationError(ErrUpstreamRealtimeURLRequiresWSS, http.StatusBadRequest)
		}
	default:
		return "", upstreamRealtimeURLValidationError(ErrUpstreamRealtimeURLUnsupported, http.StatusInternalServerError)
	}

	host := parsed.Hostname()
	policyHost, err := normalizeUpstreamRealtimePolicyHost(host)
	if err != nil {
		return "", upstreamRealtimeURLValidationError(ErrUpstreamRealtimeURLInvalid, http.StatusBadRequest)
	}
	if UpstreamRealtimeMetadataHostBlocked(policyHost) {
		return "", upstreamRealtimeURLValidationError(ErrUpstreamRealtimeURLHostBlocked, http.StatusBadRequest)
	}
	if !policy.AllowSelfHosted {
		if UpstreamRealtimeHostBlocked(policyHost) {
			return "", upstreamRealtimeURLValidationError(ErrUpstreamRealtimeURLHostBlocked, http.StatusBadRequest)
		}
		if policy.ResolveHost {
			blocked, err := upstreamRealtimeResolvedHostBlocked(policyHost)
			if err != nil {
				return "", upstreamRealtimeURLValidationError(ErrUpstreamRealtimeURLResolveFailed, http.StatusBadRequest)
			}
			if blocked {
				return "", upstreamRealtimeURLValidationError(ErrUpstreamRealtimeURLHostBlocked, http.StatusBadRequest)
			}
		}
	} else if policy.ResolveHost {
		blocked, err := upstreamRealtimeResolvedMetadataHostBlocked(policyHost)
		if err != nil {
			return "", upstreamRealtimeURLValidationError(ErrUpstreamRealtimeURLResolveFailed, http.StatusBadRequest)
		}
		if blocked {
			return "", upstreamRealtimeURLValidationError(ErrUpstreamRealtimeURLHostBlocked, http.StatusBadRequest)
		}
	}

	return parsed.String(), nil
}

func normalizeUpstreamRealtimePolicyHost(host string) (string, error) {
	host = strings.TrimSpace(host)
	if host == "" {
		return "", nil
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return host, nil
	}
	return idna.Lookup.ToASCII(host)
}

func upstreamRealtimeURLValidationError(err error, status int) error {
	return &UpstreamRealtimeURLValidationError{Err: err, StatusCode: status}
}

func upstreamResponsesHTTPURLValidationError(err error, status int) error {
	return &UpstreamResponsesHTTPURLValidationError{Err: err, StatusCode: status}
}

func UpstreamRealtimeHostBlocked(host string) bool {
	host = strings.TrimSuffix(strings.TrimSpace(strings.ToLower(host)), ".")
	if host == "" || host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		return UpstreamRealtimeIPBlocked(ip)
	}
	return false
}

func UpstreamRealtimeMetadataHostBlocked(host string) bool {
	host = strings.TrimSuffix(strings.TrimSpace(strings.ToLower(host)), ".")
	if host == "metadata.google.internal" {
		return true
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		return UpstreamRealtimeMetadataIPBlocked(ip)
	}
	return false
}

func UpstreamRealtimeIPBlocked(ip netip.Addr) bool {
	if !ip.IsValid() {
		return true
	}
	if ip.Is4In6() {
		ip = ip.Unmap()
	}
	return ip.IsLoopback() ||
		ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsPrivate() ||
		UpstreamRealtimeMetadataIPBlocked(ip)
}

func UpstreamRealtimeMetadataIPBlocked(ip netip.Addr) bool {
	if !ip.IsValid() {
		return false
	}
	if ip.Is4In6() {
		ip = ip.Unmap()
	}
	return ip == netip.MustParseAddr("169.254.169.254") ||
		ip == netip.MustParseAddr("100.100.100.200") ||
		ip == netip.MustParseAddr("fd00:ec2::254")
}

func upstreamRealtimeResolvedHostBlocked(host string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), upstreamRealtimeURLLookupTimeout)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return false, err
	}
	if len(addrs) == 0 {
		return false, net.ErrClosed
	}
	for _, addr := range addrs {
		if UpstreamRealtimeIPBlocked(addr) {
			return true, nil
		}
	}
	return false, nil
}

func upstreamRealtimeResolvedMetadataHostBlocked(host string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), upstreamRealtimeURLLookupTimeout)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return false, err
	}
	if len(addrs) == 0 {
		return false, net.ErrClosed
	}
	for _, addr := range addrs {
		if UpstreamRealtimeMetadataIPBlocked(addr) {
			return true, nil
		}
	}
	return false, nil
}
