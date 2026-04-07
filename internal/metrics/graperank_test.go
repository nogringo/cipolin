package metrics

import (
	"context"
	"iter"
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

func (f *fakeEventStore) QueryEvents(filter nostr.Filter, _ int) iter.Seq[nostr.Event] {
	f.mu.Lock()
	f.queryCount++
	f.mu.Unlock()

	return func(yield func(nostr.Event) bool) {
		for _, event := range f.events {
			if matchesFilter(event, filter) && !yield(*event) {
				return
			}
		}
	}
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
			{Kind: 3, PubKey: nostr.MustPubKeyFromHex(a), Tags: nostr.Tags{{"p", b}}},
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
			{Kind: 3, PubKey: nostr.MustPubKeyFromHex(a), Tags: nostr.Tags{{"p", b}}},
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

func TestGrapeRankInvalidateCacheForcesRefreshBeforeTTL(t *testing.T) {
	a := testPubkey('a')
	b := testPubkey('b')

	store := &fakeEventStore{}
	engine := NewGrapeRankEngine(store, 5*time.Minute)

	_, ok := engine.GetRank(context.Background(), a, b)
	if !ok {
		t.Fatalf("expected rank computation to succeed")
	}

	store.mu.Lock()
	firstQueryCount := store.queryCount
	store.events = []*nostr.Event{{Kind: SignalFollow, PubKey: nostr.MustPubKeyFromHex(a), Tags: nostr.Tags{{"p", b}}}}
	store.mu.Unlock()

	// Without invalidation and before TTL expiry, graph should still be cached.
	_, _ = engine.GetRank(context.Background(), a, b)
	store.mu.Lock()
	secondQueryCount := store.queryCount
	store.mu.Unlock()
	if secondQueryCount != firstQueryCount {
		t.Fatalf("expected cached graph reuse before invalidation, got %d -> %d", firstQueryCount, secondQueryCount)
	}

	engine.InvalidateCache()
	_, _ = engine.GetRank(context.Background(), a, b)

	store.mu.Lock()
	thirdQueryCount := store.queryCount
	store.mu.Unlock()

	if thirdQueryCount <= secondQueryCount {
		t.Fatalf("expected immediate graph refresh after invalidation, got %d -> %d", secondQueryCount, thirdQueryCount)
	}
}

func TestGrapeRankAppliesReportPenalty(t *testing.T) {
	a := testPubkey('a')
	b := testPubkey('b')
	c := testPubkey('c')

	store := &fakeEventStore{
		events: []*nostr.Event{
			{Kind: 3, PubKey: nostr.MustPubKeyFromHex(a), Tags: nostr.Tags{{"p", b}, {"p", c}}},
			{Kind: 3, PubKey: nostr.MustPubKeyFromHex(b), Tags: nostr.Tags{{"p", c}}},
			{Kind: 3, PubKey: nostr.MustPubKeyFromHex(c), Tags: nostr.Tags{{"p", b}}},
			{Kind: 1984, PubKey: nostr.MustPubKeyFromHex(a), Tags: nostr.Tags{{"p", b}}},
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

func TestGrapeRankRespectsPositiveSignalWeights(t *testing.T) {
	a := testPubkey('a')
	b := testPubkey('b')
	c := testPubkey('c')

	store := &fakeEventStore{
		events: []*nostr.Event{
			{Kind: SignalFollow, PubKey: nostr.MustPubKeyFromHex(a), Tags: nostr.Tags{{"p", b}}},
			{Kind: SignalReaction, PubKey: nostr.MustPubKeyFromHex(a), Tags: nostr.Tags{{"p", c}}},
		},
	}

	engine := NewGrapeRankEngine(store, 5*time.Minute, RankWeights{
		PositiveByKind: map[int]float64{
			SignalFollow:   3.0,
			SignalReaction: 0.2,
		},
	})

	bRank, ok := engine.GetRank(context.Background(), a, b)
	if !ok {
		t.Fatalf("expected rank for b")
	}
	cRank, ok := engine.GetRank(context.Background(), a, c)
	if !ok {
		t.Fatalf("expected rank for c")
	}

	if bRank <= cRank {
		t.Fatalf("expected stronger follow signal to rank b (%d) above c (%d)", bRank, cRank)
	}
}

func TestGrapeRankRespectsNegativeSignalWeights(t *testing.T) {
	a := testPubkey('a')
	b := testPubkey('b')
	c := testPubkey('c')

	store := &fakeEventStore{
		events: []*nostr.Event{
			{Kind: SignalFollow, PubKey: nostr.MustPubKeyFromHex(a), Tags: nostr.Tags{{"p", b}, {"p", c}}},
			{Kind: SignalFollow, PubKey: nostr.MustPubKeyFromHex(b), Tags: nostr.Tags{{"p", a}}},
			{Kind: SignalFollow, PubKey: nostr.MustPubKeyFromHex(c), Tags: nostr.Tags{{"p", a}}},
			{Kind: SignalReport, PubKey: nostr.MustPubKeyFromHex(a), Tags: nostr.Tags{{"p", b}}},
		},
	}

	weakPenalty := RankWeights{
		NegativeByKind: map[int]float64{SignalReport: 0.1},
	}
	strongPenalty := RankWeights{
		NegativeByKind: map[int]float64{SignalReport: 3.0},
	}

	weakEngine := NewGrapeRankEngine(store, 5*time.Minute, weakPenalty)
	strongEngine := NewGrapeRankEngine(store, 5*time.Minute, strongPenalty)

	bWeak, ok := weakEngine.GetRank(context.Background(), a, b)
	if !ok {
		t.Fatalf("expected weak-penalty rank for b")
	}
	bStrong, ok := strongEngine.GetRank(context.Background(), a, b)
	if !ok {
		t.Fatalf("expected strong-penalty rank for b")
	}
	cStrong, ok := strongEngine.GetRank(context.Background(), a, c)
	if !ok {
		t.Fatalf("expected strong-penalty rank for c")
	}

	if bStrong >= bWeak {
		t.Fatalf("expected stronger report weight to lower b rank (%d -> %d)", bWeak, bStrong)
	}
	if bStrong >= cStrong {
		t.Fatalf("expected strongly penalized b rank (%d) to be below c (%d)", bStrong, cStrong)
	}
}
