package wire

import "strings"

type HeaderPlanInput struct {
	Operation  Operation
	Headers    HeaderSnapshot
	Credential Credential
	Policy     ChannelPolicy
	Identity   Identity
}

func BuildHeaders(in HeaderPlanInput) (HeaderPlan, error) {
	if strings.TrimSpace(in.Credential.AccessToken) == "" {
		return HeaderPlan{}, reject("credential", "access token is required")
	}

	plan := HeaderPlan{Entries: make([]HeaderEntry, 0, 24)}
	add := func(name, value string, source Source, reason string) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		plan.Entries = append(plan.Entries, HeaderEntry{Name: name, Value: value})
		plan.Decisions = append(plan.Decisions, valueDecision(name, "set", source, reason, value))
	}

	add("Authorization", "Bearer "+strings.TrimSpace(in.Credential.AccessToken), SourceCredential, "channel-oauth-token")
	add("ChatGPT-Account-ID", in.Credential.AccountID, SourceCredential, "channel-account-id")
	add("User-Agent", in.Identity.UserAgent, sourceFor(in.Identity, "User-Agent", SourceProtocol), "resolved-identity")
	add("originator", in.Identity.Originator, sourceFor(in.Identity, "originator", SourceChannel), "resolved-identity")
	add("session-id", in.Identity.SessionID, sourceFor(in.Identity, "session-id", SourceGenerated), "resolved-identity")
	add("thread-id", in.Identity.ThreadID, sourceFor(in.Identity, "thread-id", SourceGenerated), "resolved-identity")
	add("x-client-request-id", in.Identity.ClientRequestID, sourceFor(in.Identity, "x-client-request-id", SourceGenerated), "resolved-identity")

	if in.Policy.FedRAMP {
		add("X-OpenAI-Fedramp", "true", SourceChannel, "channel-policy")
	}
	add("x-openai-internal-codex-residency", in.Policy.Residency, SourceChannel, "channel-policy")

	switch in.Operation {
	case OpResponsesCreate:
		add("Content-Type", "application/json", SourceProtocol, "http-json")
		add("Accept", "text/event-stream", SourceProtocol, "codex-streaming")
		addOptionalHeaders(&plan, in, outputHTTPCreate)
	case OpResponsesCompact:
		add("Content-Type", "application/json", SourceProtocol, "http-json")
		add("Accept", "application/json", SourceProtocol, "compact-json")
		addOptionalHeaders(&plan, in, outputHTTPCompact)
	case OpResponsesWSOpen:
		add("OpenAI-Beta", "responses_websockets=2026-02-06", SourceProtocol, "responses-websocket")
		addOptionalHeaders(&plan, in, outputWSHandshake)
	default:
		return HeaderPlan{}, reject("operation", "unsupported Codex operation %q", in.Operation)
	}

	return plan, nil
}

func addOptionalHeaders(plan *HeaderPlan, in HeaderPlanInput, output operationMask) {
	for _, spec := range fieldSpecs {
		if spec.output&output == 0 {
			continue
		}
		if spec.clientHeader {
			addClientOptional(plan, in.Headers, spec.header)
			continue
		}
		value := identityValue(in.Identity, spec.identity)
		source := sourceFor(in.Identity, spec.header, SourceClientHeader)
		if spec.identity == identityResponsesLite && value == "" && in.Policy.ResponsesLite {
			value = "true"
			source = SourceModel
		}
		addIdentityOptional(plan, spec.header, value, source)
	}
}

func identityValue(identity Identity, field identityField) string {
	switch field {
	case identityWindowID:
		return identity.WindowID
	case identityInstallationID:
		return identity.InstallationID
	case identityTurnMetadata:
		return identity.TurnMetadata
	case identityParentThreadID:
		return identity.ParentThreadID
	case identitySubagent:
		return identity.Subagent
	case identityMemgenRequest:
		return identity.MemgenRequest
	case identityTurnState:
		return identity.TurnState
	case identityResponsesLite:
		return identity.ResponsesLite
	case identityInferenceCallID:
		return identity.InferenceCallID
	default:
		return ""
	}
}

func addIdentityOptional(plan *HeaderPlan, name, value string, source Source) {
	if plan == nil {
		return
	}
	value = strings.TrimSpace(value)
	if value == "" {
		plan.Decisions = append(plan.Decisions, valueDecision(name, "omit", "", "missing", ""))
		return
	}
	plan.Entries = append(plan.Entries, HeaderEntry{Name: name, Value: value})
	plan.Decisions = append(plan.Decisions, valueDecision(name, "set", source, "resolved-identity", value))
}

func addClientOptional(plan *HeaderPlan, headers HeaderSnapshot, name string) {
	if plan == nil {
		return
	}
	header := headers.Singleton(name, singletonValidator(name))
	if header.State != FieldPresent {
		plan.Decisions = append(plan.Decisions, valueDecision(name, "omit", "", "missing", ""))
		return
	}
	plan.Entries = append(plan.Entries, HeaderEntry{Name: name, Value: header.Value})
	plan.Decisions = append(plan.Decisions, valueDecision(name, "set", SourceClientHeader, "client-header-present", header.Value))
}

func sourceFor(identity Identity, name string, fallback Source) Source {
	if identity.Sources == nil {
		return fallback
	}
	if source := identity.Sources[name]; source != "" {
		return source
	}
	return fallback
}
