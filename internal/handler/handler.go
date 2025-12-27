package handler

import (
	"context"
	"log"
	"strings"
	"time"

	"cipolin/internal/keys"
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
	keyManager    *keys.MetricKeyManager
}

// NewHandler creates a new NIP-85 handler
func NewHandler(syncer *relay.Syncer, db metrics.EventStore, storageRelays []string, keyManager *keys.MetricKeyManager) *Handler {
	return &Handler{
		syncer:        syncer,
		db:            db,
		storageRelays: storageRelays,
		keyManager:    keyManager,
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

		// Build set of requested author pubkeys (metric filters)
		requestedAuthors := make(map[string]bool)
		for _, author := range filter.Authors {
			requestedAuthors[author] = true
		}

		for _, kind := range filter.Kinds {
			for _, subject := range dTags {
				start := time.Now()
				var events []*nostr.Event
				var err error

				switch kind {
				case KindUserAssertion:
					events, err = h.generateUserAssertions(ctx, subject, requestedAuthors)
				case KindEventAssertion:
					events, err = h.generateEventAssertions(ctx, subject, requestedAuthors)
				case KindAddressAssertion:
					events, err = h.generateAddressAssertions(ctx, subject, requestedAuthors)
				}

				duration := time.Since(start)

				if err != nil {
					log.Printf("[handler] Error generating assertions for %s (took %v): %v", subject, duration, err)
					continue
				}

				// Send all metric events
				for _, event := range events {
					if event != nil {
						log.Printf("[handler] Sending event %s (kind %d, metric pubkey %s) for subject %s",
							event.ID[:16]+"...", event.Kind, event.PubKey[:16]+"...", truncateSubject(subject))
						select {
						case ch <- event:
						case <-ctx.Done():
							return
						}
					}
				}

				if len(events) > 0 {
					log.Printf("[handler] Sent %d assertion events for %s (took %v)", len(events), truncateSubject(subject), duration)
				}
			}
		}
	}()

	return ch, nil
}

func truncateSubject(s string) string {
	if len(s) > 16 {
		return s[:16] + "..."
	}
	return s
}

// publishToStorageRelays publishes an event to all storage relays
func (h *Handler) publishToStorageRelays(event *nostr.Event) {
	for _, relayURL := range h.storageRelays {
		go func(url string) {
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

// createMetricEvent creates and signs an event for a single metric
func (h *Handler) createMetricEvent(kind int, subject string, metric string, value string, extraTags ...nostr.Tag) (*nostr.Event, error) {
	event := &nostr.Event{
		Kind:      kind,
		CreatedAt: nostr.Timestamp(time.Now().Unix()),
		Tags: nostr.Tags{
			{"d", subject},
			{metric, value},
		},
		Content: "",
	}

	// Add extra tags (like p, e, a tags for relay hints)
	for _, tag := range extraTags {
		event.Tags = append(event.Tags, tag)
	}

	// Sign with metric-specific key
	if err := h.keyManager.SignEventForMetric(event, metric); err != nil {
		return nil, err
	}

	return event, nil
}

// shouldGenerateMetric checks if a metric should be generated based on author filter
func (h *Handler) shouldGenerateMetric(metric string, requestedAuthors map[string]bool) bool {
	// If no author filter, generate all metrics
	if len(requestedAuthors) == 0 {
		return true
	}
	// Check if this metric's pubkey is in the requested authors
	metricPubkey := h.keyManager.GetPubKey(metric)
	return requestedAuthors[metricPubkey]
}

// generateUserAssertions creates kind 30382 assertions for a pubkey (one per metric)
func (h *Handler) generateUserAssertions(ctx context.Context, pubkey string, requestedAuthors map[string]bool) ([]*nostr.Event, error) {
	if len(pubkey) != 64 {
		return nil, nil
	}

	log.Printf("[handler] Generating user assertions for %s", pubkey[:16]+"...")

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

	log.Printf("[handler] Metrics computed for %s, building events...", pubkey[:16]+"...")

	var events []*nostr.Event
	pTag := nostr.Tag{"p", pubkey}

	// Create one event per metric
	metricValues := []struct {
		name  string
		value string
	}{
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
	}

	for _, mv := range metricValues {
		// Skip if this metric wasn't requested
		if !h.shouldGenerateMetric(mv.name, requestedAuthors) {
			continue
		}

		event, err := h.createMetricEvent(KindUserAssertion, pubkey, mv.name, mv.value, pTag)
		if err != nil {
			log.Printf("[handler] Failed to create %s event: %v", mv.name, err)
			continue
		}
		events = append(events, event)
		h.publishToStorageRelays(event)
	}

	// Handle topics separately (multiple t tags in one event)
	if h.shouldGenerateMetric("t", requestedAuthors) {
		if topics, ok := m["_topics"]; ok && topics != "" {
			topicEvent := &nostr.Event{
				Kind:      KindUserAssertion,
				CreatedAt: nostr.Timestamp(time.Now().Unix()),
				Tags: nostr.Tags{
					{"d", pubkey},
					pTag,
				},
				Content: "",
			}
			for _, topic := range strings.Split(topics, ",") {
				if topic != "" {
					topicEvent.Tags = append(topicEvent.Tags, nostr.Tag{"t", topic})
				}
			}
			if err := h.keyManager.SignEventForMetric(topicEvent, "t"); err == nil {
				events = append(events, topicEvent)
				h.publishToStorageRelays(topicEvent)
			}
		}
	}

	return events, nil
}

// generateEventAssertions creates kind 30383 assertions for an event ID (one per metric)
func (h *Handler) generateEventAssertions(ctx context.Context, eventID string, requestedAuthors map[string]bool) ([]*nostr.Event, error) {
	if len(eventID) != 64 {
		return nil, nil
	}

	log.Printf("[handler] Generating event assertions for %s", eventID[:16]+"...")

	// Sync interactions for this event
	if err := h.syncer.SyncEventInteractions(ctx, eventID); err != nil {
		log.Printf("[handler] Sync failed: %v", err)
	}

	// Compute metrics from local DB
	m := metrics.ComputeEventMetrics(ctx, h.db, eventID)

	var events []*nostr.Event
	eTag := nostr.Tag{"e", eventID}

	metricValues := []struct {
		name  string
		value string
	}{
		{"comment_cnt", m["comment_cnt"]},
		{"quote_cnt", m["quote_cnt"]},
		{"repost_cnt", m["repost_cnt"]},
		{"reaction_cnt", m["reaction_cnt"]},
		{"zap_cnt", m["zap_cnt"]},
		{"zap_amount", m["zap_amount"]},
		{"rank", m["rank"]},
	}

	for _, mv := range metricValues {
		// Skip if this metric wasn't requested
		if !h.shouldGenerateMetric(mv.name, requestedAuthors) {
			continue
		}

		event, err := h.createMetricEvent(KindEventAssertion, eventID, mv.name, mv.value, eTag)
		if err != nil {
			log.Printf("[handler] Failed to create %s event: %v", mv.name, err)
			continue
		}
		events = append(events, event)
		h.publishToStorageRelays(event)
	}

	return events, nil
}

// generateAddressAssertions creates kind 30384 assertions for an event address (one per metric)
func (h *Handler) generateAddressAssertions(ctx context.Context, address string, requestedAuthors map[string]bool) ([]*nostr.Event, error) {
	log.Printf("[handler] Generating address assertions for %s", address)

	// Sync interactions for this address
	if err := h.syncer.SyncAddressInteractions(ctx, address); err != nil {
		log.Printf("[handler] Sync failed: %v", err)
	}

	// Compute metrics from local DB
	m := metrics.ComputeAddressMetrics(ctx, h.db, address)

	var events []*nostr.Event
	aTag := nostr.Tag{"a", address}

	metricValues := []struct {
		name  string
		value string
	}{
		{"comment_cnt", m["comment_cnt"]},
		{"quote_cnt", m["quote_cnt"]},
		{"repost_cnt", m["repost_cnt"]},
		{"reaction_cnt", m["reaction_cnt"]},
		{"zap_cnt", m["zap_cnt"]},
		{"zap_amount", m["zap_amount"]},
		{"rank", m["rank"]},
	}

	for _, mv := range metricValues {
		// Skip if this metric wasn't requested
		if !h.shouldGenerateMetric(mv.name, requestedAuthors) {
			continue
		}

		event, err := h.createMetricEvent(KindAddressAssertion, address, mv.name, mv.value, aTag)
		if err != nil {
			log.Printf("[handler] Failed to create %s event: %v", mv.name, err)
			continue
		}
		events = append(events, event)
		h.publishToStorageRelays(event)
	}

	return events, nil
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
