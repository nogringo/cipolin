package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"iter"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"cipolin/internal/fetcher"
	"cipolin/internal/handler"
	"cipolin/internal/keys"
	"cipolin/internal/metrics"
	"cipolin/internal/relay"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/eventstore/boltdb"
	"fiatjaf.com/nostr/khatru"
	"fiatjaf.com/nostr/khatru/policies"
	"github.com/joho/godotenv"
)

// Config holds application configuration
type Config struct {
	MasterKey     string
	Port          string
	StorageRelays []string
	DBPath        string
	FetchTTL      time.Duration
	FetchTimeout  time.Duration
	RelayURL      string // Public relay URL for kind 10040 tags
	RankCacheTTL  time.Duration
}

type rankEngine interface {
	GetRank(ctx context.Context, requesterPubkey, targetPubkey string) (int, bool)
	InvalidateCache()
}

type userEventSyncer interface {
	SyncUserEvents(ctx context.Context, pubkey string, userRelays []string) error
}

type relayLookupFunc func(ctx context.Context, storageRelays []string, pubkey string) ([]string, error)

type rankHTTPService struct {
	engine        rankEngine
	syncer        userEventSyncer
	storageRelays []string
	lookupRelays  relayLookupFunc
}

type rankResponse struct {
	Pubkey string `json:"pubkey"`
	Rank   int    `json:"rank"`
}

func loadConfig() Config {
	cfg := Config{
		MasterKey: os.Getenv("NIP85_MASTER_KEY"),
		Port:      os.Getenv("PORT"),
		DBPath:    os.Getenv("DB_PATH"),
		RelayURL:  os.Getenv("RELAY_URL"),
	}

	// Fallback to old env var name for backwards compatibility
	if cfg.MasterKey == "" {
		cfg.MasterKey = os.Getenv("NIP85_PRIVATE_KEY")
	}

	if cfg.Port == "" {
		cfg.Port = "3334"
	}
	if cfg.DBPath == "" {
		cfg.DBPath = "./data/cipolin.db"
	}
	if cfg.RelayURL == "" {
		cfg.RelayURL = "wss://localhost:" + cfg.Port
	}

	// Parse fetch TTL
	if ttlStr := os.Getenv("FETCH_TTL_SECONDS"); ttlStr != "" {
		if ttl, err := strconv.Atoi(ttlStr); err == nil {
			cfg.FetchTTL = time.Duration(ttl) * time.Second
		}
	}
	if cfg.FetchTTL == 0 {
		cfg.FetchTTL = fetcher.DefaultTTL
	}

	// Parse fetch timeout
	if timeoutStr := os.Getenv("FETCH_TIMEOUT_SECONDS"); timeoutStr != "" {
		if timeout, err := strconv.Atoi(timeoutStr); err == nil {
			cfg.FetchTimeout = time.Duration(timeout) * time.Second
		}
	}
	if cfg.FetchTimeout == 0 {
		cfg.FetchTimeout = fetcher.DefaultFetchTimeout
	}

	if ttlStr := os.Getenv("RANK_CACHE_TTL_SECONDS"); ttlStr != "" {
		if ttl, err := strconv.Atoi(ttlStr); err == nil {
			cfg.RankCacheTTL = time.Duration(ttl) * time.Second
		}
	}
	if cfg.RankCacheTTL == 0 {
		cfg.RankCacheTTL = 5 * time.Minute
	}

	// Parse storage relays
	if relays := os.Getenv("STORAGE_RELAYS"); relays != "" {
		cfg.StorageRelays = splitRelays(relays)
	} else {
		cfg.StorageRelays = []string{
			"wss://nostr-01.uid.ovh",
			"wss://nostr-02.uid.ovh",
		}
	}

	return cfg
}

// splitRelays parses a comma-separated relay list
func splitRelays(s string) []string {
	parts := strings.Split(s, ",")
	relays := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			relays = append(relays, p)
		}
	}
	return relays
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		log.Printf("Failed to encode JSON response: %v", err)
	}
}

func firstQueryValue(req *http.Request, keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(req.URL.Query().Get(key)); value != "" {
			return value
		}
	}
	return ""
}

func queryValues(req *http.Request, keys ...string) []string {
	values := req.URL.Query()
	collected := make([]string, 0, len(keys))
	for _, key := range keys {
		collected = append(collected, values[key]...)
	}
	return collected
}

func isValidHexPubkey(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func parseBoolParam(value string, defaultValue bool) (bool, error) {
	if value == "" {
		return defaultValue, nil
	}

	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on":
		return true, nil
	case "0", "false", "no", "off":
		return false, nil
	default:
		return false, strconv.ErrSyntax
	}
}

func parseTargetPubkeys(req *http.Request) []string {
	rawTargets := queryValues(req, "target", "target_pubkey", "pubkey")
	if len(rawTargets) == 0 {
		return nil
	}

	targets := make([]string, 0, len(rawTargets))
	seen := make(map[string]struct{})
	for _, raw := range rawTargets {
		for _, part := range strings.Split(raw, ",") {
			target := strings.TrimSpace(part)
			if target == "" {
				continue
			}
			if _, ok := seen[target]; ok {
				continue
			}
			seen[target] = struct{}{}
			targets = append(targets, target)
		}
	}

	return targets
}

func validatePubkeys(pubkeys ...string) bool {
	for _, pubkey := range pubkeys {
		if !isValidHexPubkey(pubkey) {
			return false
		}
	}
	return true
}

func (s *rankHTTPService) refreshRankInputs(ctx context.Context, requesterPubkey string, targetPubkeys []string) error {
	if s == nil || s.syncer == nil || s.lookupRelays == nil {
		return nil
	}

	pubkeys := make([]string, 0, len(targetPubkeys)+1)
	seen := make(map[string]struct{}, len(targetPubkeys)+1)
	for _, pubkey := range append([]string{requesterPubkey}, targetPubkeys...) {
		if _, ok := seen[pubkey]; ok {
			continue
		}
		seen[pubkey] = struct{}{}
		pubkeys = append(pubkeys, pubkey)
	}

	for _, pubkey := range pubkeys {
		userRelays, err := s.lookupRelays(ctx, s.storageRelays, pubkey)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			log.Printf("[rank] Failed to discover relays for %s: %v", pubkey[:16]+"...", err)
			userRelays = s.storageRelays
		}
		if len(userRelays) == 0 {
			userRelays = s.storageRelays
		}

		if err := s.syncer.SyncUserEvents(ctx, pubkey, userRelays); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
	}

	if s.engine != nil {
		s.engine.InvalidateCache()
	}

	return nil
}

func (s *rankHTTPService) getRanks(ctx context.Context, requesterPubkey string, targetPubkeys []string, refresh bool) ([]rankResponse, error) {
	if s == nil || s.engine == nil {
		return nil, http.ErrServerClosed
	}

	if refresh {
		if err := s.refreshRankInputs(ctx, requesterPubkey, targetPubkeys); err != nil {
			return nil, err
		}
	}

	results := make([]rankResponse, 0, len(targetPubkeys))
	for _, targetPubkey := range targetPubkeys {
		rank, ok := s.engine.GetRank(ctx, requesterPubkey, targetPubkey)
		if !ok {
			return nil, nil
		}
		results = append(results, rankResponse{Pubkey: targetPubkey, Rank: rank})
	}

	return results, nil
}

func newRankHandler(service *rankHTTPService) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
			return
		}

		if service == nil || service.engine == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "rank engine unavailable"})
			return
		}

		requesterPubkey := firstQueryValue(req, "requester", "requester_pubkey")
		targetPubkeys := parseTargetPubkeys(req)
		refresh, err := parseBoolParam(firstQueryValue(req, "refresh"), true)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "refresh must be a boolean"})
			return
		}

		if requesterPubkey == "" || len(targetPubkeys) == 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "missing required query params: requester and target",
			})
			return
		}
		if len(targetPubkeys) != 1 {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "use /rank/batch for multiple targets",
			})
			return
		}

		targetPubkey := targetPubkeys[0]
		if !validatePubkeys(requesterPubkey, targetPubkey) {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "requester and target must be 64-character hex pubkeys",
			})
			return
		}

		results, err := service.getRanks(req.Context(), requesterPubkey, targetPubkeys, refresh)
		if err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "rank refresh failed"})
			return
		}
		if len(results) != 1 {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "rank unavailable"})
			return
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"requester": requesterPubkey,
			"target":    targetPubkey,
			"rank":      results[0].Rank,
			"refresh":   refresh,
		})
	}
}

func newRankBatchHandler(service *rankHTTPService) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
			return
		}

		if service == nil || service.engine == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "rank engine unavailable"})
			return
		}

		requesterPubkey := firstQueryValue(req, "requester", "requester_pubkey")
		targetPubkeys := parseTargetPubkeys(req)
		refresh, err := parseBoolParam(firstQueryValue(req, "refresh"), true)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "refresh must be a boolean"})
			return
		}

		if requesterPubkey == "" || len(targetPubkeys) == 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "missing required query params: requester and target",
			})
			return
		}
		if !validatePubkeys(append([]string{requesterPubkey}, targetPubkeys...)...) {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "requester and targets must be 64-character hex pubkeys",
			})
			return
		}

		results, err := service.getRanks(req.Context(), requesterPubkey, targetPubkeys, refresh)
		if err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "rank refresh failed"})
			return
		}
		if len(results) != len(targetPubkeys) {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "rank unavailable"})
			return
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"requester": requesterPubkey,
			"refresh":   refresh,
			"results":   results,
		})
	}
}

func main() {
	// Load .env file
	if err := godotenv.Load(); err != nil {
		log.Printf("No .env file found, using environment variables")
	}

	config := loadConfig()

	// Load or generate master key
	masterKey := config.MasterKey
	if masterKey == "" {
		sk := nostr.Generate()
		masterKey = sk.Hex()
		log.Printf("WARNING: No NIP85_MASTER_KEY set. Generated temporary key.")
		log.Printf("WARNING: Set NIP85_MASTER_KEY in .env for persistent metric keys.")
	}

	// Initialize metric key manager
	keyManager := keys.NewMetricKeyManager(masterKey)

	// Log all metric pubkeys
	log.Printf("=== Metric Public Keys ===")
	for metric, pubkey := range keyManager.GetAllPubKeys() {
		log.Printf("  %s: %s", metric, pubkey)
	}

	// Initialize database
	db := &boltdb.BoltBackend{
		Path: config.DBPath,
	}
	if err := db.Init(); err != nil {
		log.Fatalf("Failed to initialize database: %v", err)
	}
	defer db.Close()
	log.Printf("Database initialized at %s", config.DBPath)

	// Initialize cursor store and event fetcher
	cursorStore := fetcher.NewCursorStore(db.DB)
	eventFetcher := fetcher.NewEventFetcher(cursorStore, fetcher.Config{
		TTL:          config.FetchTTL,
		FetchTimeout: config.FetchTimeout,
	})
	log.Printf("Event fetcher initialized (TTL: %v, Timeout: %v)", config.FetchTTL, config.FetchTimeout)

	// Initialize syncer and handler
	syncer := relay.NewSyncer(eventFetcher, db, config.StorageRelays)
	rankEngine := metrics.NewGrapeRankEngine(db, config.RankCacheTTL, metrics.RankWeights{
		PositiveByKind: map[int]float64{
			metrics.SignalFollow:   1.0,
			metrics.SignalPost:     0.5,
			metrics.SignalRepost:   0.8,
			metrics.SignalReaction: 0.4,
			metrics.SignalNutzap:   1.2,
			metrics.SignalZap:      1.2,
		},
		NegativeByKind: map[int]float64{
			metrics.SignalReport: 1.0,
		},
		ReportPenaltyFactor:        0.75,
		RequesterPenaltyMultiplier: 2.0,
		PageRankAlpha:              0.85,
		PageRankMaxIter:            300,
		PageRankTolerance:          1e-8,
	})
	h := handler.NewHandler(syncer, db, config.StorageRelays, keyManager, rankEngine)

	// Create relay
	r := khatru.NewRelay()
	r.Info.Name = "NIP-85 Trusted Assertions Provider"
	r.Info.Description = "On-demand computation of user and event metrics with per-metric signing keys"
	pubKey := nostr.MustPubKeyFromHex(keyManager.GetPubKey("rank"))
	r.Info.PubKey = &pubKey // Use rank pubkey as relay identity
	r.Info.SupportedNIPs = []any{11, 85}

	// Use eventstore for storage
	r.UseEventstore(db, 10_000_000)

	// Compose event validation policies using SeqEvent
	r.OnEvent = policies.SeqEvent(
		policies.PreventTimestampsInThePast(24*time.Hour),
		policies.PreventTimestampsInTheFuture(2*time.Hour),
	)

	// Setup query handlers - order matters!
	// 1. NIP-85 handler (generates fresh assertions on-demand)
	// 2. Wrapper that skips NIP-85 kinds for regular DB queries
	originalQueryStored := r.QueryStored
	r.QueryStored = func(ctx context.Context, filter nostr.Filter) iter.Seq[nostr.Event] {
		// Chain: NIP-85 handler first, then skip wrapper, then original
		return func(yield func(nostr.Event) bool) {
			// First: NIP-85 handler
			for evt := range h.HandleNIP85QueryQueryStored(ctx, filter) {
				if !yield(evt) {
					return
				}
			}
			// Skip if only querying NIP-85 kinds
			if handler.IsOnlyNIP85Kinds(filter.Kinds) {
				return
			}
			// Then: original eventstore queries
			for evt := range originalQueryStored(ctx, filter) {
				if !yield(evt) {
					return
				}
			}
		}
	}

	// Enable negentropy support
	r.Negentropy = true

	// Setup HTTP routes using the relay's router
	mux := http.NewServeMux()
	rankService := &rankHTTPService{
		engine:        rankEngine,
		syncer:        syncer,
		storageRelays: config.StorageRelays,
		lookupRelays:  relay.GetUserNIP65Relays,
	}
	rankHandler := newRankHandler(rankService)
	rankBatchHandler := newRankBatchHandler(rankService)

	// Delegate to khatru's built-in HTTP handler (WebSocket support)
	mux.HandleFunc("/", func(w http.ResponseWriter, req *http.Request) {
		// Let Khatru handle WebSocket upgrades
		if req.Header.Get("Upgrade") == "websocket" {
			r.ServeHTTP(w, req)
			return
		}

		// Custom endpoints
		switch req.URL.Path {
		case "/rank/batch":
			rankBatchHandler(w, req)
			return
		case "/rank":
			rankHandler(w, req)
			return
		case "/keys":
			response := map[string]interface{}{
				"pubkeys":       keyManager.GetAllPubKeys(),
				"kind10040":     keyManager.GetKind10040Tags(config.RelayURL),
				"relay_url":     config.RelayURL,
				"user_metrics":  keys.UserMetrics,
				"event_metrics": keys.EventMetrics,
			}
			writeJSON(w, http.StatusOK, response)
			return
		default:
			response := map[string]interface{}{
				"name":        "NIP-85 Trusted Assertions Provider",
				"description": "On-demand computation of user and event metrics",
				"nip85":       true,
				"endpoints": map[string]string{
					"/":           "Relay info / WebSocket",
					"/keys":       "Metric pubkeys for kind 10040 configuration",
					"/rank":       "Requester-personalized GrapeRank as JSON",
					"/rank/batch": "Requester-personalized GrapeRank for multiple targets as JSON",
				},
				"supported_kinds": []int{30382, 30383, 30384},
			}
			writeJSON(w, http.StatusOK, response)
			return
		}
	})

	log.Printf("Starting NIP-85 relay on :%s", config.Port)
	log.Printf("Public relay URL: %s", config.RelayURL)
	log.Printf("Storage relays: %v", config.StorageRelays)
	log.Printf("Endpoints: / (relay), /keys (metric pubkeys), /rank, /rank/batch")

	if err := http.ListenAndServe(":"+config.Port, mux); err != nil {
		log.Fatalf("Failed to start server: %v", err)
	}
}
