package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"cipolin/internal/fetcher"
	"cipolin/internal/handler"
	"cipolin/internal/relay"

	"github.com/fiatjaf/eventstore/badger"
	"github.com/fiatjaf/khatru"
	"github.com/joho/godotenv"
	"github.com/nbd-wtf/go-nostr"
)

// Config holds application configuration
type Config struct {
	PrivateKey    string
	Port          string
	StorageRelays []string
	DBPath        string
	FetchTTL      time.Duration
	FetchTimeout  time.Duration
}

func loadConfig() Config {
	cfg := Config{
		PrivateKey: os.Getenv("NIP85_PRIVATE_KEY"),
		Port:       os.Getenv("PORT"),
		DBPath:     os.Getenv("DB_PATH"),
	}

	if cfg.Port == "" {
		cfg.Port = "3334"
	}
	if cfg.DBPath == "" {
		cfg.DBPath = "./data/cipolin.db"
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

	// Load or generate private key
	privateKey := config.PrivateKey
	if privateKey == "" {
		privateKey = nostr.GeneratePrivateKey()
		log.Printf("WARNING: No NIP85_PRIVATE_KEY set. Generated temporary key.")
	}

	pubKey, err := nostr.GetPublicKey(privateKey)
	if err != nil {
		log.Fatalf("Invalid private key: %v", err)
	}
	log.Printf("Service provider pubkey: %s", pubKey)

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
	h := handler.NewHandler(syncer, db, config.StorageRelays, privateKey, pubKey)

	// Create relay
	r := khatru.NewRelay()
	r.Info.Name = "NIP-85 Trusted Assertions Provider"
	r.Info.Description = "On-demand computation of user and event metrics"
	r.Info.PubKey = pubKey
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

	log.Printf("Starting NIP-85 relay on :%s", config.Port)
	log.Printf("Storage relays: %v", config.StorageRelays)

	if err := http.ListenAndServe(":"+config.Port, r); err != nil {
		log.Fatalf("Failed to start server: %v", err)
	}
}
