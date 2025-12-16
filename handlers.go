package main

import (
	"context"
	"log"

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

				if err != nil {
					log.Printf("[handler] Error generating assertion for %s: %v", subject, err)
					continue
				}

				if event != nil {
					// Store the generated assertion locally
					if err := db.SaveEvent(ctx, event); err != nil {
						log.Printf("[handler] Failed to store assertion: %v", err)
					}

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

	// Compute metrics from local DB
	metrics := computeUserMetricsFromDB(ctx, pubkey)

	// Build assertion event
	event := &nostr.Event{
		Kind: KindUserAssertion,
		Tags: nostr.Tags{
			{"d", pubkey},
			{"p", pubkey},
		},
		Content: "",
	}

	// Add metric tags
	for key, value := range metrics {
		event.Tags = append(event.Tags, nostr.Tag{key, value})
	}

	if err := signEvent(event); err != nil {
		return nil, err
	}

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

	event := &nostr.Event{
		Kind: KindEventAssertion,
		Tags: nostr.Tags{
			{"d", eventID},
			{"e", eventID},
		},
		Content: "",
	}

	for key, value := range metrics {
		event.Tags = append(event.Tags, nostr.Tag{key, value})
	}

	if err := signEvent(event); err != nil {
		return nil, err
	}

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

	event := &nostr.Event{
		Kind: KindAddressAssertion,
		Tags: nostr.Tags{
			{"d", address},
			{"a", address},
		},
		Content: "",
	}

	for key, value := range metrics {
		event.Tags = append(event.Tags, nostr.Tag{key, value})
	}

	if err := signEvent(event); err != nil {
		return nil, err
	}

	return event, nil
}

