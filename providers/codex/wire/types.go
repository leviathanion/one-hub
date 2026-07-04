package wire

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"one-api/common/requestctx"
	"time"
)

type HeaderSnapshot = requestctx.HeaderSnapshot
type FieldState = requestctx.FieldState

const (
	FieldMissing  = requestctx.FieldMissing
	FieldEmpty    = requestctx.FieldEmpty
	FieldPresent  = requestctx.FieldPresent
	FieldInvalid  = requestctx.FieldInvalid
	FieldMultiple = requestctx.FieldMultiple
)

type Operation string

const (
	OpResponsesCreate  Operation = "responses.create.http"
	OpResponsesCompact Operation = "responses.compact.http"
	OpResponsesWSOpen  Operation = "responses.ws.open"
)

type Source string

const (
	SourceClientHeader  Source = "client_header"
	SourceBodyMetadata  Source = "body_client_metadata"
	SourceFrameMetadata Source = "frame_client_metadata"
	SourceChannel       Source = "channel_policy"
	SourceGenerated     Source = "proxy_generated"
	SourceCredential    Source = "channel_credential"
	SourceProtocol      Source = "protocol"
	SourceModel         Source = "model_capability"
)

type Clock interface {
	Now() time.Time
}

type RealClock struct{}

func (RealClock) Now() time.Time { return time.Now() }

type ChannelPolicy struct {
	FedRAMP                     bool
	Residency                   string
	DefaultOriginator           string
	TrustClientAttestation      bool
	GenerateProxyInstallationID bool
	ResponsesLite               bool
}

type Credential struct {
	AccessToken string
	AccountID   string
}

type PrincipalFingerprint struct {
	Kind string
	HMAC string
}

type HeaderEntry struct {
	Name  string
	Value string
}

type Decision struct {
	Name      string
	Action    string
	Source    Source
	Reason    string
	ValueLen  int
	ValueHash string
}

type HeaderPlan struct {
	Entries   []HeaderEntry
	Decisions []Decision
}

func (p HeaderPlan) HTTPHeader() http.Header {
	headers := make(http.Header, len(p.Entries))
	for _, entry := range p.Entries {
		if entry.Name == "" {
			continue
		}
		headers.Set(entry.Name, entry.Value)
	}
	return headers
}

func (p HeaderPlan) Map() map[string]string {
	headers := make(map[string]string, len(p.Entries))
	for _, entry := range p.Entries {
		if entry.Name == "" {
			continue
		}
		headers[entry.Name] = entry.Value
	}
	return headers
}

func valueDecision(name, action string, source Source, reason, value string) Decision {
	decision := Decision{
		Name:     name,
		Action:   action,
		Source:   source,
		Reason:   reason,
		ValueLen: len(value),
	}
	if value != "" {
		sum := sha256.Sum256([]byte(value))
		decision.ValueHash = "sha256:" + hex.EncodeToString(sum[:])
	}
	return decision
}
