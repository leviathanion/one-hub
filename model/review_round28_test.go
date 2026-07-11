package model

import (
	"sync"
	"testing"

	"one-api/common/config"
)

func TestConcurrentChannelTagReplacementsDoNotFormUnion(t *testing.T) {
	useTestChannelDB(t)
	if err := DB.Create(&Channel{Name: "original", Key: "key-original", Tag: "replace-team", Type: config.ChannelTypeOpenAI}).Error; err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	keys := []string{"key-a", "key-b"}
	errs := make(chan error, len(keys))
	var wg sync.WaitGroup
	for _, key := range keys {
		key := key
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs <- UpdateChannelsTag("replace-team", &Channel{Name: "replacement", Key: key, Tag: "replace-team"})
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent replacement failed: %v", err)
		}
	}

	members, err := GetChannelsByTag("replace-team")
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 1 || members[0].Key != "key-a" && members[0].Key != "key-b" {
		t.Fatalf("serialized replacements must leave exactly one submitted set, got %+v", members)
	}
}

func TestConcurrentAddChannelToTagRejectsSameKeyOnce(t *testing.T) {
	useTestChannelDB(t)
	if err := DB.Create(&Channel{Name: "original", Key: "key-original", Tag: "add-team", Type: config.ChannelTypeOpenAI}).Error; err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := AddChannelToTag("add-team", &Channel{Key: "same-key"})
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errs)

	successes, failures := 0, 0
	for err := range errs {
		if err == nil {
			successes++
		} else {
			failures++
		}
	}
	if successes != 1 || failures != 1 {
		t.Fatalf("expected one insert and one duplicate rejection, successes=%d failures=%d", successes, failures)
	}
	members, err := GetChannelsByTag("add-team")
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 2 {
		t.Fatalf("same key was inserted more than once: %+v", members)
	}
}
