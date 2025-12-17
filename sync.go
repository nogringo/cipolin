package main

import (
	"context"
	"log"

	"github.com/nbd-wtf/go-nostr"
)

// syncUserEvents triggers Dacite to fetch and store events for computing user metrics
func syncUserEvents(ctx context.Context, pubkey string, userRelays []string) error {
	log.Printf("[sync] Fetching events for %s via Dacite...", pubkey[:16]+"...")

	filters := []nostr.Filter{
		{Authors: []string{pubkey}, Kinds: []int{1}, Limit: 1000},                     // Posts/replies
		{Authors: []string{pubkey}, Kinds: []int{7}, Limit: 500},                      // Reactions
		{Authors: []string{pubkey}, Kinds: []int{9734}, Limit: 500},                   // Zap requests sent
		{Authors: []string{pubkey}, Kinds: []int{1984}, Limit: 100},                   // Reports sent
		{Kinds: []int{3}, Tags: nostr.TagMap{"p": []string{pubkey}}, Limit: 1000},     // Followers
		{Kinds: []int{9735}, Tags: nostr.TagMap{"p": []string{pubkey}}, Limit: 500},   // Zaps received
		{Kinds: []int{1984}, Tags: nostr.TagMap{"p": []string{pubkey}}, Limit: 100},   // Reports received
		{Authors: []string{pubkey}, Kinds: []int{10002}, Limit: 1},                    // NIP-65 relay list
	}

	// Dacite fetches events and publishes them directly to Khatru
	if _, err := triggerDaciteFetch(ctx, userRelays, filters); err != nil {
		log.Printf("[sync] Dacite fetch failed: %v", err)
		return err
	}

	log.Printf("[sync] Dacite fetch completed for %s", pubkey[:16]+"...")
	return nil
}

// syncEventInteractions triggers Dacite to fetch and store interactions for an event
func syncEventInteractions(ctx context.Context, eventID string) error {
	log.Printf("[sync] Fetching interactions for event %s via Dacite...", eventID[:16]+"...")

	filters := []nostr.Filter{
		{Kinds: []int{1}, Tags: nostr.TagMap{"e": []string{eventID}}, Limit: 500},    // Replies
		{Kinds: []int{6}, Tags: nostr.TagMap{"e": []string{eventID}}, Limit: 500},    // Reposts
		{Kinds: []int{7}, Tags: nostr.TagMap{"e": []string{eventID}}, Limit: 500},    // Reactions
		{Kinds: []int{9735}, Tags: nostr.TagMap{"e": []string{eventID}}, Limit: 500}, // Zaps
	}

	defaultRelays := []string{"wss://relay.damus.io", "wss://nos.lol", "wss://relay.nostr.band"}
	if _, err := triggerDaciteFetch(ctx, defaultRelays, filters); err != nil {
		log.Printf("[sync] Dacite fetch failed: %v", err)
		return err
	}

	return nil
}

// syncAddressInteractions triggers Dacite to fetch and store interactions for an addressable event
func syncAddressInteractions(ctx context.Context, address string) error {
	log.Printf("[sync] Fetching interactions for address %s via Dacite...", address)

	filters := []nostr.Filter{
		{Kinds: []int{1}, Tags: nostr.TagMap{"a": []string{address}}, Limit: 500},     // Comments
		{Kinds: []int{6, 16}, Tags: nostr.TagMap{"a": []string{address}}, Limit: 500}, // Reposts
		{Kinds: []int{7}, Tags: nostr.TagMap{"a": []string{address}}, Limit: 500},     // Reactions
		{Kinds: []int{9735}, Tags: nostr.TagMap{"a": []string{address}}, Limit: 500},  // Zaps
	}

	defaultRelays := []string{"wss://relay.damus.io", "wss://nos.lol", "wss://relay.nostr.band"}
	if _, err := triggerDaciteFetch(ctx, defaultRelays, filters); err != nil {
		log.Printf("[sync] Dacite fetch failed: %v", err)
		return err
	}

	return nil
}
