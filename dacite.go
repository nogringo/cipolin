package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/nbd-wtf/go-nostr"
)

// DaciteFetchRequest is the request body for /events/fetch
type DaciteFetchRequest struct {
	Relays  []string         `json:"relays"`
	Filters []map[string]any `json:"filters"`
}

// DaciteFetchResponse is the response from /events/fetch
type DaciteFetchResponse struct {
	Count      int  `json:"count"`
	Stored     int  `json:"stored"`
	HasMore    bool `json:"hasMore"`
	SkippedTTL bool `json:"skippedTtl,omitempty"`
	Complete   bool `json:"complete,omitempty"`
}

// triggerDaciteFetch calls dacite API to fetch events from relays to storage
func triggerDaciteFetch(ctx context.Context, relays []string, filters []nostr.Filter) (*DaciteFetchResponse, error) {
	// Convert filters to map format
	filterMaps := make([]map[string]any, len(filters))
	for i, f := range filters {
		filterMaps[i] = filterToMap(f)
	}

	reqBody := DaciteFetchRequest{
		Relays:  relays,
		Filters: filterMaps,
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	log.Printf("[dacite] Request: %s", string(body))

	url := config.DaciteURL + "/events/fetch"
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to call dacite: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("dacite returned %d: %s", resp.StatusCode, string(respBody))
	}

	var result DaciteFetchResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	log.Printf("[dacite] Fetched %d events (stored: %d, hasMore: %v)", result.Count, result.Stored, result.HasMore)
	return &result, nil
}

// filterToMap converts a nostr.Filter to a map for JSON serialization
func filterToMap(f nostr.Filter) map[string]any {
	m := make(map[string]any)

	if len(f.IDs) > 0 {
		m["ids"] = f.IDs
	}
	if len(f.Authors) > 0 {
		m["authors"] = f.Authors
	}
	if len(f.Kinds) > 0 {
		m["kinds"] = f.Kinds
	}
	if f.Since != nil {
		m["since"] = int64(*f.Since)
	}
	if f.Until != nil {
		m["until"] = int64(*f.Until)
	}
	// Don't send limit to Dacite

	// Tag filters
	for tag, values := range f.Tags {
		if len(values) > 0 {
			m["#"+tag] = values
		}
	}

	return m
}

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
