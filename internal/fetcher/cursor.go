package fetcher

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"time"

	"github.com/nbd-wtf/go-nostr"
)

// FetchDirection indicates backward (older) or forward (newer) fetching
type FetchDirection int

const (
	FetchBackward FetchDirection = iota
	FetchForward
)

// FetchStatus indicates the result of a fetch operation
type FetchStatus int

const (
	FetchSuccess FetchStatus = iota
	FetchSkippedTTL
	FetchSkippedComplete
	FetchSkippedNoPrior
)

// FetchResult holds the result of a fetch operation
type FetchResult struct {
	Events    []*nostr.Event
	Cursor    *FetchCursor
	Status    FetchStatus
	Direction FetchDirection
}

// FetchCursor tracks the fetched range for a relay+filter combination
type FetchCursor struct {
	ID                string          `json:"id"`
	Relay             string          `json:"relay"`
	FilterHash        string          `json:"filterHash"`
	Filter            map[string]any  `json:"filter"`
	OldestFetched     *int64          `json:"oldestFetched,omitempty"`
	NewestFetched     *int64          `json:"newestFetched,omitempty"`
	ReachedBeginning  bool            `json:"reachedBeginning"`
	LastFetchedAt     *time.Time      `json:"lastFetchedAt,omitempty"`
	OldestBoundaryIDs map[string]bool `json:"oldestBoundaryIds"`
	NewestBoundaryIDs map[string]bool `json:"newestBoundaryIds"`
}

// NewFetchCursor creates a new cursor for a relay+filter combination
func NewFetchCursor(relay string, filter nostr.Filter) *FetchCursor {
	filterMap := filterToCanonicalMap(filter)
	return &FetchCursor{
		ID:                GenerateCursorID(relay, filter),
		Relay:             relay,
		FilterHash:        GenerateFilterHash(filter),
		Filter:            filterMap,
		ReachedBeginning:  false,
		OldestBoundaryIDs: make(map[string]bool),
		NewestBoundaryIDs: make(map[string]bool),
	}
}

// GenerateCursorID creates unique ID from relay and filter (SHA256 truncated to 16 chars)
func GenerateCursorID(relay string, filter nostr.Filter) string {
	filterJSON, _ := json.Marshal(filterToCanonicalMap(filter))
	combined := relay + "|" + string(filterJSON)
	hash := sha256.Sum256([]byte(combined))
	return hex.EncodeToString(hash[:])[:16]
}

// GenerateFilterHash creates a hash of the filter for identification
func GenerateFilterHash(filter nostr.Filter) string {
	filterJSON, _ := json.Marshal(filterToCanonicalMap(filter))
	hash := sha256.Sum256(filterJSON)
	return hex.EncodeToString(hash[:])[:16]
}

// IsWithinTTL checks if cursor was fetched within the given TTL duration
func (c *FetchCursor) IsWithinTTL(ttl time.Duration) bool {
	if c.LastFetchedAt == nil {
		return false
	}
	return time.Since(*c.LastFetchedAt) < ttl
}

// HasMoreBackward returns true if more events can be fetched backward
func (c *FetchCursor) HasMoreBackward() bool {
	return !c.ReachedBeginning
}

// filterToCanonicalMap converts nostr.Filter to a canonical map representation
// Excludes since/until as they are dynamic and not part of filter identity
func filterToCanonicalMap(f nostr.Filter) map[string]any {
	m := make(map[string]any)

	if len(f.IDs) > 0 {
		ids := make([]string, len(f.IDs))
		copy(ids, f.IDs)
		sort.Strings(ids)
		m["ids"] = ids
	}
	if len(f.Authors) > 0 {
		authors := make([]string, len(f.Authors))
		copy(authors, f.Authors)
		sort.Strings(authors)
		m["authors"] = authors
	}
	if len(f.Kinds) > 0 {
		kinds := make([]int, len(f.Kinds))
		copy(kinds, f.Kinds)
		sort.Ints(kinds)
		m["kinds"] = kinds
	}

	// Tag filters - sort for consistency
	if len(f.Tags) > 0 {
		tagKeys := make([]string, 0, len(f.Tags))
		for k := range f.Tags {
			tagKeys = append(tagKeys, k)
		}
		sort.Strings(tagKeys)

		for _, tag := range tagKeys {
			values := f.Tags[tag]
			if len(values) > 0 {
				sortedValues := make([]string, len(values))
				copy(sortedValues, values)
				sort.Strings(sortedValues)
				m["#"+tag] = sortedValues
			}
		}
	}

	return m
}
