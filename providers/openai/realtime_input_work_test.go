package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"one-api/common/wsconn"
	runtimesession "one-api/runtime/session"
)

type realtimeInputTestObserver struct {
	recordingOpenAIRealtimeObserver
	admission    runtimesession.TurnAdmission
	admissionErr error
	rollbacks    int
	result       runtimesession.TurnFinalizationResult
}

func (o *realtimeInputTestObserver) AdmitBoundedTurn(a runtimesession.TurnAdmission) error {
	o.admission = a
	return o.admissionErr
}
func (o *realtimeInputTestObserver) AdmitTurn() error { return nil }
func (o *realtimeInputTestObserver) ObserveProviderInitiatedTurn(a runtimesession.TurnAdmission) error {
	o.admission = a
	return nil
}
func (o *realtimeInputTestObserver) RollbackTurnAdmission(string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.rollbacks++
	return o.result.Err
}
func (o *realtimeInputTestObserver) FinalizationResult() runtimesession.TurnFinalizationResult {
	return o.result
}

type realtimeInputTestPolicy struct {
	count   int
	maximum int
	denied  bool
}

func (p *realtimeInputTestPolicy) ResolveModel(model string) (runtimesession.ModelBinding, error) {
	if p.denied {
		return runtimesession.ModelBinding{}, errors.New("model denied")
	}
	return runtimesession.ModelBinding{RequestedModel: model, ProviderModel: "upstream-" + model, BillingModel: model}, nil
}
func (p *realtimeInputTestPolicy) CheckFutureWork(_ runtimesession.ModelBinding, count bool) error {
	if p.denied {
		return errors.New("work denied")
	}
	if count {
		if p.maximum > 0 && p.count >= p.maximum {
			return errors.New("work rate limited")
		}
		p.count++
	}
	return nil
}

func inputWorkFixture(t *testing.T, automatic bool) (*openAIRealtimeSession, *realtimeInputTestPolicy, *[]*realtimeInputTestObserver) {
	t.Helper()
	s := newOpenAIRealtimeHelperSession()
	p := &realtimeInputTestPolicy{}
	s.workPolicy = p
	s.models = runtimesession.ModelBinding{RequestedModel: "voice-public", ProviderModel: s.model, BillingModel: "voice-public"}
	models := runtimesession.ModelBinding{RequestedModel: "transcribe-public", ProviderModel: "upstream-transcribe", BillingModel: "transcribe-public"}
	s.transcriptionModels = &models
	s.manualAudioCommit = !automatic
	s.automaticFeaturesDisabled = !automatic
	owners := []*realtimeInputTestObserver{}
	s.turnObserverFactory = func() runtimesession.TurnObserver {
		o := &realtimeInputTestObserver{}
		owners = append(owners, o)
		return o
	}
	t.Cleanup(func() { s.Abort("test_cleanup") })
	return s, p, &owners
}
func admitTestInput(t *testing.T, s *openAIRealtimeSession, item string) *openAIRealtimeInputWork {
	t.Helper()
	w, created, err := s.prepareInputWork("input_audio_buffer.commit", []byte(`{"type":"input_audio_buffer.commit"}`))
	if err != nil || !created {
		t.Fatalf("admit input: created=%v err=%v", created, err)
	}
	s.inputWriteFinished(w, "input_audio_buffer.commit")
	payload := []byte(fmt.Sprintf(`{"type":"input_audio_buffer.committed","item_id":%q}`, item))
	if _, err := s.observeInputEvent("input_audio_buffer.committed", payload, nil); err != nil {
		t.Fatal(err)
	}
	return w
}
func completeTestInput(s *openAIRealtimeSession, item string) (openAIRealtimeOutbound, bool) {
	return s.observeSupplierMessage(wsconn.TextMessage, []byte(fmt.Sprintf(`{"type":"conversation.item.input_audio_transcription.completed","item_id":%q,"content_index":0,"usage":{"type":"tokens","input_tokens":7,"total_tokens":7}}`, item)))
}

func TestRealtimeInputOwnerSurvivesResponseAndSessionConfigChanges(t *testing.T) {
	s, p, owners := inputWorkFixture(t, false)
	work := admitTestInput(t, s, "input-a")
	response := &realtimeInputTestObserver{}
	s.turn = newOpenAIRealtimeTurnState(1, time.Now(), runtimesession.GuardTurnObserver(response))
	s.turn.rememberResponseID("response-a")
	s.observeSupplierMessage(wsconn.TextMessage, []byte(`{"type":"response.done","response":{"id":"response-a","status":"completed","usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5}}}`))
	next := runtimesession.ModelBinding{RequestedModel: "next-transcribe", ProviderModel: "next-upstream", BillingModel: "next-price"}
	s.transcriptionModels = &next
	for i := 0; i < 2; i++ {
		if event, close := completeTestInput(s, "input-a"); close || event.err != nil {
			t.Fatalf("completion failed: %+v", event)
		}
	}
	if len(*owners) != 1 || (*owners)[0].observeCount() != 1 || (*owners)[0].finalizeCount() != 1 {
		t.Fatalf("unexpected owner count: %+v", *owners)
	}
	payload := (*owners)[0].lastPayload()
	if payload.WorkID != work.id || payload.Models.BillingModel != "transcribe-public" || payload.InputItemID != "input-a" {
		t.Fatalf("owner rebound: %+v", payload)
	}
	if response.observeCount() != 1 || response.lastPayload().Usage.TotalTokens != 5 || p.count != 1 {
		t.Fatal("transcription was added to response or RPM counted twice")
	}
}

func TestRealtimeAutomaticInputSharesPermitAcrossChunksAndResponse(t *testing.T) {
	s, p, owners := inputWorkFixture(t, true)
	p.maximum = 1
	first, created, err := s.prepareInputWork("input_audio_buffer.append", []byte(`{"type":"input_audio_buffer.append","audio":"AAAA"}`))
	if err != nil || !created {
		t.Fatal(err)
	}
	s.inputWriteFinished(first, "input_audio_buffer.append")
	for i := 0; i < 3; i++ {
		work, created, err := s.prepareInputWork("input_audio_buffer.append", nil)
		if err != nil || created || work != first {
			t.Fatalf("chunk admission: %v %v", created, err)
		}
	}
	s.observeInputEvent("input_audio_buffer.committed", []byte(`{"type":"input_audio_buffer.committed","item_id":"automatic-a"}`), nil)
	if event, close := s.observeSupplierMessage(wsconn.TextMessage, []byte(`{"type":"response.created","response":{"id":"auto-response"}}`)); close || event.err != nil {
		t.Fatalf("automatic response: %+v", event)
	}
	if len(*owners) != 2 || !(*owners)[1].admission.WorkAuthorized || (*owners)[1].admission.WorkID != first.id || p.count != 1 {
		t.Fatalf("permit not shared: count=%d owners=%+v", p.count, *owners)
	}
	if _, _, err := s.prepareInputWork("input_audio_buffer.append", nil); err == nil {
		t.Fatal("second work bypassed RPM")
	}
}

func TestRealtimeInputFailureAndFinalCloseFinalizeEveryOwner(t *testing.T) {
	s, _, owners := inputWorkFixture(t, false)
	admitTestInput(t, s, "failed-input")
	admitTestInput(t, s, "pending-input")
	response := &realtimeInputTestObserver{}
	s.turn = newOpenAIRealtimeTurnState(1, time.Now(), runtimesession.GuardTurnObserver(response))
	s.Detach("temporary detach")
	if (*owners)[0].finalizeCount() != 0 || (*owners)[1].finalizeCount() != 0 {
		t.Fatal("detach prematurely finalized inputs")
	}
	failed := []byte(`{"type":"conversation.item.input_audio_transcription.failed","item_id":"failed-input","content_index":0,"error":{"message":"upstream failure"}}`)
	s.observeSupplierMessage(wsconn.TextMessage, failed)
	s.Abort("final close")
	for _, owner := range *owners {
		if owner.finalizeCount() != 1 || owner.observeCount() != 0 {
			t.Fatal("input was missed or estimated usage")
		}
	}
	if response.finalizeCount() != 1 || len(s.inputWorks) != 0 {
		t.Fatal("close missed a response/input")
	}
}

func TestRealtimeInputCapacityAndCompletedHistoryAreBounded(t *testing.T) {
	s, p, owners := inputWorkFixture(t, false)
	s.pendingInputLimit = 2
	s.recentInputLimit = 2
	admitTestInput(t, s, "input-0")
	admitTestInput(t, s, "input-1")
	if _, _, err := s.prepareInputWork("input_audio_buffer.commit", nil); err == nil {
		t.Fatal("pending input capacity bypassed")
	}
	if p.count != 2 || len(*owners) != 2 {
		t.Fatal("capacity rejection performed admission")
	}
	for i := 0; i < 6; i++ {
		if i >= 2 {
			admitTestInput(t, s, fmt.Sprintf("input-%d", i))
		}
		completeTestInput(s, fmt.Sprintf("input-%d", i))
	}
	if len(s.recentInputResults) != 2 || len(s.inputWorks) != 0 {
		t.Fatalf("unbounded input state: pending=%d recent=%d", len(s.inputWorks), len(s.recentInputResults))
	}
	completeTestInput(s, "input-0")
	if len(*owners) != 6 {
		t.Fatal("evicted input result recreated an owner")
	}
}

func TestRealtimeInputConcurrentTerminalCloseRecordsUnsettledOnce(t *testing.T) {
	s, _, owners := inputWorkFixture(t, false)
	admitTestInput(t, s, "unknown-commit")
	owner := (*owners)[0]
	owner.result = runtimesession.TurnFinalizationResult{Unsettled: true, StopFutureWork: true, Err: errors.New("commit_unknown")}
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%3 == 0 {
				s.Abort("close")
			} else if i%3 == 1 {
				completeTestInput(s, "unknown-commit")
			} else {
				s.observeSupplierMessage(wsconn.TextMessage, []byte(`{"type":"conversation.item.input_audio_transcription.failed","item_id":"unknown-commit"}`))
			}
		}(i)
	}
	wg.Wait()
	if owner.finalizeCount() != 1 || len(s.recentInputResults) != 1 || !s.recentInputResults[0].Result.Unsettled {
		t.Fatalf("terminal result lost: finalized=%d results=%+v", owner.finalizeCount(), s.recentInputResults)
	}
	if err := s.checkFutureWork(s.models, false); err == nil {
		t.Fatal("unsettled work left future work open")
	}
}

func TestRealtimeInputBeforeWriteRollbackAndAmbiguousWriteUseOriginalOwner(t *testing.T) {
	for _, attempted := range []bool{false, true} {
		t.Run(fmt.Sprint(attempted), func(t *testing.T) {
			s, _, owners := inputWorkFixture(t, false)
			w, _, err := s.prepareInputWork("input_audio_buffer.commit", nil)
			if err != nil {
				t.Fatal(err)
			}
			s.discardInput(w, "write_failed", attempted)
			s.Abort("close")
			owner := (*owners)[0]
			if attempted {
				if owner.finalizeCount() != 1 || owner.rollbacks != 0 {
					t.Fatal("ambiguous write treated as unsent")
				}
			} else if owner.rollbacks != 1 || owner.finalizeCount() != 0 {
				t.Fatal("unsent input did not rollback once")
			}
		})
	}
}

func TestRealtimeInputOnlyAudioEventsBindPendingWork(t *testing.T) {
	s, _, owners := inputWorkFixture(t, false)
	w, _, err := s.prepareInputWork("input_audio_buffer.commit", nil)
	if err != nil {
		t.Fatal(err)
	}
	s.inputWriteFinished(w, "input_audio_buffer.commit")
	s.observeInputEvent("conversation.item.created", []byte(`{"type":"conversation.item.created","item":{"id":"text-item","content":[{"type":"input_text","text":"hello"}]}}`), nil)
	if w.itemID != "" {
		t.Fatal("text item stole audio owner")
	}
	s.observeInputEvent("input_audio_buffer.committed", []byte(`{"type":"input_audio_buffer.committed","item_id":"audio-item"}`), nil)
	completeTestInput(s, "audio-item")
	if (*owners)[0].finalizeCount() != 1 {
		t.Fatal("audio result missed admitted owner")
	}
}

func TestRealtimeInputSettingsMapOnlyOwnedModelsAndRejectBeforeSend(t *testing.T) {
	s, p, _ := inputWorkFixture(t, false)
	input := []byte(`{"type":"session.update","future":{"v":1},"session":{"model":"voice-public","future_flag":[true],"audio":{"input":{"transcription":{"model":"transcribe-public","future_option":"keep"},"turn_detection":null}}}}`)
	payload, settings, err := s.prepareInputSettings(input, "session.update")
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if json.Unmarshal(payload, &wire) != nil {
		t.Fatal("invalid normalized json")
	}
	if !strings.Contains(string(payload), `"future_option":"keep"`) || !strings.Contains(string(payload), `"future_flag":[true]`) || settings.transcription.ProviderModel != "upstream-transcribe-public" {
		t.Fatalf("wire mapping lost fields: %s", payload)
	}
	if _, _, err := s.prepareInputSettings([]byte(`{"type":"response.create","response":{"model":"upstream-voice"}}`), "response.create"); err == nil {
		t.Fatal("upstream model accepted as public override")
	}
	p.denied = true
	if _, _, err := s.prepareInputSettings(input, "session.update"); err == nil {
		t.Fatal("unauthorized transcription configuration passed")
	}
}

func TestRealtimeInputSendRejectsCapacityBeforeUpstreamWrite(t *testing.T) {
	received := make(chan struct{}, 1)
	release := make(chan struct{})
	server := newOpenAIRealtimeTestServer(t, func(conn *openAIRealtimeTestConn) {
		defer func() { <-release }()
		if _, _, err := conn.ReadMessage(); err == nil {
			received <- struct{}{}
		}
	})
	defer server.Close()
	defer close(release)
	provider := newOpenAIRealtimeTestProvider(server.URL)
	raw, apiErr := provider.OpenRealtimeSession("gpt-4o-realtime-preview")
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	s := raw.(*openAIRealtimeSession)
	defer s.Abort("test_cleanup")
	s.pendingInputLimit = 1
	s.inputWorks = []*openAIRealtimeInputWork{{id: "existing"}}
	if err := s.SendClient(context.Background(), openAITestTextFrame([]byte(`{"type":"input_audio_buffer.append","audio":"AAAA"}`))); err == nil {
		t.Fatal("capacity exhausted input was sent")
	}
	select {
	case <-received:
		t.Fatal("upstream observed rejected input")
	default:
	}
}

func TestRealtimeRejectedSettingsKeepOriginalInputOwnerBinding(t *testing.T) {
	release := make(chan struct{})
	server := newOpenAIRealtimeTestServer(t, func(conn *openAIRealtimeTestConn) {
		_, request, err := conn.ReadMessage()
		if err != nil {
			t.Error(err)
			return
		}
		var update struct {
			EventID string `json:"event_id"`
		}
		if json.Unmarshal(request, &update) != nil || update.EventID == "" {
			t.Errorf("update correlation missing: %s", request)
			return
		}
		rejected := fmt.Sprintf(`{"type":"error","error":{"type":"invalid_request_error","code":"invalid_session_setting","event_id":%q}}`, update.EventID)
		if err := conn.WriteMessage(wsconn.TextMessage, []byte(rejected)); err != nil {
			t.Error(err)
			return
		}
		if _, _, err := conn.ReadMessage(); err != nil {
			t.Error(err)
			return
		}
		for _, event := range []string{
			`{"type":"input_audio_buffer.committed","item_id":"original-model-input"}`,
			`{"type":"conversation.item.input_audio_transcription.completed","item_id":"original-model-input","usage":{"type":"tokens","input_tokens":7,"total_tokens":7}}`,
		} {
			if err := conn.WriteMessage(wsconn.TextMessage, []byte(event)); err != nil {
				t.Error(err)
				return
			}
		}
		<-release
	})
	defer server.Close()
	defer close(release)
	provider := newOpenAIRealtimeTestProvider(server.URL)
	raw, apiErr := provider.OpenRealtimeSession("gpt-4o-realtime-preview")
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	s := raw.(*openAIRealtimeSession)
	defer s.Abort("cleanup")
	original := runtimesession.ModelBinding{RequestedModel: "original-transcribe", ProviderModel: "upstream-original", BillingModel: "original-transcribe"}
	s.transcriptionModels = &original
	s.manualAudioCommit = true
	s.automaticFeaturesDisabled = true
	s.workPolicy = &realtimeInputTestPolicy{}
	owner := &realtimeInputTestObserver{}
	s.SetTurnObserverFactory(func() runtimesession.TurnObserver { return owner })
	update := `{"type":"session.update","session":{"turn_detection":{"type":"server_vad"},"input_audio_transcription":{"model":"new-transcribe"},"upstream_invalid_setting":true}}`
	if err := s.SendClient(context.Background(), openAITestTextFrame([]byte(update))); err != nil {
		t.Fatal(err)
	}
	if err := s.SendClient(context.Background(), openAITestTextFrame([]byte(`{"type":"input_audio_buffer.commit"}`))); err != nil {
		t.Fatalf("next input could not use original accepted config: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, _, _, _, err := openAITestRecv(context.Background(), s); err != nil {
			t.Fatal(err)
		}
	}
	if owner.admission.Models != original || owner.finalizeCount() != 1 {
		t.Fatalf("rejected configuration changed owner: %+v", owner.admission)
	}
	if !s.manualAudioCommit || !s.automaticFeaturesDisabled {
		t.Fatal("rejected configuration enabled automatic work")
	}
}

func TestRealtimeClosingForOneOwnerDrainsAlreadyReceivedOtherInputUsage(t *testing.T) {
	s, _, owners := inputWorkFixture(t, false)
	admitTestInput(t, s, "first-input")
	admitTestInput(t, s, "second-input")
	(*owners)[0].result = runtimesession.TurnFinalizationResult{Unsettled: true, StopFutureWork: true, Err: errors.New("commit_unknown")}
	queued := make(chan openAIRealtimeProviderFrame, 3)
	for _, item := range []string{"first-input", "second-input", "unknown-input"} {
		queued <- openAIRealtimeProviderFrame{messageType: wsconn.TextMessage, payload: []byte(fmt.Sprintf(`{"type":"conversation.item.input_audio_transcription.completed","item_id":%q,"usage":{"type":"tokens","input_tokens":7,"total_tokens":7}}`, item))}
	}
	close(queued)
	s.consumeProviderFrames(queued)
	s.close("end drain")
	if len(*owners) != 2 {
		t.Fatal("drain created a new billing owner")
	}
	for _, owner := range *owners {
		if owner.observeCount() != 1 || owner.finalizeCount() != 1 {
			t.Fatalf("already received evidence lost: observed=%d finalized=%d", owner.observeCount(), owner.finalizeCount())
		}
	}
}

func TestRealtimeManualAudioWithoutTranscriptionDoesNotUseWorkRPM(t *testing.T) {
	s, p, _ := inputWorkFixture(t, false)
	s.transcriptionModels = nil
	for _, kind := range []string{"input_audio_buffer.append", "input_audio_buffer.commit"} {
		if work, created, err := s.prepareInputWork(kind, nil); err != nil || created || work != nil {
			t.Fatalf("nonworking audio input: %s %v", kind, err)
		}
	}
	if p.count != 0 {
		t.Fatal("manual audio buffering consumed response work permission")
	}
}

func TestRealtimeRejectedAutomaticAdmissionKeepsQueuedResponseEvidence(t *testing.T) {
	s := newOpenAIRealtimeHelperSession()
	observer := &failingAdmissionOpenAIRealtimeObserver{admitErr: errors.New("principal revoked")}
	s.turnObserverFactory = func() runtimesession.TurnObserver { return observer }
	queued := make(chan openAIRealtimeProviderFrame, 3)
	for _, payload := range []string{
		`{"type":"response.created","response":{"id":"already-started"}}`,
		`{"type":"response.done","response":{"id":"already-started","status":"completed","usage":{"input_tokens":3,"output_tokens":4,"total_tokens":7}}}`,
		`{"type":"response.done","response":{"id":"not-admitted","status":"completed","usage":{"input_tokens":50,"output_tokens":50,"total_tokens":100}}}`,
	} {
		queued <- openAIRealtimeProviderFrame{messageType: wsconn.TextMessage, payload: []byte(payload)}
	}
	close(queued)
	s.consumeProviderFrames(queued)
	s.close("drained")
	admitted, _ := observer.counts()
	if admitted != 1 || observer.observeCount() != 1 || observer.finalizeCount() != 1 || observer.lastPayload().Usage.TotalTokens != 7 {
		t.Fatalf("revoked existing work lost or unknown work admitted: admitted=%d observed=%d finalized=%d", admitted, observer.observeCount(), observer.finalizeCount())
	}
}

func TestRealtimeMultipleAudioPartsShareOnePermitAndFinalizeByContentIndex(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		t.Run(fmt.Sprint(reverse), func(t *testing.T) {
			s, p, owners := inputWorkFixture(t, false)
			payload := []byte(`{"type":"conversation.item.create","item":{"type":"message","role":"user","content":[{"type":"input_audio","audio":"AAAA"},{"type":"input_text","text":"separator"},{"type":"input_audio","audio":"BBBB"}]}}`)
			work, created, err := s.prepareInputWork("conversation.item.create", payload)
			if err != nil || !created {
				t.Fatalf("multi audio admission: %v", err)
			}
			if len(*owners) != 2 || len(s.inputWorks) != 2 || p.count != 1 {
				t.Fatalf("one logical input did not own two parts: owners=%d pending=%d count=%d", len(*owners), len(s.inputWorks), p.count)
			}
			s.inputWriteFinished(work, "conversation.item.create")
			s.observeInputEvent("conversation.item.created", []byte(`{"type":"conversation.item.created","item":{"id":"multi-audio","content":[{"type":"input_audio"},{"type":"input_text"},{"type":"input_audio"}]}}`), nil)
			order := []int{0, 2}
			if reverse {
				order = []int{2, 0}
			}
			for _, index := range order {
				payload := []byte(fmt.Sprintf(`{"type":"conversation.item.input_audio_transcription.completed","item_id":"multi-audio","content_index":%d,"usage":{"type":"tokens","input_tokens":%d,"total_tokens":%d}}`, index, index+1, index+1))
				if event, close := s.observeSupplierMessage(wsconn.TextMessage, payload); close || event.err != nil {
					t.Fatalf("part completion: %+v", event)
				}
				s.observeSupplierMessage(wsconn.TextMessage, payload)
			}
			for i, owner := range *owners {
				if owner.observeCount() != 1 || owner.finalizeCount() != 1 || owner.lastPayload().Usage.InputTokens != 2*i+1 {
					t.Fatalf("part %d was duplicated or crossed: %+v", i, owner.lastPayload())
				}
			}
			if len(s.inputWorks) != 0 || len(s.recentInputResults) != 2 {
				t.Fatalf("multi part lifecycle remained pending: %d", len(s.inputWorks))
			}
		})
	}
}

func TestRealtimeMultipleAudioPartFailureClosesTheSubmittedBatch(t *testing.T) {
	s, _, owners := inputWorkFixture(t, false)
	payload := []byte(`{"type":"conversation.item.create","item":{"id":"multi-audio","content":[{"type":"input_audio"},{"type":"input_audio"}]}}`)
	work, _, err := s.prepareInputWork("conversation.item.create", payload)
	if err != nil {
		t.Fatal(err)
	}
	s.discardInput(work, "definitely_not_sent", false)
	s.close("final close")
	for _, owner := range *owners {
		if owner.rollbacks != 1 || owner.finalizeCount() != 0 {
			t.Fatal("whole unsent batch was not cancelled exactly once")
		}
	}
}

func TestRealtimeMultipleAudioCapacityFailsBeforeAnyUpstreamWrite(t *testing.T) {
	received := make(chan struct{}, 1)
	release := make(chan struct{})
	server := newOpenAIRealtimeTestServer(t, func(conn *openAIRealtimeTestConn) {
		if _, _, err := conn.ReadMessage(); err == nil {
			received <- struct{}{}
		}
		<-release
	})
	defer server.Close()
	defer close(release)
	provider := newOpenAIRealtimeTestProvider(server.URL)
	raw, apiErr := provider.OpenRealtimeSession("gpt-4o-realtime-preview")
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	s := raw.(*openAIRealtimeSession)
	defer s.Abort("cleanup")
	s.pendingInputLimit = 1
	models := runtimesession.ModelBinding{RequestedModel: "transcribe", ProviderModel: "transcribe", BillingModel: "transcribe"}
	s.transcriptionModels = &models
	policy := &realtimeInputTestPolicy{}
	s.workPolicy = policy
	owners := 0
	s.turnObserverFactory = func() runtimesession.TurnObserver { owners++; return &realtimeInputTestObserver{} }
	payload := []byte(`{"type":"conversation.item.create","item":{"content":[{"type":"input_audio","audio":"AAAA"},{"type":"input_audio","audio":"BBBB"}]}}`)
	if err := s.SendClient(context.Background(), openAITestTextFrame(payload)); err == nil {
		t.Fatal("two-part input exceeded capacity")
	}
	if owners != 0 || policy.count != 0 {
		t.Fatal("batch capacity failure reserved or counted work")
	}
	select {
	case <-received:
		t.Fatal("capacity failure sent the input")
	default:
	}
}

func TestRealtimeMultipleAudioCloseFinalizesOnlyRemainingPart(t *testing.T) {
	s, _, owners := inputWorkFixture(t, false)
	input := []byte(`{"type":"conversation.item.create","item":{"id":"partial-audio","content":[{"type":"input_audio"},{"type":"input_text"},{"type":"input_audio"}]}}`)
	work, _, err := s.prepareInputWork("conversation.item.create", input)
	if err != nil {
		t.Fatal(err)
	}
	s.inputWriteFinished(work, "conversation.item.create")
	completed := []byte(`{"type":"conversation.item.input_audio_transcription.completed","item_id":"partial-audio","content_index":0,"usage":{"type":"tokens","input_tokens":7,"total_tokens":7}}`)
	if event, close := s.observeSupplierMessage(wsconn.TextMessage, completed); close || event.err != nil {
		t.Fatalf("first part failed: %+v", event)
	}
	if len(s.inputWorks) != 1 || s.inputWorks[0].contentIndex != 2 {
		t.Fatal("completion removed the wrong part")
	}
	s.Abort("partial input close")
	s.observeSupplierMessage(wsconn.TextMessage, completed)
	if (*owners)[0].observeCount() != 1 || (*owners)[0].finalizeCount() != 1 || (*owners)[0].lastPayload().Usage.InputTokens != 7 {
		t.Fatal("close replayed the completed part")
	}
	if (*owners)[1].observeCount() != 0 || (*owners)[1].finalizeCount() != 1 || (*owners)[1].lastPayload().Usage != nil || (*owners)[1].rollbacks != 0 {
		t.Fatal("close did not finalize the submitted remaining part without estimating usage")
	}
	if len(s.inputWorks) != 0 || len(s.recentInputResults) != 2 {
		t.Fatal("partial input close left an owner pending")
	}
}

func TestRealtimeMultipleAudioAdmissionFailureCancelsEarlierPartWithoutSending(t *testing.T) {
	received := make(chan struct{}, 1)
	upstreamDone := make(chan struct{})
	server := newOpenAIRealtimeTestServer(t, func(conn *openAIRealtimeTestConn) {
		defer close(upstreamDone)
		if _, _, err := conn.ReadMessage(); err == nil {
			received <- struct{}{}
		}
	})
	defer server.Close()
	provider := newOpenAIRealtimeTestProvider(server.URL)
	raw, apiErr := provider.OpenRealtimeSession("gpt-4o-realtime-preview")
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	s := raw.(*openAIRealtimeSession)
	defer s.Abort("cleanup")
	models := runtimesession.ModelBinding{RequestedModel: "transcribe", ProviderModel: "transcribe", BillingModel: "transcribe"}
	s.transcriptionModels = &models
	policy := &realtimeInputTestPolicy{}
	s.workPolicy = policy
	first := &realtimeInputTestObserver{}
	second := &realtimeInputTestObserver{admissionErr: errors.New("second part reserve failed")}
	owners := []*realtimeInputTestObserver{first, second}
	created := 0
	s.turnObserverFactory = func() runtimesession.TurnObserver { owner := owners[created]; created++; return owner }
	input := []byte(`{"type":"conversation.item.create","item":{"content":[{"type":"input_audio","audio":"AAAA"},{"type":"input_audio","audio":"BBBB"}]}}`)
	if err := s.SendClient(context.Background(), openAITestTextFrame(input)); err == nil {
		t.Fatal("second part admission failure was ignored")
	}
	s.Abort("failed admission close")
	select {
	case <-upstreamDone:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream did not observe final close")
	}
	select {
	case <-received:
		t.Fatal("partially admitted input was sent upstream")
	default:
	}
	for _, owner := range owners {
		if owner.rollbacks != 1 || owner.finalizeCount() != 0 {
			t.Fatal("failed batch did not cancel each admitted owner exactly once")
		}
	}
	if created != 2 || policy.count != 1 || len(s.inputWorks) != 0 || len(s.recentInputResults) != 2 {
		t.Fatalf("failed batch left work: created=%d rpm=%d pending=%d recent=%d", created, policy.count, len(s.inputWorks), len(s.recentInputResults))
	}
}
