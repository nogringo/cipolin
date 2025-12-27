package handler

import (
	"context"
	"log"
	"strings"
	"sync/atomic"
	"time"

	"cipolin/internal/keys"
	"cipolin/internal/metrics"
	"cipolin/internal/relay"

	"github.com/nbd-wtf/go-nostr"
)

// Update interval for lazy loading
const lazyLoadUpdateInterval = 500 * time.Millisecond

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

// HandleNIP85Query intercepts queries for NIP-85 kinds and generates assertions on-demand with lazy loading
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
				switch kind {
				case KindUserAssertion:
					// Use streaming/lazy loading for user assertions
					h.streamUserAssertions(ctx, subject, requestedAuthors, ch)
				case KindEventAssertion:
					h.streamEventAssertions(ctx, subject, requestedAuthors, ch)
				case KindAddressAssertion:
					h.streamAddressAssertions(ctx, subject, requestedAuthors, ch)
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

// streamUserAssertions streams user assertions with lazy loading
func (h *Handler) streamUserAssertions(ctx context.Context, pubkey string, requestedAuthors map[string]bool, ch chan *nostr.Event) {
	if len(pubkey) != 64 {
		return
	}

	log.Printf("[handler] Starting lazy load for user %s", pubkey[:16]+"...")

	// Get user's NIP-65 relays
	userRelays, err := relay.GetUserNIP65Relays(ctx, h.storageRelays, pubkey)
	if err != nil {
		log.Printf("[handler] Failed to get NIP-65 relays: %v", err)
	}

	// Send initial metrics from local DB (will be 0 if no cached data)
	h.sendUserMetrics(ctx, pubkey, requestedAuthors, ch, false)

	// Start async sync (will stop when ctx is cancelled)
	progress := h.syncer.SyncUserEventsAsync(ctx, pubkey, userRelays)

	// Track last event count to detect changes
	var lastEventCount int64 = 0
	ticker := time.NewTicker(lazyLoadUpdateInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Printf("[handler] Client disconnected for %s, stopping sync", pubkey[:16]+"...")
			return

		case <-progress.Done:
			// Final update
			log.Printf("[handler] Sync complete for %s, sending final metrics", pubkey[:16]+"...")
			h.sendUserMetrics(ctx, pubkey, requestedAuthors, ch, true)
			return

		case <-ticker.C:
			// Check if new events have arrived
			currentCount := atomic.LoadInt64(&progress.EventCount)
			if currentCount > lastEventCount {
				log.Printf("[handler] Progress update for %s: %d events (was %d)", pubkey[:16]+"...", currentCount, lastEventCount)
				lastEventCount = currentCount
				h.sendUserMetrics(ctx, pubkey, requestedAuthors, ch, false)
			}
		}
	}
}

// sendUserMetrics computes and sends user metric events
// isFinal indicates this is the last update (publish to storage relays)
func (h *Handler) sendUserMetrics(ctx context.Context, pubkey string, requestedAuthors map[string]bool, ch chan *nostr.Event, isFinal bool) {
	pTag := nostr.Tag{"p", pubkey}

	// Always compute from local DB (returns 0s if no data)
	m := metrics.ComputeUserMetrics(ctx, h.db, pubkey)

	metricNames := []string{
		"followers", "rank", "first_created_at",
		"post_cnt", "reply_cnt", "reactions_cnt",
		"zap_amt_recd", "zap_amt_sent", "zap_cnt_recd", "zap_cnt_sent",
		"zap_avg_amt_day_recd", "zap_avg_amt_day_sent",
		"reports_cnt_recd", "reports_cnt_sent",
		"active_hours_start", "active_hours_end",
	}

	for _, name := range metricNames {
		if !h.shouldGenerateMetric(name, requestedAuthors) {
			continue
		}

		event, err := h.createMetricEvent(KindUserAssertion, pubkey, name, m[name], pTag)
		if err != nil {
			log.Printf("[handler] Failed to create %s event: %v", name, err)
			continue
		}

		select {
		case ch <- event:
			// Only publish to storage relays on final update
			if isFinal {
				h.publishToStorageRelays(event)
			}
		case <-ctx.Done():
			return
		}
	}

	// Handle topics
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
				select {
				case ch <- topicEvent:
					if isFinal {
						h.publishToStorageRelays(topicEvent)
					}
				case <-ctx.Done():
					return
				}
			}
		}
	}
}

// generateUserAssertions creates kind 30382 assertions for a pubkey (one per metric) - blocking version
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

// streamEventAssertions streams event assertions with lazy loading
func (h *Handler) streamEventAssertions(ctx context.Context, eventID string, requestedAuthors map[string]bool, ch chan *nostr.Event) {
	if len(eventID) != 64 {
		return
	}

	log.Printf("[handler] Starting lazy load for event %s", eventID[:16]+"...")

	// Send initial metrics from local DB
	h.sendEventMetrics(ctx, eventID, requestedAuthors, ch, false)

	// Start async sync
	progress := h.syncer.SyncEventInteractionsAsync(ctx, eventID)

	var lastEventCount int64 = 0
	ticker := time.NewTicker(lazyLoadUpdateInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Printf("[handler] Client disconnected for event %s, stopping sync", eventID[:16]+"...")
			return
		case <-progress.Done:
			log.Printf("[handler] Sync complete for event %s, sending final metrics", eventID[:16]+"...")
			h.sendEventMetrics(ctx, eventID, requestedAuthors, ch, true)
			return
		case <-ticker.C:
			currentCount := atomic.LoadInt64(&progress.EventCount)
			if currentCount > lastEventCount {
				lastEventCount = currentCount
				h.sendEventMetrics(ctx, eventID, requestedAuthors, ch, false)
			}
		}
	}
}

// sendEventMetrics computes and sends event metric events
func (h *Handler) sendEventMetrics(ctx context.Context, eventID string, requestedAuthors map[string]bool, ch chan *nostr.Event, isFinal bool) {
	eTag := nostr.Tag{"e", eventID}
	m := metrics.ComputeEventMetrics(ctx, h.db, eventID)

	metricNames := []string{"comment_cnt", "quote_cnt", "repost_cnt", "reaction_cnt", "zap_cnt", "zap_amount", "rank"}

	for _, name := range metricNames {
		if !h.shouldGenerateMetric(name, requestedAuthors) {
			continue
		}

		event, err := h.createMetricEvent(KindEventAssertion, eventID, name, m[name], eTag)
		if err != nil {
			continue
		}

		select {
		case ch <- event:
			if isFinal {
				h.publishToStorageRelays(event)
			}
		case <-ctx.Done():
			return
		}
	}
}

// streamAddressAssertions streams address assertions with lazy loading
func (h *Handler) streamAddressAssertions(ctx context.Context, address string, requestedAuthors map[string]bool, ch chan *nostr.Event) {
	log.Printf("[handler] Starting lazy load for address %s", address)

	// Send initial metrics from local DB
	h.sendAddressMetrics(ctx, address, requestedAuthors, ch, false)

	// Start async sync
	progress := h.syncer.SyncAddressInteractionsAsync(ctx, address)

	var lastEventCount int64 = 0
	ticker := time.NewTicker(lazyLoadUpdateInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Printf("[handler] Client disconnected for address %s, stopping sync", address)
			return
		case <-progress.Done:
			log.Printf("[handler] Sync complete for address %s, sending final metrics", address)
			h.sendAddressMetrics(ctx, address, requestedAuthors, ch, true)
			return
		case <-ticker.C:
			currentCount := atomic.LoadInt64(&progress.EventCount)
			if currentCount > lastEventCount {
				lastEventCount = currentCount
				h.sendAddressMetrics(ctx, address, requestedAuthors, ch, false)
			}
		}
	}
}

// sendAddressMetrics computes and sends address metric events
func (h *Handler) sendAddressMetrics(ctx context.Context, address string, requestedAuthors map[string]bool, ch chan *nostr.Event, isFinal bool) {
	aTag := nostr.Tag{"a", address}
	m := metrics.ComputeAddressMetrics(ctx, h.db, address)

	metricNames := []string{"comment_cnt", "quote_cnt", "repost_cnt", "reaction_cnt", "zap_cnt", "zap_amount", "rank"}

	for _, name := range metricNames {
		if !h.shouldGenerateMetric(name, requestedAuthors) {
			continue
		}

		event, err := h.createMetricEvent(KindAddressAssertion, address, name, m[name], aTag)
		if err != nil {
			continue
		}

		select {
		case ch <- event:
			if isFinal {
				h.publishToStorageRelays(event)
			}
		case <-ctx.Done():
			return
		}
	}
}

// generateEventAssertions creates kind 30383 assertions for an event ID (one per metric) - blocking version
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
