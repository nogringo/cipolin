package metrics

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"fiatjaf.com/nostr"
	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
)

const graphName = "cipolin-follow-graph"

// UserRankProvider provides rank values for user assertions.
type UserRankProvider interface {
	GetUserRank(ctx context.Context, pubkey string) (int, error)
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
}

// GraphRankEngine computes user rank with Neo4j GDS PageRank.
type GraphRankEngine struct {
	driver         neo4j.DriverWithContext
	database       string
	cacheTTL       time.Duration
	queryTimeout   time.Duration
	projectionName string

	mu              sync.RWMutex
	cache           map[string]float64
	maxScore        float64
	cacheExpiresAt  time.Time
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

	driver, err := neo4j.NewDriverWithContext(cfg.URI, neo4j.BasicAuth(cfg.Username, cfg.Password, ""))
	if err != nil {
		return nil, fmt.Errorf("create neo4j driver: %w", err)
	}

	if err := driver.VerifyConnectivity(ctx); err != nil {
		_ = driver.Close(ctx)
		return nil, fmt.Errorf("verify neo4j connectivity: %w", err)
	}

	engine := &GraphRankEngine{
		driver:          driver,
		database:        cfg.Database,
		cacheTTL:        cfg.CacheTTL,
		queryTimeout:    cfg.QueryTimeout,
		projectionName:  graphName,
		cache:           make(map[string]float64),
		projectionDirty: true,
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
	g.cache = make(map[string]float64)
	g.maxScore = 0
	g.cacheExpiresAt = time.Time{}
	g.projectionDirty = true
}

func (g *GraphRankEngine) IngestEvent(ctx context.Context, event nostr.Event) (bool, error) {
	if event.Kind != 3 {
		return false, nil
	}

	source := event.PubKey.Hex()
	targets := extractFollowTargets(event)

	qctx, cancel := context.WithTimeout(ctx, g.queryTimeout)
	defer cancel()

	session := g.driver.NewSession(qctx, neo4j.SessionConfig{DatabaseName: g.database})
	defer session.Close(qctx)

	applied, err := session.ExecuteWrite(qctx, func(tx neo4j.ManagedTransaction) (any, error) {
		res, err := tx.Run(qctx, `
			MERGE (src:User {pubkey: $source})
			WITH src, coalesce(src.contact_list_created_at, 0) AS lastSeen
			RETURN lastSeen AS lastSeen
		`, map[string]any{"source": source})
		if err != nil {
			return false, err
		}
		if !res.Next(qctx) {
			if res.Err() != nil {
				return false, res.Err()
			}
			return false, errors.New("neo4j query returned no rows")
		}
		lastSeenAny, _ := res.Record().Get("lastSeen")
		lastSeen, _ := toInt64(lastSeenAny)
		createdAt := int64(event.CreatedAt)
		if createdAt <= lastSeen {
			return false, nil
		}

		delRes, err := tx.Run(qctx, `
			MERGE (src:User {pubkey: $source})
			SET src.contact_list_created_at = $createdAt,
			    src.updated_at = timestamp()
			WITH src
			MATCH (src)-[r:FOLLOWS]->(:User)
			DELETE r
			RETURN 1 AS ok
		`, map[string]any{
			"source":    source,
			"createdAt": createdAt,
		})
		if err != nil {
			return false, err
		}
		for delRes.Next(qctx) {
		}
		if delRes.Err() != nil {
			return false, delRes.Err()
		}

		if len(targets) == 0 {
			return true, nil
		}

		insertRes, err := tx.Run(qctx, `
			MERGE (src:User {pubkey: $source})
			UNWIND $targets AS target
			MERGE (dst:User {pubkey: target})
			SET dst.updated_at = timestamp()
			MERGE (src)-[r:FOLLOWS]->(dst)
			SET r.signal = "follow",
			    r.weight = 1.0,
			    r.source_event_id = $eventID,
			    r.created_at = $createdAt,
			    r.updated_at = timestamp()
		`, map[string]any{
			"source":    source,
			"targets":   targets,
			"eventID":   event.ID.String(),
			"createdAt": createdAt,
		})
		if err != nil {
			return false, err
		}
		for insertRes.Next(qctx) {
		}
		if insertRes.Err() != nil {
			return false, insertRes.Err()
		}

		return true, nil
	})
	if err != nil {
		return false, fmt.Errorf("upsert follow graph: %w", err)
	}

	appliedBool, _ := applied.(bool)
	return appliedBool, nil
}

func (g *GraphRankEngine) GetUserRank(ctx context.Context, pubkey string) (int, error) {
	if pubkey == "" {
		return 0, nil
	}

	g.mu.RLock()
	if time.Now().Before(g.cacheExpiresAt) {
		score := g.cache[pubkey]
		maxScore := g.maxScore
		g.mu.RUnlock()
		return normalizeRank(score, maxScore), nil
	}
	g.mu.RUnlock()

	g.mu.Lock()
	defer g.mu.Unlock()

	if time.Now().Before(g.cacheExpiresAt) {
		score := g.cache[pubkey]
		return normalizeRank(score, g.maxScore), nil
	}

	if err := g.rebuildCacheLocked(ctx); err != nil {
		return 0, err
	}

	score := g.cache[pubkey]
	return normalizeRank(score, g.maxScore), nil
}

func (g *GraphRankEngine) rebuildCacheLocked(ctx context.Context) error {
	qctx, cancel := context.WithTimeout(ctx, g.queryTimeout)
	defer cancel()

	if g.projectionDirty {
		if err := g.dropProjectionIfExists(qctx); err != nil {
			return err
		}
	}

	if err := g.ensureProjection(qctx); err != nil {
		return err
	}

	session := g.driver.NewSession(qctx, neo4j.SessionConfig{DatabaseName: g.database})
	defer session.Close(qctx)

	ranksAny, err := session.ExecuteRead(qctx, func(tx neo4j.ManagedTransaction) (any, error) {
		res, err := tx.Run(qctx, `
			CALL gds.pageRank.stream($graphName, {
				maxIterations: 30,
				dampingFactor: 0.85
			})
			YIELD nodeId, score
			RETURN gds.util.asNode(nodeId).pubkey AS pubkey, score
		`, map[string]any{"graphName": g.projectionName})
		if err != nil {
			return nil, err
		}

		ranks := make(map[string]float64)
		maxScore := 0.0
		for res.Next(qctx) {
			record := res.Record()
			pkAny, _ := record.Get("pubkey")
			scoreAny, _ := record.Get("score")
			pk, _ := pkAny.(string)
			score, _ := toFloat64(scoreAny)
			if pk == "" {
				continue
			}
			ranks[pk] = score
			if score > maxScore {
				maxScore = score
			}
		}
		if res.Err() != nil {
			return nil, res.Err()
		}

		return struct {
			Ranks    map[string]float64
			MaxScore float64
		}{
			Ranks:    ranks,
			MaxScore: maxScore,
		}, nil
	})
	if err != nil {
		return fmt.Errorf("run gds pagerank: %w", err)
	}

	ranks := ranksAny.(struct {
		Ranks    map[string]float64
		MaxScore float64
	})

	g.cache = ranks.Ranks
	g.maxScore = ranks.MaxScore
	g.cacheExpiresAt = time.Now().Add(g.cacheTTL)
	g.projectionDirty = false
	return nil
}

func (g *GraphRankEngine) ensureProjection(ctx context.Context) error {
	exists, err := g.projectionExists(ctx)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}

	session := g.driver.NewSession(ctx, neo4j.SessionConfig{DatabaseName: g.database})
	defer session.Close(ctx)

	_, err = session.ExecuteWrite(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		res, err := tx.Run(ctx, `
			CALL gds.graph.project($graphName, 'User', {
				FOLLOWS: {
					orientation: 'NATURAL'
				}
			})
		`, map[string]any{"graphName": g.projectionName})
		if err != nil {
			return nil, err
		}
		for res.Next(ctx) {
		}
		return nil, res.Err()
	})
	if err != nil {
		return fmt.Errorf("create graph projection: %w", err)
	}

	return nil
}

func (g *GraphRankEngine) dropProjectionIfExists(ctx context.Context) error {
	exists, err := g.projectionExists(ctx)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}

	session := g.driver.NewSession(ctx, neo4j.SessionConfig{DatabaseName: g.database})
	defer session.Close(ctx)

	_, err = session.ExecuteWrite(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		res, err := tx.Run(ctx, `
			CALL gds.graph.drop($graphName, false)
		`, map[string]any{"graphName": g.projectionName})
		if err != nil {
			return nil, err
		}
		for res.Next(ctx) {
		}
		return nil, res.Err()
	})
	if err != nil {
		return fmt.Errorf("drop graph projection: %w", err)
	}
	return nil
}

func (g *GraphRankEngine) projectionExists(ctx context.Context) (bool, error) {
	session := g.driver.NewSession(ctx, neo4j.SessionConfig{DatabaseName: g.database})
	defer session.Close(ctx)

	existsAny, err := session.ExecuteRead(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		res, err := tx.Run(ctx, `
			CALL gds.graph.exists($graphName)
			YIELD exists
			RETURN exists
		`, map[string]any{"graphName": g.projectionName})
		if err != nil {
			return false, err
		}
		if !res.Next(ctx) {
			if res.Err() != nil {
				return false, res.Err()
			}
			return false, errors.New("neo4j graph.exists returned no rows")
		}
		existsAny, _ := res.Record().Get("exists")
		exists, _ := existsAny.(bool)
		return exists, nil
	})
	if err != nil {
		return false, fmt.Errorf("check graph projection: %w", err)
	}
	return existsAny.(bool), nil
}

func (g *GraphRankEngine) checkGDSAvailability(ctx context.Context) error {
	session := g.driver.NewSession(ctx, neo4j.SessionConfig{DatabaseName: g.database})
	defer session.Close(ctx)

	_, err := session.ExecuteRead(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		res, err := tx.Run(ctx, `
			RETURN gds.version() AS version
		`, nil)
		if err != nil {
			return nil, err
		}
		if !res.Next(ctx) {
			if res.Err() != nil {
				return nil, res.Err()
			}
			return nil, errors.New("neo4j gds.version returned no rows")
		}
		return nil, nil
	})
	if err != nil {
		return fmt.Errorf("neo4j GDS plugin unavailable: %w", err)
	}
	return nil
}

func extractFollowTargets(event nostr.Event) []string {
	unique := make(map[string]struct{})
	for _, tag := range event.Tags {
		if len(tag) < 2 || tag[0] != "p" {
			continue
		}
		target := tag[1]
		if len(target) != 64 {
			continue
		}
		unique[target] = struct{}{}
	}

	targets := make([]string, 0, len(unique))
	for target := range unique {
		targets = append(targets, target)
	}
	sort.Strings(targets)
	return targets
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
