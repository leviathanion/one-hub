package wire

import (
	"encoding/json"

	"one-api/common/jsonobject"

	"github.com/google/uuid"
)

const (
	clientMetadataSessionID      = "session_id"
	clientMetadataThreadID       = "thread_id"
	clientMetadataTurnID         = "turn_id"
	clientMetadataWindowID       = "x-codex-window-id"
	clientMetadataInstallationID = "x-codex-installation-id"
	clientMetadataTurnMetadata   = "x-codex-turn-metadata"
	turnMetadataInstallationID   = "installation_id"
	turnMetadataWindowID         = "window_id"
	turnMetadataRequestKind      = "request_kind"
	turnMetadataStartedAtUnixMS  = "turn_started_at_unix_ms"
	turnMetadataRequestKindTurn  = "turn"
)

type ProbeMetadataInput struct {
	ChannelID                  int
	Principal                  PrincipalFingerprint
	AutoGenerateInstallationID bool
	Clock                      Clock
}

func WithOfficialProbeMetadata(object *jsonobject.Object, in ProbeMetadataInput) (*jsonobject.Object, error) {
	if object == nil {
		return nil, reject("body", "request body is required")
	}
	clock := in.Clock
	if clock == nil {
		clock = RealClock{}
	}

	sessionID := uuid.NewString()
	threadID := uuid.NewString()
	turnID := uuid.NewString()
	windowID := uuid.NewString()

	installationID := ""
	if in.AutoGenerateInstallationID {
		installationID = GenerateProxyInstallationID(in.ChannelID, in.Principal, sessionID)
	}

	turnMetadata := map[string]any{
		clientMetadataSessionID:     sessionID,
		clientMetadataThreadID:      threadID,
		clientMetadataTurnID:        turnID,
		turnMetadataWindowID:        windowID,
		turnMetadataRequestKind:     turnMetadataRequestKindTurn,
		turnMetadataStartedAtUnixMS: clock.Now().UnixMilli(),
	}
	if installationID != "" {
		turnMetadata[turnMetadataInstallationID] = installationID
	}
	turnMetadataJSON, err := json.Marshal(turnMetadata)
	if err != nil {
		return nil, err
	}

	clientMetadata := map[string]string{
		clientMetadataSessionID:    sessionID,
		clientMetadataThreadID:     threadID,
		clientMetadataTurnID:       turnID,
		clientMetadataWindowID:     windowID,
		clientMetadataTurnMetadata: string(turnMetadataJSON),
	}
	if installationID != "" {
		clientMetadata[clientMetadataInstallationID] = installationID
	}

	updated := object.Clone()
	if err := updated.SetJSON("client_metadata", clientMetadata); err != nil {
		return nil, err
	}
	return updated, nil
}
