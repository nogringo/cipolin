package main

import (
	"encoding/json"
	"fmt"

	"github.com/dgraph-io/badger/v4"
	"github.com/nbd-wtf/go-nostr"
)

const cursorPrefix = "cursor:"

// CursorStore manages cursor persistence in Badger DB
type CursorStore struct {
	db *badger.DB
}

// NewCursorStore creates a new cursor store using the provided Badger DB
func NewCursorStore(db *badger.DB) *CursorStore {
	return &CursorStore{db: db}
}

// LoadCursor loads a cursor from DB or creates a new one if not found
func (s *CursorStore) LoadCursor(relay string, filter nostr.Filter) (*FetchCursor, error) {
	id := GenerateCursorID(relay, filter)
	key := []byte(cursorPrefix + id)

	var cursor *FetchCursor
	err := s.db.View(func(txn *badger.Txn) error {
		item, err := txn.Get(key)
		if err == badger.ErrKeyNotFound {
			return nil // Will create new cursor
		}
		if err != nil {
			return err
		}
		return item.Value(func(val []byte) error {
			cursor = &FetchCursor{}
			return json.Unmarshal(val, cursor)
		})
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

	return s.db.Update(func(txn *badger.Txn) error {
		return txn.Set(key, val)
	})
}

// DeleteCursor removes a cursor from DB
func (s *CursorStore) DeleteCursor(cursorID string) error {
	key := []byte(cursorPrefix + cursorID)
	return s.db.Update(func(txn *badger.Txn) error {
		return txn.Delete(key)
	})
}

// ListCursors returns all cursor IDs
func (s *CursorStore) ListCursors() ([]string, error) {
	var ids []string
	prefix := []byte(cursorPrefix)

	err := s.db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.PrefetchValues = false
		it := txn.NewIterator(opts)
		defer it.Close()

		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			key := it.Item().Key()
			id := string(key[len(prefix):])
			ids = append(ids, id)
		}
		return nil
	})

	return ids, err
}
