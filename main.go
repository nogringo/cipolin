package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/fiatjaf/eventstore/badger"
	"github.com/fiatjaf/khatru"
	"github.com/joho/godotenv"
	"github.com/nbd-wtf/go-nostr"
)

var (
	servicePrivateKey string
	servicePubKey     string
	db                *badger.BadgerBackend
	config            Config
	fetcher           *EventFetcher
)

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
		cfg.FetchTTL = DefaultTTL
	}

	// Parse fetch timeout
	if timeoutStr := os.Getenv("FETCH_TIMEOUT_SECONDS"); timeoutStr != "" {
		if timeout, err := strconv.Atoi(timeoutStr); err == nil {
			cfg.FetchTimeout = time.Duration(timeout) * time.Second
		}
	}
	if cfg.FetchTimeout == 0 {
		cfg.FetchTimeout = DefaultFetchTimeout
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

func main() {
	// Load .env file
	if err := godotenv.Load(); err != nil {
		log.Printf("No .env file found, using environment variables")
	}

	config = loadConfig()

	// Load or generate private key
	servicePrivateKey = config.PrivateKey
	if servicePrivateKey == "" {
		servicePrivateKey = nostr.GeneratePrivateKey()
		log.Printf("WARNING: No NIP85_PRIVATE_KEY set. Generated temporary key.")
	}

	pub, err := nostr.GetPublicKey(servicePrivateKey)
	if err != nil {
		log.Fatalf("Invalid private key: %v", err)
	}
	servicePubKey = pub
	log.Printf("Service provider pubkey: %s", servicePubKey)

	// Initialize database
	db = &badger.BadgerBackend{
		Path:     config.DBPath,
		MaxLimit: 10_000_000, // High limit for metrics computation (default is 1000)
	}
	if err := db.Init(); err != nil {
		log.Fatalf("Failed to initialize database: %v", err)
	}
	defer db.Close()
	log.Printf("Database initialized at %s", config.DBPath)

	// Initialize cursor store and event fetcher
	cursorStore := NewCursorStore(db.DB)
	fetcher = NewEventFetcher(cursorStore, FetchConfig{
		TTL:          config.FetchTTL,
		FetchTimeout: config.FetchTimeout,
	})
	log.Printf("Event fetcher initialized (TTL: %v, Timeout: %v)", config.FetchTTL, config.FetchTimeout)

	// Create relay
	relay := khatru.NewRelay()
	relay.Info.Name = "NIP-85 Trusted Assertions Provider"
	relay.Info.Description = "On-demand computation of user and event metrics"
	relay.Info.PubKey = servicePubKey
	relay.Info.SupportedNIPs = []any{11, 85}

	// Use database for storage
	relay.StoreEvent = append(relay.StoreEvent, db.SaveEvent)
	relay.DeleteEvent = append(relay.DeleteEvent, db.DeleteEvent)

	// Wrap DB query to skip NIP-85 kinds (we generate them on-demand)
	relay.QueryEvents = append(relay.QueryEvents, func(ctx context.Context, filter nostr.Filter) (chan *nostr.Event, error) {
		// Skip if only querying NIP-85 kinds
		if isOnlyNIP85Kinds(filter.Kinds) {
			ch := make(chan *nostr.Event)
			close(ch)
			return ch, nil
		}
		return db.QueryEvents(ctx, filter)
	})

	// Register NIP-85 query handler (generates fresh assertions)
	relay.QueryEvents = append(
		[]func(ctx context.Context, filter nostr.Filter) (chan *nostr.Event, error){handleNIP85Query},
		relay.QueryEvents...,
	)

	// Accept all events - Dacite will publish fetched events here

	// Enable negentropy support
	relay.Negentropy = true

	log.Printf("Starting NIP-85 relay on :%s", config.Port)
	log.Printf("Storage relays: %v", config.StorageRelays)

	if err := http.ListenAndServe(":"+config.Port, relay); err != nil {
		log.Fatalf("Failed to start server: %v", err)
	}
}
