package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

type fakeRankEngine struct {
	ranks map[string]int
	ok    bool
	seen  struct {
		requester string
		target    string
	}
	invalidateCalls int
}

func (f *fakeRankEngine) GetRank(_ context.Context, requesterPubkey, targetPubkey string) (int, bool) {
	f.seen.requester = requesterPubkey
	f.seen.target = targetPubkey
	if !f.ok {
		return 0, false
	}
	return f.ranks[targetPubkey], true
}

func (f *fakeRankEngine) InvalidateCache() {
	f.invalidateCalls++
}

type fakeSyncer struct {
	pubkeys []string
}

func (f *fakeSyncer) SyncUserEvents(_ context.Context, pubkey string, _ []string) error {
	f.pubkeys = append(f.pubkeys, pubkey)
	return nil
}

func fakeRelayLookup(_ context.Context, _ []string, _ string) ([]string, error) {
	return []string{"wss://example.test"}, nil
}

func TestNewRankHandlerReturnsRank(t *testing.T) {
	requester := testHex('a')
	target := testHex('b')
	engine := &fakeRankEngine{ranks: map[string]int{target: 77}, ok: true}
	syncer := &fakeSyncer{}
	service := &rankHTTPService{engine: engine, syncer: syncer, storageRelays: []string{"wss://example.test"}, lookupRelays: fakeRelayLookup}

	req := httptest.NewRequest(http.MethodGet, "/rank?requester="+requester+"&target="+target, nil)
	rec := httptest.NewRecorder()

	newRankHandler(service).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, rec.Code)
	}

	if engine.seen.requester != requester || engine.seen.target != target {
		t.Fatalf("expected engine to receive requester=%s target=%s, got requester=%s target=%s", requester, target, engine.seen.requester, engine.seen.target)
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if got := int(body["rank"].(float64)); got != 77 {
		t.Fatalf("expected rank 77, got %d", got)
	}
	if body["requester"] != requester {
		t.Fatalf("expected requester %s, got %v", requester, body["requester"])
	}
	if body["target"] != target {
		t.Fatalf("expected target %s, got %v", target, body["target"])
	}
	if body["refresh"] != true {
		t.Fatalf("expected refresh=true, got %v", body["refresh"])
	}

	if !reflect.DeepEqual(syncer.pubkeys, []string{requester, target}) {
		t.Fatalf("expected refresh sync for requester and target, got %v", syncer.pubkeys)
	}
	if engine.invalidateCalls != 1 {
		t.Fatalf("expected cache invalidation once, got %d", engine.invalidateCalls)
	}

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("expected CORS header '*', got %q", got)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("expected content type application/json, got %q", got)
	}
}

func TestNewRankHandlerRejectsBadInput(t *testing.T) {
	requester := testHex('a')
	target := testHex('b')
	engine := &fakeRankEngine{ranks: map[string]int{target: 77}, ok: true}
	service := &rankHTTPService{engine: engine}

	t.Run("missing params", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/rank?requester="+requester, nil)
		rec := httptest.NewRecorder()

		newRankHandler(service).ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected status %d, got %d", http.StatusBadRequest, rec.Code)
		}
	})

	t.Run("invalid pubkey", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/rank?requester=not-hex&target="+target, nil)
		rec := httptest.NewRecorder()

		newRankHandler(service).ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected status %d, got %d", http.StatusBadRequest, rec.Code)
		}
	})

	t.Run("multiple targets belong on batch endpoint", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/rank?requester="+requester+"&target="+target+"&target="+testHex('c'), nil)
		rec := httptest.NewRecorder()

		newRankHandler(service).ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected status %d, got %d", http.StatusBadRequest, rec.Code)
		}
	})

	t.Run("invalid refresh flag", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/rank?requester="+requester+"&target="+target+"&refresh=maybe", nil)
		rec := httptest.NewRecorder()

		newRankHandler(service).ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected status %d, got %d", http.StatusBadRequest, rec.Code)
		}
	})

	t.Run("method not allowed", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/rank?requester="+requester+"&target="+target, nil)
		rec := httptest.NewRecorder()

		newRankHandler(service).ServeHTTP(rec, req)

		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("expected status %d, got %d", http.StatusMethodNotAllowed, rec.Code)
		}
	})

	t.Run("rank unavailable", func(t *testing.T) {
		unavailable := &fakeRankEngine{ok: false}
		unavailableService := &rankHTTPService{engine: unavailable}
		req := httptest.NewRequest(http.MethodGet, "/rank?requester="+requester+"&target="+target+"&refresh=false", nil)
		rec := httptest.NewRecorder()

		newRankHandler(unavailableService).ServeHTTP(rec, req)

		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("expected status %d, got %d", http.StatusServiceUnavailable, rec.Code)
		}
	})
}

func TestNewRankHandlerCanSkipRefresh(t *testing.T) {
	requester := testHex('a')
	target := testHex('b')
	engine := &fakeRankEngine{ranks: map[string]int{target: 42}, ok: true}
	syncer := &fakeSyncer{}
	service := &rankHTTPService{engine: engine, syncer: syncer, storageRelays: []string{"wss://example.test"}, lookupRelays: fakeRelayLookup}

	req := httptest.NewRequest(http.MethodGet, "/rank?requester="+requester+"&target="+target+"&refresh=false", nil)
	rec := httptest.NewRecorder()

	newRankHandler(service).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, rec.Code)
	}
	if len(syncer.pubkeys) != 0 {
		t.Fatalf("expected no sync calls when refresh=false, got %v", syncer.pubkeys)
	}
	if engine.invalidateCalls != 0 {
		t.Fatalf("expected no invalidation when refresh=false, got %d", engine.invalidateCalls)
	}
}

func TestNewRankBatchHandlerReturnsRanks(t *testing.T) {
	requester := testHex('a')
	targetOne := testHex('b')
	targetTwo := testHex('c')
	engine := &fakeRankEngine{ranks: map[string]int{targetOne: 55, targetTwo: 81}, ok: true}
	service := &rankHTTPService{engine: engine}

	req := httptest.NewRequest(http.MethodGet, "/rank/batch?requester="+requester+"&target="+targetOne+","+targetTwo+"&refresh=false", nil)
	rec := httptest.NewRecorder()

	newRankBatchHandler(service).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, rec.Code)
	}

	var body struct {
		Requester string         `json:"requester"`
		Refresh   bool           `json:"refresh"`
		Results   []rankResponse `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("failed to decode batch response: %v", err)
	}

	if body.Requester != requester {
		t.Fatalf("expected requester %s, got %s", requester, body.Requester)
	}
	if body.Refresh {
		t.Fatalf("expected refresh=false, got true")
	}
	expected := []rankResponse{{Pubkey: targetOne, Rank: 55}, {Pubkey: targetTwo, Rank: 81}}
	if !reflect.DeepEqual(body.Results, expected) {
		t.Fatalf("expected results %v, got %v", expected, body.Results)
	}
}

func TestNewRankBatchHandlerRejectsBadInput(t *testing.T) {
	requester := testHex('a')
	target := testHex('b')
	engine := &fakeRankEngine{ranks: map[string]int{target: 10}, ok: true}
	service := &rankHTTPService{engine: engine}

	t.Run("missing target", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/rank/batch?requester="+requester, nil)
		rec := httptest.NewRecorder()

		newRankBatchHandler(service).ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected status %d, got %d", http.StatusBadRequest, rec.Code)
		}
	})

	t.Run("invalid target", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/rank/batch?requester="+requester+"&target=bad-target", nil)
		rec := httptest.NewRecorder()

		newRankBatchHandler(service).ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected status %d, got %d", http.StatusBadRequest, rec.Code)
		}
	})
}

func testHex(ch byte) string {
	b := make([]byte, 64)
	for i := range b {
		b[i] = ch
	}
	return string(b)
}
