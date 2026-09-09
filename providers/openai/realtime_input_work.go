package openai

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"one-api/common/config"
	runtimerealtime "one-api/runtime/realtime"
	"strings"
	"time"

	runtimesession "one-api/runtime/session"
	"one-api/types"
)

const (
	openAIRealtimePendingInputLimit    = 32
	openAIRealtimePendingSettingsLimit = 32
	openAIRealtimeRecentInputLimit     = 256
	// Client and provider acknowledgement IDs are retained for the session
	// lifetime so a late event can never be confused with a new update.
	openAIRealtimeSettingsIDLimit = 64
)

// 输入先取得本地工作身份。供应商 item 只绑定已有工作，结果事件不创建 owner。
type openAIRealtimeInputWork struct {
	id                     string
	submissionID           string
	clientEventID          string
	manualCommitEventID    string
	itemID                 string
	contentIndex           int
	models                 runtimesession.ModelBinding
	transcriptionModels    runtimesession.ModelBinding
	observer               runtimesession.TurnObserver
	startedAt              time.Time
	submitted              bool
	automaticResponse      bool
	automaticSourceSeen    bool
	manualCommitCandidate  bool
	manualCommitSent       bool
	providerCommitSeen     bool
	responseClaimed        bool
	transcriptionFinalized bool
}

type openAIRealtimeInputResult struct {
	WorkID string
	ItemID string
	Result runtimesession.TurnFinalizationResult
}

type openAIRealtimeSettingsUpdate struct {
	settings  *openAIRealtimeInputSettings
	eventID   string
	eventType string
	done      chan struct{}
	resolved  bool
	accepted  bool
}

type openAIRealtimeInputSettings struct {
	manual               bool
	manualSet            bool
	automaticDisabled    bool
	automaticDisabledSet bool
	transcription        *runtimesession.ModelBinding
	transcriptionSet     bool
}

func (s *openAIRealtimeSession) modelBinding() runtimesession.ModelBinding {
	models := s.models
	if models.RequestedModel == "" {
		models = runtimesession.ModelBinding{RequestedModel: s.model, ProviderModel: s.model, BillingModel: s.model}
	}
	return models
}

func (s *openAIRealtimeSession) checkFutureWork(models runtimesession.ModelBinding, count bool) error {
	s.mu.Lock()
	stopped := s.futureWorkErr
	s.mu.Unlock()
	if stopped != nil {
		return stopped
	}
	if s.workPolicy != nil {
		return s.workPolicy.CheckFutureWork(models, count)
	}
	return nil
}

func (s *openAIRealtimeSession) stopFutureWork(err error) {
	if err == nil {
		return
	}
	s.mu.Lock()
	if s.futureWorkErr == nil {
		s.futureWorkErr = err
	}
	s.mu.Unlock()
}

func (s *openAIRealtimeSession) observeFinalization(observer runtimesession.TurnObserver) error {
	result := runtimesession.TurnResult(observer)
	if !result.StopFutureWork && !result.Unsettled {
		return nil
	}
	err := result.Err
	if err == nil {
		err = errors.New("realtime billing settlement is unresolved")
	}
	s.stopFutureWork(err)
	return err
}

func (s *openAIRealtimeSession) runFinalizers(finalized []openAIRealtimeFinalizedTurn) {
	for _, current := range finalized {
		if current.observer == nil {
			continue
		}
		current.observer.FinalizeTurn(current.payload)
		s.observeFinalization(current.observer)
	}
}

// 会话模型在连接时绑定；客户端覆盖只能重复同一公开模型。
// 转写模型有独立公开命名空间，经 relay policy 授权与映射。
func (s *openAIRealtimeSession) prepareInputSettings(payload []byte, eventType string) ([]byte, *openAIRealtimeInputSettings, error) {
	var event map[string]json.RawMessage
	if json.Unmarshal(payload, &event) != nil {
		return payload, nil, nil
	}
	models := s.modelBinding()
	field := ""
	switch eventType {
	case "response.create":
		field = "response"
	case "session.update", "transcription_session.update":
		field = "session"
	default:
		return payload, nil, nil
	}
	var settings *openAIRealtimeInputSettings
	if eventType == "session.update" || eventType == "transcription_session.update" {
		if err := s.checkFutureWork(models, false); err != nil {
			return nil, nil, err
		}
		s.mu.Lock()
		settings = &openAIRealtimeInputSettings{
			manual:            s.manualAudioCommit,
			automaticDisabled: s.automaticFeaturesDisabled,
		}
		if s.transcriptionModels != nil {
			transcription := *s.transcriptionModels
			settings.transcription = &transcription
		}
		s.mu.Unlock()
	}
	topModelChanged := false
	if override := rawJSONString(event["model"]); override != "" {
		if override != models.RequestedModel {
			return nil, nil, newOpenAIRealtimeClientError("realtime_model_override_unsupported", "realtime model is bound to the requested public model")
		}
		event["model"], _ = json.Marshal(models.ProviderModel)
		topModelChanged = true
	}
	body, ok := rawJSONObject(event[field])
	if !ok {
		if topModelChanged {
			payload, _ = json.Marshal(event)
		}
		return payload, settings, nil
	}
	if override := rawJSONString(body["model"]); override != "" {
		if override != models.RequestedModel {
			return nil, nil, newOpenAIRealtimeClientError("realtime_model_override_unsupported", "realtime model is bound to the requested public model")
		}
		body["model"], _ = json.Marshal(models.ProviderModel)
	}
	if eventType == "response.create" {
		event[field], _ = json.Marshal(body)
		updated, _ := json.Marshal(event)
		return updated, nil, nil
	}
	audio, _ := rawJSONObject(body["audio"])
	input, _ := rawJSONObject(audio["input"])
	turnDetection, turnPresent := body["turn_detection"]
	if nested, present := input["turn_detection"]; present {
		turnDetection, turnPresent = nested, true
	}
	if turnPresent {
		settings.manual = bytes.Equal(bytes.TrimSpace(turnDetection), []byte("null"))
		settings.automaticDisabled, _ = openAIRealtimeAutomaticFeaturesDisabled(payload)
		settings.manualSet = true
		settings.automaticDisabledSet = true
	}
	transcription, transcriptionPresent := body["input_audio_transcription"]
	nestedTranscription := false
	if nested, present := input["transcription"]; present {
		transcription, transcriptionPresent, nestedTranscription = nested, true, true
	}
	if transcriptionPresent {
		settings.transcriptionSet = true
		if bytes.Equal(bytes.TrimSpace(transcription), []byte("null")) {
			settings.transcription = nil
		} else {
			config, valid := rawJSONObject(transcription)
			if !valid || rawJSONString(config["model"]) == "" {
				return nil, nil, newOpenAIRealtimeClientError("realtime_transcription_model_required", "transcription requires an explicit public model")
			}
			requested := rawJSONString(config["model"])
			binding := runtimesession.ModelBinding{RequestedModel: requested, ProviderModel: requested, BillingModel: requested}
			var err error
			if s.workPolicy != nil {
				binding, err = s.workPolicy.ResolveModel(requested)
			}
			if err != nil {
				return nil, nil, err
			}
			settings.transcription = &binding
			config["model"], _ = json.Marshal(binding.ProviderModel)
			transcription, _ = json.Marshal(config)
			if nestedTranscription {
				input["transcription"] = transcription
			} else {
				body["input_audio_transcription"] = transcription
			}
		}
	}
	if nestedTranscription {
		audio["input"], _ = json.Marshal(input)
		body["audio"], _ = json.Marshal(audio)
	}
	event[field], _ = json.Marshal(body)
	updated, _ := json.Marshal(event)
	return updated, settings, nil
}

func (s *openAIRealtimeSession) waitInputSettings(ctx context.Context) error {
	waitCtx, cancel := context.WithTimeout(ctx, config.RealtimeWebsocketWriteTimeout())
	defer cancel()
	for {
		s.mu.Lock()
		var pending *openAIRealtimeSettingsUpdate
		if len(s.pendingSettings) > 0 {
			pending = s.pendingSettings[0]
		}
		s.mu.Unlock()
		if pending == nil {
			return nil
		}
		select {
		case <-pending.done:
			continue
		case <-s.closed:
			return runtimerealtime.ErrSessionClosed
		case <-waitCtx.Done():
			err := newOpenAIRealtimeClientError("realtime_configuration_pending", "realtime session configuration was not acknowledged")
			s.stopFutureWork(err)
			return err
		}
	}
}

func (s *openAIRealtimeSession) beginInputSettings(settings *openAIRealtimeInputSettings, eventID, eventType string) *openAIRealtimeSettingsUpdate {
	if settings == nil {
		return nil
	}
	update := &openAIRealtimeSettingsUpdate{settings: settings, eventID: eventID, eventType: eventType, done: make(chan struct{})}
	s.mu.Lock()
	if len(s.pendingSettings) >= openAIRealtimePendingSettingsLimit {
		s.mu.Unlock()
		return nil
	}
	if s.settingsBeginRejectionLocked(eventID) != "" {
		s.mu.Unlock()
		return nil
	}
	if !s.rememberInputSettingsRequestIDLocked(eventID) {
		s.mu.Unlock()
		return nil
	}
	s.pendingSettings = append(s.pendingSettings, update)
	s.mu.Unlock()
	return update
}

func (s *openAIRealtimeSession) resolveInputSettings(update *openAIRealtimeSettingsUpdate, accepted bool) {
	if update == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resolveInputSettingsLocked(update, accepted)
}

func (s *openAIRealtimeSession) resolveInputSettingsLocked(update *openAIRealtimeSettingsUpdate, accepted bool) bool {
	if update == nil || update.resolved {
		return false
	}
	found := false
	for _, pending := range s.pendingSettings {
		if pending == update {
			found = true
			break
		}
	}
	if !found {
		return false
	}
	update.resolved = true
	update.accepted = accepted
	close(update.done)
	s.drainInputSettingsLocked()
	return true
}

func (s *openAIRealtimeSession) drainInputSettingsLocked() {
	for len(s.pendingSettings) > 0 {
		update := s.pendingSettings[0]
		if update == nil {
			s.pendingSettings = s.pendingSettings[1:]
			continue
		}
		if !update.resolved {
			return
		}
		if update.accepted {
			s.applyInputSettingsLocked(update.settings)
		}
		s.pendingSettings[0] = nil
		s.pendingSettings = s.pendingSettings[1:]
	}
}

func (s *openAIRealtimeSession) applyInputSettingsLocked(settings *openAIRealtimeInputSettings) {
	if settings == nil {
		return
	}
	if settings.manualSet {
		s.manualAudioCommit = settings.manual
	}
	if settings.automaticDisabledSet {
		s.automaticFeaturesDisabled = settings.automaticDisabled
	}
	if settings.transcriptionSet {
		if settings.transcription == nil {
			s.transcriptionModels = nil
		} else {
			transcription := *settings.transcription
			s.transcriptionModels = &transcription
		}
	}
}

func (s *openAIRealtimeSession) observeInputSettings(eventType string, payload []byte) bool {
	if eventType == "session.updated" || eventType == "transcription_session.updated" {
		ackEventID := openAIRealtimeSettingsEventID(payload)
		s.mu.Lock()
		var update *openAIRealtimeSettingsUpdate
		// session.updated.event_id identifies the provider acknowledgement itself;
		// it is not the client event_id from the session.update. Use it only to
		// consume duplicate acknowledgements, never to select a pending update.
		if ackEventID != "" && s.hasUsedInputSettingsAckIDLocked(ackEventID) {
			s.mu.Unlock()
			return true
		}
		// Only the first unresolved update in actual write order may consume a
		// success acknowledgement. Do not skip an incompatible update and
		// accidentally commit a later one. A duplicate without a provider event
		// ID is inherently indistinguishable from the next success; the finite
		// guarantee here is FIFO while a pending update exists, with no invented
		// cross-event association.
		for _, pending := range s.pendingSettings {
			if pending == nil || pending.resolved {
				continue
			}
			if !inputSettingsAckMatches(pending.eventType, eventType) {
				s.mu.Unlock()
				return false
			}
			update = pending
			break
		}
		if update == nil {
			s.mu.Unlock()
			return false
		}
		if ackEventID != "" {
			s.rememberInputSettingsAckIDLocked(ackEventID)
		}
		resolved := s.resolveInputSettingsLocked(update, true)
		s.mu.Unlock()
		return resolved
	}
	if eventType != "error" {
		return false
	}
	var event struct {
		Error struct {
			EventID string `json:"event_id"`
		} `json:"error"`
	}
	if json.Unmarshal(payload, &event) != nil {
		return false
	}
	eventID := strings.TrimSpace(event.Error.EventID)
	if eventID == "" || len(eventID) > openAIRealtimeIdentifierMaxBytes {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, pending := range s.pendingSettings {
		if pending != nil && pending.eventID == eventID {
			if pending.resolved {
				return true
			}
			return s.resolveInputSettingsLocked(pending, false)
		}
	}
	// A completed settings ID is deliberately not treated as a duplicate
	// settings error: the same wire event_id may belong to an active response
	// or input owner, and that owner must see its provider error.
	return false
}

func inputSettingsAckMatches(updateType, ackType string) bool {
	switch updateType {
	case "session.update":
		return ackType == "session.updated"
	case "transcription_session.update":
		return ackType == "transcription_session.updated" || ackType == "session.updated"
	default:
		return false
	}
}

func openAIRealtimeSettingsEventID(payload []byte) string {
	var event struct {
		EventID string `json:"event_id"`
	}
	if json.Unmarshal(payload, &event) != nil {
		return ""
	}
	return strings.TrimSpace(event.EventID)
}

func (s *openAIRealtimeSession) rememberInputSettingsRequestIDLocked(eventID string) bool {
	eventID = strings.TrimSpace(eventID)
	if eventID == "" {
		return true
	}
	for _, existing := range s.usedSettingsRequestIDs {
		if existing == eventID {
			return false
		}
	}
	if len(s.usedSettingsRequestIDs) >= openAIRealtimeSettingsIDLimit {
		return false
	}
	s.usedSettingsRequestIDs = append(s.usedSettingsRequestIDs, eventID)
	return true
}

func (s *openAIRealtimeSession) rememberInputSettingsAckIDLocked(eventID string) bool {
	key := inputSettingsAckIDKey(eventID)
	if key == "" {
		return true
	}
	for _, existing := range s.usedSettingsAckIDs {
		if existing == key {
			return false
		}
	}
	if len(s.usedSettingsAckIDs) >= openAIRealtimeSettingsIDLimit {
		return false
	}
	s.usedSettingsAckIDs = append(s.usedSettingsAckIDs, key)
	return true
}

func (s *openAIRealtimeSession) hasUsedInputSettingsRequestIDLocked(eventID string) bool {
	eventID = strings.TrimSpace(eventID)
	if eventID == "" {
		return false
	}
	for _, existing := range s.usedSettingsRequestIDs {
		if existing == eventID {
			return true
		}
	}
	return false
}

func (s *openAIRealtimeSession) hasUsedInputSettingsAckIDLocked(eventID string) bool {
	key := inputSettingsAckIDKey(eventID)
	if key == "" {
		return false
	}
	for _, existing := range s.usedSettingsAckIDs {
		if existing == key {
			return true
		}
	}
	return false
}

func inputSettingsAckIDKey(eventID string) string {
	eventID = strings.TrimSpace(eventID)
	if eventID == "" {
		return ""
	}
	digest := sha256.Sum256([]byte(eventID))
	return string(digest[:])
}

// settingsBeginRejectionLocked distinguishes a settings ID collision from
// the pending or bounded settings lifetime limits. The caller must hold s.mu.
func (s *openAIRealtimeSession) settingsBeginRejectionLocked(eventID string) string {
	eventID = strings.TrimSpace(eventID)
	if len(s.pendingSettings) >= openAIRealtimePendingSettingsLimit {
		return "pending_capacity"
	}
	if eventID != "" {
		if s.hasUsedInputSettingsRequestIDLocked(eventID) || s.activeInputEventIDLocked(eventID) {
			return "reused"
		}
		if len(eventID) > openAIRealtimeIdentifierMaxBytes {
			return "too_long"
		}
	}
	if len(s.usedSettingsRequestIDs) >= openAIRealtimeSettingsIDLimit {
		return "settings_capacity"
	}
	return ""
}

func (s *openAIRealtimeSession) activeInputEventIDLocked(eventID string) bool {
	eventID = strings.TrimSpace(eventID)
	if eventID == "" {
		return false
	}
	if s.turn != nil && strings.TrimSpace(s.turn.clientEventID) == eventID {
		return true
	}
	for _, work := range s.inputWorks {
		if work != nil && !work.transcriptionFinalized && strings.TrimSpace(work.clientEventID) == eventID {
			return true
		}
	}
	return false
}

func realtimeSettingsBeginError(s *openAIRealtimeSession, eventID string) error {
	if s == nil {
		return newOpenAIRealtimeClientError("invalid_event", "invalid realtime event_id")
	}
	eventID = strings.TrimSpace(eventID)
	s.mu.Lock()
	reason := s.settingsBeginRejectionLocked(eventID)
	s.mu.Unlock()
	switch reason {
	case "reused":
		return newOpenAIRealtimeClientError("realtime_configuration_event_id_reused", "client event_id was already used by this session; use a new event_id")
	case "pending_capacity":
		return newOpenAIRealtimeClientError("realtime_configuration_capacity_exceeded", "too many realtime configuration updates awaiting acknowledgement")
	case "settings_capacity":
		return newOpenAIRealtimeClientError("realtime_configuration_id_capacity_exhausted", "realtime configuration event_id capacity is exhausted; reconnect the session")
	case "too_long":
		return newOpenAIRealtimeClientError("invalid_event", "realtime event_id exceeds the supported limit")
	}
	return newOpenAIRealtimeClientError("invalid_event", "realtime configuration update could not be associated")
}

func (s *openAIRealtimeSession) prepareInputWork(eventType string, payload []byte) (*openAIRealtimeInputWork, bool, error) {
	switch eventType {
	case "input_audio_buffer.append", "input_audio_buffer.commit", "conversation.item.create":
	default:
		return nil, false, nil
	}
	models := s.modelBinding()
	if err := s.checkFutureWork(models, false); err != nil {
		return nil, false, err
	}
	itemID := ""
	contentIndices := []int{0}
	if eventType == "conversation.item.create" {
		var event struct {
			Item struct {
				ID      string `json:"id"`
				Content []struct {
					Type string `json:"type"`
				} `json:"content"`
			} `json:"item"`
		}
		if json.Unmarshal(payload, &event) != nil {
			return nil, false, nil
		}
		contentIndices = nil
		for i, content := range event.Item.Content {
			if content.Type == "input_audio" {
				contentIndices = append(contentIndices, i)
			}
		}
		if len(contentIndices) == 0 {
			return nil, false, nil
		}
		itemID = event.Item.ID
		if len(itemID) > openAIRealtimeIdentifierMaxBytes {
			return nil, false, newOpenAIRealtimeClientError("invalid_event", "input item identifier exceeds the supported limit")
		}
	}
	s.mu.Lock()
	if s.transcriptionModels == nil && (s.automaticFeaturesDisabled || eventType == "conversation.item.create") {
		s.mu.Unlock()
		return nil, false, nil
	}
	if eventType == "input_audio_buffer.append" && s.manualAudioCommit {
		s.mu.Unlock()
		return nil, false, nil
	}
	if eventType != "conversation.item.create" && s.audioWork != nil {
		work := s.audioWork
		s.mu.Unlock()
		return work, false, nil
	}
	limit := s.pendingInputLimit
	if limit <= 0 {
		limit = openAIRealtimePendingInputLimit
	}
	if len(s.inputWorks)+len(contentIndices) > limit {
		s.mu.Unlock()
		return nil, false, newOpenAIRealtimeClientError("realtime_input_capacity_exceeded", "too many realtime inputs awaiting completion")
	}
	s.inputSeq++
	submissionID := fmt.Sprintf("input-%d", s.inputSeq)
	works := make([]*openAIRealtimeInputWork, 0, len(contentIndices))
	for _, index := range contentIndices {
		id := submissionID
		if len(contentIndices) > 1 {
			id = fmt.Sprintf("%s/content-%d", submissionID, index)
		}
		automaticResponse := !s.automaticFeaturesDisabled && eventType != "conversation.item.create"
		work := &openAIRealtimeInputWork{id: id, submissionID: submissionID, itemID: itemID, contentIndex: index, models: models, startedAt: time.Now(), automaticResponse: automaticResponse}
		if s.transcriptionModels != nil {
			work.transcriptionModels = *s.transcriptionModels
			if s.turnObserverFactory != nil {
				work.observer = runtimesession.GuardTurnObserver(s.turnObserverFactory())
			}
		}
		works = append(works, work)
	}
	s.mu.Unlock()
	if err := s.checkFutureWork(models, true); err != nil {
		return nil, false, err
	}
	if works[0].observer != nil {
		if err := s.checkFutureWork(works[0].transcriptionModels, false); err != nil {
			return nil, false, err
		}
		for i, work := range works {
			err := runtimesession.AdmitBoundedTurn(work.observer, runtimesession.TurnAdmission{Models: work.transcriptionModels, SessionID: s.sessionID, InputItemID: work.itemID, WorkID: work.id, WorkAuthorized: true, Transcription: true})
			if err != nil {
				for _, admitted := range works[:i+1] {
					s.finishInput(admitted, "input_admission_failed", nil, true)
				}
				return nil, false, err
			}
		}
	}
	s.mu.Lock()
	s.inputWorks = append(s.inputWorks, works...)
	if eventType != "conversation.item.create" {
		s.audioWork = works[0]
	}
	s.mu.Unlock()
	return works[0], true, nil
}

func (s *openAIRealtimeSession) finishInput(work *openAIRealtimeInputWork, reason string, usage *types.UsageEvent, notSubmitted bool) error {
	if work == nil {
		return nil
	}
	s.mu.Lock()
	if work.transcriptionFinalized {
		s.mu.Unlock()
		return nil
	}
	work.transcriptionFinalized = true
	observer := work.observer
	payload := runtimesession.TurnFinalizePayload{SessionID: s.sessionID, Model: work.transcriptionModels.BillingModel, Models: work.transcriptionModels, WorkID: work.id, InputItemID: work.itemID, TerminationReason: reason, StartedAt: work.startedAt, CompletedAt: time.Now(), Usage: usage.Clone()}
	if usage != nil {
		payload.Models.ReportedModel = usage.ResponseModel
	}
	s.pruneInputWorksLocked()
	s.mu.Unlock()
	var err error
	if observer != nil {
		if notSubmitted {
			err = runtimesession.RollbackTurnAdmission(observer, reason)
		} else {
			if usage != nil {
				err = observer.ObserveTurnUsage(usage)
			}
			observer.FinalizeTurn(payload)
		}
		err = errors.Join(err, s.observeFinalization(observer))
	}
	result := runtimesession.TurnResult(observer)
	s.mu.Lock()
	limit := s.recentInputLimit
	if limit <= 0 {
		limit = openAIRealtimeRecentInputLimit
	}
	s.recentInputResults = append(s.recentInputResults, openAIRealtimeInputResult{WorkID: work.id, ItemID: work.itemID, Result: result})
	if len(s.recentInputResults) > limit {
		s.recentInputResults = append([]openAIRealtimeInputResult(nil), s.recentInputResults[len(s.recentInputResults)-limit:]...)
	}
	s.mu.Unlock()
	return err
}

func (s *openAIRealtimeSession) pruneInputWorksLocked() {
	kept := s.inputWorks[:0]
	for _, work := range s.inputWorks {
		if work != s.audioWork && (work.observer == nil || work.transcriptionFinalized) && (!work.automaticResponse || work.responseClaimed) {
			continue
		}
		kept = append(kept, work)
	}
	for i := len(kept); i < len(s.inputWorks); i++ {
		s.inputWorks[i] = nil
	}
	s.inputWorks = kept
}

// 同一个客户端提交可以包含多个音频 part；各 owner 共享发送事实和工作许可。
func (s *openAIRealtimeSession) submissionPartsLocked(work *openAIRealtimeInputWork) []*openAIRealtimeInputWork {
	var parts []*openAIRealtimeInputWork
	for _, candidate := range s.inputWorks {
		if candidate.submissionID == work.submissionID {
			parts = append(parts, candidate)
		}
	}
	return parts
}

func (s *openAIRealtimeSession) inputWriteFinished(work *openAIRealtimeInputWork, eventType string) {
	if work == nil {
		return
	}
	s.mu.Lock()
	parts := s.submissionPartsLocked(work)
	for _, part := range parts {
		part.submitted = true
	}
	if eventType == "input_audio_buffer.commit" {
		// A successful socket write does not identify which commit the
		// provider is acknowledging. Keep the automatic owner until an
		// explicit response resolves the one commit candidate, or a VAD
		// source event proves that the provider initiated the work.
		manualCandidate := s.audioWork == work
		for _, part := range parts {
			if part.automaticSourceSeen || !part.manualCommitCandidate {
				part.manualCommitCandidate = false
				part.manualCommitSent = false
				continue
			}
			// providerCommitSeen may refer to the VAD commit that was already
			// in flight. It is therefore evidence that the provider observed a
			// commit, but not evidence that this client commit owns the item.
			part.manualCommitSent = true
		}
		if manualCandidate {
			s.audioWork = nil
		}
	}
	s.pruneInputWorksLocked()
	s.mu.Unlock()
}

func (s *openAIRealtimeSession) rollbackManualCommitCandidate(work *openAIRealtimeInputWork) {
	if work == nil {
		return
	}
	s.mu.Lock()
	for _, part := range s.submissionPartsLocked(work) {
		if part.manualCommitCandidate && !part.manualCommitSent {
			part.manualCommitCandidate = false
			part.manualCommitEventID = ""
		}
	}
	s.mu.Unlock()
}

func (s *openAIRealtimeSession) markAutomaticInputSourceLocked(work *openAIRealtimeInputWork) {
	if work == nil {
		return
	}
	for _, part := range s.submissionPartsLocked(work) {
		part.automaticSourceSeen = true
		part.manualCommitCandidate = false
		part.manualCommitSent = false
		if s.manualResponseInput == part {
			s.manualResponseInput = nil
		}
	}
}

func (s *openAIRealtimeSession) markManualInputSourceLocked(work *openAIRealtimeInputWork) {
	if work == nil || work.automaticSourceSeen {
		return
	}
	for _, part := range s.submissionPartsLocked(work) {
		part.automaticResponse = false
		part.manualCommitCandidate = false
		part.manualCommitSent = false
	}
}

func (s *openAIRealtimeSession) discardInput(work *openAIRealtimeInputWork, reason string, attempted bool) {
	if work == nil {
		return
	}
	s.mu.Lock()
	if s.audioWork == work {
		s.audioWork = nil
	}
	if s.manualResponseInput == work {
		s.manualResponseInput = nil
	}
	parts := s.submissionPartsLocked(work)
	for _, part := range parts {
		part.responseClaimed = true
	}
	s.mu.Unlock()
	for _, part := range parts {
		s.finishInput(part, reason, nil, !attempted)
	}
}

func (s *openAIRealtimeSession) hasAutomaticWorkLocked() bool {
	for _, work := range s.inputWorks {
		if work.automaticResponse && !work.responseClaimed {
			return true
		}
	}
	return false
}

func (s *openAIRealtimeSession) claimAutomaticWork() runtimesession.TurnAdmission {
	s.mu.Lock()
	defer s.mu.Unlock()
	admission := runtimesession.TurnAdmission{SessionID: s.sessionID, Models: s.modelBinding()}
	for _, work := range s.inputWorks {
		if work.automaticResponse && !work.responseClaimed {
			work.responseClaimed = true
			if s.audioWork == work {
				s.audioWork = nil
			}
			if s.manualResponseInput == work {
				s.manualResponseInput = nil
			}
			admission.Models, admission.WorkID, admission.WorkAuthorized = work.models, work.id, true
			admission.InputItemID = work.itemID
			s.pruneInputWorksLocked()
			break
		}
	}
	return admission
}

// 合法关联事件绑定已授权的本地输入；completed/failed 只能查找已绑定输入。
func (s *openAIRealtimeSession) observeInputEvent(eventType string, payload []byte, usage *types.UsageEvent) (bool, error) {
	var event struct {
		ItemID       string `json:"item_id"`
		ContentIndex int    `json:"content_index"`
		Item         struct {
			ID      string `json:"id"`
			Content []struct {
				Type string `json:"type"`
			} `json:"content"`
		} `json:"item"`
		Error struct {
			EventID string `json:"event_id"`
		} `json:"error"`
	}
	if json.Unmarshal(payload, &event) != nil {
		return false, nil
	}
	if eventType == "error" && event.Error.EventID != "" {
		s.mu.Lock()
		var rejected []*openAIRealtimeInputWork
		handledCommitError := false
		for _, candidate := range s.inputWorks {
			if candidate.manualCommitEventID == event.Error.EventID {
				handledCommitError = true
				for _, part := range s.submissionPartsLocked(candidate) {
					part.manualCommitCandidate = false
					part.manualCommitSent = false
				}
				if candidate.automaticResponse || candidate.automaticSourceSeen {
					// A client commit error can be the empty-buffer commit sent
					// after VAD already committed this work. Keep the automatic
					// owner for the provider response instead of rejecting it.
					if candidate.automaticResponse && candidate.providerCommitSeen {
						s.markAutomaticInputSourceLocked(candidate)
					}
					continue
				}
			}
			if candidate.clientEventID == event.Error.EventID && candidate.manualCommitEventID == "" && !candidate.automaticSourceSeen {
				if s.audioWork == candidate {
					s.audioWork = nil
				}
				candidate.responseClaimed = true
				rejected = append(rejected, candidate)
			}
		}
		s.mu.Unlock()
		if len(rejected) > 0 {
			var err error
			for _, work := range rejected {
				err = errors.Join(err, s.finishInput(work, "input_rejected", nil, false))
			}
			return true, err
		}
		if handledCommitError {
			return true, nil
		}
	}
	itemID := strings.TrimSpace(event.ItemID)
	if itemID == "" {
		itemID = strings.TrimSpace(event.Item.ID)
	}
	terminal := eventType == types.EventTypeInputAudioTranscriptionCompleted || eventType == "conversation.item.input_audio_transcription.failed"
	audioItemCreated := false
	if eventType == "conversation.item.created" {
		for _, content := range event.Item.Content {
			if content.Type == "input_audio" {
				audioItemCreated = true
			}
		}
	}
	binding := eventType == "input_audio_buffer.committed" || eventType == "input_audio_buffer.speech_started" || eventType == "input_audio_buffer.speech_stopped" || eventType == "input_audio_buffer.timeout_triggered" || audioItemCreated
	if !terminal && !binding {
		return false, nil
	}
	if len(itemID) > openAIRealtimeIdentifierMaxBytes {
		return terminal, errOpenAIRealtimeUsageStateLimit
	}
	s.mu.Lock()
	var work *openAIRealtimeInputWork
	for _, candidate := range s.inputWorks {
		if candidate.itemID == itemID && itemID != "" && (!terminal || candidate.contentIndex == event.ContentIndex) {
			work = candidate
			break
		}
	}
	if binding && work == nil && itemID != "" {
		for _, candidate := range s.inputWorks {
			if candidate.itemID == "" {
				work = candidate
				for _, part := range s.submissionPartsLocked(work) {
					part.itemID = itemID
				}
				break
			}
		}
	}
	if work != nil {
		switch eventType {
		case "input_audio_buffer.speech_started", "input_audio_buffer.speech_stopped", "input_audio_buffer.timeout_triggered":
			s.markAutomaticInputSourceLocked(work)
		case "input_audio_buffer.committed", "conversation.item.created":
			work.providerCommitSeen = true
			if work.automaticResponse && !work.automaticSourceSeen && !work.manualCommitCandidate {
				s.markAutomaticInputSourceLocked(work)
			}
			/*
				A provider acknowledgement can race the client write result.
				When the client commit is still only a candidate, leave the
				decision for inputWriteFinished, which knows whether the
				candidate actually reached the socket.
			*/
			if work.automaticResponse && !work.automaticSourceSeen && work.manualCommitCandidate && !work.manualCommitSent {
				// Keep the conservative automatic wait until the client write
				// establishes the manual commit candidate.
			}
		}
	}
	if eventType == "input_audio_buffer.committed" && work != nil && s.audioWork == work {
		s.audioWork = nil
	}
	s.pruneInputWorksLocked()
	s.mu.Unlock()
	if !terminal {
		return false, nil
	}
	if work == nil || work.contentIndex != event.ContentIndex {
		return true, nil
	}
	if usage != nil {
		usage.ResponseModel = work.transcriptionModels.ProviderModel
	}
	return true, s.finishInput(work, eventType, usage, false)
}
