package keys

import (
	"crypto/sha256"
	"fmt"
	"log"

	"fiatjaf.com/nostr"
)

// MetricKey holds the keypair for a specific metric
type MetricKey struct {
	Kind       string        `json:"kind"`
	Metric     string        `json:"metric"`
	PrivateKey [32]byte
	PublicKey  nostr.PubKey
}

// MetricKeyManager manages deterministic keys for each metric
type MetricKeyManager struct {
	masterKey  string
	metricKeys map[string]*MetricKey // key is "kind:metric"
}

// MetricKindPair represents a kind+metric combination for key derivation
type MetricKindPair struct {
	Kind    string
	Metrics []string
}

// AllMetricKinds defines all kind+metric pairs to derive keys for
var AllMetricKinds = []MetricKindPair{
	{"30382", UserMetrics},
	{"30383", EventMetrics},
	{"30384", EventMetrics},
}

// All supported metrics across all NIP-85 kinds (kept for backwards compat)
var AllMetrics = []string{
	// Kind 30382 (User assertions)
	"followers", "rank", "first_created_at", "first_seen_at",
	"post_cnt", "reply_cnt", "reactions_cnt",
	"zap_amt_recd", "zap_amt_sent", "zap_cnt_recd", "zap_cnt_sent",
	"zap_avg_amt_day_recd", "zap_avg_amt_day_sent",
	"reports_cnt_recd", "reports_cnt_sent",
	"active_hours_start", "active_hours_end",
	"t", // topics
	// Kind 30383/30384 (Event/Address assertions)
	"comment_cnt", "quote_cnt", "repost_cnt", "reaction_cnt",
	"zap_cnt", "zap_amount",
}

// FilterType represents different types of data to fetch
type FilterType int

const (
	FilterPosts      FilterType = 1 << iota // kind 1 authored
	FilterReactions                         // kind 7 authored
	FilterFollowers                         // kind 3 tagging user
	FilterZapsRecd                          // kind 9735, 9321 tagging user (received)
	FilterZapsSent                          // kind 9735, 9321 authored (sent)
	FilterReportsRecd                       // kind 1984 tagging user
	FilterReportsSent                       // kind 1984 authored
	FilterAll        = FilterPosts | FilterReactions | FilterFollowers | FilterZapsRecd | FilterZapsSent | FilterReportsRecd | FilterReportsSent
)

// MetricFilters maps each metric to the filter types it needs
var MetricFilters = map[string]FilterType{
	// Posts are needed for post_cnt, reply_cnt, first_created_at, active hours, topics, and rank
	"post_cnt":           FilterPosts,
	"reply_cnt":          FilterPosts,
	"first_created_at":   FilterPosts,
	"active_hours_start": FilterPosts,
	"active_hours_end":   FilterPosts,
	"t":                  FilterPosts,

	// Reactions
	"reactions_cnt": FilterReactions,

	// Followers
	"followers": FilterFollowers,

	// Zaps received
	"zap_amt_recd":         FilterZapsRecd,
	"zap_cnt_recd":         FilterZapsRecd,
	"zap_avg_amt_day_recd": FilterZapsRecd | FilterPosts, // needs first post date

	// Zaps sent
	"zap_amt_sent":         FilterZapsSent,
	"zap_cnt_sent":         FilterZapsSent,
	"zap_avg_amt_day_sent": FilterZapsSent | FilterPosts, // needs first post date

	// Reports
	"reports_cnt_recd": FilterReportsRecd,
	"reports_cnt_sent": FilterReportsSent,

	// Rank needs everything for proper calculation
	"rank": FilterAll,
}

// GetRequiredFilters returns the combined filter types needed for a set of metrics
func GetRequiredFilters(metrics []string) FilterType {
	if len(metrics) == 0 {
		return FilterAll
	}

	var required FilterType
	for _, m := range metrics {
		if f, ok := MetricFilters[m]; ok {
			required |= f
		}
	}

	// If no specific filters found, fetch all
	if required == 0 {
		return FilterAll
	}
	return required
}

// UserMetrics are the metrics for kind 30382
var UserMetrics = []string{
	"followers", "rank", "first_created_at",
	"post_cnt", "reply_cnt", "reactions_cnt",
	"zap_amt_recd", "zap_amt_sent", "zap_cnt_recd", "zap_cnt_sent",
	"zap_avg_amt_day_recd", "zap_avg_amt_day_sent",
	"reports_cnt_recd", "reports_cnt_sent",
	"active_hours_start", "active_hours_end",
	"t",
}

// EventMetrics are the metrics for kind 30383/30384
var EventMetrics = []string{
	"comment_cnt", "quote_cnt", "repost_cnt", "reaction_cnt",
	"zap_cnt", "zap_amount", "rank",
}

// NewMetricKeyManager creates a new key manager with deterministic derivation
func NewMetricKeyManager(masterKey string) *MetricKeyManager {
	km := &MetricKeyManager{
		masterKey:  masterKey,
		metricKeys: make(map[string]*MetricKey),
	}
	km.deriveAllKeys()
	return km
}

// mapKey returns the composite map key "kind:metric"
func mapKey(kind, metric string) string {
	return kind + ":" + metric
}

// deriveKey deterministically derives a 32-byte private key from master + kind + metric name
func (km *MetricKeyManager) deriveKey(kind string, metric string) [32]byte {
	// Use SHA256(masterKey + ":nip85:" + kind + ":" + metric) as the private key
	seed := km.masterKey + ":nip85:" + kind + ":" + metric
	hash := sha256.Sum256([]byte(seed))
	return hash
}

// deriveAllKeys generates keys for all supported kind+metric combinations
func (km *MetricKeyManager) deriveAllKeys() {
	for _, mk := range AllMetricKinds {
		for _, metric := range mk.Metrics {
			privKey := km.deriveKey(mk.Kind, metric)
			pubKey := nostr.GetPublicKey(privKey)
			key := mapKey(mk.Kind, metric)

			km.metricKeys[key] = &MetricKey{
				Kind:       mk.Kind,
				Metric:     metric,
				PrivateKey: privKey,
				PublicKey:  pubKey,
			}
		}
	}
	log.Printf("[keys] Derived %d metric keys from master key", len(km.metricKeys))
}

// GetKey returns the keypair for a specific kind+metric
func (km *MetricKeyManager) GetKey(kind string, metric string) (*MetricKey, error) {
	key, ok := km.metricKeys[mapKey(kind, metric)]
	if !ok {
		return nil, fmt.Errorf("unknown metric: kind=%s metric=%s", kind, metric)
	}
	return key, nil
}

// GetPubKey returns the hex-encoded public key for a kind+metric
func (km *MetricKeyManager) GetPubKey(kind string, metric string) string {
	if key, ok := km.metricKeys[mapKey(kind, metric)]; ok {
		return key.PublicKey.Hex()
	}
	return ""
}

// SignEventForMetric signs an event using the metric-specific key
func (km *MetricKeyManager) SignEventForMetric(event *nostr.Event, kind string, metric string) error {
	key, err := km.GetKey(kind, metric)
	if err != nil {
		return err
	}
	event.PubKey = key.PublicKey
	return event.Sign(key.PrivateKey)
}

// GetAllPubKeys returns a map of "kind:metric" -> pubkey (hex-encoded) for client configuration
func (km *MetricKeyManager) GetAllPubKeys() map[string]string {
	result := make(map[string]string)
	for key, mk := range km.metricKeys {
		result[key] = mk.PublicKey.Hex()
	}
	return result
}

// GetKind10040Tags returns the tags needed for a client's kind 10040 event
func (km *MetricKeyManager) GetKind10040Tags(relayURL string) [][]string {
	var tags [][]string

	// Kind 30382 (User) metrics
	for _, metric := range UserMetrics {
		if pk := km.GetPubKey("30382", metric); pk != "" {
			tags = append(tags, []string{fmt.Sprintf("30382:%s", metric), pk, relayURL})
		}
	}

	// Kind 30383 (Event) metrics
	for _, metric := range EventMetrics {
		if pk := km.GetPubKey("30383", metric); pk != "" {
			tags = append(tags, []string{fmt.Sprintf("30383:%s", metric), pk, relayURL})
		}
	}

	// Kind 30384 (Address) metrics
	for _, metric := range EventMetrics {
		if pk := km.GetPubKey("30384", metric); pk != "" {
			tags = append(tags, []string{fmt.Sprintf("30384:%s", metric), pk, relayURL})
		}
	}

	return tags
}
