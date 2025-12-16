package main

import (
	"context"
	"strconv"
	"time"

	"github.com/nbd-wtf/go-nostr"
)

// computeUserMetricsFromDB computes kind 30382 metrics for a pubkey from local DB
func computeUserMetricsFromDB(ctx context.Context, pubkey string) map[string]string {
	metrics := make(map[string]string)

	var (
		followerCount  int
		postCount      int
		replyCount     int
		reactionCount  int
		zapAmountRecd  int64
		zapAmountSent  int64
		zapCountRecd   int
		zapCountSent   int
		firstCreatedAt int64 = time.Now().Unix()
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
		if event.CreatedAt.Time().Unix() < firstCreatedAt {
			firstCreatedAt = event.CreatedAt.Time().Unix()
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
		for _, tag := range event.Tags {
			if len(tag) >= 2 && tag[0] == "amount" {
				if amt, err := strconv.ParseInt(tag[1], 10, 64); err == nil {
					zapAmountRecd += amt / 1000 // msats to sats
				}
			}
		}
	}

	// Count zaps sent (kind 9734 by author)
	zapSentFilter := nostr.Filter{
		Authors: []string{pubkey},
		Kinds:   []int{9734},
	}
	zapSentCh, _ := db.QueryEvents(ctx, zapSentFilter)
	for event := range zapSentCh {
		zapCountSent++
		for _, tag := range event.Tags {
			if len(tag) >= 2 && tag[0] == "amount" {
				if amt, err := strconv.ParseInt(tag[1], 10, 64); err == nil {
					zapAmountSent += amt / 1000
				}
			}
		}
	}

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

	return metrics
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
