package metrics

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"fiatjaf.com/nostr"
	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
)

// IngestEvent processes a Nostr event and updates the Neo4j graph.
// Handles kinds 3 (follow lists), 10000 (mute lists), and 1984 (reports).
// All three are always ingested regardless of whether GrapeRank is enabled.
// Returns (true, nil) when the graph was updated, (false, nil) for a no-op.
func (g *GraphRankEngine) IngestEvent(ctx context.Context, event nostr.Event) (bool, error) {
	switch event.Kind {
	case 3:
		return g.ingestFollowList(ctx, event)
	case 10000:
		return g.ingestMuteList(ctx, event)
	case 1984:
		return g.ingestReport(ctx, event)
	default:
		return false, nil
	}
}

func (g *GraphRankEngine) ingestFollowList(ctx context.Context, event nostr.Event) (bool, error) {
	source := event.PubKey.Hex()
	targets := extractPTagTargets(event)

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
			    r.weight = $weight,
			    r.source_event_id = $eventID,
			    r.created_at = $createdAt,
			    r.updated_at = timestamp()
		`, map[string]any{
			"source":    source,
			"targets":   targets,
			"weight":    g.signalWeightFollow,
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

func (g *GraphRankEngine) ingestMuteList(ctx context.Context, event nostr.Event) (bool, error) {
	source := event.PubKey.Hex()
	targets := extractPTagTargets(event)

	qctx, cancel := context.WithTimeout(ctx, g.queryTimeout)
	defer cancel()

	session := g.driver.NewSession(qctx, neo4j.SessionConfig{DatabaseName: g.database})
	defer session.Close(qctx)

	applied, err := session.ExecuteWrite(qctx, func(tx neo4j.ManagedTransaction) (any, error) {
		res, err := tx.Run(qctx, `
			MERGE (src:User {pubkey: $source})
			WITH src, coalesce(src.mute_list_created_at, 0) AS lastSeen
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
			SET src.mute_list_created_at = $createdAt,
			    src.updated_at = timestamp()
			WITH src
			MATCH (src)-[r:MUTES]->(:User)
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
			MERGE (src)-[r:MUTES]->(dst)
			SET r.signal = "mute",
			    r.weight = $weight,
			    r.source_event_id = $eventID,
			    r.created_at = $createdAt,
			    r.updated_at = timestamp()
		`, map[string]any{
			"source":    source,
			"targets":   targets,
			"weight":    g.signalWeightMute,
			"eventID":   event.ID.String(),
			"createdAt": createdAt,
		})
		if err != nil {
			return false, err
		}
		for insertRes.Next(qctx) {
		}
		return true, insertRes.Err()
	})
	if err != nil {
		return false, fmt.Errorf("upsert mute list: %w", err)
	}

	appliedBool, _ := applied.(bool)
	return appliedBool, nil
}

func (g *GraphRankEngine) ingestReport(ctx context.Context, event nostr.Event) (bool, error) {
	source := event.PubKey.Hex()
	targets := extractPTagTargets(event)

	if len(targets) == 0 {
		return false, nil
	}

	qctx, cancel := context.WithTimeout(ctx, g.queryTimeout)
	defer cancel()

	session := g.driver.NewSession(qctx, neo4j.SessionConfig{DatabaseName: g.database})
	defer session.Close(qctx)

	createdAt := int64(event.CreatedAt)
	_, err := session.ExecuteWrite(qctx, func(tx neo4j.ManagedTransaction) (any, error) {
		res, err := tx.Run(qctx, `
			MERGE (src:User {pubkey: $source})
			SET src.updated_at = timestamp()
			WITH src
			UNWIND $targets AS target
			MERGE (dst:User {pubkey: target})
			SET dst.updated_at = timestamp()
			MERGE (src)-[r:REPORTS]->(dst)
			SET r.signal = "report",
			    r.weight = $weight,
			    r.source_event_id = $eventID,
			    r.created_at = CASE WHEN r.created_at IS NULL OR $createdAt < r.created_at THEN $createdAt ELSE r.created_at END,
			    r.updated_at = timestamp()
		`, map[string]any{
			"source":    source,
			"targets":   targets,
			"weight":    g.signalWeightReport,
			"eventID":   event.ID.String(),
			"createdAt": createdAt,
		})
		if err != nil {
			return nil, err
		}
		for res.Next(qctx) {
		}
		return nil, res.Err()
	})
	if err != nil {
		return false, fmt.Errorf("upsert report: %w", err)
	}

	return true, nil
}

// extractPTagTargets returns deduplicated 64-hex pubkeys from all "p" tags in an event.
func extractPTagTargets(event nostr.Event) []string {
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
