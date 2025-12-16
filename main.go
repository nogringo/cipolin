package main

import (
	"context"
	"log"
	"net/http"
	"os"

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
)

type Config struct {
	PrivateKey    string
	Port          string
	DaciteURL     string
	StorageRelays []string
	DBPath        string
}

func loadConfig() Config {
	cfg := Config{
		PrivateKey:    os.Getenv("NIP85_PRIVATE_KEY"),
		Port:          os.Getenv("PORT"),
		DaciteURL:     os.Getenv("DACITE_URL"),
		DBPath:        os.Getenv("DB_PATH"),
	}

	if cfg.Port == "" {
		cfg.Port = "3334"
	}
	if cfg.DaciteURL == "" {
		cfg.DaciteURL = "http://localhost:8090"
	}
	if cfg.DBPath == "" {
		cfg.DBPath = "./data/cipolin.db"
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
	db = &badger.BadgerBackend{Path: config.DBPath}
	if err := db.Init(); err != nil {
		log.Fatalf("Failed to initialize database: %v", err)
	}
	defer db.Close()
	log.Printf("Database initialized at %s", config.DBPath)

	// Create relay
	relay := khatru.NewRelay()
	relay.Info.Name = "NIP-85 Trusted Assertions Provider"
	relay.Info.Description = "On-demand computation of user and event metrics"
	relay.Info.PubKey = servicePubKey
	relay.Info.SupportedNIPs = []any{11, 85}

	// Use database for storage
	relay.StoreEvent = append(relay.StoreEvent, db.SaveEvent)
	relay.DeleteEvent = append(relay.DeleteEvent, db.DeleteEvent)
	relay.QueryEvents = append(relay.QueryEvents, db.QueryEvents)

	// Register NIP-85 query handler (runs before DB query)
	relay.QueryEvents = append(
		[]func(ctx context.Context, filter nostr.Filter) (chan *nostr.Event, error){handleNIP85Query},
		relay.QueryEvents...,
	)

	// Reject external events - only accept our own assertions
	relay.RejectEvent = append(relay.RejectEvent, func(_ context.Context, event *nostr.Event) (bool, string) {
		if event.PubKey != servicePubKey {
			return true, "this relay only serves NIP-85 assertions from this provider"
		}
		return false, ""
	})

	// Enable negentropy support
	relay.Negentropy = true

	log.Printf("Starting NIP-85 relay on :%s", config.Port)
	log.Printf("Dacite API: %s", config.DaciteURL)
	log.Printf("Storage relays: %v", config.StorageRelays)

	if err := http.ListenAndServe(":"+config.Port, relay); err != nil {
		log.Fatalf("Failed to start server: %v", err)
	}
}
