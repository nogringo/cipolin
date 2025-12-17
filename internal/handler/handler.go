package handler

import (
	"context"
	"log"
	"strings"
	"time"

	"cipolin/internal/metrics"
	"cipolin/internal/relay"

	"github.com/nbd-wtf/go-nostr"
)

// NIP-85 event kinds
const (
	KindUserAssertion    = 30382
	KindEventAssertion   = 30383
	KindAddressAssertion = 30384
)

// Handler handles NIP-85 assertion requests
type Handler struct {
	syncer        *relay.Syncer
	db            metrics.EventStore
	storageRelays []string
	privateKey    string
	pubKey        string
}

// NewHandler creates a new NIP-85 handler
func NewHandler(syncer *relay.Syncer, db metrics.EventStore, storageRelays []string, privateKey, pubKey string) *Handler {
	return &Handler{
		syncer:        syncer,
		db:            db,
		storageRelays: storageRelays,
		privateKey:    privateKey,
		pubKey:        pubKey,
	}
}

// HandleNIP85Query intercepts queries for NIP-85 kinds and generates assertions on-demand
func (h *Handler) HandleNIP85Query(ctx context.Context, filter nostr.Filter) (chan *nostr.Event, error) {
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
					event, err = h.generateUserAssertion(ctx, subject)
				case KindEventAssertion:
					event, err = h.generateEventAssertion(ctx, subject)
				case KindAddressAssertion:
					event, err = h.generateAddressAssertion(ctx, subject)
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
func (h *Handler) publishToStorageRelays(event *nostr.Event) {
	for _, relayURL := range h.storageRelays {
		go func(url string) {
			// Each goroutine gets its own context
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			r, err := nostr.RelayConnect(ctx, url)
			if err != nil {
				log.Printf("[publish] Failed to connect to %s: %v", url, err)
				return
			}
			defer r.Close()

			if err := r.Publish(ctx, *event); err != nil {
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

// signEvent signs an event with the service provider's private key
func (h *Handler) signEvent(event *nostr.Event) error {
	event.PubKey = h.pubKey
	event.CreatedAt = nostr.Timestamp(time.Now().Unix())
	return event.Sign(h.privateKey)
}

// generateUserAssertion creates a kind 30382 assertion for a pubkey
func (h *Handler) generateUserAssertion(ctx context.Context, pubkey string) (*nostr.Event, error) {
	if len(pubkey) != 64 {
		return nil, nil
	}

	log.Printf("[handler] Generating user assertion for %s", pubkey[:16]+"...")

	// Get user's NIP-65 relays
	userRelays, err := relay.GetUserNIP65Relays(ctx, h.storageRelays, pubkey)
	if err != nil {
		log.Printf("[handler] Failed to get NIP-65 relays: %v", err)
	}

	// Sync events for this user
	if err := h.syncer.SyncUserEvents(ctx, pubkey, userRelays); err != nil {
		log.Printf("[handler] Sync failed: %v", err)
	}

	log.Printf("[handler] Computing metrics for %s...", pubkey[:16]+"...")

	// Compute metrics from local DB
	m := metrics.ComputeUserMetrics(ctx, h.db, pubkey)

	log.Printf("[handler] Metrics computed for %s, building event...", pubkey[:16]+"...")

	// Build assertion event with tags in NIP-85 order
	event := &nostr.Event{
		Kind: KindUserAssertion,
		Tags: nostr.Tags{
			{"d", pubkey},
			{"p", pubkey},
			{"followers", m["followers"]},
			{"rank", m["rank"]},
			{"first_created_at", m["first_created_at"]},
			{"post_cnt", m["post_cnt"]},
			{"reply_cnt", m["reply_cnt"]},
			{"reactions_cnt", m["reactions_cnt"]},
			{"zap_amt_recd", m["zap_amt_recd"]},
			{"zap_amt_sent", m["zap_amt_sent"]},
			{"zap_cnt_recd", m["zap_cnt_recd"]},
			{"zap_cnt_sent", m["zap_cnt_sent"]},
			{"zap_avg_amt_day_recd", m["zap_avg_amt_day_recd"]},
			{"zap_avg_amt_day_sent", m["zap_avg_amt_day_sent"]},
			{"reports_cnt_recd", m["reports_cnt_recd"]},
			{"reports_cnt_sent", m["reports_cnt_sent"]},
			{"active_hours_start", m["active_hours_start"]},
			{"active_hours_end", m["active_hours_end"]},
		},
		Content: "",
	}

	// Add topic tags
	if topics, ok := m["_topics"]; ok && topics != "" {
		for _, topic := range strings.Split(topics, ",") {
			if topic != "" {
				event.Tags = append(event.Tags, nostr.Tag{"t", topic})
			}
		}
	}

	if err := h.signEvent(event); err != nil {
		return nil, err
	}

	// Publish to storage relays
	h.publishToStorageRelays(event)

	return event, nil
}

// generateEventAssertion creates a kind 30383 assertion for an event ID
func (h *Handler) generateEventAssertion(ctx context.Context, eventID string) (*nostr.Event, error) {
	if len(eventID) != 64 {
		return nil, nil
	}

	log.Printf("[handler] Generating event assertion for %s", eventID[:16]+"...")

	// Sync interactions for this event
	if err := h.syncer.SyncEventInteractions(ctx, eventID); err != nil {
		log.Printf("[handler] Sync failed: %v", err)
	}

	// Compute metrics from local DB
	m := metrics.ComputeEventMetrics(ctx, h.db, eventID)

	// Build assertion event with tags in NIP-85 order
	event := &nostr.Event{
		Kind: KindEventAssertion,
		Tags: nostr.Tags{
			{"d", eventID},
			{"e", eventID},
			{"comment_cnt", m["comment_cnt"]},
			{"quote_cnt", m["quote_cnt"]},
			{"repost_cnt", m["repost_cnt"]},
			{"reaction_cnt", m["reaction_cnt"]},
			{"zap_cnt", m["zap_cnt"]},
			{"zap_amount", m["zap_amount"]},
			{"rank", m["rank"]},
		},
		Content: "",
	}

	if err := h.signEvent(event); err != nil {
		return nil, err
	}

	// Publish to storage relays
	h.publishToStorageRelays(event)

	return event, nil
}

// generateAddressAssertion creates a kind 30384 assertion for an event address
func (h *Handler) generateAddressAssertion(ctx context.Context, address string) (*nostr.Event, error) {
	log.Printf("[handler] Generating address assertion for %s", address)

	// Sync interactions for this address
	if err := h.syncer.SyncAddressInteractions(ctx, address); err != nil {
		log.Printf("[handler] Sync failed: %v", err)
	}

	// Compute metrics from local DB
	m := metrics.ComputeAddressMetrics(ctx, h.db, address)

	// Build assertion event with tags in NIP-85 order
	event := &nostr.Event{
		Kind: KindAddressAssertion,
		Tags: nostr.Tags{
			{"d", address},
			{"a", address},
			{"comment_cnt", m["comment_cnt"]},
			{"quote_cnt", m["quote_cnt"]},
			{"repost_cnt", m["repost_cnt"]},
			{"reaction_cnt", m["reaction_cnt"]},
			{"zap_cnt", m["zap_cnt"]},
			{"zap_amount", m["zap_amount"]},
			{"rank", m["rank"]},
		},
		Content: "",
	}

	if err := h.signEvent(event); err != nil {
		return nil, err
	}

	// Publish to storage relays
	h.publishToStorageRelays(event)

	return event, nil
}

// IsOnlyNIP85Kinds checks if kinds slice only contains NIP-85 kinds
func IsOnlyNIP85Kinds(kinds []int) bool {
	if len(kinds) == 0 {
		return false
	}
	for _, k := range kinds {
		if k != KindUserAssertion && k != KindEventAssertion && k != KindAddressAssertion {
			return false
		}
	}
	return true
}
