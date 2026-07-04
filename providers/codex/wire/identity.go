package wire

import (
	"fmt"
	"runtime"
	"strings"

	"github.com/google/uuid"
)

const DialectVersion = "2026-06-29"

func DefaultUserAgent() string {
	return fmt.Sprintf("codex_cli_rs/%s (%s; %s) one-hub", DialectVersion, runtime.GOOS, runtime.GOARCH)
}

type Identity struct {
	UserAgent       string
	Originator      string
	SessionID       string
	ThreadID        string
	ClientRequestID string
	WindowID        string
	InstallationID  string

	TurnMetadata    string
	ParentThreadID  string
	Subagent        string
	MemgenRequest   string
	TurnState       string
	ResponsesLite   string
	InferenceCallID string

	Sources map[string]Source
}

type IdentityInput struct {
	Operation Operation
	Headers   HeaderSnapshot
	Metadata  Metadata
	Policy    ChannelPolicy
	Principal PrincipalFingerprint
	ChannelID int
	Clock     Clock
}

func ResolveIdentity(in IdentityInput) (Identity, []Decision, error) {
	if err := validateSingletonHeaders(in.Headers); err != nil {
		return Identity{}, nil, err
	}
	if err := validateMetadata(in.Metadata); err != nil {
		return Identity{}, nil, err
	}
	if !in.Policy.TrustClientAttestation && in.Headers.Singleton("x-oai-attestation", validAttestation).State == FieldPresent {
		return Identity{}, nil, reject("x-oai-attestation", "client attestation is not trusted for this channel")
	}

	identity := Identity{Sources: make(map[string]Source)}
	decisions := make([]Decision, 0, 16)
	var err error

	identity.UserAgent = headerOrDefault(in.Headers, "User-Agent", validUserAgent, DefaultUserAgent(), SourceProtocol, &decisions, identity.Sources)
	defaultOriginator := strings.TrimSpace(in.Policy.DefaultOriginator)
	if defaultOriginator == "" {
		defaultOriginator = "codex_cli_rs"
	}
	if !validOriginator(defaultOriginator) {
		return Identity{}, nil, reject("originator", "default originator is invalid")
	}
	identity.Originator = headerOrDefault(in.Headers, "originator", validOriginator, defaultOriginator, SourceChannel, &decisions, identity.Sources)

	identity.SessionID = headerMetadataOrGenerated(in.Headers, in.Metadata, "session-id", "session_id", validID, &decisions, identity.Sources)
	identity.ThreadID = headerMetadataOrGenerated(in.Headers, in.Metadata, "thread-id", "thread_id", validID, &decisions, identity.Sources)
	identity.ClientRequestID = headerOrFallback(in.Headers, "x-client-request-id", validID, identity.ThreadID, "thread-id", &decisions, identity.Sources)
	if identity.WindowID, err = headerMetadataOptional(in.Headers, in.Metadata, "x-codex-window-id", "x-codex-window-id", validID, &decisions, identity.Sources); err != nil {
		return Identity{}, nil, err
	}
	if identity.InstallationID, err = resolveInstallationID(in, identity.SessionID, &decisions, identity.Sources); err != nil {
		return Identity{}, nil, err
	}

	if identity.TurnMetadata, err = headerMetadataOptional(in.Headers, in.Metadata, "x-codex-turn-metadata", "x-codex-turn-metadata", validTurnMetadata, &decisions, identity.Sources); err != nil {
		return Identity{}, nil, err
	}
	if identity.ParentThreadID, err = headerMetadataOptional(in.Headers, in.Metadata, "x-codex-parent-thread-id", "x-codex-parent-thread-id", validID, &decisions, identity.Sources); err != nil {
		return Identity{}, nil, err
	}
	if identity.Subagent, err = headerMetadataOptional(in.Headers, in.Metadata, "x-openai-subagent", "x-openai-subagent", validSubagent, &decisions, identity.Sources); err != nil {
		return Identity{}, nil, err
	}
	identity.MemgenRequest = headerOptional(in.Headers, "x-openai-memgen-request", validBoolString, &decisions, identity.Sources)
	identity.TurnState = headerOptional(in.Headers, "x-codex-turn-state", validTurnState, &decisions, identity.Sources)
	identity.ResponsesLite = headerOptional(in.Headers, "x-openai-internal-codex-responses-lite", validBoolString, &decisions, identity.Sources)
	identity.InferenceCallID = headerOptional(in.Headers, "x-codex-inference-call-id", validID, &decisions, identity.Sources)
	return identity, decisions, nil
}

func headerOrDefault(headers HeaderSnapshot, name string, valid func(string) bool, fallback string, fallbackSource Source, decisions *[]Decision, sources map[string]Source) string {
	header := headers.Singleton(name, valid)
	if header.State == FieldPresent {
		*decisions = append(*decisions, valueDecision(name, "copy", SourceClientHeader, "client-header-present", header.Value))
		sources[name] = SourceClientHeader
		return header.Value
	}
	*decisions = append(*decisions, valueDecision(name, "fallback", fallbackSource, "client-header-missing-or-empty", fallback))
	sources[name] = fallbackSource
	return fallback
}

func headerOrFallback(headers HeaderSnapshot, name string, valid func(string) bool, fallback, fallbackReason string, decisions *[]Decision, sources map[string]Source) string {
	header := headers.Singleton(name, valid)
	if header.State == FieldPresent {
		*decisions = append(*decisions, valueDecision(name, "copy", SourceClientHeader, "client-header-present", header.Value))
		sources[name] = SourceClientHeader
		return header.Value
	}
	*decisions = append(*decisions, valueDecision(name, "fallback", SourceGenerated, fallbackReason, fallback))
	sources[name] = SourceGenerated
	return fallback
}

func headerMetadataOrGenerated(headers HeaderSnapshot, metadata Metadata, headerName, metadataName string, valid func(string) bool, decisions *[]Decision, sources map[string]Source) string {
	header := headers.Singleton(headerName, valid)
	if header.State == FieldPresent {
		*decisions = append(*decisions, valueDecision(headerName, "copy", SourceClientHeader, "client-header-present", header.Value))
		sources[headerName] = SourceClientHeader
		return header.Value
	}
	if value, state, _ := metadata.String(metadataName, valid); state == FieldPresent {
		*decisions = append(*decisions, valueDecision(headerName, "fallback", metadata.Source, "client-header-missing-or-empty", value))
		sources[headerName] = metadata.Source
		return value
	}
	value := uuid.NewString()
	*decisions = append(*decisions, valueDecision(headerName, "fallback", SourceGenerated, "identity-missing", value))
	sources[headerName] = SourceGenerated
	return value
}

func headerMetadataOptional(headers HeaderSnapshot, metadata Metadata, headerName, metadataName string, valid func(string) bool, decisions *[]Decision, sources map[string]Source) (string, error) {
	header := headers.Singleton(headerName, valid)
	if header.State == FieldPresent {
		*decisions = append(*decisions, valueDecision(headerName, "copy", SourceClientHeader, "client-header-present", header.Value))
		sources[headerName] = SourceClientHeader
		return header.Value, nil
	}
	value, state, err := metadata.String(metadataName, valid)
	if err != nil {
		return "", err
	}
	if state == FieldInvalid {
		return "", reject("client_metadata."+metadataName, "metadata value is invalid")
	}
	if state == FieldPresent {
		*decisions = append(*decisions, valueDecision(headerName, "copy", metadata.Source, "metadata-present", value))
		sources[headerName] = metadata.Source
		return value, nil
	}
	*decisions = append(*decisions, valueDecision(headerName, "omit", "", "missing", ""))
	return "", nil
}

func headerOptional(headers HeaderSnapshot, name string, valid func(string) bool, decisions *[]Decision, sources map[string]Source) string {
	header := headers.Singleton(name, valid)
	if header.State == FieldPresent {
		*decisions = append(*decisions, valueDecision(name, "copy", SourceClientHeader, "client-header-present", header.Value))
		sources[name] = SourceClientHeader
		return header.Value
	}
	*decisions = append(*decisions, valueDecision(name, "omit", "", "missing", ""))
	return ""
}

func resolveInstallationID(in IdentityInput, sessionID string, decisions *[]Decision, sources map[string]Source) (string, error) {
	header := in.Headers.Singleton("x-codex-installation-id", validInstallationID)
	bodyValue, bodyState, err := in.Metadata.String("x-codex-installation-id", validInstallationID)
	if err != nil {
		return "", err
	}
	if bodyState == FieldInvalid {
		return "", reject("client_metadata.x-codex-installation-id", "metadata value is invalid")
	}
	switch in.Operation {
	case OpResponsesCreate:
		if bodyState == FieldPresent {
			*decisions = append(*decisions, valueDecision("x-codex-installation-id", "copy", in.Metadata.Source, "metadata-present", bodyValue))
			sources["x-codex-installation-id"] = in.Metadata.Source
			return bodyValue, nil
		}
		if header.State == FieldPresent {
			*decisions = append(*decisions, valueDecision("x-codex-installation-id", "copy", SourceClientHeader, "client-header-present", header.Value))
			sources["x-codex-installation-id"] = SourceClientHeader
			return header.Value, nil
		}
		*decisions = append(*decisions, valueDecision("x-codex-installation-id", "omit", "", "missing", ""))
		return "", nil
	case OpResponsesCompact:
		if header.State == FieldPresent {
			*decisions = append(*decisions, valueDecision("x-codex-installation-id", "copy", SourceClientHeader, "client-header-present", header.Value))
			sources["x-codex-installation-id"] = SourceClientHeader
			return header.Value, nil
		}
		if bodyState == FieldPresent {
			*decisions = append(*decisions, valueDecision("x-codex-installation-id", "fallback", in.Metadata.Source, "client-header-missing-or-empty", bodyValue))
			sources["x-codex-installation-id"] = in.Metadata.Source
			return bodyValue, nil
		}
	case OpResponsesWSOpen:
		if bodyState == FieldPresent {
			*decisions = append(*decisions, valueDecision("x-codex-installation-id", "copy", in.Metadata.Source, "metadata-present", bodyValue))
			sources["x-codex-installation-id"] = in.Metadata.Source
			return bodyValue, nil
		}
	}
	if in.Policy.GenerateProxyInstallationID {
		value := GenerateProxyInstallationID(in.ChannelID, in.Principal, sessionID)
		*decisions = append(*decisions, valueDecision("x-codex-installation-id", "fallback", SourceGenerated, "proxy-generated", value))
		sources["x-codex-installation-id"] = SourceGenerated
		return value, nil
	}
	*decisions = append(*decisions, valueDecision("x-codex-installation-id", "omit", "", "missing", ""))
	return "", nil
}

func GenerateProxyInstallationID(channelID int, principal PrincipalFingerprint, sessionID string) string {
	namespace := uuid.NewSHA1(uuid.NameSpaceOID, []byte(fmt.Sprintf("one-hub:codex-installation:%d", channelID)))
	name := strings.TrimSpace(principal.Kind) + ":" + strings.TrimSpace(principal.HMAC) + ":" + strings.TrimSpace(sessionID)
	return uuid.NewSHA1(namespace, []byte(name)).String()
}
