package main

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"github.com/nbd-wtf/go-nostr"
)

// getPopularRelays fetches popular online relays from Breccia's kind 6301 events
func getPopularRelays(ctx context.Context) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	filter := nostr.Filter{
		Kinds: []int{6301},
		Tags:  nostr.TagMap{"i": []string{"online-relays"}},
		Limit: 1,
	}

	// Try each storage relay
	for _, relayURL := range config.StorageRelays {
		relay, err := nostr.RelayConnect(ctx, relayURL)
		if err != nil {
			continue
		}

		events, err := relay.QuerySync(ctx, filter)
		relay.Close()

		if err != nil || len(events) == 0 {
			continue
		}

		// Parse relay list from content (JSON array)
		var relays []string
		if err := json.Unmarshal([]byte(events[0].Content), &relays); err != nil {
			continue
		}

		if len(relays) > 0 {
			// Limit to first 10 relays
			if len(relays) > 10 {
				relays = relays[:10]
			}
			log.Printf("[breccia] Fetched %d popular relays", len(relays))
			return relays, nil
		}
	}

	// Fallback to default relays
	log.Printf("[breccia] Using fallback relays")
	return []string{
		"wss://relay.damus.io",
		"wss://nos.lol",
		"wss://relay.nostr.band",
	}, nil
}

// getUserNIP65Relays fetches a user's relay list from storage relays
func getUserNIP65Relays(ctx context.Context, pubkey string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	filter := nostr.Filter{
		Kinds:   []int{10002},
		Authors: []string{pubkey},
		Limit:   1,
	}

	// Try each storage relay
	for _, relayURL := range config.StorageRelays {
		relay, err := nostr.RelayConnect(ctx, relayURL)
		if err != nil {
			continue
		}

		events, err := relay.QuerySync(ctx, filter)
		relay.Close()

		if err != nil || len(events) == 0 {
			continue
		}

		// Extract relay URLs from the event
		var userRelays []string
		for _, tag := range events[0].Tags {
			if len(tag) >= 2 && tag[0] == "r" {
				userRelays = append(userRelays, tag[1])
			}
		}

		if len(userRelays) > 0 {
			return userRelays, nil
		}
	}

	// Fallback to default relays
	return []string{
		"wss://relay.damus.io",
		"wss://nos.lol",
		"wss://relay.nostr.band",
		"wss://nostr.wine",
		"wss://relay.primal.net",
	}, nil
}
