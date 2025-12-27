package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"cipolin/internal/fetcher"
	"cipolin/internal/handler"
	"cipolin/internal/keys"
	"cipolin/internal/relay"

	"github.com/fiatjaf/eventstore/badger"
	"github.com/fiatjaf/khatru"
	"github.com/joho/godotenv"
	"github.com/nbd-wtf/go-nostr"
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

func main() {
	// Load .env file
	if err := godotenv.Load(); err != nil {
		log.Printf("No .env file found, using environment variables")
	}

	config := loadConfig()

	// Load or generate master key
	masterKey := config.MasterKey
	if masterKey == "" {
		masterKey = nostr.GeneratePrivateKey()
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
	db := &badger.BadgerBackend{
		Path:     config.DBPath,
		MaxLimit: 10_000_000, // High limit for metrics computation (default is 1000)
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
	h := handler.NewHandler(syncer, db, config.StorageRelays, keyManager)

	// Create relay
	r := khatru.NewRelay()
	r.Info.Name = "NIP-85 Trusted Assertions Provider"
	r.Info.Description = "On-demand computation of user and event metrics with per-metric signing keys"
	r.Info.PubKey = keyManager.GetPubKey("rank") // Use rank pubkey as relay identity
	r.Info.SupportedNIPs = []any{11, 85}

	// Use database for storage
	r.StoreEvent = append(r.StoreEvent, db.SaveEvent)
	r.DeleteEvent = append(r.DeleteEvent, db.DeleteEvent)

	// Wrap DB query to skip NIP-85 kinds (we generate them on-demand)
	r.QueryEvents = append(r.QueryEvents, func(ctx context.Context, filter nostr.Filter) (chan *nostr.Event, error) {
		// Skip if only querying NIP-85 kinds
		if handler.IsOnlyNIP85Kinds(filter.Kinds) {
			ch := make(chan *nostr.Event)
			close(ch)
			return ch, nil
		}
		return db.QueryEvents(ctx, filter)
	})

	// Register NIP-85 query handler (generates fresh assertions)
	r.QueryEvents = append(
		[]func(ctx context.Context, filter nostr.Filter) (chan *nostr.Event, error){h.HandleNIP85Query},
		r.QueryEvents...,
	)

	// Enable negentropy support
	r.Negentropy = true

	// Setup HTTP routes
	mux := r.Router()

	// Endpoint to get metric pubkeys for client kind 10040 configuration
	mux.HandleFunc("/keys", func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")

		response := map[string]interface{}{
			"pubkeys":      keyManager.GetAllPubKeys(),
			"kind10040":    keyManager.GetKind10040Tags(config.RelayURL),
			"relay_url":    config.RelayURL,
			"user_metrics": keys.UserMetrics,
			"event_metrics": keys.EventMetrics,
		}
		json.NewEncoder(w).Encode(response)
	})

	// Root endpoint with info
	mux.HandleFunc("/", func(w http.ResponseWriter, req *http.Request) {
		// Let Khatru handle WebSocket upgrades
		if req.Header.Get("Upgrade") == "websocket" {
			return
		}

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
	})

	log.Printf("Starting NIP-85 relay on :%s", config.Port)
	log.Printf("Public relay URL: %s", config.RelayURL)
	log.Printf("Storage relays: %v", config.StorageRelays)
	log.Printf("Endpoints: / (relay), /keys (metric pubkeys)")

	if err := http.ListenAndServe(":"+config.Port, r); err != nil {
		log.Fatalf("Failed to start server: %v", err)
	}
}
