package main

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nbd-wtf/go-nostr"
)

// extractAmountFromZapRequest parses the embedded kind 9734 JSON and extracts the amount
func extractAmountFromZapRequest(descriptionJSON string) int64 {
	var zapRequest struct {
		Tags [][]string `json:"tags"`
	}
	if err := json.Unmarshal([]byte(descriptionJSON), &zapRequest); err != nil {
		return 0
	}
	for _, tag := range zapRequest.Tags {
		if len(tag) >= 2 && tag[0] == "amount" {
			if amt, err := strconv.ParseInt(tag[1], 10, 64); err == nil {
				return amt
			}
		}
	}
	return 0
}

// extractAmountFromNutzap extracts amount from Cashu proof tags in a nutzap event
func extractAmountFromNutzap(event *nostr.Event) int64 {
	var totalAmount int64
	for _, tag := range event.Tags {
		if len(tag) >= 2 && tag[0] == "proof" {
			// Parse the Cashu proof JSON
			var proofs []struct {
				Amount int64 `json:"amount"`
			}
			if err := json.Unmarshal([]byte(tag[1]), &proofs); err != nil {
				// Try single proof format
				var singleProof struct {
					Amount int64 `json:"amount"`
				}
				if err := json.Unmarshal([]byte(tag[1]), &singleProof); err == nil {
					totalAmount += singleProof.Amount
				}
				continue
			}
			for _, proof := range proofs {
				totalAmount += proof.Amount
			}
		}
	}
	return totalAmount
}

// computeUserMetricsFromDB computes kind 30382 metrics for a pubkey from local DB
func computeUserMetricsFromDB(ctx context.Context, pubkey string) map[string]string {
	metrics := make(map[string]string)

	var (
		followerCount    int
		postCount        int
		replyCount       int
		reactionCount    int
		zapAmountRecd    int64
		zapAmountSent    int64
		zapCountRecd     int
		zapCountSent     int
		reportsRecdCount int
		reportsSentCount int
		firstCreatedAt   int64 = time.Now().Unix()
		topicCounts            = make(map[string]int)
		hourCounts             = make([]int, 24)
		oldestZapRecd    int64 = time.Now().Unix()
		newestZapRecd    int64 = 0
		oldestZapSent    int64 = time.Now().Unix()
		newestZapSent    int64 = 0
	)

	// Count followers (kind 3 contact lists with p tag)
	followerFilter := nostr.Filter{
		Kinds: []int{3},
		Tags:  nostr.TagMap{"p": []string{pubkey}},
	}
	followerCh, _ := db.QueryEvents(ctx, followerFilter)
	for range followerCh {
		followerCount++
	}

	// Count posts and replies (kind 1 by author)
	postFilter := nostr.Filter{
		Authors: []string{pubkey},
		Kinds:   []int{1},
	}
	postCh, _ := db.QueryEvents(ctx, postFilter)
	for event := range postCh {
		ts := event.CreatedAt.Time().Unix()
		if ts < firstCreatedAt {
			firstCreatedAt = ts
		}

		// Track active hours
		hour := event.CreatedAt.Time().UTC().Hour()
		hourCounts[hour]++

		// Extract topics (t tags)
		for _, tag := range event.Tags {
			if len(tag) >= 2 && tag[0] == "t" {
				topic := strings.ToLower(tag[1])
				topicCounts[topic]++
			}
		}

		// Check if it's a reply (has e tag)
		isReply := false
		for _, tag := range event.Tags {
			if len(tag) >= 1 && tag[0] == "e" {
				isReply = true
				break
			}
		}
		if isReply {
			replyCount++
		} else {
			postCount++
		}
	}

	// Count reactions sent (kind 7 by author)
	reactionFilter := nostr.Filter{
		Authors: []string{pubkey},
		Kinds:   []int{7},
	}
	reactionCh, _ := db.QueryEvents(ctx, reactionFilter)
	for range reactionCh {
		reactionCount++
	}

	// Count zaps received (kind 9735 with p tag)
	zapRecdFilter := nostr.Filter{
		Kinds: []int{9735},
		Tags:  nostr.TagMap{"p": []string{pubkey}},
	}
	zapRecdCh, _ := db.QueryEvents(ctx, zapRecdFilter)
	for event := range zapRecdCh {
		zapCountRecd++
		ts := event.CreatedAt.Time().Unix()
		if ts < oldestZapRecd {
			oldestZapRecd = ts
		}
		if ts > newestZapRecd {
			newestZapRecd = ts
		}
		// Get amount from description tag (embedded 9734)
		for _, tag := range event.Tags {
			if len(tag) >= 2 && tag[0] == "description" {
				amount := extractAmountFromZapRequest(tag[1])
				zapAmountRecd += amount / 1000 // msats to sats
				break
			}
		}
	}

	// Count nutzaps received (kind 9321 with p tag)
	nutzapRecdFilter := nostr.Filter{
		Kinds: []int{9321},
		Tags:  nostr.TagMap{"p": []string{pubkey}},
	}
	nutzapRecdCh, _ := db.QueryEvents(ctx, nutzapRecdFilter)
	for event := range nutzapRecdCh {
		zapCountRecd++
		ts := event.CreatedAt.Time().Unix()
		if ts < oldestZapRecd {
			oldestZapRecd = ts
		}
		if ts > newestZapRecd {
			newestZapRecd = ts
		}
		zapAmountRecd += extractAmountFromNutzap(event)
	}

	// Count zaps sent (kind 9735 with P tag = sender pubkey)
	// Note: kind 9734 is not published to relays, we use 9735 receipts with P tag
	zapSentFilter := nostr.Filter{
		Kinds: []int{9735},
		Tags:  nostr.TagMap{"P": []string{pubkey}},
	}
	zapSentCh, _ := db.QueryEvents(ctx, zapSentFilter)
	for event := range zapSentCh {
		zapCountSent++
		ts := event.CreatedAt.Time().Unix()
		if ts < oldestZapSent {
			oldestZapSent = ts
		}
		if ts > newestZapSent {
			newestZapSent = ts
		}
		// Get amount from the description tag (embedded 9734 request)
		for _, tag := range event.Tags {
			if len(tag) >= 2 && tag[0] == "description" {
				amount := extractAmountFromZapRequest(tag[1])
				zapAmountSent += amount / 1000 // msats to sats
				break
			}
		}
	}

	// Count nutzaps sent (kind 9321 by author)
	nutzapSentFilter := nostr.Filter{
		Authors: []string{pubkey},
		Kinds:   []int{9321},
	}
	nutzapSentCh, _ := db.QueryEvents(ctx, nutzapSentFilter)
	for event := range nutzapSentCh {
		zapCountSent++
		ts := event.CreatedAt.Time().Unix()
		if ts < oldestZapSent {
			oldestZapSent = ts
		}
		if ts > newestZapSent {
			newestZapSent = ts
		}
		zapAmountSent += extractAmountFromNutzap(event)
	}

	// Count reports received (kind 1984 with p tag)
	reportsRecdFilter := nostr.Filter{
		Kinds: []int{1984},
		Tags:  nostr.TagMap{"p": []string{pubkey}},
	}
	reportsRecdCh, _ := db.QueryEvents(ctx, reportsRecdFilter)
	for range reportsRecdCh {
		reportsRecdCount++
	}

	// Count reports sent (kind 1984 by author)
	reportsSentFilter := nostr.Filter{
		Authors: []string{pubkey},
		Kinds:   []int{1984},
	}
	reportsSentCh, _ := db.QueryEvents(ctx, reportsSentFilter)
	for range reportsSentCh {
		reportsSentCount++
	}

	// Calculate average zap per day
	var zapAvgAmtDayRecd, zapAvgAmtDaySent int64
	if newestZapRecd > oldestZapRecd {
		days := (newestZapRecd - oldestZapRecd) / 86400
		if days < 1 {
			days = 1
		}
		zapAvgAmtDayRecd = zapAmountRecd / days
	}
	if newestZapSent > oldestZapSent {
		days := (newestZapSent - oldestZapSent) / 86400
		if days < 1 {
			days = 1
		}
		zapAvgAmtDaySent = zapAmountSent / days
	}

	// Find top topics (up to 5)
	topTopics := getTopTopics(topicCounts, 5)

	// Find active hours range
	activeStart, activeEnd := findActiveHoursRange(hourCounts)

	// Calculate rank
	rank := calculateUserRank(followerCount, postCount, zapAmountRecd, zapCountRecd)

	// Build metrics map
	metrics["followers"] = strconv.Itoa(followerCount)
	metrics["rank"] = strconv.Itoa(rank)
	metrics["first_created_at"] = strconv.FormatInt(firstCreatedAt, 10)
	metrics["post_cnt"] = strconv.Itoa(postCount)
	metrics["reply_cnt"] = strconv.Itoa(replyCount)
	metrics["reactions_cnt"] = strconv.Itoa(reactionCount)
	metrics["zap_amt_recd"] = strconv.FormatInt(zapAmountRecd, 10)
	metrics["zap_amt_sent"] = strconv.FormatInt(zapAmountSent, 10)
	metrics["zap_cnt_recd"] = strconv.Itoa(zapCountRecd)
	metrics["zap_cnt_sent"] = strconv.Itoa(zapCountSent)
	metrics["zap_avg_amt_day_recd"] = strconv.FormatInt(zapAvgAmtDayRecd, 10)
	metrics["zap_avg_amt_day_sent"] = strconv.FormatInt(zapAvgAmtDaySent, 10)
	metrics["reports_cnt_recd"] = strconv.Itoa(reportsRecdCount)
	metrics["reports_cnt_sent"] = strconv.Itoa(reportsSentCount)
	metrics["active_hours_start"] = strconv.Itoa(activeStart)
	metrics["active_hours_end"] = strconv.Itoa(activeEnd)

	// Add topics as comma-separated (handler will split into multiple tags)
	if len(topTopics) > 0 {
		metrics["_topics"] = strings.Join(topTopics, ",")
	}

	return metrics
}

// getTopTopics returns the top N topics by count
func getTopTopics(topicCounts map[string]int, n int) []string {
	type kv struct {
		Key   string
		Value int
	}
	var sorted []kv
	for k, v := range topicCounts {
		sorted = append(sorted, kv{k, v})
	}
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Value > sorted[j].Value
	})

	var result []string
	for i := 0; i < len(sorted) && i < n; i++ {
		result = append(result, sorted[i].Key)
	}
	return result
}

// findActiveHoursRange finds the range of hours with most activity
func findActiveHoursRange(hourCounts []int) (start, end int) {
	if len(hourCounts) != 24 {
		return 0, 24
	}

	// Find the peak hour
	maxCount := 0
	peakHour := 0
	for h, count := range hourCounts {
		if count > maxCount {
			maxCount = count
			peakHour = h
		}
	}

	if maxCount == 0 {
		return 0, 24
	}

	// Expand from peak to find active range (>25% of peak activity)
	threshold := maxCount / 4
	start = peakHour
	end = peakHour

	// Expand backward
	for i := 1; i < 12; i++ {
		h := (peakHour - i + 24) % 24
		if hourCounts[h] >= threshold {
			start = h
		} else {
			break
		}
	}

	// Expand forward
	for i := 1; i < 12; i++ {
		h := (peakHour + i) % 24
		if hourCounts[h] >= threshold {
			end = h
		} else {
			break
		}
	}

	// Convert to end of hour
	end = (end + 1) % 24

	return start, end
}

// computeEventMetricsFromDB computes kind 30383 metrics for an event ID from local DB
func computeEventMetricsFromDB(ctx context.Context, eventID string) map[string]string {
	metrics := make(map[string]string)

	var (
		commentCount  int
		quoteCount    int
		repostCount   int
		reactionCount int
		zapCount      int
		zapAmount     int64
	)

	// Comments/replies (kind 1 with e tag)
	commentFilter := nostr.Filter{
		Kinds: []int{1},
		Tags:  nostr.TagMap{"e": []string{eventID}},
	}
	commentCh, _ := db.QueryEvents(ctx, commentFilter)
	for event := range commentCh {
		// Check if it's a quote (has q tag)
		isQuote := false
		for _, tag := range event.Tags {
			if len(tag) >= 2 && tag[0] == "q" && tag[1] == eventID {
				isQuote = true
				break
			}
		}
		if isQuote {
			quoteCount++
		} else {
			commentCount++
		}
	}

	// Reposts (kind 6)
	repostFilter := nostr.Filter{
		Kinds: []int{6},
		Tags:  nostr.TagMap{"e": []string{eventID}},
	}
	repostCh, _ := db.QueryEvents(ctx, repostFilter)
	for range repostCh {
		repostCount++
	}

	// Reactions (kind 7)
	reactionFilter := nostr.Filter{
		Kinds: []int{7},
		Tags:  nostr.TagMap{"e": []string{eventID}},
	}
	reactionCh, _ := db.QueryEvents(ctx, reactionFilter)
	for range reactionCh {
		reactionCount++
	}

	// Zaps (kind 9735)
	zapFilter := nostr.Filter{
		Kinds: []int{9735},
		Tags:  nostr.TagMap{"e": []string{eventID}},
	}
	zapCh, _ := db.QueryEvents(ctx, zapFilter)
	for event := range zapCh {
		zapCount++
		for _, tag := range event.Tags {
			if len(tag) >= 2 && tag[0] == "amount" {
				if amt, err := strconv.ParseInt(tag[1], 10, 64); err == nil {
					zapAmount += amt / 1000
				}
			}
		}
	}

	// Calculate rank
	rank := calculateEventRank(commentCount, quoteCount, repostCount, reactionCount, zapCount, zapAmount)

	metrics["comment_cnt"] = strconv.Itoa(commentCount)
	metrics["quote_cnt"] = strconv.Itoa(quoteCount)
	metrics["repost_cnt"] = strconv.Itoa(repostCount)
	metrics["reaction_cnt"] = strconv.Itoa(reactionCount)
	metrics["zap_cnt"] = strconv.Itoa(zapCount)
	metrics["zap_amount"] = strconv.FormatInt(zapAmount, 10)
	metrics["rank"] = strconv.Itoa(rank)

	return metrics
}

// computeAddressMetricsFromDB computes kind 30384 metrics for an address from local DB
func computeAddressMetricsFromDB(ctx context.Context, address string) map[string]string {
	metrics := make(map[string]string)

	var (
		commentCount  int
		quoteCount    int
		repostCount   int
		reactionCount int
		zapCount      int
		zapAmount     int64
	)

	// Comments (kind 1 with a tag)
	commentFilter := nostr.Filter{
		Kinds: []int{1},
		Tags:  nostr.TagMap{"a": []string{address}},
	}
	commentCh, _ := db.QueryEvents(ctx, commentFilter)
	for range commentCh {
		commentCount++
	}

	// Reposts (kind 6, 16)
	repostFilter := nostr.Filter{
		Kinds: []int{6, 16},
		Tags:  nostr.TagMap{"a": []string{address}},
	}
	repostCh, _ := db.QueryEvents(ctx, repostFilter)
	for range repostCh {
		repostCount++
	}

	// Reactions (kind 7)
	reactionFilter := nostr.Filter{
		Kinds: []int{7},
		Tags:  nostr.TagMap{"a": []string{address}},
	}
	reactionCh, _ := db.QueryEvents(ctx, reactionFilter)
	for range reactionCh {
		reactionCount++
	}

	// Zaps (kind 9735)
	zapFilter := nostr.Filter{
		Kinds: []int{9735},
		Tags:  nostr.TagMap{"a": []string{address}},
	}
	zapCh, _ := db.QueryEvents(ctx, zapFilter)
	for event := range zapCh {
		zapCount++
		for _, tag := range event.Tags {
			if len(tag) >= 2 && tag[0] == "amount" {
				if amt, err := strconv.ParseInt(tag[1], 10, 64); err == nil {
					zapAmount += amt / 1000
				}
			}
		}
	}

	rank := calculateEventRank(commentCount, quoteCount, repostCount, reactionCount, zapCount, zapAmount)

	metrics["comment_cnt"] = strconv.Itoa(commentCount)
	metrics["quote_cnt"] = strconv.Itoa(quoteCount)
	metrics["repost_cnt"] = strconv.Itoa(repostCount)
	metrics["reaction_cnt"] = strconv.Itoa(reactionCount)
	metrics["zap_cnt"] = strconv.Itoa(zapCount)
	metrics["zap_amount"] = strconv.FormatInt(zapAmount, 10)
	metrics["rank"] = strconv.Itoa(rank)

	return metrics
}
