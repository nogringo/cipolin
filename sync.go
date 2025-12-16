package main

import (
	"context"
	"log"
	"time"

	"github.com/nbd-wtf/go-nostr"
	"github.com/nbd-wtf/go-nostr/nip77"
)

// relayStoreWrapper wraps badger backend to implement nostr.RelayStore
type relayStoreWrapper struct{}

func (w *relayStoreWrapper) Publish(ctx context.Context, event nostr.Event) error {
	return db.SaveEvent(ctx, &event)
}

func (w *relayStoreWrapper) QueryEvents(ctx context.Context, filter nostr.Filter) (chan *nostr.Event, error) {
	return db.QueryEvents(ctx, filter)
}

func (w *relayStoreWrapper) QuerySync(ctx context.Context, filter nostr.Filter) ([]*nostr.Event, error) {
	ch, err := db.QueryEvents(ctx, filter)
	if err != nil {
		return nil, err
	}
	var events []*nostr.Event
	for event := range ch {
		events = append(events, event)
	}
	return events, nil
}

var store nostr.RelayStore = &relayStoreWrapper{}

// syncEventsFromStorage syncs events matching the filter from storage relays using NIP-77
func syncEventsFromStorage(ctx context.Context, filter nostr.Filter) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	for _, relayURL := range config.StorageRelays {
		err := nip77.NegentropySync(ctx, store, relayURL, filter, nip77.Down)
		if err != nil {
			log.Printf("[sync] NIP-77 sync from %s failed: %v", relayURL, err)
			continue
		}
		log.Printf("[sync] NIP-77 synced from %s", relayURL)
	}

	return nil
}

// syncUserEvents syncs all events relevant to computing user metrics
func syncUserEvents(ctx context.Context, pubkey string, userRelays []string) error {
	// First, trigger dacite to fetch events from user's relays to storage
	filters := []nostr.Filter{
		{Authors: []string{pubkey}, Kinds: []int{1}, Limit: 1000},       // Posts/replies
		{Authors: []string{pubkey}, Kinds: []int{7}, Limit: 500},        // Reactions
		{Authors: []string{pubkey}, Kinds: []int{9734}, Limit: 500},     // Zap requests sent
		{Kinds: []int{3}, Tags: nostr.TagMap{"p": []string{pubkey}}, Limit: 1000},   // Followers
		{Kinds: []int{9735}, Tags: nostr.TagMap{"p": []string{pubkey}}, Limit: 500}, // Zaps received
		{Authors: []string{pubkey}, Kinds: []int{10002}, Limit: 1},      // NIP-65 relay list
	}

	// Trigger dacite fetch
	if _, err := triggerDaciteFetch(ctx, userRelays, filters); err != nil {
		log.Printf("[sync] Dacite fetch failed: %v", err)
	}

	// NIP-77 sync from storage relays to local DB
	for _, f := range filters {
		if err := syncEventsFromStorage(ctx, f); err != nil {
			log.Printf("[sync] Storage sync failed: %v", err)
		}
	}

	return nil
}

// syncEventInteractions syncs all events related to a specific event
func syncEventInteractions(ctx context.Context, eventID string) error {
	filters := []nostr.Filter{
		{Kinds: []int{1}, Tags: nostr.TagMap{"e": []string{eventID}}, Limit: 500},    // Replies
		{Kinds: []int{6}, Tags: nostr.TagMap{"e": []string{eventID}}, Limit: 500},    // Reposts
		{Kinds: []int{7}, Tags: nostr.TagMap{"e": []string{eventID}}, Limit: 500},    // Reactions
		{Kinds: []int{9735}, Tags: nostr.TagMap{"e": []string{eventID}}, Limit: 500}, // Zaps
	}

	defaultRelays := []string{"wss://relay.damus.io", "wss://nos.lol", "wss://relay.nostr.band"}
	if _, err := triggerDaciteFetch(ctx, defaultRelays, filters); err != nil {
		log.Printf("[sync] Dacite fetch failed: %v", err)
	}

	for _, f := range filters {
		if err := syncEventsFromStorage(ctx, f); err != nil {
			log.Printf("[sync] Storage sync failed: %v", err)
		}
	}

	return nil
}

// syncAddressInteractions syncs all events related to an addressable event
func syncAddressInteractions(ctx context.Context, address string) error {
	filters := []nostr.Filter{
		{Kinds: []int{1}, Tags: nostr.TagMap{"a": []string{address}}, Limit: 500},      // Comments
		{Kinds: []int{6, 16}, Tags: nostr.TagMap{"a": []string{address}}, Limit: 500},  // Reposts
		{Kinds: []int{7}, Tags: nostr.TagMap{"a": []string{address}}, Limit: 500},      // Reactions
		{Kinds: []int{9735}, Tags: nostr.TagMap{"a": []string{address}}, Limit: 500},   // Zaps
	}

	defaultRelays := []string{"wss://relay.damus.io", "wss://nos.lol", "wss://relay.nostr.band"}
	if _, err := triggerDaciteFetch(ctx, defaultRelays, filters); err != nil {
		log.Printf("[sync] Dacite fetch failed: %v", err)
	}

	for _, f := range filters {
		if err := syncEventsFromStorage(ctx, f); err != nil {
			log.Printf("[sync] Storage sync failed: %v", err)
		}
	}

	return nil
}
