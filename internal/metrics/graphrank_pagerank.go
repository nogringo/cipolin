package metrics

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
)

func (g *GraphRankEngine) getGlobalRank(ctx context.Context, pubkey string) (int, error) {
	g.mu.RLock()
	if time.Now().Before(g.globalCache.expiresAt) {
		score := g.globalCache.scores[pubkey]
		maxScore := g.globalCache.maxScore
		g.mu.RUnlock()
		return normalizeRank(score, maxScore), nil
	}
	g.mu.RUnlock()

	g.mu.Lock()
	defer g.mu.Unlock()

	if time.Now().Before(g.globalCache.expiresAt) {
		score := g.globalCache.scores[pubkey]
		return normalizeRank(score, g.globalCache.maxScore), nil
	}

	if err := g.rebuildGlobalCacheLocked(ctx); err != nil {
		return 0, err
	}

	score := g.globalCache.scores[pubkey]
	return normalizeRank(score, g.globalCache.maxScore), nil
}

func (g *GraphRankEngine) rebuildGlobalCacheLocked(ctx context.Context) error {
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

	g.globalCache = rankCacheEntry{
		scores:    ranks.Ranks,
		maxScore:  ranks.MaxScore,
		expiresAt: time.Now().Add(g.cacheTTL),
	}
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
