package relay

import (
	"context"
	"log"
	"strings"
	"sync"
	"time"

	"cipolin/internal/fetcher"

	"github.com/nbd-wtf/go-nostr"
)

// EventStore interface for querying and saving events
type EventStore interface {
	QueryEvents(ctx context.Context, filter nostr.Filter) (chan *nostr.Event, error)
	SaveEvent(ctx context.Context, event *nostr.Event) error
}

// Syncer handles event synchronization
type Syncer struct {
	fetcher       *fetcher.EventFetcher
	db            EventStore
	storageRelays []string
}

// NewSyncer creates a new event syncer
func NewSyncer(f *fetcher.EventFetcher, db EventStore, storageRelays []string) *Syncer {
	return &Syncer{
		fetcher:       f,
		db:            db,
		storageRelays: storageRelays,
	}
}

// SyncUserEvents fetches and stores events for computing user metrics
func (s *Syncer) SyncUserEvents(ctx context.Context, pubkey string, userRelays []string) error {
	log.Printf("[sync] Fetching events for %s...", pubkey[:16]+"...")

	// Filters for user's own relays
	userFilters := []nostr.Filter{
		{Authors: []string{pubkey}, Kinds: []int{1}},                    // Posts/replies
		{Authors: []string{pubkey}, Kinds: []int{7}},                    // Reactions
		{Kinds: []int{3}, Tags: nostr.TagMap{"p": []string{pubkey}}},    // Followers
		{Kinds: []int{9735}, Tags: nostr.TagMap{"p": []string{pubkey}}}, // Zaps received
		{Kinds: []int{9735}, Tags: nostr.TagMap{"P": []string{pubkey}}}, // Zaps sent (P = sender)
		{Kinds: []int{9321}, Tags: nostr.TagMap{"p": []string{pubkey}}}, // Nutzaps received
		{Authors: []string{pubkey}, Kinds: []int{9321}},                 // Nutzaps sent
	}

	// Reports should be fetched from popular relays + user relays
	popularRelays, _ := GetPopularRelays(ctx, s.storageRelays)
	reportRelays := MergeRelays(popularRelays, userRelays)
	reportFilters := []nostr.Filter{
		{Authors: []string{pubkey}, Kinds: []int{1984}},                 // Reports sent
		{Kinds: []int{1984}, Tags: nostr.TagMap{"p": []string{pubkey}}}, // Reports received
	}

	deadline := time.Now().Add(30 * time.Second)

	// Event handler to store events - must be thread-safe
	var storeMu sync.Mutex
	onEvent := func(event *nostr.Event) {
		storeMu.Lock()
		defer storeMu.Unlock()
		if err := s.db.SaveEvent(ctx, event); err != nil {
			log.Printf("[sync] Failed to store event: %v", err)
		}
	}

	// Fetch in parallel
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		results := s.fetcher.FetchAllContinuously(ctx, userRelays, userFilters, fetcher.FetchBackward, deadline, onEvent)
		logFetchResults("user relays", results)
	}()

	go func() {
		defer wg.Done()
		results := s.fetcher.FetchAllContinuously(ctx, reportRelays, reportFilters, fetcher.FetchBackward, deadline, onEvent)
		logFetchResults("reports", results)
	}()

	wg.Wait()

	log.Printf("[sync] Fetch completed for %s", pubkey[:16]+"...")
	return nil
}

// SyncEventInteractions fetches interactions for an event
func (s *Syncer) SyncEventInteractions(ctx context.Context, eventID string) error {
	log.Printf("[sync] Fetching interactions for event %s...", eventID[:16]+"...")

	filters := []nostr.Filter{
		{Kinds: []int{1}, Tags: nostr.TagMap{"e": []string{eventID}}},    // Replies
		{Kinds: []int{1}, Tags: nostr.TagMap{"q": []string{eventID}}},    // Quotes
		{Kinds: []int{6}, Tags: nostr.TagMap{"e": []string{eventID}}},    // Reposts
		{Kinds: []int{7}, Tags: nostr.TagMap{"e": []string{eventID}}},    // Reactions
		{Kinds: []int{9735}, Tags: nostr.TagMap{"e": []string{eventID}}}, // Zaps
		{Kinds: []int{9321}, Tags: nostr.TagMap{"e": []string{eventID}}}, // Nutzaps
	}

	popularRelays, _ := GetPopularRelays(ctx, s.storageRelays)

	// Try to get the event author's relays
	authorRelays := s.getEventAuthorRelays(ctx, eventID, popularRelays)
	relays := MergeRelays(popularRelays, authorRelays)

	deadline := time.Now().Add(30 * time.Second)

	onEvent := func(event *nostr.Event) {
		if err := s.db.SaveEvent(ctx, event); err != nil {
			log.Printf("[sync] Failed to store event: %v", err)
		}
	}

	results := s.fetcher.FetchAllContinuously(ctx, relays, filters, fetcher.FetchBackward, deadline, onEvent)
	logFetchResults("event interactions", results)

	return nil
}

// SyncAddressInteractions fetches interactions for an addressable event
func (s *Syncer) SyncAddressInteractions(ctx context.Context, address string) error {
	log.Printf("[sync] Fetching interactions for address %s...", address)

	filters := []nostr.Filter{
		{Kinds: []int{1}, Tags: nostr.TagMap{"a": []string{address}}},     // Comments
		{Kinds: []int{6, 16}, Tags: nostr.TagMap{"a": []string{address}}}, // Reposts
		{Kinds: []int{7}, Tags: nostr.TagMap{"a": []string{address}}},     // Reactions
		{Kinds: []int{9735}, Tags: nostr.TagMap{"a": []string{address}}},  // Zaps
		{Kinds: []int{9321}, Tags: nostr.TagMap{"a": []string{address}}},  // Nutzaps
	}

	popularRelays, _ := GetPopularRelays(ctx, s.storageRelays)

	// Extract author pubkey from address (format: kind:pubkey:d-tag)
	authorRelays := s.getAddressAuthorRelays(ctx, address)
	relays := MergeRelays(popularRelays, authorRelays)

	deadline := time.Now().Add(30 * time.Second)

	onEvent := func(event *nostr.Event) {
		if err := s.db.SaveEvent(ctx, event); err != nil {
			log.Printf("[sync] Failed to store event: %v", err)
		}
	}

	results := s.fetcher.FetchAllContinuously(ctx, relays, filters, fetcher.FetchBackward, deadline, onEvent)
	logFetchResults("address interactions", results)

	return nil
}

// logFetchResults logs summary of fetch operations
func logFetchResults(label string, results []fetcher.ContinuousFetchSummary) {
	var total int
	var errors int
	var skipped int
	for _, r := range results {
		total += r.Total
		if r.Error != nil {
			errors++
		}
		if r.SkippedTTL {
			skipped++
		}
	}
	log.Printf("[sync] %s: fetched %d events from %d relays (%d errors, %d skipped TTL)",
		label, total, len(results), errors, skipped)
}

// getAddressAuthorRelays extracts the author pubkey from address and returns their relays
func (s *Syncer) getAddressAuthorRelays(ctx context.Context, address string) []string {
	// Address format: kind:pubkey:d-tag
	parts := strings.Split(address, ":")
	if len(parts) < 2 {
		return nil
	}

	pubkey := parts[1]
	if len(pubkey) != 64 {
		return nil
	}

	relays, _ := GetUserNIP65Relays(ctx, s.storageRelays, pubkey)
	return relays
}

// getEventAuthorRelays fetches the event to get its author, then returns the author's relays
func (s *Syncer) getEventAuthorRelays(ctx context.Context, eventID string, fallbackRelays []string) []string {
	// First try local DB
	filter := nostr.Filter{IDs: []string{eventID}}
	ch, err := s.db.QueryEvents(ctx, filter)
	if err == nil {
		for event := range ch {
			if event != nil {
				relays, _ := GetUserNIP65Relays(ctx, s.storageRelays, event.PubKey)
				return relays
			}
		}
	}

	// Try fetching from popular relays
	for _, relayURL := range fallbackRelays {
		relay, err := nostr.RelayConnect(ctx, relayURL)
		if err != nil {
			continue
		}

		events, err := relay.QuerySync(ctx, filter)
		relay.Close()

		if err != nil || len(events) == 0 {
			continue
		}

		relays, _ := GetUserNIP65Relays(ctx, s.storageRelays, events[0].PubKey)
		return relays
	}

	return nil
}
