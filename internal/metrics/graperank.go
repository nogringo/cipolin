package metrics

import (
	"context"
	"math"
	"sync"
	"time"

	"fiatjaf.com/nostr"
)

const (
	defaultRankCacheTTL      = 5 * time.Minute
	defaultTraversalDepth    = 10
	defaultPageRankAlpha     = 0.85
	defaultPageRankMaxIter   = 300
	defaultPageRankTolerance = 1e-8
)

const (
	SignalPost     = 1
	SignalFollow   = 3
	SignalRepost   = 6
	SignalReaction = 7
	SignalReport   = 1984
	SignalNutzap   = 9321
	SignalZap      = 9735
)

type RankWeights struct {
	PositiveByKind             map[int]float64
	NegativeByKind             map[int]float64
	ReportPenaltyFactor        float64
	RequesterPenaltyMultiplier float64
	PageRankAlpha              float64
	PageRankMaxIter            uint
	PageRankTolerance          float64
}

func DefaultRankWeights() RankWeights {
	return RankWeights{
		PositiveByKind: map[int]float64{
			SignalPost:     1.0,
			SignalFollow:   1.0,
			SignalRepost:   1.0,
			SignalReaction: 1.0,
			SignalNutzap:   1.0,
			SignalZap:      1.0,
		},
		NegativeByKind: map[int]float64{
			SignalReport: 1.0,
		},
		ReportPenaltyFactor:        0.75,
		RequesterPenaltyMultiplier: 2.0,
		PageRankAlpha:              defaultPageRankAlpha,
		PageRankMaxIter:            defaultPageRankMaxIter,
		PageRankTolerance:          defaultPageRankTolerance,
	}
}

func (w RankWeights) normalized() RankWeights {
	if w.PositiveByKind == nil {
		w.PositiveByKind = make(map[int]float64)
	}
	if w.NegativeByKind == nil {
		w.NegativeByKind = make(map[int]float64)
	}

	defaults := DefaultRankWeights()
	for k, v := range defaults.PositiveByKind {
		if _, ok := w.PositiveByKind[k]; !ok {
			w.PositiveByKind[k] = v
		}
	}
	for k, v := range defaults.NegativeByKind {
		if _, ok := w.NegativeByKind[k]; !ok {
			w.NegativeByKind[k] = v
		}
	}
	if w.ReportPenaltyFactor == 0 {
		w.ReportPenaltyFactor = defaults.ReportPenaltyFactor
	}
	if w.RequesterPenaltyMultiplier == 0 {
		w.RequesterPenaltyMultiplier = defaults.RequesterPenaltyMultiplier
	}
	if w.PageRankAlpha <= 0 || w.PageRankAlpha >= 1 {
		w.PageRankAlpha = defaults.PageRankAlpha
	}
	if w.PageRankMaxIter == 0 {
		w.PageRankMaxIter = defaults.PageRankMaxIter
	}
	if w.PageRankTolerance <= 0 {
		w.PageRankTolerance = defaults.PageRankTolerance
	}

	return w
}

type graphCache struct {
	builtAt   time.Time
	version   uint64
	positive  map[string]map[string]float64
	negative  map[string]map[string]float64
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
	weights       RankWeights
	graph         graphCache
	requesterRank map[string]requesterRankCache
}

// InvalidateCache marks cached graph/requester rank results as stale so the
// next rank lookup rebuilds from the latest events in storage.
func (g *GrapeRankEngine) InvalidateCache() {
	g.mu.Lock()
	g.graph.builtAt = time.Time{}
	g.graph.lastError = nil
	for requester := range g.requesterRank {
		delete(g.requesterRank, requester)
	}
	g.mu.Unlock()
}

func NewGrapeRankEngine(db EventStore, ttl time.Duration, weights ...RankWeights) *GrapeRankEngine {
	if ttl <= 0 {
		ttl = defaultRankCacheTTL
	}

	resolvedWeights := DefaultRankWeights()
	if len(weights) > 0 {
		resolvedWeights = weights[0].normalized()
	}

	return &GrapeRankEngine{
		db:            db,
		ttl:           ttl,
		weights:       resolvedWeights,
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
	positive := make(map[string]map[string]float64)
	negative := make(map[string]map[string]float64)
	allNodes := make(map[string]struct{})

	addPositive := func(from, to string, weight float64) {
		if len(from) != 64 || len(to) != 64 || from == to || weight <= 0 {
			return
		}
		neighbors, ok := positive[from]
		if !ok {
			neighbors = make(map[string]float64)
			positive[from] = neighbors
		}
		neighbors[to] += weight
		allNodes[from] = struct{}{}
		allNodes[to] = struct{}{}
	}

	addNegative := func(from, to string, weight float64) {
		if len(from) != 64 || len(to) != 64 || from == to || weight <= 0 {
			return
		}
		if _, ok := negative[from]; !ok {
			negative[from] = make(map[string]float64)
		}
		negative[from][to] += weight
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
				weight := g.weights.PositiveByKind[int(kind)]
				for _, tag := range event.Tags {
					if len(tag) >= 2 && tag[0] == "p" {
						addPositive(author, tag[1], weight)
					}
				}

			case 9321:
				weight := g.weights.PositiveByKind[int(kind)]
				for _, tag := range event.Tags {
					if len(tag) >= 2 && tag[0] == "p" {
						addPositive(author, tag[1], weight)
					}
				}

			case 9735:
				weight := g.weights.PositiveByKind[int(kind)]
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
					addPositive(sender, recipient, weight)
				}

			case 1984:
				weight := g.weights.NegativeByKind[int(kind)]
				for _, tag := range event.Tags {
					if len(tag) >= 2 && tag[0] == "p" {
						addNegative(author, tag[1], weight)
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

	rawScores := runWeightedPageRank(reachable, graph.positive, g.weights)

	adjusted := make(map[string]float64, len(reachable))
	maxAdjusted := 0.0

	for node := range reachable {
		raw := rawScores[node]

		reportPenalty := 0.0
		for reporter := range reachable {
			reportPenalty += graph.negative[reporter][node]
		}
		reportPenalty += graph.negative[requesterPubkey][node] * g.weights.RequesterPenaltyMultiplier

		penalized := raw / (1.0 + g.weights.ReportPenaltyFactor*reportPenalty)
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

func runWeightedPageRank(reachable map[string]struct{}, positive map[string]map[string]float64, cfg RankWeights) map[string]float64 {
	nodeCount := len(reachable)
	if nodeCount == 0 {
		return map[string]float64{}
	}

	incoming := make(map[string]map[string]float64, nodeCount)
	outWeights := make(map[string]float64, nodeCount)
	for node := range reachable {
		incoming[node] = make(map[string]float64)
	}

	for from := range reachable {
		for to, w := range positive[from] {
			if w <= 0 {
				continue
			}
			if _, ok := reachable[to]; !ok {
				continue
			}
			incoming[to][from] += w
			outWeights[from] += w
		}
	}

	ranks := make(map[string]float64, nodeCount)
	initial := 1.0 / float64(nodeCount)
	for node := range reachable {
		ranks[node] = initial
	}

	alpha := cfg.PageRankAlpha
	for iter := uint(0); iter < cfg.PageRankMaxIter; iter++ {
		dangling := 0.0
		for node := range reachable {
			if outWeights[node] <= 0 {
				dangling += ranks[node]
			}
		}

		base := (1.0-alpha)/float64(nodeCount) + alpha*(dangling/float64(nodeCount))
		next := make(map[string]float64, nodeCount)
		l1err := 0.0

		for node := range reachable {
			sumIncoming := 0.0
			for from, w := range incoming[node] {
				out := outWeights[from]
				if out <= 0 {
					continue
				}
				sumIncoming += ranks[from] * (w / out)
			}

			newRank := base + alpha*sumIncoming
			next[node] = newRank
			l1err += math.Abs(newRank - ranks[node])
		}

		ranks = next
		if l1err < cfg.PageRankTolerance {
			break
		}
	}

	total := 0.0
	for _, v := range ranks {
		total += v
	}
	if total > 0 {
		for node, v := range ranks {
			ranks[node] = v / total
		}
	}

	return ranks
}

func collectReachable(positive map[string]map[string]float64, seed string, depth int) map[string]struct{} {
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
