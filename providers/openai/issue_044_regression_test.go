package openai

import (
	"testing"

	"one-api/common/wsconn"
)

func issue044PendingInputs(session *openAIRealtimeSession) int {
	session.mu.Lock()
	defer session.mu.Unlock()
	return len(session.inputWorks)
}

func TestIssue044VADSourceWinsWhenManualCommitFollowsProviderSpeech(t *testing.T) {
	s, _, _ := inputWorkFixture(t, true)
	s.transcriptionModels = nil
	work, created, err := s.prepareInputWork("input_audio_buffer.append", []byte(`{"type":"input_audio_buffer.append","audio":"AAAA"}`))
	if err != nil || !created || work == nil {
		t.Fatalf("create VAD input work: created=%v work=%+v err=%v", created, work, err)
	}
	s.inputWriteFinished(work, "input_audio_buffer.append")
	commitWork, created, err := s.prepareInputWork("input_audio_buffer.commit", []byte(`{"type":"input_audio_buffer.commit"}`))
	if err != nil || created || commitWork != work {
		t.Fatalf("reuse VAD input work for manual commit: created=%v work=%p want=%p err=%v", created, commitWork, work, err)
	}
	s.mu.Lock()
	for _, part := range s.submissionPartsLocked(work) {
		part.manualCommitCandidate = true
		part.manualCommitEventID = "manual-commit"
		part.manualCommitSent = true
	}
	s.mu.Unlock()
	if _, err := s.observeInputEvent("input_audio_buffer.speech_started", []byte(`{"type":"input_audio_buffer.speech_started","item_id":"auto-item"}`), nil); err != nil {
		t.Fatal(err)
	}
	s.inputWriteFinished(commitWork, "input_audio_buffer.commit")
	if issue044PendingInputs(s) != 1 || !work.automaticResponse || !work.automaticSourceSeen {
		t.Fatalf("provider VAD source was released by manual commit: pending=%d work=%+v", issue044PendingInputs(s), work)
	}
	if _, closeSession := s.observeSupplierMessage(wsconn.TextMessage, []byte(`{"type":"response.created","response":{"id":"auto-response"}}`)); closeSession {
		t.Fatal("automatic response unexpectedly closed session")
	}
	if issue044PendingInputs(s) != 0 || !work.responseClaimed {
		t.Fatalf("automatic response did not claim original input work: pending=%d work=%+v", issue044PendingInputs(s), work)
	}
}

func TestIssue044ProviderCommitBeforeManualWriteKeepsAutomaticOwner(t *testing.T) {
	s, _, _ := inputWorkFixture(t, true)
	s.transcriptionModels = nil
	work, created, err := s.prepareInputWork("input_audio_buffer.append", []byte(`{"type":"input_audio_buffer.append","audio":"AAAA"}`))
	if err != nil || !created || work == nil {
		t.Fatalf("create VAD input work: created=%v work=%+v err=%v", created, work, err)
	}
	s.inputWriteFinished(work, "input_audio_buffer.append")
	commitWork, created, err := s.prepareInputWork("input_audio_buffer.commit", []byte(`{"type":"input_audio_buffer.commit"}`))
	if err != nil || created || commitWork != work {
		t.Fatalf("reuse VAD input work for delayed manual commit: created=%v work=%p want=%p err=%v", created, commitWork, work, err)
	}
	// The read pump observed the provider VAD commit after prepareInputWork
	// captured A but before the client commit reached the socket.
	if _, err := s.observeInputEvent("input_audio_buffer.committed", []byte(`{"type":"input_audio_buffer.committed","item_id":"auto-item"}`), nil); err != nil {
		t.Fatal(err)
	}
	s.inputWriteFinished(commitWork, "input_audio_buffer.commit")
	if issue044PendingInputs(s) != 1 || !work.automaticResponse || !work.automaticSourceSeen {
		t.Fatalf("provider VAD owner was released by a late manual write: pending=%d work=%+v", issue044PendingInputs(s), work)
	}
	if _, closeSession := s.observeSupplierMessage(wsconn.TextMessage, []byte(`{"type":"response.created","response":{"id":"auto-response"}}`)); closeSession {
		t.Fatal("automatic response unexpectedly closed session")
	}
	if issue044PendingInputs(s) != 0 || !work.responseClaimed {
		t.Fatalf("automatic response did not claim captured input: pending=%d work=%+v", issue044PendingInputs(s), work)
	}
}

func TestIssue044ManualCommitWaitsForLateASRAndThenReleases(t *testing.T) {
	s, _, owners := inputWorkFixture(t, true)
	work, created, err := s.prepareInputWork("input_audio_buffer.append", []byte(`{"type":"input_audio_buffer.append","audio":"AAAA"}`))
	if err != nil || !created || work == nil {
		t.Fatalf("create ASR input work: created=%v work=%+v err=%v", created, work, err)
	}
	s.inputWriteFinished(work, "input_audio_buffer.append")
	if commitWork, created, err := s.prepareInputWork("input_audio_buffer.commit", []byte(`{"type":"input_audio_buffer.commit"}`)); err != nil || created || commitWork != work {
		t.Fatalf("reuse ASR input work for commit: created=%v work=%p want=%p err=%v", created, commitWork, work, err)
	}
	// The client-side commit is now known to own this buffer. The provider
	// acknowledgement is deliberately delayed so ASR remains the last owner.
	s.mu.Lock()
	for _, part := range s.submissionPartsLocked(work) {
		part.manualCommitCandidate = true
		part.manualCommitEventID = "manual-commit"
		part.manualCommitSent = true
	}
	s.mu.Unlock()
	s.mu.Lock()
	s.audioWork = nil
	s.mu.Unlock()
	s.observeInputEvent("input_audio_buffer.committed", []byte(`{"type":"input_audio_buffer.committed","item_id":"late-asr-item"}`), nil)
	if issue044PendingInputs(s) != 1 {
		t.Fatalf("manual input released before ASR terminal: pending=%d", issue044PendingInputs(s))
	}
	// The explicit response is the only unambiguous owner signal for a
	// client commit whose provider acknowledgement arrived without VAD
	// markers. Resolve that input before allowing late ASR to finalize it.
	s.mu.Lock()
	s.manualResponseInput = work
	s.mu.Unlock()
	s.resolveManualResponseInput()
	s.observeSupplierMessage(wsconn.TextMessage, []byte(`{"type":"conversation.item.input_audio_transcription.completed","item_id":"late-asr-item","content_index":0,"usage":{"type":"duration","seconds":1}}`))
	if issue044PendingInputs(s) != 0 {
		t.Fatalf("late ASR completion did not release input: pending=%d", issue044PendingInputs(s))
	}
	if len(*owners) != 1 || (*owners)[0].finalizeCount() != 1 || (*owners)[0].observeCount() != 1 {
		t.Fatalf("late ASR owner finalized incorrectly: owners=%d observe=%d finalize=%d", len(*owners), (*owners)[0].observeCount(), (*owners)[0].finalizeCount())
	}
}

func TestIssue044CommitErrorAfterVADCommitKeepsAutomaticOwner(t *testing.T) {
	s, _, _ := inputWorkFixture(t, true)
	s.transcriptionModels = nil
	work, created, err := s.prepareInputWork("input_audio_buffer.append", []byte(`{"type":"input_audio_buffer.append","audio":"AAAA"}`))
	if err != nil || !created || work == nil {
		t.Fatalf("create VAD input work: created=%v work=%+v err=%v", created, work, err)
	}
	s.inputWriteFinished(work, "input_audio_buffer.append")
	s.mu.Lock()
	work.manualCommitCandidate = true
	work.manualCommitEventID = "manual-empty-commit"
	work.manualCommitSent = true
	s.mu.Unlock()
	if _, err := s.observeInputEvent("input_audio_buffer.committed", []byte(`{"type":"input_audio_buffer.committed","item_id":"auto-item"}`), nil); err != nil {
		t.Fatal(err)
	}
	if handled, err := s.observeInputEvent("error", []byte(`{"type":"error","error":{"event_id":"manual-empty-commit"}}`), nil); !handled || err != nil {
		t.Fatalf("empty manual commit error was not retained as provider-owned input: handled=%v err=%v", handled, err)
	}
	if issue044PendingInputs(s) != 1 || !work.automaticResponse || !work.automaticSourceSeen {
		t.Fatalf("manual empty commit error rejected automatic owner: pending=%d work=%+v", issue044PendingInputs(s), work)
	}
}

func TestIssue044CloseAndClearRemainIdempotent(t *testing.T) {
	s, _, owners := inputWorkFixture(t, false)
	work, created, err := s.prepareInputWork("input_audio_buffer.commit", []byte(`{"type":"input_audio_buffer.commit"}`))
	if err != nil || !created || work == nil {
		t.Fatalf("create manual input work: created=%v work=%+v err=%v", created, work, err)
	}
	s.inputWriteFinished(work, "input_audio_buffer.commit")
	s.discardInput(work, "input_audio_buffer.clear", true)
	s.discardInput(work, "input_audio_buffer.clear", true)
	s.Abort("issue044_close")
	if issue044PendingInputs(s) != 0 || len(*owners) != 1 || (*owners)[0].finalizeCount() != 1 {
		t.Fatalf("clear/close was not idempotent: pending=%d owners=%d finalized=%d", issue044PendingInputs(s), len(*owners), (*owners)[0].finalizeCount())
	}
	if (*owners)[0].observeCount() != 0 {
		t.Fatal("clear path invented ASR usage")
	}
}
