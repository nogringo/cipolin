package metrics

import (
	"context"
	"iter"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/eventstore/boltdb"
)

type recordingStore struct {
	events     []nostr.Event
	maxLimits  []int
	lastFilter []nostr.Filter
}

func (s *recordingStore) QueryEvents(filter nostr.Filter, maxLimit int) iter.Seq[nostr.Event] {
	s.maxLimits = append(s.maxLimits, maxLimit)
	s.lastFilter = append(s.lastFilter, filter)

	return func(yield func(nostr.Event) bool) {
		if maxLimit <= 0 {
			return
		}

		emitted := 0
		for _, event := range s.events {
			if !filter.Matches(event) {
				continue
			}
			if !yield(event) {
				return
			}
			emitted++
			if emitted >= maxLimit {
				return
			}
		}
	}
}

func TestComputeUserMetricsUsesPositiveQueryLimit(t *testing.T) {
	targetPriv := nostr.Generate()
	targetPub := nostr.GetPublicKey(targetPriv)
	followerPriv := nostr.Generate()
	followerPub := nostr.GetPublicKey(followerPriv)

	store := &recordingStore{
		events: []nostr.Event{
			mustSignEvent(t, followerPriv, nostr.Event{
				CreatedAt: nostr.Now(),
				PubKey:    followerPub,
				Kind:      3,
				Tags:      nostr.Tags{{"p", targetPub.Hex()}},
			}),
			mustSignEvent(t, targetPriv, nostr.Event{
				CreatedAt: nostr.Now(),
				PubKey:    targetPub,
				Kind:      1,
				Content:   "hello",
			}),
		},
	}

	metrics := ComputeUserMetrics(context.Background(), store, targetPub.Hex(), "", nil)

	if metrics["followers"] != "1" {
		t.Fatalf("expected follower count 1, got %q", metrics["followers"])
	}
	if metrics["post_cnt"] != "1" {
		t.Fatalf("expected post count 1, got %q", metrics["post_cnt"])
	}
	if len(store.maxLimits) == 0 {
		t.Fatal("expected QueryEvents to be called")
	}
	for _, maxLimit := range store.maxLimits {
		if maxLimit <= 0 {
			t.Fatalf("expected positive maxLimit for QueryEvents, got %d", maxLimit)
		}
	}
}

func TestBoltBackendQueryEventsByKindAndTag(t *testing.T) {
	tmpDir := t.TempDir()
	db := &boltdb.BoltBackend{Path: filepath.Join(tmpDir, "events.db")}
	if err := db.Init(); err != nil {
		t.Fatalf("Init failed: %v", err)
	}
	defer db.Close()

	targetPriv := nostr.Generate()
	targetPub := nostr.GetPublicKey(targetPriv)
	followerPriv := nostr.Generate()
	followerPub := nostr.GetPublicKey(followerPriv)
	otherPriv := nostr.Generate()
	otherPub := nostr.GetPublicKey(otherPriv)

	matching := mustSignEvent(t, followerPriv, nostr.Event{
		CreatedAt: nostr.Timestamp(time.Now().Unix()),
		PubKey:    followerPub,
		Kind:      3,
		Tags:      nostr.Tags{{"p", targetPub.Hex()}},
	})
	nonMatching := mustSignEvent(t, otherPriv, nostr.Event{
		CreatedAt: nostr.Timestamp(time.Now().Add(time.Second).Unix()),
		PubKey:    otherPub,
		Kind:      3,
		Tags:      nostr.Tags{{"p", otherPub.Hex()}},
	})

	if err := db.SaveEvent(matching); err != nil {
		t.Fatalf("SaveEvent matching failed: %v", err)
	}
	if err := db.SaveEvent(nonMatching); err != nil {
		t.Fatalf("SaveEvent nonMatching failed: %v", err)
	}

	filter := nostr.Filter{
		Kinds: []nostr.Kind{3},
		Tags:  nostr.TagMap{"p": {targetPub.Hex()}},
	}

	got := slices.Collect(db.QueryEvents(filter, 10))
	if len(got) != 1 {
		t.Fatalf("expected 1 matching event, got %d", len(got))
	}
	if got[0].ID != matching.ID {
		t.Fatalf("expected event %s, got %s", matching.ID.Hex(), got[0].ID.Hex())
	}
}

func mustSignEvent(t *testing.T, secretKey [32]byte, event nostr.Event) nostr.Event {
	t.Helper()
	if err := event.Sign(secretKey); err != nil {
		t.Fatalf("Sign failed: %v", err)
	}
	return event
}
