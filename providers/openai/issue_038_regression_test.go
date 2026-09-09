package openai

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"one-api/common/wsconn"
	runtimesession "one-api/runtime/session"
)

func issue038Binding(public, provider, billing string) runtimesession.ModelBinding {
	return runtimesession.ModelBinding{RequestedModel: public, ProviderModel: provider, BillingModel: billing}
}

func issue038TranscriptionSettings(binding runtimesession.ModelBinding) *openAIRealtimeInputSettings {
	return &openAIRealtimeInputSettings{transcriptionSet: true, transcription: &binding}
}

func TestIssue038EverySessionUpdateHasAnOrderedConfirmationRecord(t *testing.T) {
	s := newOpenAIRealtimeHelperSession()
	s.transcriptionModels = func() *runtimesession.ModelBinding {
		binding := issue038Binding("whisper-public", "whisper-upstream", "whisper-price")
		return &binding
	}()

	payload, settings, err := s.prepareInputSettings([]byte(`{"type":"session.update","session":{"instructions":"A"}}`), "session.update")
	if err != nil {
		t.Fatal(err)
	}
	if settings == nil || settings.manualSet || settings.automaticDisabledSet || settings.transcriptionSet {
		t.Fatalf("instructions-only update did not produce an empty owned patch: payload=%s settings=%+v", payload, settings)
	}
	if got := s.beginInputSettings(settings, "event-a", "session.update"); got == nil {
		t.Fatal("instructions-only update was not tracked")
	}
	if len(s.pendingSettings) != 1 || s.pendingSettings[0].eventID != "event-a" {
		t.Fatalf("confirmation order record missing: %+v", s.pendingSettings)
	}
}

func TestIssue038AckAndErrorResolveOnlyMatchingUpdates(t *testing.T) {
	old := issue038Binding("whisper-public", "whisper-upstream", "whisper-price")
	next := issue038Binding("next-whisper", "next-upstream", "next-price")
	cases := []struct {
		name      string
		firstOK   bool
		secondErr bool
		ackSecond bool
		want      runtimesession.ModelBinding
	}{
		{name: "A accepted B rejected", firstOK: true, secondErr: true, want: old},
		{name: "A rejected B accepted", firstOK: false, ackSecond: true, want: next},
		{name: "both accepted with provider acknowledgement IDs", firstOK: true, ackSecond: true, want: next},
		{name: "B error arrives before A acknowledgement", firstOK: true, secondErr: true, want: old},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newOpenAIRealtimeHelperSession()
			s.transcriptionModels = &old
			first := s.beginInputSettings(&openAIRealtimeInputSettings{}, "event-a", "session.update")
			second := s.beginInputSettings(issue038TranscriptionSettings(next), "event-b", "session.update")

			if tc.secondErr {
				if !s.observeInputSettings("error", []byte(`{"type":"error","error":{"event_id":"event-b"}}`)) {
					t.Fatal("second error was not correlated")
				}
			}
			if tc.firstOK {
				if !s.observeInputSettings("session.updated", []byte(`{"type":"session.updated","event_id":"server-ack-a"}`)) {
					t.Fatal("first acknowledgement was not correlated")
				}
			} else if !s.observeInputSettings("error", []byte(`{"type":"error","error":{"event_id":"event-a"}}`)) {
				t.Fatal("first error was not correlated")
			}
			if tc.ackSecond {
				if !s.observeInputSettings("session.updated", []byte(`{"type":"session.updated","event_id":"server-ack-b"}`)) {
					t.Fatal("second acknowledgement was not correlated")
				}
			}

			if s.transcriptionModels == nil || *s.transcriptionModels != tc.want {
				t.Fatalf("confirmed model changed incorrectly: got=%+v want=%+v", s.transcriptionModels, tc.want)
			}
			if len(s.pendingSettings) != 0 {
				t.Fatalf("resolved settings remained queued: %+v", s.pendingSettings)
			}
			// A late settings error is left for the ordinary provider/owner error
			// path once its settings update has drained; it must not be swallowed
			// merely because its client ID appeared in an old settings update.
			if s.observeInputSettings("error", []byte(`{"type":"error","error":{"event_id":"event-b"}}`)) {
				t.Fatal("late correlated error was swallowed as a settings duplicate")
			}
			if s.transcriptionModels == nil || *s.transcriptionModels != tc.want {
				t.Fatalf("duplicate error changed confirmed model: got=%+v want=%+v", s.transcriptionModels, tc.want)
			}
			_ = first
			_ = second
		})
	}
}

func TestIssue038OnlyPendingASRUpdateCanCommitAndDuplicateAckIsHarmless(t *testing.T) {
	old := issue038Binding("whisper-public", "whisper-upstream", "whisper-price")
	next := issue038Binding("next-whisper", "next-upstream", "next-price")
	s := newOpenAIRealtimeHelperSession()
	s.transcriptionModels = &old
	s.beginInputSettings(issue038TranscriptionSettings(next), "event-b", "session.update")
	if !s.observeInputSettings("error", []byte(`{"type":"error","error":{"event_id":"event-b"}}`)) {
		t.Fatal("only pending ASR error was not correlated")
	}
	if s.transcriptionModels == nil || *s.transcriptionModels != old || len(s.pendingSettings) != 0 {
		t.Fatalf("rejected only pending ASR update changed state: model=%+v pending=%+v", s.transcriptionModels, s.pendingSettings)
	}

	s = newOpenAIRealtimeHelperSession()
	s.transcriptionModels = &old
	s.beginInputSettings(issue038TranscriptionSettings(next), "event-b", "session.update")
	ack := []byte(`{"type":"session.updated","event_id":"server-ack-b"}`)
	if !s.observeInputSettings("session.updated", ack) {
		t.Fatal("ASR acknowledgement was not correlated")
	}
	if s.transcriptionModels == nil || *s.transcriptionModels != next {
		t.Fatalf("successful ASR update did not commit: model=%+v", s.transcriptionModels)
	}
	if !s.observeInputSettings("session.updated", ack) {
		t.Fatal("duplicate acknowledgement was not consumed")
	}
	if s.transcriptionModels == nil || *s.transcriptionModels != next || len(s.pendingSettings) != 0 {
		t.Fatalf("duplicate acknowledgement changed committed state: model=%+v pending=%+v", s.transcriptionModels, s.pendingSettings)
	}
}

func TestIssue038RejectsReusedClientEventIDForOwnedSettings(t *testing.T) {
	s := newOpenAIRealtimeHelperSession()
	first := s.beginInputSettings(&openAIRealtimeInputSettings{}, "same-client-id", "session.update")
	if first == nil {
		t.Fatal("first settings update was not tracked")
	}
	if duplicate := s.beginInputSettings(issue038TranscriptionSettings(issue038Binding("next", "next", "next")), "same-client-id", "session.update"); duplicate != nil {
		t.Fatal("simultaneous reused client event_id was accepted")
	}
	if !s.observeInputSettings("session.updated", []byte(`{"type":"session.updated","event_id":"server-ack"}`)) {
		t.Fatal("first settings acknowledgement was not consumed")
	}
	if reused := s.beginInputSettings(&openAIRealtimeInputSettings{}, "same-client-id", "session.update"); reused != nil {
		t.Fatal("client event_id was reusable after the first update completed")
	}
}

func TestIssue038SendRejectsReusedClientEventIDBeforeSecondWrite(t *testing.T) {
	received := make(chan []byte, 1)
	secondReceived := make(chan []byte, 1)
	noSecond := make(chan struct{})
	release := make(chan struct{})
	server := newOpenAIRealtimeTestServer(t, func(conn *openAIRealtimeTestConn) {
		_, payload, err := conn.ReadMessage()
		if err != nil {
			t.Errorf("read first settings update: %v", err)
			close(noSecond)
			return
		}
		received <- payload
		if err := conn.WriteMessage(wsconn.TextMessage, []byte(`{"type":"session.updated","event_id":"server-ack"}`)); err != nil {
			t.Errorf("write settings acknowledgement: %v", err)
			close(noSecond)
			return
		}
		_ = conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		_, second, err := conn.ReadMessage()
		if err == nil {
			secondReceived <- second
		} else {
			close(noSecond)
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
	defer s.Abort("test_cleanup")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	payload := []byte(`{"type":"session.update","event_id":"same-client-id","session":{"instructions":"A"}}`)
	if err := s.SendClient(ctx, openAITestTextFrame(payload)); err != nil {
		t.Fatal(err)
	}
	select {
	case first := <-received:
		if string(first) == "" {
			t.Fatal("first settings update was empty")
		}
	case <-ctx.Done():
		t.Fatal("upstream did not observe the first settings update")
	}
	if _, _, _, _, err := openAITestRecv(ctx, s); err != nil {
		t.Fatalf("first settings acknowledgement failed: %v", err)
	}
	if err := s.SendClient(ctx, openAITestTextFrame(payload)); err == nil {
		t.Fatal("reused client event_id was sent a second time")
	}
	select {
	case duplicate := <-secondReceived:
		t.Fatalf("reused settings event reached upstream: %s", duplicate)
	case <-noSecond:
	}
}

func TestIssue038SettingsIDCannotBeReusedByResponseOrInput(t *testing.T) {
	for _, test := range []struct {
		name    string
		payload string
	}{
		{name: "response.create", payload: `{"type":"response.create","event_id":"same-client-id","response":{"input":[]}}`},
		{name: "input_audio_buffer.commit", payload: `{"type":"input_audio_buffer.commit","event_id":"same-client-id"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			firstSeen := make(chan struct{})
			secondSeen := make(chan []byte, 1)
			release := make(chan struct{})
			server := newOpenAIRealtimeTestServer(t, func(conn *openAIRealtimeTestConn) {
				if _, _, err := conn.ReadMessage(); err != nil {
					t.Errorf("read settings update: %v", err)
					return
				}
				close(firstSeen)
				if err := conn.WriteMessage(wsconn.TextMessage, []byte(`{"type":"session.updated","event_id":"server-settings"}`)); err != nil {
					t.Errorf("write settings acknowledgement: %v", err)
					return
				}
				_ = conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
				if _, payload, err := conn.ReadMessage(); err == nil {
					secondSeen <- payload
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
			defer s.Abort("test_cleanup")
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			first := []byte(`{"type":"session.update","event_id":"same-client-id","session":{"instructions":"A"}}`)
			if err := s.SendClient(ctx, openAITestTextFrame(first)); err != nil {
				t.Fatal(err)
			}
			select {
			case <-firstSeen:
			case <-ctx.Done():
				t.Fatal("upstream did not observe settings update")
			}
			if _, _, _, _, err := openAITestRecv(ctx, s); err != nil {
				t.Fatalf("settings acknowledgement failed: %v", err)
			}
			if err := s.SendClient(ctx, openAITestTextFrame([]byte(test.payload))); err == nil {
				t.Fatalf("%s reused a settings event_id", test.name)
			}
			select {
			case payload := <-secondSeen:
				t.Fatalf("reused settings ID reached upstream in %s: %s", test.name, payload)
			case <-time.After(550 * time.Millisecond):
			}
		})
	}
}

func TestIssue038ActiveInputIDCannotBeReusedBySettings(t *testing.T) {
	firstSeen := make(chan struct{})
	secondSeen := make(chan []byte, 1)
	release := make(chan struct{})
	server := newOpenAIRealtimeTestServer(t, func(conn *openAIRealtimeTestConn) {
		if _, _, err := conn.ReadMessage(); err != nil {
			t.Errorf("read active input commit: %v", err)
			return
		}
		close(firstSeen)
		_ = conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		if _, payload, err := conn.ReadMessage(); err == nil {
			secondSeen <- payload
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
	defer s.Abort("test_cleanup")
	old := issue038Binding("old-whisper", "old-whisper-upstream", "old-whisper-price")
	s.transcriptionModels = &old
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	first := []byte(`{"type":"input_audio_buffer.commit","event_id":"same-client-id"}`)
	if err := s.SendClient(ctx, openAITestTextFrame(first)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-firstSeen:
	case <-ctx.Done():
		t.Fatal("upstream did not observe active input commit")
	}
	second := []byte(`{"type":"session.update","event_id":"same-client-id","session":{"input_audio_transcription":{"model":"next-whisper"}}}`)
	if err := s.SendClient(ctx, openAITestTextFrame(second)); err == nil {
		t.Fatal("settings update reused an active input event_id")
	}
	select {
	case payload := <-secondSeen:
		t.Fatalf("settings reuse reached upstream while input was active: %s", payload)
	case <-time.After(550 * time.Millisecond):
	}
}

func TestIssue038LongProviderAcknowledgementIDUsesFixedDedupKey(t *testing.T) {
	s := newOpenAIRealtimeHelperSession()
	settings := &openAIRealtimeInputSettings{}
	if s.beginInputSettings(settings, "client-settings", "session.update") == nil {
		t.Fatal("settings update was not tracked")
	}
	longID := strings.Repeat("s", openAIRealtimeIdentifierMaxBytes*1024)
	ack := []byte(`{"type":"session.updated","event_id":"` + longID + `"}`)
	if !s.observeInputSettings("session.updated", ack) {
		t.Fatal("long provider acknowledgement was not accepted")
	}
	if len(s.usedSettingsAckIDs) != 1 || len(s.usedSettingsAckIDs[0]) != sha256.Size {
		t.Fatalf("provider acknowledgement ID was retained without a fixed key: count=%d key-bytes=%d", len(s.usedSettingsAckIDs), len(s.usedSettingsAckIDs[0]))
	}
	if !s.observeInputSettings("session.updated", ack) {
		t.Fatal("duplicate long provider acknowledgement was not consumed")
	}
}

func TestIssue038NonSettingsEventsDoNotConsumeSettingsIDCapacity(t *testing.T) {
	const total = 257
	received := make(chan struct{})
	release := make(chan struct{})
	server := newOpenAIRealtimeTestServer(t, func(conn *openAIRealtimeTestConn) {
		for i := 0; i < total; i++ {
			if _, _, err := conn.ReadMessage(); err != nil {
				t.Errorf("read non-settings event %d: %v", i, err)
				return
			}
		}
		close(received)
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
	defer s.Abort("test_cleanup")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for i := 0; i < total; i++ {
		payload := fmt.Sprintf(`{"type":"input_audio_buffer.append","event_id":"ordinary-%d","audio":"AAAA"}`, i)
		if err := s.SendClient(ctx, openAITestTextFrame([]byte(payload))); err != nil {
			t.Fatalf("non-settings event %d was rejected: %v", i, err)
		}
	}
	select {
	case <-received:
	case <-ctx.Done():
		t.Fatalf("upstream did not observe all %d non-settings events", total)
	}
}

func TestIssue038EventIDFreeAcknowledgementsKeepMixedUpdateOrder(t *testing.T) {
	s := newOpenAIRealtimeHelperSession()
	old := issue038Binding("whisper-public", "whisper-upstream", "whisper-price")
	next := issue038Binding("next-whisper", "next-upstream", "next-price")
	s.transcriptionModels = &old
	s.beginInputSettings(issue038TranscriptionSettings(next), "transcription-a", "transcription_session.update")
	s.beginInputSettings(&openAIRealtimeInputSettings{}, "session-b", "session.update")
	if !s.observeInputSettings("transcription_session.updated", []byte(`{"type":"transcription_session.updated"}`)) {
		t.Fatal("first mixed-protocol acknowledgement was not consumed")
	}
	if s.transcriptionModels == nil || *s.transcriptionModels != next || len(s.pendingSettings) != 1 {
		t.Fatalf("first mixed update did not commit in order: model=%+v pending=%+v", s.transcriptionModels, s.pendingSettings)
	}
	if !s.observeInputSettings("session.updated", []byte(`{"type":"session.updated"}`)) {
		t.Fatal("second mixed-protocol acknowledgement was not consumed")
	}
	if len(s.pendingSettings) != 0 {
		t.Fatalf("mixed update queue remained pending: %+v", s.pendingSettings)
	}

	s = newOpenAIRealtimeHelperSession()
	s.beginInputSettings(&openAIRealtimeInputSettings{}, "session-a", "session.update")
	s.beginInputSettings(issue038TranscriptionSettings(next), "transcription-b", "transcription_session.update")
	if s.observeInputSettings("transcription_session.updated", []byte(`{"type":"transcription_session.updated"}`)) {
		t.Fatal("acknowledgement skipped an earlier incompatible update")
	}
	if len(s.pendingSettings) != 2 || s.pendingSettings[0].resolved {
		t.Fatalf("incompatible mixed acknowledgement changed queue: %+v", s.pendingSettings)
	}
}

func TestIssue038UnknownErrorDoesNotResolvePendingConfiguration(t *testing.T) {
	s := newOpenAIRealtimeHelperSession()
	old := issue038Binding("whisper-public", "whisper-upstream", "whisper-price")
	next := issue038Binding("next-whisper", "next-upstream", "next-price")
	s.transcriptionModels = &old
	s.beginInputSettings(issue038TranscriptionSettings(next), "event-b", "session.update")
	if s.observeInputSettings("error", []byte(`{"type":"error","error":{"event_id":"other-event"}}`)) {
		t.Fatal("error for another update resolved the pending configuration")
	}
	if len(s.pendingSettings) != 1 || s.pendingSettings[0].resolved {
		t.Fatalf("unknown error altered pending state: %+v", s.pendingSettings)
	}
}

func TestIssue038InputWorkReadsCommittedModelWhenCalledAfterSettingsAdmission(t *testing.T) {
	s := newOpenAIRealtimeHelperSession()
	old := issue038Binding("whisper-public", "whisper-upstream", "whisper-price")
	next := issue038Binding("next-whisper", "next-upstream", "next-price")
	s.transcriptionModels = &old
	s.beginInputSettings(issue038TranscriptionSettings(next), "event-b", "session.update")

	// prepareInputWork is an internal helper; the public SendClient path calls
	// waitInputSettings first. This direct assertion protects the narrower
	// invariant that it reads the committed owner, never a pending draft.
	work, created, err := s.prepareInputWork("input_audio_buffer.commit", []byte(`{"type":"input_audio_buffer.commit"}`))
	if err != nil || !created || work == nil {
		t.Fatalf("input admission failed while configuration was pending: created=%v work=%+v err=%v", created, work, err)
	}
	if work.transcriptionModels != old {
		t.Fatalf("input used pending configuration: got=%+v want=%+v", work.transcriptionModels, old)
	}
	s.discardInput(work, "test_cleanup", false)
}

func TestIssue038PublicInputWaitsForSettingsConfirmationBeforeWriting(t *testing.T) {
	settingsSeen := make(chan struct{})
	allowAcknowledgement := make(chan struct{}, 1)
	commitSeen := make(chan []byte, 1)
	server := newOpenAIRealtimeTestServer(t, func(conn *openAIRealtimeTestConn) {
		_, _, err := conn.ReadMessage()
		if err != nil {
			t.Errorf("read pending settings update: %v", err)
			return
		}
		close(settingsSeen)
		select {
		case <-allowAcknowledgement:
			if err := conn.WriteMessage(wsconn.TextMessage, []byte(`{"type":"session.updated","event_id":"server-settings"}`)); err != nil {
				t.Errorf("write settings acknowledgement: %v", err)
				return
			}
		case <-time.After(time.Second):
			return
		}
		_, payload, err := conn.ReadMessage()
		if err == nil {
			commitSeen <- payload
		}
	})
	defer server.Close()
	provider := newOpenAIRealtimeTestProvider(server.URL)
	raw, apiErr := provider.OpenRealtimeSession("gpt-4o-realtime-preview")
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	s := raw.(*openAIRealtimeSession)
	defer s.Abort("test_cleanup")
	old := issue038Binding("old-whisper", "old-whisper-upstream", "old-whisper-price")
	s.transcriptionModels = &old
	next := issue038Binding("next-whisper", "next-whisper", "next-whisper")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.SendClient(ctx, openAITestTextFrame([]byte(`{"type":"session.update","event_id":"new-settings","session":{"input_audio_transcription":{"model":"next-whisper"}}}`))); err != nil {
		t.Fatal(err)
	}
	select {
	case <-settingsSeen:
	case <-ctx.Done():
		t.Fatal("upstream did not observe settings update")
	}
	ackDone := make(chan error, 1)
	go func() {
		_, _, _, _, err := openAITestRecv(ctx, s)
		ackDone <- err
	}()
	commitDone := make(chan error, 1)
	go func() {
		commitDone <- s.SendClient(ctx, openAITestTextFrame([]byte(`{"type":"input_audio_buffer.commit"}`)))
	}()
	select {
	case err := <-commitDone:
		t.Fatalf("public input write bypassed pending settings: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	allowAcknowledgement <- struct{}{}
	if err := <-ackDone; err != nil {
		t.Fatalf("settings acknowledgement failed: %v", err)
	}
	if err := <-commitDone; err != nil {
		t.Fatalf("input write after settings acknowledgement failed: %v", err)
	}
	select {
	case payload := <-commitSeen:
		var event struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(payload, &event) != nil || event.Type != "input_audio_buffer.commit" {
			t.Fatalf("unexpected post-confirmation input payload: %s", payload)
		}
	case <-ctx.Done():
		t.Fatal("upstream did not observe input after settings acknowledgement")
	}
	s.mu.Lock()
	got := s.transcriptionModels
	s.mu.Unlock()
	if got == nil || *got != next {
		t.Fatalf("confirmed settings were not applied before public input: got=%+v want=%+v", got, next)
	}
}
