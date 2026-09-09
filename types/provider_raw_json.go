package types

// ProviderRawJSONState is transport-only state embedded by response types that
// can opt into bounded exact-wire capture and replay.
type ProviderRawJSONState struct {
	raw     []byte
	capture bool
	replay  bool
}

func (s *ProviderRawJSONState) SetProviderRawJSON(raw []byte) {
	if s != nil {
		s.raw = append(s.raw[:0], raw...)
	}
}

func (s *ProviderRawJSONState) ProviderRawJSON() []byte {
	if s == nil {
		return nil
	}
	return append([]byte(nil), s.raw...)
}

func (s *ProviderRawJSONState) EnableProviderRawJSONCapture() {
	if s != nil {
		s.capture = true
	}
}

func (s *ProviderRawJSONState) CaptureProviderRawJSON() bool {
	return s != nil && s.capture
}

func (s *ProviderRawJSONState) EnableProviderRawJSONReplay() {
	if s != nil {
		s.replay = true
	}
}

func (s *ProviderRawJSONState) ReplayProviderRawJSON() []byte {
	if s == nil || !s.replay {
		return nil
	}
	return s.ProviderRawJSON()
}
