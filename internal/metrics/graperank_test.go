package metrics

import (
	"context"
	"sync"
	"testing"
	"time"

	"fiatjaf.com/nostr"
)

type fakeEventStore struct {
	mu         sync.Mutex
	events     []*nostr.Event
	queryCount int
}

func (f *fakeEventStore) QueryEvents(ctx context.Context, filter nostr.Filter) (chan *nostr.Event, error) {
	f.mu.Lock()
	f.queryCount++
	f.mu.Unlock()

	ch := make(chan *nostr.Event)
	go func() {
		defer close(ch)
		for _, event := range f.events {
			if matchesFilter(event, filter) {
				select {
				case ch <- event:
				case <-ctx.Done():
					return
				}
			}
		}
	}()

	return ch, nil
}

func matchesFilter(event *nostr.Event, filter nostr.Filter) bool {
	if len(filter.Kinds) > 0 {
		kindMatch := false
		for _, kind := range filter.Kinds {
			if event.Kind == kind {
				kindMatch = true
				break
			}
		}
		if !kindMatch {
			return false
		}
	}

	if len(filter.Authors) > 0 {
		authorMatch := false
		for _, author := range filter.Authors {
			if event.PubKey == author {
				authorMatch = true
				break
			}
		}
		if !authorMatch {
			return false
		}
	}

	for tagName, values := range filter.Tags {
		if len(values) == 0 {
			continue
		}
		tagMatch := false
		for _, tag := range event.Tags {
			if len(tag) < 2 || tag[0] != tagName {
				continue
			}
			for _, expected := range values {
				if tag[1] == expected {
					tagMatch = true
					break
				}
			}
			if tagMatch {
				break
			}
		}
		if !tagMatch {
			return false
		}
	}

	return true
}

func testPubkey(ch rune) string {
	b := make([]rune, 64)
	for i := range b {
		b[i] = ch
	}
	return string(b)
}

func TestGrapeRankCachesGraphAndRequesterResults(t *testing.T) {
	a := testPubkey('a')
	b := testPubkey('b')

	store := &fakeEventStore{
		events: []*nostr.Event{
			{Kind: 3, PubKey: a, Tags: nostr.Tags{{"p", b}}},
		},
	}

	engine := NewGrapeRankEngine(store, 5*time.Minute)

	_, ok := engine.GetRank(context.Background(), a, b)
	if !ok {
		t.Fatalf("expected rank to be computed")
	}

	store.mu.Lock()
	firstQueryCount := store.queryCount
	store.mu.Unlock()

	_, ok = engine.GetRank(context.Background(), a, b)
	if !ok {
		t.Fatalf("expected cached rank to be returned")
	}

	store.mu.Lock()
	secondQueryCount := store.queryCount
	store.mu.Unlock()

	if secondQueryCount != firstQueryCount {
		t.Fatalf("expected no additional graph queries on cached read, got %d -> %d", firstQueryCount, secondQueryCount)
	}
}

func TestGrapeRankCacheExpiresAfterTTL(t *testing.T) {
	a := testPubkey('a')
	b := testPubkey('b')

	store := &fakeEventStore{
		events: []*nostr.Event{
			{Kind: 3, PubKey: a, Tags: nostr.Tags{{"p", b}}},
		},
	}

	engine := NewGrapeRankEngine(store, 20*time.Millisecond)

	_, _ = engine.GetRank(context.Background(), a, b)

	store.mu.Lock()
	firstQueryCount := store.queryCount
	store.mu.Unlock()

	time.Sleep(35 * time.Millisecond)
	_, _ = engine.GetRank(context.Background(), a, b)

	store.mu.Lock()
	secondQueryCount := store.queryCount
	store.mu.Unlock()

	if secondQueryCount <= firstQueryCount {
		t.Fatalf("expected graph cache refresh after ttl, got %d -> %d", firstQueryCount, secondQueryCount)
	}
}

func TestGrapeRankAppliesReportPenalty(t *testing.T) {
	a := testPubkey('a')
	b := testPubkey('b')
	c := testPubkey('c')

	store := &fakeEventStore{
		events: []*nostr.Event{
			{Kind: 3, PubKey: a, Tags: nostr.Tags{{"p", b}, {"p", c}}},
			{Kind: 3, PubKey: b, Tags: nostr.Tags{{"p", c}}},
			{Kind: 3, PubKey: c, Tags: nostr.Tags{{"p", b}}},
			{Kind: 1984, PubKey: a, Tags: nostr.Tags{{"p", b}}},
		},
	}

	engine := NewGrapeRankEngine(store, 5*time.Minute)

	bRank, ok := engine.GetRank(context.Background(), a, b)
	if !ok {
		t.Fatalf("expected rank for b")
	}
	cRank, ok := engine.GetRank(context.Background(), a, c)
	if !ok {
		t.Fatalf("expected rank for c")
	}

	if bRank >= cRank {
		t.Fatalf("expected reported node b rank (%d) to be lower than c rank (%d)", bRank, cRank)
	}
}
