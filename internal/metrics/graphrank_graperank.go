package metrics

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
)

func (g *GraphRankEngine) getPersonalizedRank(ctx context.Context, pubkey, perspective string) (int, error) {
	g.mu.RLock()
	if entry, ok := g.perspCaches[perspective]; ok && time.Now().Before(entry.expiresAt) {
		score := entry.scores[pubkey]
		maxScore := entry.maxScore
		g.mu.RUnlock()
		return normalizeRank(score, maxScore), nil
	}
	g.mu.RUnlock()

	g.mu.Lock()
	defer g.mu.Unlock()

	if entry, ok := g.perspCaches[perspective]; ok && time.Now().Before(entry.expiresAt) {
		score := entry.scores[pubkey]
		return normalizeRank(score, entry.maxScore), nil
	}

	if err := g.rebuildPersonalizedCacheLocked(ctx, perspective); err != nil {
		return 0, err
	}

	entry := g.perspCaches[perspective]
	return normalizeRank(entry.scores[pubkey], entry.maxScore), nil
}

func (g *GraphRankEngine) rebuildPersonalizedCacheLocked(ctx context.Context, perspective string) error {
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

	type pageRankResult struct {
		Ranks    map[string]float64
		MaxScore float64
	}

	ranksAny, err := session.ExecuteRead(qctx, func(tx neo4j.ManagedTransaction) (any, error) {
		// Personalized PageRank: random restart is biased toward the perspective node.
		// If the perspective user isn't in the graph yet the MATCH returns no rows and
		// the CALL is skipped, yielding an empty result set (handled below).
		res, err := tx.Run(qctx, `
			MATCH (src:User {pubkey: $perspective})
			CALL gds.pageRank.stream($graphName, {
				maxIterations: 30,
				dampingFactor: 0.85,
				sourceNodes: [src]
			})
			YIELD nodeId, score
			RETURN gds.util.asNode(nodeId).pubkey AS pubkey, score
		`, map[string]any{
			"graphName":   g.projectionName,
			"perspective": perspective,
		})
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

		return pageRankResult{Ranks: ranks, MaxScore: maxScore}, nil
	})
	if err != nil {
		return fmt.Errorf("run personalized pagerank: %w", err)
	}

	result := ranksAny.(pageRankResult)

	if len(result.Ranks) == 0 {
		// Perspective user not in graph yet; cache global ranks under this perspective.
		if !time.Now().Before(g.globalCache.expiresAt) {
			if err := g.rebuildGlobalCacheLocked(ctx); err != nil {
				return err
			}
		}
		g.perspCaches[perspective] = g.globalCache
		return nil
	}

	// Post-process: apply mute weight multiplier on muted users' scores.
	if g.signalWeightMute != 1.0 {
		mutedPubkeys, err := g.getMutedPubkeys(qctx, perspective)
		if err != nil {
			log.Printf("[graperank] Failed to fetch muted pubkeys for %s: %v", perspective[:min(8, len(perspective))], err)
		} else {
			for _, pk := range mutedPubkeys {
				if score, ok := result.Ranks[pk]; ok {
					result.Ranks[pk] = score * g.signalWeightMute
				}
			}
		}
	}

	// Post-process: apply report weight multiplier on reported users' scores.
	if g.signalWeightReport != 1.0 {
		reportedPubkeys, err := g.getReportedPubkeys(qctx, perspective)
		if err != nil {
			log.Printf("[graperank] Failed to fetch reported pubkeys for %s: %v", perspective[:min(8, len(perspective))], err)
		} else {
			for _, pk := range reportedPubkeys {
				if score, ok := result.Ranks[pk]; ok {
					result.Ranks[pk] = score * g.signalWeightReport
				}
			}
		}
	}

	// Recalculate maxScore after post-processing multipliers.
	maxScore := 0.0
	for _, score := range result.Ranks {
		if score > maxScore {
			maxScore = score
		}
	}

	g.perspCaches[perspective] = rankCacheEntry{
		scores:    result.Ranks,
		maxScore:  maxScore,
		expiresAt: time.Now().Add(g.cacheTTL),
	}
	g.projectionDirty = false
	return nil
}

func (g *GraphRankEngine) getMutedPubkeys(ctx context.Context, perspective string) ([]string, error) {
	session := g.driver.NewSession(ctx, neo4j.SessionConfig{DatabaseName: g.database})
	defer session.Close(ctx)

	resultAny, err := session.ExecuteRead(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		res, err := tx.Run(ctx, `
			MATCH (:User {pubkey: $perspective})-[:MUTES]->(m:User)
			RETURN m.pubkey AS pubkey
		`, map[string]any{"perspective": perspective})
		if err != nil {
			return nil, err
		}
		var pubkeys []string
		for res.Next(ctx) {
			pkAny, _ := res.Record().Get("pubkey")
			if pk, ok := pkAny.(string); ok && pk != "" {
				pubkeys = append(pubkeys, pk)
			}
		}
		return pubkeys, res.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("fetch muted pubkeys: %w", err)
	}
	if resultAny == nil {
		return nil, nil
	}
	return resultAny.([]string), nil
}

func (g *GraphRankEngine) getReportedPubkeys(ctx context.Context, perspective string) ([]string, error) {
	session := g.driver.NewSession(ctx, neo4j.SessionConfig{DatabaseName: g.database})
	defer session.Close(ctx)

	resultAny, err := session.ExecuteRead(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		res, err := tx.Run(ctx, `
			MATCH (:User {pubkey: $perspective})-[:REPORTS]->(r:User)
			RETURN r.pubkey AS pubkey
		`, map[string]any{"perspective": perspective})
		if err != nil {
			return nil, err
		}
		var pubkeys []string
		for res.Next(ctx) {
			pkAny, _ := res.Record().Get("pubkey")
			if pk, ok := pkAny.(string); ok && pk != "" {
				pubkeys = append(pubkeys, pk)
			}
		}
		return pubkeys, res.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("fetch reported pubkeys: %w", err)
	}
	if resultAny == nil {
		return nil, nil
	}
	return resultAny.([]string), nil
}
