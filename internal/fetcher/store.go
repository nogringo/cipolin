package fetcher

import (
	"encoding/json"
	"fmt"

	"fiatjaf.com/nostr"
	bolt "go.etcd.io/bbolt"
)

const cursorPrefix = "cursor:"

var cursorsBucket = []byte("cursors")

// CursorStore manages cursor persistence in Bolt DB
type CursorStore struct {
	db *bolt.DB
}

// NewCursorStore creates a new cursor store using the provided Bolt DB
func NewCursorStore(db *bolt.DB) *CursorStore {
	// Ensure bucket exists
	db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists(cursorsBucket)
		return err
	})
	return &CursorStore{db: db}
}

// LoadCursor loads a cursor from DB or creates a new one if not found
func (s *CursorStore) LoadCursor(relay string, filter nostr.Filter) (*FetchCursor, error) {
	id := GenerateCursorID(relay, filter)
	key := []byte(cursorPrefix + id)

	var cursor *FetchCursor
	err := s.db.View(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(cursorsBucket)
		val := bucket.Get(key)
		if val == nil {
			return nil // Will create new cursor
		}
		cursor = &FetchCursor{}
		return json.Unmarshal(val, cursor)
	})

	if err != nil {
		return nil, fmt.Errorf("failed to load cursor: %w", err)
	}

	if cursor == nil {
		cursor = NewFetchCursor(relay, filter)
	}

	return cursor, nil
}

// SaveCursor persists a cursor to DB
func (s *CursorStore) SaveCursor(cursor *FetchCursor) error {
	key := []byte(cursorPrefix + cursor.ID)
	val, err := json.Marshal(cursor)
	if err != nil {
		return fmt.Errorf("failed to marshal cursor: %w", err)
	}

	return s.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(cursorsBucket)
		return bucket.Put(key, val)
	})
}

// DeleteCursor removes a cursor from DB
func (s *CursorStore) DeleteCursor(cursorID string) error {
	key := []byte(cursorPrefix + cursorID)
	return s.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(cursorsBucket)
		return bucket.Delete(key)
	})
}

// ListCursors returns all cursor IDs
func (s *CursorStore) ListCursors() ([]string, error) {
	var ids []string

	err := s.db.View(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(cursorsBucket)
		return bucket.ForEach(func(k, v []byte) error {
			if len(k) > len(cursorPrefix) && string(k[:len(cursorPrefix)]) == cursorPrefix {
				ids = append(ids, string(k[len(cursorPrefix):]))
			}
			return nil
		})
	})

	return ids, err
}
