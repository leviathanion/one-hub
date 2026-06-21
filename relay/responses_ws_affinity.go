package relay

import (
	"errors"
	"fmt"
	"one-api/common/config"
	runtimeaffinity "one-api/runtime/channelaffinity"
	"one-api/types"
	"strings"

	"github.com/gin-gonic/gin"
)

type ResponsesTurnAffinity struct {
	State              *channelAffinityState
	PreviousResponseID string
	SelectedChannelID  int
	ExplicitPinID      int
}

type ResponsesAffinityInput struct {
	Context *gin.Context
	Request *types.OpenAIResponsesRequest
}

func PrepareResponsesTurnAffinity(input ResponsesAffinityInput) (*ResponsesTurnAffinity, error) {
	if input.Context == nil || input.Request == nil {
		return &ResponsesTurnAffinity{}, nil
	}
	prepareResponsesChannelAffinity(input.Context, input.Request)
	state := currentChannelAffinityState(input.Context)
	pin := explicitChannelPinID(input.Context)
	if pin > 0 && state != nil && state.Hit && state.PreferredChannelID > 0 && state.PreferredChannelID != pin {
		return nil, fmt.Errorf("explicit channel pin #%d conflicts with responses affinity owner #%d", pin, state.PreferredChannelID)
	}
	return &ResponsesTurnAffinity{
		State:              state,
		PreviousResponseID: strings.TrimSpace(input.Request.PreviousResponseID),
		ExplicitPinID:      pin,
	}, nil
}

func CommitResponsesTurnAffinity(candidate *ResponsesTurnAffinity, selectedChannelID int) *ResponsesTurnAffinity {
	if candidate == nil {
		return nil
	}
	committed := *candidate
	committed.SelectedChannelID = selectedChannelID
	return &committed
}

func RecordResponsesTurnSuccess(c *gin.Context, active *ResponsesTurnAffinity, final *types.OpenAIResponsesResponses) {
	if active == nil || active.SelectedChannelID <= 0 {
		return
	}
	if active.ExplicitPinID <= 0 {
		recordResponsesChannelAffinity(c, active.SelectedChannelID, final)
		return
	}
	recordPinnedResponsesDerivedAffinity(active, final)
}

func ClearResponsesTurnStaleBindings(activeOrCandidate *ResponsesTurnAffinity, ownerChannelID int) {
	if activeOrCandidate == nil || activeOrCandidate.State == nil || ownerChannelID <= 0 {
		return
	}
	manager := channelAffinityManager()
	deleted := map[string]struct{}{}
	for _, binding := range activeOrCandidate.State.RequestBindings {
		if binding == nil || strings.TrimSpace(binding.Key) == "" {
			continue
		}
		if _, ok := deleted[binding.Key]; ok {
			continue
		}
		record, ok := manager.Get(binding.Key)
		if ok && record.ChannelID == ownerChannelID {
			manager.Delete(binding.Key)
			deleted[binding.Key] = struct{}{}
		}
	}
}

func ClearResponsesTurnContinuationMissBindings(activeOrCandidate *ResponsesTurnAffinity, ownerChannelID int, attemptedPreviousResponseID string) {
	ClearResponsesTurnStaleBindings(activeOrCandidate, ownerChannelID)
	if activeOrCandidate == nil || activeOrCandidate.State == nil || ownerChannelID <= 0 {
		return
	}
	attemptedPreviousResponseID = strings.TrimSpace(attemptedPreviousResponseID)
	if attemptedPreviousResponseID == "" {
		return
	}
	manager := channelAffinityManager()
	deleted := map[string]struct{}{}
	for _, recorder := range activeOrCandidate.State.DerivedRecorders[config.ChannelAffinityAliasResponseID] {
		key := recorder.BuildKey(attemptedPreviousResponseID)
		if strings.TrimSpace(key) == "" {
			continue
		}
		if _, ok := deleted[key]; ok {
			continue
		}
		record, ok := manager.Get(key)
		if ok && record.ChannelID == ownerChannelID {
			manager.Delete(key)
			deleted[key] = struct{}{}
		}
	}
}

func responsesAffinityOwnerConflict(candidate *ResponsesTurnAffinity, currentChannelID int) error {
	if candidate == nil || candidate.State == nil || currentChannelID <= 0 {
		return nil
	}
	if candidate.State.Hit && candidate.State.PreferredChannelID > 0 && candidate.State.PreferredChannelID != currentChannelID {
		return errors.New("responses continuation owner is bound to a different channel")
	}
	return nil
}

func recordPinnedResponsesDerivedAffinity(active *ResponsesTurnAffinity, response *types.OpenAIResponsesResponses) {
	if active == nil || active.State == nil || response == nil || active.SelectedChannelID <= 0 {
		return
	}
	manager := channelAffinityManager()
	for alias, value := range derivedResponseAffinityValues(response) {
		if value == "" {
			continue
		}
		for _, recorder := range active.State.DerivedRecorders[alias] {
			key := recorder.BuildKey(value)
			if key == "" {
				continue
			}
			manager.SetRecord(key, runtimeAffinityRecord(active.State, active.SelectedChannelID), recorder.TTL)
		}
	}
}

func runtimeAffinityRecord(state *channelAffinityState, channelID int) runtimeaffinity.Record {
	return runtimeaffinity.Record{
		ChannelID:         channelID,
		ResumeFingerprint: channelAffinityStateResumeFingerprint(state),
	}
}
