package base

// ChatAudioResourceFact contains only routing/ownership metadata, never audio.
// Index associates a later expires_at observation with a previously seen ID.
type ChatAudioResourceFact struct {
	Index     *int
	ID        string
	ExpiresAt *int64
}

// ChatAudioOwnerSupport keeps SQL ownership in relay. Prepare is called for the
// final request before sending; commit is a barrier before exposing real IDs.
type ChatAudioOwnerSupport interface {
	SetChatAudioOwnerPolicy(prepare func(int) error, commit func([]ChatAudioResourceFact) error)
}
