package main

import (
	"context"
	"log"
	"sync"

	"github.com/nbd-wtf/go-nostr"
)

// syncUserEvents triggers Dacite to fetch and store events for computing user metrics
func syncUserEvents(ctx context.Context, pubkey string, userRelays []string) error {
	log.Printf("[sync] Fetching events for %s via Dacite...", pubkey[:16]+"...")

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

	// Reports should be fetched from popular relays
	popularRelays, _ := getPopularRelays(ctx)
	reportFilters := []nostr.Filter{
		{Authors: []string{pubkey}, Kinds: []int{1984}},                 // Reports sent
		{Kinds: []int{1984}, Tags: nostr.TagMap{"p": []string{pubkey}}}, // Reports received
	}

	// Fetch in parallel
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		if _, err := triggerDaciteFetch(ctx, userRelays, userFilters); err != nil {
			log.Printf("[sync] Dacite fetch (user relays) failed: %v", err)
		}
	}()

	go func() {
		defer wg.Done()
		if _, err := triggerDaciteFetch(ctx, popularRelays, reportFilters); err != nil {
			log.Printf("[sync] Dacite fetch (reports) failed: %v", err)
		}
	}()

	wg.Wait()

	log.Printf("[sync] Dacite fetch completed for %s", pubkey[:16]+"...")
	return nil
}

// syncEventInteractions triggers Dacite to fetch and store interactions for an event
func syncEventInteractions(ctx context.Context, eventID string) error {
	log.Printf("[sync] Fetching interactions for event %s via Dacite...", eventID[:16]+"...")

	filters := []nostr.Filter{
		{Kinds: []int{1}, Tags: nostr.TagMap{"e": []string{eventID}}},    // Replies
		{Kinds: []int{6}, Tags: nostr.TagMap{"e": []string{eventID}}},    // Reposts
		{Kinds: []int{7}, Tags: nostr.TagMap{"e": []string{eventID}}},    // Reactions
		{Kinds: []int{9735}, Tags: nostr.TagMap{"e": []string{eventID}}}, // Zaps
		{Kinds: []int{9321}, Tags: nostr.TagMap{"e": []string{eventID}}}, // Nutzaps
	}

	popularRelays, _ := getPopularRelays(ctx)
	if _, err := triggerDaciteFetch(ctx, popularRelays, filters); err != nil {
		log.Printf("[sync] Dacite fetch failed: %v", err)
		return err
	}

	return nil
}

// syncAddressInteractions triggers Dacite to fetch and store interactions for an addressable event
func syncAddressInteractions(ctx context.Context, address string) error {
	log.Printf("[sync] Fetching interactions for address %s via Dacite...", address)

	filters := []nostr.Filter{
		{Kinds: []int{1}, Tags: nostr.TagMap{"a": []string{address}}},     // Comments
		{Kinds: []int{6, 16}, Tags: nostr.TagMap{"a": []string{address}}}, // Reposts
		{Kinds: []int{7}, Tags: nostr.TagMap{"a": []string{address}}},     // Reactions
		{Kinds: []int{9735}, Tags: nostr.TagMap{"a": []string{address}}},  // Zaps
		{Kinds: []int{9321}, Tags: nostr.TagMap{"a": []string{address}}},  // Nutzaps
	}

	popularRelays, _ := getPopularRelays(ctx)
	if _, err := triggerDaciteFetch(ctx, popularRelays, filters); err != nil {
		log.Printf("[sync] Dacite fetch failed: %v", err)
		return err
	}

	return nil
}
