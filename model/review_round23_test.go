package model

import (
	"testing"
	"time"

	"one-api/common/config"
)

func blockRound23Invalidation(t *testing.T, expected []int) (<-chan struct{}, chan<- struct{}) {
	t.Helper()
	original := invalidateChannelCodexDerivedCaches
	started := make(chan struct{})
	unblock := make(chan struct{})
	invalidateChannelCodexDerivedCaches = func(ids []int) {
		if len(ids) != len(expected) {
			t.Errorf("unexpected invalidation ids: got %v want %v", ids, expected)
		} else {
			for i := range ids {
				if ids[i] != expected[i] {
					t.Errorf("unexpected invalidation ids: got %v want %v", ids, expected)
					break
				}
			}
		}
		close(started)
		<-unblock
	}
	t.Cleanup(func() { invalidateChannelCodexDerivedCaches = original })
	return started, unblock
}

func TestUpdateRawPublishesCompleteSnapshotBeforeCacheCleanup(t *testing.T) {
	useTestChannelDB(t)
	snapshot := snapshotTestChannelGroup(t)
	t.Cleanup(func() { restoreTestChannelGroup(t, snapshot) })
	insertTestChannel(t, &Channel{Id: 23001, Type: config.ChannelTypeCodex, Status: config.ChannelStatusEnabled, Name: "old", Key: `{"access_token":"old"}`, Group: "default", Models: "gpt-5"})
	requireChannelGroupLoad(t)
	started, unblock := blockRound23Invalidation(t, []int{23001})

	done := make(chan error, 1)
	go func() { done <- (&Channel{Id: 23001, Name: "new"}).UpdateRaw(false) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("cache invalidation did not start")
	}
	channel, err := ChannelGroup.Next("default", "gpt-5")
	if err != nil || channel == nil || channel.Name != "new" {
		t.Fatalf("routing did not publish committed snapshot before cache cleanup: channel=%+v err=%v", channel, err)
	}
	close(unblock)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("raw update did not finish")
	}
	if channel := ChannelGroup.GetChannel(23001); channel == nil || channel.Name != "new" {
		t.Fatalf("chooser did not reload new config: %+v", channel)
	}
}

func TestBatchDelModelPublishesCompleteSnapshotBeforeCacheCleanup(t *testing.T) {
	useTestChannelDB(t)
	snapshot := snapshotTestChannelGroup(t)
	t.Cleanup(func() { restoreTestChannelGroup(t, snapshot) })
	for _, id := range []int{23002, 23003} {
		insertTestChannel(t, &Channel{Id: id, Type: config.ChannelTypeCodex, Status: config.ChannelStatusEnabled, Name: "batch", Key: `{"access_token":"old"}`, Group: "default", Models: "gpt-5,gpt-5-mini"})
	}
	requireChannelGroupLoad(t)
	started, unblock := blockRound23Invalidation(t, []int{23002, 23003})

	done := make(chan error, 1)
	go func() {
		_, err := BatchDelModelChannels(&BatchDelModelChannelsParams{Ids: []int{23002, 23003}, Value: "gpt-5"})
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("cache invalidation did not start")
	}
	if _, err := ChannelGroup.Next("default", "gpt-5-mini"); err != nil {
		t.Fatalf("unchanged route was unavailable during cache cleanup: %v", err)
	}
	if _, err := ChannelGroup.Next("default", "gpt-5"); err == nil {
		t.Fatal("removed route remained in the published snapshot")
	}
	close(unblock)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("batch update did not finish")
	}
}
