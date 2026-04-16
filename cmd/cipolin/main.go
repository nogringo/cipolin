package main

import (
	"context"
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
	"cipolin/internal/requestpolicy"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/eventstore/boltdb"
	"fiatjaf.com/nostr/khatru"
	"fiatjaf.com/nostr/khatru/policies"
	"github.com/joho/godotenv"
)

// Config holds application configuration
type Config struct {
	MasterKey             string
	Port                  string
	StorageRelays         []string
	DBPath                string
	FetchTTL              time.Duration
	FetchTimeout          time.Duration
	RelayURL              string // Public relay URL for kind 10040 tags
	Neo4jURI              string
	Neo4jUsername         string
	Neo4jPassword         string
	Neo4jDatabase         string
	RankCacheTTL          time.Duration
	Neo4jTimeout          time.Duration
	EnableNIP42Auth       bool
	RequestPolicyPlugin   string
	RequestPolicyTimeout  time.Duration
	RequestPolicyFailOpen bool
}

func loadConfig() Config {
	cfg := Config{
		MasterKey:           os.Getenv("NIP85_MASTER_KEY"),
		Port:                os.Getenv("PORT"),
		DBPath:              os.Getenv("DB_PATH"),
		RelayURL:            os.Getenv("RELAY_URL"),
		Neo4jURI:            os.Getenv("NEO4J_URI"),
		Neo4jUsername:       os.Getenv("NEO4J_USERNAME"),
		Neo4jPassword:       os.Getenv("NEO4J_PASSWORD"),
		Neo4jDatabase:       os.Getenv("NEO4J_DATABASE"),
		RequestPolicyPlugin: os.Getenv("REQUEST_POLICY_PLUGIN"),
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

	if cfg.Neo4jURI == "" {
		cfg.Neo4jURI = "neo4j://localhost:7687"
	}
	if cfg.Neo4jUsername == "" {
		cfg.Neo4jUsername = "neo4j"
	}
	if cfg.Neo4jDatabase == "" {
		cfg.Neo4jDatabase = "neo4j"
	}

	if ttlStr := os.Getenv("RANK_CACHE_TTL_SECONDS"); ttlStr != "" {
		if ttl, err := strconv.Atoi(ttlStr); err == nil {
			cfg.RankCacheTTL = time.Duration(ttl) * time.Second
		}
	}
	if cfg.RankCacheTTL == 0 {
		cfg.RankCacheTTL = 15 * time.Second
	}

	if timeoutStr := os.Getenv("NEO4J_QUERY_TIMEOUT_SECONDS"); timeoutStr != "" {
		if timeout, err := strconv.Atoi(timeoutStr); err == nil {
			cfg.Neo4jTimeout = time.Duration(timeout) * time.Second
		}
	}
	if cfg.Neo4jTimeout == 0 {
		cfg.Neo4jTimeout = 20 * time.Second
	}

	cfg.EnableNIP42Auth = parseBoolEnv("ENABLE_NIP42_AUTH", false)
	cfg.RequestPolicyFailOpen = parseBoolEnv("REQUEST_POLICY_FAIL_OPEN", false)

	if timeoutMsStr := os.Getenv("REQUEST_POLICY_TIMEOUT_MS"); timeoutMsStr != "" {
		if timeoutMs, err := strconv.Atoi(timeoutMsStr); err == nil {
			cfg.RequestPolicyTimeout = time.Duration(timeoutMs) * time.Millisecond
		}
	}
	if cfg.RequestPolicyTimeout == 0 {
		cfg.RequestPolicyTimeout = 1500 * time.Millisecond
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

func parseBoolEnv(key string, defaultValue bool) bool {
	v := strings.TrimSpace(strings.ToLower(os.Getenv(key)))
	if v == "" {
		return defaultValue
	}

	switch v {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return defaultValue
	}
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
	if config.Neo4jPassword == "" {
		log.Fatalf("NEO4J_PASSWORD must be set in environment/.env")
	}

	graphRank, err := metrics.NewGraphRankEngine(context.Background(), metrics.GraphRankEngineConfig{
		URI:          config.Neo4jURI,
		Username:     config.Neo4jUsername,
		Password:     config.Neo4jPassword,
		Database:     config.Neo4jDatabase,
		CacheTTL:     config.RankCacheTTL,
		QueryTimeout: config.Neo4jTimeout,
	})
	if err != nil {
		log.Fatalf("Failed to initialize Neo4j rank engine: %v", err)
	}
	defer graphRank.Close(context.Background())

	syncer := relay.NewSyncer(eventFetcher, db, config.StorageRelays, graphRank, graphRank)
	h := handler.NewHandler(syncer, db, config.StorageRelays, keyManager, graphRank)

	// Create relay
	r := khatru.NewRelay()
	r.Info.Name = "NIP-85 Trusted Assertions Provider"
	r.Info.Description = "On-demand computation of user and event metrics with per-metric signing keys"
	pubKey := nostr.MustPubKeyFromHex(keyManager.GetPubKey("30382", "rank"))
	r.Info.PubKey = &pubKey // Use rank pubkey as relay identity
	r.Info.SupportedNIPs = []any{11, 42, 85}

	var requestPlugin *requestpolicy.PluginEngine
	if config.RequestPolicyPlugin != "" {
		requestPlugin = requestpolicy.NewPluginEngine(requestpolicy.Config{
			Command:  config.RequestPolicyPlugin,
			Timeout:  config.RequestPolicyTimeout,
			FailOpen: config.RequestPolicyFailOpen,
		}, log.Default())
		defer requestPlugin.Close()
	}

	requestPolicies := make([]func(context.Context, nostr.Filter) (bool, string), 0, 2)
	countPolicies := make([]func(context.Context, nostr.Filter) (bool, string), 0, 2)

	if config.EnableNIP42Auth {
		requestPolicies = append(requestPolicies, policies.MustAuth)
		countPolicies = append(countPolicies, policies.MustAuth)
	}

	if requestPlugin != nil {
		requestPolicies = append(requestPolicies, func(ctx context.Context, filter nostr.Filter) (bool, string) {
			return requestPlugin.EvaluateRequest(ctx, filter, "REQ")
		})
		countPolicies = append(countPolicies, func(ctx context.Context, filter nostr.Filter) (bool, string) {
			return requestPlugin.EvaluateRequest(ctx, filter, "COUNT")
		})
	}

	if len(requestPolicies) > 0 {
		r.OnRequest = policies.SeqRequest(requestPolicies...)
	}

	if len(countPolicies) > 0 {
		r.OnCount = policies.SeqRequest(countPolicies...)
	}

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

	// Delegate to khatru's built-in HTTP handler (WebSocket support)
	mux.HandleFunc("/", func(w http.ResponseWriter, req *http.Request) {
		// Let Khatru handle WebSocket upgrades
		if req.Header.Get("Upgrade") == "websocket" {
			r.ServeHTTP(w, req)
			return
		}

		// Custom endpoints
		switch req.URL.Path {
		case "/keys":
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Access-Control-Allow-Origin", "*")

			response := map[string]interface{}{
				"pubkeys":       keyManager.GetAllPubKeys(),
				"kind10040":     keyManager.GetKind10040Tags(config.RelayURL),
				"relay_url":     config.RelayURL,
				"user_metrics":  keys.UserMetrics,
				"event_metrics": keys.EventMetrics,
			}
			json.NewEncoder(w).Encode(response)
		default:
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Access-Control-Allow-Origin", "*")

			response := map[string]interface{}{
				"name":        "NIP-85 Trusted Assertions Provider",
				"description": "On-demand computation of user and event metrics",
				"nip85":       true,
				"endpoints": map[string]string{
					"/":     "Relay info / WebSocket",
					"/keys": "Metric pubkeys for kind 10040 configuration",
				},
				"supported_kinds": []int{30382, 30383, 30384},
			}
			json.NewEncoder(w).Encode(response)
		}
	})

	log.Printf("Starting NIP-85 relay on :%s", config.Port)
	log.Printf("Public relay URL: %s", config.RelayURL)
	log.Printf("Storage relays: %v", config.StorageRelays)
	log.Printf("Neo4j: %s (db=%s, rank cache ttl=%v)", config.Neo4jURI, config.Neo4jDatabase, config.RankCacheTTL)
	log.Printf("NIP-42 required for REQ/COUNT: %t", config.EnableNIP42Auth)
	if config.RequestPolicyPlugin != "" {
		log.Printf("Request policy plugin enabled: %s (timeout=%v, fail_open=%t)", config.RequestPolicyPlugin, config.RequestPolicyTimeout, config.RequestPolicyFailOpen)
	}
	log.Printf("Endpoints: / (relay), /keys (metric pubkeys)")

	if err := http.ListenAndServe(":"+config.Port, r); err != nil {
		log.Fatalf("Failed to start server: %v", err)
	}
}
