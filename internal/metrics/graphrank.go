package metrics

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"fiatjaf.com/nostr"
	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
)

const graphName = "cipolin-follow-graph"

// UserRankProvider provides rank values for user assertions.
// perspective is the hex pubkey of the requesting user for personalized ranking;
// pass an empty string to get global (non-personalized) rank.
type UserRankProvider interface {
	GetUserRank(ctx context.Context, pubkey string, perspective string) (int, error)
}

// GraphIngestor ingests events into the backing graph store.
type GraphIngestor interface {
	IngestEvent(ctx context.Context, event nostr.Event) (bool, error)
}

// RankCacheInvalidator invalidates cached rank results.
type RankCacheInvalidator interface {
	InvalidateCache()
}

// GraphRankEngineConfig contains Neo4j and cache settings.
type GraphRankEngineConfig struct {
	URI          string
	Username     string
	Password     string
	Database     string
	CacheTTL     time.Duration
	QueryTimeout time.Duration

	// GrapeRankEnabled switches from global PageRank to personalized PageRank
	// (graperank). Requires NIP-42 authentication. When disabled, or when the
	// requesting user is not authenticated, global PageRank is used instead.
	GrapeRankEnabled bool

	// SignalWeightFollow is the edge weight stored on FOLLOWS relationships.
	// Default: 1.0.
	SignalWeightFollow float64

	// SignalWeightMute is applied as a post-processing score multiplier on users
	// that the perspective user mutes. Default 0.0 fully suppresses muted users.
	SignalWeightMute float64

	// SignalWeightReport is applied as a post-processing score multiplier on users
	// that the perspective user has reported. Default 0.0 fully suppresses them.
	SignalWeightReport float64
}

// rankCacheEntry holds a scored cache result for a single rank perspective.
type rankCacheEntry struct {
	scores    map[string]float64
	maxScore  float64
	expiresAt time.Time
}

// GraphRankEngine computes user rank with Neo4j GDS PageRank.
type GraphRankEngine struct {
	driver         neo4j.DriverWithContext
	database       string
	cacheTTL       time.Duration
	queryTimeout   time.Duration
	projectionName string

	grapeRankEnabled   bool
	signalWeightFollow float64
	signalWeightMute   float64
	signalWeightReport float64

	mu              sync.RWMutex
	globalCache     rankCacheEntry
	perspCaches     map[string]rankCacheEntry
	projectionDirty bool
}

func NewGraphRankEngine(ctx context.Context, cfg GraphRankEngineConfig) (*GraphRankEngine, error) {
	if cfg.URI == "" || cfg.Username == "" || cfg.Password == "" {
		return nil, errors.New("neo4j config requires URI, username, and password")
	}
	if cfg.Database == "" {
		cfg.Database = "neo4j"
	}
	if cfg.CacheTTL <= 0 {
		cfg.CacheTTL = 30 * time.Second
	}
	if cfg.QueryTimeout <= 0 {
		cfg.QueryTimeout = 20 * time.Second
	}
	if cfg.SignalWeightFollow == 0 {
		cfg.SignalWeightFollow = 1.0
	}
	// SignalWeightMute and SignalWeightReport intentionally default to 0.0.

	driver, err := neo4j.NewDriverWithContext(cfg.URI, neo4j.BasicAuth(cfg.Username, cfg.Password, ""))
	if err != nil {
		return nil, fmt.Errorf("create neo4j driver: %w", err)
	}

	if err := driver.VerifyConnectivity(ctx); err != nil {
		_ = driver.Close(ctx)
		return nil, fmt.Errorf("verify neo4j connectivity: %w", err)
	}

	engine := &GraphRankEngine{
		driver:             driver,
		database:           cfg.Database,
		cacheTTL:           cfg.CacheTTL,
		queryTimeout:       cfg.QueryTimeout,
		projectionName:     graphName,
		grapeRankEnabled:   cfg.GrapeRankEnabled,
		signalWeightFollow: cfg.SignalWeightFollow,
		signalWeightMute:   cfg.SignalWeightMute,
		signalWeightReport: cfg.SignalWeightReport,
		perspCaches:        make(map[string]rankCacheEntry),
		projectionDirty:    true,
	}

	if err := engine.checkGDSAvailability(ctx); err != nil {
		_ = driver.Close(ctx)
		return nil, err
	}

	return engine, nil
}

func (g *GraphRankEngine) Close(ctx context.Context) error {
	return g.driver.Close(ctx)
}

func (g *GraphRankEngine) InvalidateCache() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.globalCache = rankCacheEntry{}
	g.perspCaches = make(map[string]rankCacheEntry)
	g.projectionDirty = true
}

// GetUserRank returns the normalized rank (0–100) for pubkey.
// Dispatches to personalized GrapeRank if enabled and perspective is set,
// otherwise falls back to global PageRank.
func (g *GraphRankEngine) GetUserRank(ctx context.Context, pubkey string, perspective string) (int, error) {
	if pubkey == "" {
		return 0, nil
	}
	if g.grapeRankEnabled && perspective != "" {
		return g.getPersonalizedRank(ctx, pubkey, perspective)
	}
	return g.getGlobalRank(ctx, pubkey)
}

func normalizeRank(score, maxScore float64) int {
	if score <= 0 || maxScore <= 0 {
		return 0
	}
	raw := int(math.Round((score / maxScore) * 100))
	if raw < 0 {
		return 0
	}
	if raw > 100 {
		return 100
	}
	return raw
}

func toInt64(v any) (int64, bool) {
	switch t := v.(type) {
	case int64:
		return t, true
	case int:
		return int64(t), true
	case int32:
		return int64(t), true
	case float64:
		return int64(t), true
	default:
		return 0, false
	}
}

func toFloat64(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case float32:
		return float64(t), true
	case int64:
		return float64(t), true
	case int:
		return float64(t), true
	default:
		return 0, false
	}
}
