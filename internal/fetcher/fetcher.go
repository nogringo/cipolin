package fetcher

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/nbd-wtf/go-nostr"
)

// Default fetch configuration
const (
	DefaultTTL          = 60 * time.Second
	DefaultFetchTimeout = 30 * time.Second
	DefaultFetchLimit   = 500
)

// Config holds fetcher configuration
type Config struct {
	TTL          time.Duration
	FetchTimeout time.Duration
	FetchLimit   int
}

// EventFetcher handles event fetching from relays with cursor-based pagination
type EventFetcher struct {
	cursorStore *CursorStore
	config      Config
}

// NewEventFetcher creates a new event fetcher
func NewEventFetcher(cursorStore *CursorStore, cfg Config) *EventFetcher {
	if cfg.TTL == 0 {
		cfg.TTL = DefaultTTL
	}
	if cfg.FetchTimeout == 0 {
		cfg.FetchTimeout = DefaultFetchTimeout
	}
	if cfg.FetchLimit == 0 {
		cfg.FetchLimit = DefaultFetchLimit
	}
	return &EventFetcher{
		cursorStore: cursorStore,
		config:      cfg,
	}
}

// FetchEvents fetches events from a relay with cursor-based pagination
func (f *EventFetcher) FetchEvents(
	ctx context.Context,
	relay string,
	filter nostr.Filter,
	direction FetchDirection,
	onEvent func(*nostr.Event),
) (*FetchResult, error) {
	cursor, err := f.cursorStore.LoadCursor(relay, filter)
	if err != nil {
		return nil, fmt.Errorf("failed to load cursor: %w", err)
	}

	// Check TTL - skip if recently fetched
	if cursor.IsWithinTTL(f.config.TTL) {
		return &FetchResult{
			Events:    nil,
			Cursor:    cursor,
			Status:    FetchSkippedTTL,
			Direction: direction,
		}, nil
	}

	if direction == FetchBackward {
		return f.fetchBackward(ctx, relay, filter, cursor, onEvent)
	}
	return f.fetchForward(ctx, relay, filter, cursor, onEvent)
}

// fetchBackward fetches older events (before oldestFetched)
func (f *EventFetcher) fetchBackward(
	ctx context.Context,
	relayURL string,
	filter nostr.Filter,
	cursor *FetchCursor,
	onEvent func(*nostr.Event),
) (*FetchResult, error) {
	// Skip if we've already reached the beginning
	if cursor.ReachedBeginning {
		return &FetchResult{
			Events:    nil,
			Cursor:    cursor,
			Status:    FetchSkippedComplete,
			Direction: FetchBackward,
		}, nil
	}

	// Connect to relay with timeout
	ctx, cancel := context.WithTimeout(ctx, f.config.FetchTimeout)
	defer cancel()

	relay, err := nostr.RelayConnect(ctx, relayURL)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to relay %s: %w", relayURL, err)
	}
	defer relay.Close()

	// Determine "until" timestamp
	now := time.Now().Unix()
	until := now
	if cursor.OldestFetched != nil {
		until = *cursor.OldestFetched
	}

	// Build query filter with until timestamp
	untilTs := nostr.Timestamp(until)
	queryFilter := nostr.Filter{
		IDs:     filter.IDs,
		Authors: filter.Authors,
		Kinds:   filter.Kinds,
		Tags:    filter.Tags,
		Until:   &untilTs,
		Limit:   f.config.FetchLimit,
	}

	// Query events
	events, err := relay.QuerySync(ctx, queryFilter)
	if err != nil {
		return nil, fmt.Errorf("query failed: %w", err)
	}

	// Filter out boundary duplicates
	var newEvents []*nostr.Event
	for _, event := range events {
		if !cursor.OldestBoundaryIDs[event.ID] {
			newEvents = append(newEvents, event)
			if onEvent != nil {
				onEvent(event)
			}
		}
	}

	// Find oldest and newest timestamps from new events
	var oldestTimestamp, newestTimestamp *int64
	for _, event := range newEvents {
		ts := int64(event.CreatedAt)
		if oldestTimestamp == nil || ts < *oldestTimestamp {
			oldestTimestamp = &ts
		}
		if newestTimestamp == nil || ts > *newestTimestamp {
			newestTimestamp = &ts
		}
	}

	// Update boundary IDs
	if oldestTimestamp != nil {
		if cursor.OldestFetched != nil && *oldestTimestamp == *cursor.OldestFetched {
			// Same boundary, add new IDs
			for _, e := range newEvents {
				if int64(e.CreatedAt) == *oldestTimestamp {
					cursor.OldestBoundaryIDs[e.ID] = true
				}
			}
		} else {
			// New boundary, replace
			cursor.OldestBoundaryIDs = make(map[string]bool)
			for _, e := range newEvents {
				if int64(e.CreatedAt) == *oldestTimestamp {
					cursor.OldestBoundaryIDs[e.ID] = true
				}
			}
		}
		cursor.OldestFetched = oldestTimestamp
	}

	// Update newest boundary on first fetch
	if cursor.NewestFetched == nil && newestTimestamp != nil {
		cursor.NewestFetched = newestTimestamp
		cursor.NewestBoundaryIDs = make(map[string]bool)
		for _, e := range newEvents {
			if int64(e.CreatedAt) == *newestTimestamp {
				cursor.NewestBoundaryIDs[e.ID] = true
			}
		}
	}

	// Handle completion
	if len(events) == 0 {
		cursor.ReachedBeginning = true
	} else if len(newEvents) == 0 && cursor.OldestFetched != nil {
		// All events already seen - move past this boundary
		newOldest := *cursor.OldestFetched - 1
		cursor.OldestFetched = &newOldest
		cursor.OldestBoundaryIDs = make(map[string]bool)
	}

	// Update last fetched time and save
	now2 := time.Now()
	cursor.LastFetchedAt = &now2
	if err := f.cursorStore.SaveCursor(cursor); err != nil {
		log.Printf("[fetcher] Warning: failed to save cursor: %v", err)
	}

	return &FetchResult{
		Events:    newEvents,
		Cursor:    cursor,
		Status:    FetchSuccess,
		Direction: FetchBackward,
	}, nil
}

// fetchForward fetches newer events (after newestFetched)
func (f *EventFetcher) fetchForward(
	ctx context.Context,
	relayURL string,
	filter nostr.Filter,
	cursor *FetchCursor,
	onEvent func(*nostr.Event),
) (*FetchResult, error) {
	// Skip if no previous fetch exists
	if cursor.NewestFetched == nil {
		return &FetchResult{
			Events:    nil,
			Cursor:    cursor,
			Status:    FetchSkippedNoPrior,
			Direction: FetchForward,
		}, nil
	}

	ctx, cancel := context.WithTimeout(ctx, f.config.FetchTimeout)
	defer cancel()

	relay, err := nostr.RelayConnect(ctx, relayURL)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to relay %s: %w", relayURL, err)
	}
	defer relay.Close()

	since := *cursor.NewestFetched
	now := time.Now().Unix()
	sinceTs := nostr.Timestamp(since)
	untilTs := nostr.Timestamp(now)

	queryFilter := nostr.Filter{
		IDs:     filter.IDs,
		Authors: filter.Authors,
		Kinds:   filter.Kinds,
		Tags:    filter.Tags,
		Since:   &sinceTs,
		Until:   &untilTs,
		Limit:   f.config.FetchLimit,
	}

	events, err := relay.QuerySync(ctx, queryFilter)
	if err != nil {
		return nil, fmt.Errorf("query failed: %w", err)
	}

	// Filter boundary duplicates and find newest
	var newEvents []*nostr.Event
	var newestTimestamp *int64

	for _, event := range events {
		if !cursor.NewestBoundaryIDs[event.ID] {
			newEvents = append(newEvents, event)
			if onEvent != nil {
				onEvent(event)
			}
			ts := int64(event.CreatedAt)
			if newestTimestamp == nil || ts > *newestTimestamp {
				newestTimestamp = &ts
			}
		}
	}

	// Update cursor
	if newestTimestamp != nil {
		cursor.NewestFetched = newestTimestamp
		cursor.NewestBoundaryIDs = make(map[string]bool)
		for _, e := range newEvents {
			if int64(e.CreatedAt) == *newestTimestamp {
				cursor.NewestBoundaryIDs[e.ID] = true
			}
		}
	}

	now2 := time.Now()
	cursor.LastFetchedAt = &now2
	if err := f.cursorStore.SaveCursor(cursor); err != nil {
		log.Printf("[fetcher] Warning: failed to save cursor: %v", err)
	}

	return &FetchResult{
		Events:    newEvents,
		Cursor:    cursor,
		Status:    FetchSuccess,
		Direction: FetchForward,
	}, nil
}

// FetchContinuously fetches events in a loop until deadline or completion
func (f *EventFetcher) FetchContinuously(
	ctx context.Context,
	relay string,
	filter nostr.Filter,
	direction FetchDirection,
	deadline time.Time,
	onEvent func(*nostr.Event),
) (totalEvents int, reachedEnd bool, timedOut bool, skippedTTL bool, err error) {
	isFirstIteration := true

	for time.Now().Before(deadline) {
		// Create a temporary fetcher with TTL override for non-first iterations
		tempFetcher := f
		if !isFirstIteration {
			// Bypass TTL on subsequent iterations
			tempConfig := f.config
			tempConfig.TTL = 0
			tempFetcher = &EventFetcher{
				cursorStore: f.cursorStore,
				config:      tempConfig,
			}
		}
		isFirstIteration = false

		result, fetchErr := tempFetcher.FetchEvents(ctx, relay, filter, direction, onEvent)
		if fetchErr != nil {
			return totalEvents, false, false, false, fetchErr
		}

		switch result.Status {
		case FetchSkippedTTL:
			return totalEvents, false, false, true, nil
		case FetchSkippedComplete:
			return totalEvents, true, false, false, nil
		}

		totalEvents += len(result.Events)

		if !result.Cursor.HasMoreBackward() || len(result.Events) == 0 {
			return totalEvents, true, false, false, nil
		}
	}

	return totalEvents, false, true, false, nil
}

// ContinuousFetchSummary summarizes a continuous fetch operation
type ContinuousFetchSummary struct {
	Relay      string
	Filter     nostr.Filter
	Total      int
	ReachedEnd bool
	TimedOut   bool
	SkippedTTL bool
	Error      error
}

// FetchAllContinuously fetches from multiple relays and filters in parallel
func (f *EventFetcher) FetchAllContinuously(
	ctx context.Context,
	relays []string,
	filters []nostr.Filter,
	direction FetchDirection,
	deadline time.Time,
	onEvent func(*nostr.Event),
) []ContinuousFetchSummary {
	var mu sync.Mutex
	var wg sync.WaitGroup
	var results []ContinuousFetchSummary

	// Wrap onEvent with mutex for thread safety
	safeOnEvent := func(event *nostr.Event) {
		if onEvent != nil {
			mu.Lock()
			onEvent(event)
			mu.Unlock()
		}
	}

	for _, relay := range relays {
		for _, filter := range filters {
			wg.Add(1)
			go func(r string, fil nostr.Filter) {
				defer wg.Done()

				total, reachedEnd, timedOut, skippedTTL, err := f.FetchContinuously(
					ctx, r, fil, direction, deadline, safeOnEvent,
				)

				mu.Lock()
				results = append(results, ContinuousFetchSummary{
					Relay:      r,
					Filter:     fil,
					Total:      total,
					ReachedEnd: reachedEnd,
					TimedOut:   timedOut,
					SkippedTTL: skippedTTL,
					Error:      err,
				})
				mu.Unlock()
			}(relay, filter)
		}
	}

	wg.Wait()
	return results
}
