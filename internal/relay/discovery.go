package relay

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"fiatjaf.com/nostr"
)

// GetPopularRelays fetches popular online relays from Breccia's kind 6301 events
func GetPopularRelays(ctx context.Context, storageRelays []string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	filter := nostr.Filter{
		Kinds: []nostr.Kind{nostr.Kind(6301)},
		Tags:  nostr.TagMap{"i": []string{"online-relays"}},
		Limit: 1,
	}

	// Try each storage relay
	for _, relayURL := range storageRelays {
		relay, err := nostr.RelayConnect(ctx, relayURL, nostr.RelayOptions{})
		if err != nil {
			continue
		}

		var firstEvent *nostr.Event
		for evt := range relay.QueryEvents(filter) {
			firstEvent = &evt
			break
		}
		relay.Close()

		if firstEvent == nil {
			continue
		}

		// Parse relay list from content (JSON array)
		var relays []string
		if err := json.Unmarshal([]byte(firstEvent.Content), &relays); err != nil {
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
		"wss://relay.camelus.app",
	}, nil
}

// GetUserNIP65Relays fetches a user's relay list from storage relays
func GetUserNIP65Relays(ctx context.Context, storageRelays []string, pubkey string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	filter := nostr.Filter{
		Kinds:   []nostr.Kind{nostr.Kind(10002)},
		Authors: []nostr.PubKey{nostr.MustPubKeyFromHex(pubkey)},
		Limit:   1,
	}

	// Try each storage relay
	for _, relayURL := range storageRelays {
		relay, err := nostr.RelayConnect(ctx, relayURL, nostr.RelayOptions{})
		if err != nil {
			continue
		}

		var firstEvent *nostr.Event
		for evt := range relay.QueryEvents(filter) {
			firstEvent = &evt
			break
		}
		relay.Close()

		if firstEvent == nil {
			continue
		}

		// Extract relay URLs from the event
		var userRelays []string
		for _, tag := range firstEvent.Tags {
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
		"wss://relay.camelus.app",
		"wss://nostr.wine",
		"wss://relay.primal.net",
	}, nil
}

// MergeRelays combines two relay lists into a unique set
func MergeRelays(a, b []string) []string {
	seen := make(map[string]bool)
	var result []string

	for _, r := range a {
		if !seen[r] {
			seen[r] = true
			result = append(result, r)
		}
	}
	for _, r := range b {
		if !seen[r] {
			seen[r] = true
			result = append(result, r)
		}
	}

	return result
}
