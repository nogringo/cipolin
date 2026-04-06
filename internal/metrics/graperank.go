package metrics

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"

	"fiatjaf.com/nostr"
	pr "github.com/max-planck-innovation-competition/pagerank/pkg/pagerank"
)

const (
	defaultRankCacheTTL   = 5 * time.Minute
	defaultTraversalDepth = 3
)

type graphCache struct {
	builtAt   time.Time
	version   uint64
	positive  map[string]map[string]struct{}
	negative  map[string]map[string]int
	allNodes  map[string]struct{}
	lastError error
}

type requesterRankCache struct {
	expiresAt time.Time
	version   uint64
	scores    map[string]int
}

type GrapeRankEngine struct {
	db EventStore

	mu            sync.RWMutex
	ttl           time.Duration
	graph         graphCache
	requesterRank map[string]requesterRankCache
}

func NewGrapeRankEngine(db EventStore, ttl time.Duration) *GrapeRankEngine {
	if ttl <= 0 {
		ttl = defaultRankCacheTTL
	}

	return &GrapeRankEngine{
		db:            db,
		ttl:           ttl,
		requesterRank: make(map[string]requesterRankCache),
	}
}

func (g *GrapeRankEngine) GetRank(ctx context.Context, requesterPubkey, targetPubkey string) (int, bool) {
	if len(requesterPubkey) != 64 || len(targetPubkey) != 64 {
		return 0, false
	}

	graph, ok := g.getOrBuildGraph(ctx)
	if !ok {
		return 0, false
	}

	now := time.Now()
	g.mu.RLock()
	if cached, found := g.requesterRank[requesterPubkey]; found && cached.version == graph.version && now.Before(cached.expiresAt) {
		if v, exists := cached.scores[targetPubkey]; exists {
			g.mu.RUnlock()
			return v, true
		}
	}
	g.mu.RUnlock()

	scores, err := g.computeRequesterScores(graph, requesterPubkey)
	if err != nil {
		g.mu.RLock()
		cached, found := g.requesterRank[requesterPubkey]
		g.mu.RUnlock()
		if found {
			if v, exists := cached.scores[targetPubkey]; exists {
				return v, true
			}
		}
		return 0, false
	}

	g.mu.Lock()
	g.requesterRank[requesterPubkey] = requesterRankCache{
		expiresAt: now.Add(g.ttl),
		version:   graph.version,
		scores:    scores,
	}
	g.mu.Unlock()

	v, exists := scores[targetPubkey]
	if !exists {
		return 0, true
	}
	return v, true
}

func (g *GrapeRankEngine) getOrBuildGraph(ctx context.Context) (graphCache, bool) {
	now := time.Now()

	g.mu.RLock()
	cached := g.graph
	if !cached.builtAt.IsZero() && now.Sub(cached.builtAt) < g.ttl {
		g.mu.RUnlock()
		return cached, true
	}
	g.mu.RUnlock()

	built, err := g.buildGraph(ctx)
	if err != nil {
		g.mu.Lock()
		if g.graph.lastError == nil || g.graph.lastError.Error() != err.Error() {
			g.graph.lastError = err
		}
		lastGood := g.graph
		g.mu.Unlock()

		if !lastGood.builtAt.IsZero() {
			return lastGood, true
		}
		return graphCache{}, false
	}

	g.mu.Lock()
	built.version = g.graph.version + 1
	g.graph = built
	for requester := range g.requesterRank {
		delete(g.requesterRank, requester)
	}
	g.mu.Unlock()

	return built, true
}

func (g *GrapeRankEngine) buildGraph(ctx context.Context) (graphCache, error) {
	positive := make(map[string]map[string]struct{})
	negative := make(map[string]map[string]int)
	allNodes := make(map[string]struct{})

	addPositive := func(from, to string) {
		if len(from) != 64 || len(to) != 64 || from == to {
			return
		}
		neighbors, ok := positive[from]
		if !ok {
			neighbors = make(map[string]struct{})
			positive[from] = neighbors
		}
		neighbors[to] = struct{}{}
		allNodes[from] = struct{}{}
		allNodes[to] = struct{}{}
	}

	addNegative := func(from, to string) {
		if len(from) != 64 || len(to) != 64 || from == to {
			return
		}
		if _, ok := negative[from]; !ok {
			negative[from] = make(map[string]int)
		}
		negative[from][to]++
		allNodes[from] = struct{}{}
		allNodes[to] = struct{}{}
	}

	queryKinds := []nostr.Kind{1, 3, 6, 7, 1984, 9321, 9735}
	for _, kind := range queryKinds {
		for event := range g.db.QueryEvents(nostr.Filter{Kinds: []nostr.Kind{kind}}, 0) {
			author := event.PubKey.Hex()
			if len(author) != 64 {
				continue
			}
			allNodes[author] = struct{}{}

			switch kind {
			case 3, 1, 6, 7:
				for _, tag := range event.Tags {
					if len(tag) >= 2 && tag[0] == "p" {
						addPositive(author, tag[1])
					}
				}

			case 9321:
				for _, tag := range event.Tags {
					if len(tag) >= 2 && tag[0] == "p" {
						addPositive(author, tag[1])
					}
				}

			case 9735:
				sender := author
				recipients := make([]string, 0, 2)
				for _, tag := range event.Tags {
					if len(tag) >= 2 && tag[0] == "P" && len(tag[1]) == 64 {
						sender = tag[1]
					}
					if len(tag) >= 2 && tag[0] == "p" && len(tag[1]) == 64 {
						recipients = append(recipients, tag[1])
					}
				}
				for _, recipient := range recipients {
					addPositive(sender, recipient)
				}

			case 1984:
				for _, tag := range event.Tags {
					if len(tag) >= 2 && tag[0] == "p" {
						addNegative(author, tag[1])
					}
				}
			}
		}
	}

	return graphCache{
		builtAt:  time.Now(),
		positive: positive,
		negative: negative,
		allNodes: allNodes,
	}, nil
}

func (g *GrapeRankEngine) computeRequesterScores(graph graphCache, requesterPubkey string) (map[string]int, error) {
	reachable := collectReachable(graph.positive, requesterPubkey, defaultTraversalDepth)
	if len(reachable) == 0 {
		reachable = map[string]struct{}{requesterPubkey: {}}
	}

	pGraph := pr.NewGraph()
	for node := range reachable {
		pGraph.AddNode(pr.NodeID(node))
	}

	for from := range reachable {
		for to := range graph.positive[from] {
			if _, ok := reachable[to]; ok {
				pGraph.AddEdge(pr.NodeID(from), pr.NodeID(to))
			}
		}
	}

	pagerank := pr.NewPageRank(pGraph)
	pagerank.MaxIter = 300
	pagerank.Tolerance = 1e-8

	var panicErr error
	func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				panicErr = fmt.Errorf("pagerank panic: %v", recovered)
			}
		}()
		pagerank.CalcPageRank()
	}()
	if panicErr != nil {
		return nil, panicErr
	}

	adjusted := make(map[string]float64, len(reachable))
	maxAdjusted := 0.0

	for node := range reachable {
		raw := 0.0
		if n := pagerank.GetNode(pr.NodeID(node)); n != nil {
			raw = n.Rank
		}

		reportPenalty := 0
		for reporter := range reachable {
			reportPenalty += graph.negative[reporter][node]
		}
		reportPenalty += graph.negative[requesterPubkey][node] * 2

		penalized := raw / (1.0 + 0.75*float64(reportPenalty))
		adjusted[node] = penalized
		if penalized > maxAdjusted {
			maxAdjusted = penalized
		}
	}

	if maxAdjusted <= 0 {
		result := make(map[string]int, len(reachable))
		for node := range reachable {
			result[node] = 0
		}
		return result, nil
	}

	result := make(map[string]int, len(reachable))
	for node, score := range adjusted {
		normalized := (score / maxAdjusted) * 100.0
		rank := int(math.Round(normalized))
		if rank < 0 {
			rank = 0
		}
		if rank > 100 {
			rank = 100
		}
		result[node] = rank
	}

	return result, nil
}

func collectReachable(positive map[string]map[string]struct{}, seed string, depth int) map[string]struct{} {
	result := map[string]struct{}{seed: {}}
	if depth <= 0 {
		return result
	}

	frontier := map[string]struct{}{seed: {}}
	for level := 0; level < depth; level++ {
		next := make(map[string]struct{})
		for from := range frontier {
			for to := range positive[from] {
				if _, seen := result[to]; !seen {
					result[to] = struct{}{}
					next[to] = struct{}{}
				}
			}
		}
		if len(next) == 0 {
			break
		}
		frontier = next
	}

	return result
}
