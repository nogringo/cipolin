package main

import (
	"context"
	"log"
	"strings"
	"time"

	"github.com/nbd-wtf/go-nostr"
)

// NIP-85 event kinds
const (
	KindUserAssertion    = 30382
	KindEventAssertion   = 30383
	KindAddressAssertion = 30384
)

// handleNIP85Query intercepts queries for NIP-85 kinds and generates assertions on-demand
func handleNIP85Query(ctx context.Context, filter nostr.Filter) (chan *nostr.Event, error) {
	ch := make(chan *nostr.Event)

	go func() {
		defer close(ch)

		// Only handle NIP-85 kinds
		if !containsNIP85Kind(filter.Kinds) {
			return
		}

		// Get the subject from d tag
		dTags := filter.Tags["d"]
		if len(dTags) == 0 {
			return
		}

		for _, kind := range filter.Kinds {
			for _, subject := range dTags {
				start := time.Now()
				var event *nostr.Event
				var err error

				switch kind {
				case KindUserAssertion:
					event, err = generateUserAssertion(ctx, subject)
				case KindEventAssertion:
					event, err = generateEventAssertion(ctx, subject)
				case KindAddressAssertion:
					event, err = generateAddressAssertion(ctx, subject)
				}

				duration := time.Since(start)

				if err != nil {
					log.Printf("[handler] Error generating assertion for %s (took %v): %v", subject, duration, err)
					continue
				}

				if event != nil {
					log.Printf("[handler] Sending event %s (kind %d) for subject %s (took %v)", event.ID[:16]+"...", event.Kind, subject[:16]+"...", duration)
					select {
					case ch <- event:
					case <-ctx.Done():
						return
					}
				}
			}
		}
	}()

	return ch, nil
}

// publishToStorageRelays publishes an event to all storage relays
func publishToStorageRelays(event *nostr.Event) {
	for _, relayURL := range config.StorageRelays {
		go func(url string) {
			// Each goroutine gets its own context
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			relay, err := nostr.RelayConnect(ctx, url)
			if err != nil {
				log.Printf("[publish] Failed to connect to %s: %v", url, err)
				return
			}
			defer relay.Close()

			if err := relay.Publish(ctx, *event); err != nil {
				log.Printf("[publish] Failed to publish to %s: %v", url, err)
				return
			}
			log.Printf("[publish] Published %s to %s", event.ID[:16]+"...", url)
		}(relayURL)
	}
}

func containsNIP85Kind(kinds []int) bool {
	for _, k := range kinds {
		if k == KindUserAssertion || k == KindEventAssertion || k == KindAddressAssertion {
			return true
		}
	}
	return false
}

// generateUserAssertion creates a kind 30382 assertion for a pubkey
func generateUserAssertion(ctx context.Context, pubkey string) (*nostr.Event, error) {
	if len(pubkey) != 64 {
		return nil, nil
	}

	log.Printf("[handler] Generating user assertion for %s", pubkey[:16]+"...")

	// Get user's NIP-65 relays
	userRelays, err := getUserNIP65Relays(ctx, pubkey)
	if err != nil {
		log.Printf("[handler] Failed to get NIP-65 relays: %v", err)
	}

	// Sync events for this user
	if err := syncUserEvents(ctx, pubkey, userRelays); err != nil {
		log.Printf("[handler] Sync failed: %v", err)
	}

	log.Printf("[handler] Computing metrics for %s...", pubkey[:16]+"...")

	// Compute metrics from local DB
	metrics := computeUserMetricsFromDB(ctx, pubkey)

	log.Printf("[handler] Metrics computed for %s, building event...", pubkey[:16]+"...")

	// Build assertion event with tags in NIP-85 order
	event := &nostr.Event{
		Kind: KindUserAssertion,
		Tags: nostr.Tags{
			{"d", pubkey},
			{"p", pubkey},
			{"followers", metrics["followers"]},
			{"rank", metrics["rank"]},
			{"first_created_at", metrics["first_created_at"]},
			{"post_cnt", metrics["post_cnt"]},
			{"reply_cnt", metrics["reply_cnt"]},
			{"reactions_cnt", metrics["reactions_cnt"]},
			{"zap_amt_recd", metrics["zap_amt_recd"]},
			{"zap_amt_sent", metrics["zap_amt_sent"]},
			{"zap_cnt_recd", metrics["zap_cnt_recd"]},
			{"zap_cnt_sent", metrics["zap_cnt_sent"]},
			{"zap_avg_amt_day_recd", metrics["zap_avg_amt_day_recd"]},
			{"zap_avg_amt_day_sent", metrics["zap_avg_amt_day_sent"]},
			{"reports_cnt_recd", metrics["reports_cnt_recd"]},
			{"reports_cnt_sent", metrics["reports_cnt_sent"]},
			{"active_hours_start", metrics["active_hours_start"]},
			{"active_hours_end", metrics["active_hours_end"]},
		},
		Content: "",
	}

	// Add topic tags
	if topics, ok := metrics["_topics"]; ok && topics != "" {
		for _, topic := range strings.Split(topics, ",") {
			if topic != "" {
				event.Tags = append(event.Tags, nostr.Tag{"t", topic})
			}
		}
	}

	if err := signEvent(event); err != nil {
		return nil, err
	}

	// Publish to storage relays
	publishToStorageRelays(event)

	return event, nil
}

// generateEventAssertion creates a kind 30383 assertion for an event ID
func generateEventAssertion(ctx context.Context, eventID string) (*nostr.Event, error) {
	if len(eventID) != 64 {
		return nil, nil
	}

	log.Printf("[handler] Generating event assertion for %s", eventID[:16]+"...")

	// Sync interactions for this event
	if err := syncEventInteractions(ctx, eventID); err != nil {
		log.Printf("[handler] Sync failed: %v", err)
	}

	// Compute metrics from local DB
	metrics := computeEventMetricsFromDB(ctx, eventID)

	// Build assertion event with tags in NIP-85 order
	event := &nostr.Event{
		Kind: KindEventAssertion,
		Tags: nostr.Tags{
			{"d", eventID},
			{"e", eventID},
			{"comment_cnt", metrics["comment_cnt"]},
			{"quote_cnt", metrics["quote_cnt"]},
			{"repost_cnt", metrics["repost_cnt"]},
			{"reaction_cnt", metrics["reaction_cnt"]},
			{"zap_cnt", metrics["zap_cnt"]},
			{"zap_amount", metrics["zap_amount"]},
			{"rank", metrics["rank"]},
		},
		Content: "",
	}

	if err := signEvent(event); err != nil {
		return nil, err
	}

	// Publish to storage relays
	publishToStorageRelays(event)

	return event, nil
}

// generateAddressAssertion creates a kind 30384 assertion for an event address
func generateAddressAssertion(ctx context.Context, address string) (*nostr.Event, error) {
	log.Printf("[handler] Generating address assertion for %s", address)

	// Sync interactions for this address
	if err := syncAddressInteractions(ctx, address); err != nil {
		log.Printf("[handler] Sync failed: %v", err)
	}

	// Compute metrics from local DB
	metrics := computeAddressMetricsFromDB(ctx, address)

	// Build assertion event with tags in NIP-85 order
	event := &nostr.Event{
		Kind: KindAddressAssertion,
		Tags: nostr.Tags{
			{"d", address},
			{"a", address},
			{"comment_cnt", metrics["comment_cnt"]},
			{"quote_cnt", metrics["quote_cnt"]},
			{"repost_cnt", metrics["repost_cnt"]},
			{"reaction_cnt", metrics["reaction_cnt"]},
			{"zap_cnt", metrics["zap_cnt"]},
			{"zap_amount", metrics["zap_amount"]},
			{"rank", metrics["rank"]},
		},
		Content: "",
	}

	if err := signEvent(event); err != nil {
		return nil, err
	}

	// Publish to storage relays
	publishToStorageRelays(event)

	return event, nil
}
