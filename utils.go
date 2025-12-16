package main

import (
	"math"
	"strings"
	"time"

	"github.com/nbd-wtf/go-nostr"
)

// splitRelays parses a comma-separated relay list
func splitRelays(s string) []string {
	parts := strings.Split(s, ",")
	relays := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			relays = append(relays, p)
		}
	}
	return relays
}

// signEvent signs an event with the service provider's private key
func signEvent(event *nostr.Event) error {
	event.PubKey = servicePubKey
	event.CreatedAt = nostr.Timestamp(time.Now().Unix())
	return event.Sign(servicePrivateKey)
}

// calculateUserRank computes a normalized 0-100 rank for a user
func calculateUserRank(followers, posts int, zapAmount int64, zapCount int) int {
	score := float64(followers)*1.0 +
		float64(posts)*0.5 +
		float64(zapAmount)*0.001 +
		float64(zapCount)*2.0

	if score <= 0 {
		return 0
	}

	// Log scale normalization (assumes max score ~1M)
	rank := int((math.Log10(score) / 6.0) * 100)
	if rank > 100 {
		rank = 100
	}
	if rank < 0 {
		rank = 0
	}
	return rank
}

// calculateEventRank computes a normalized 0-100 rank for an event
func calculateEventRank(comments, quotes, reposts, reactions, zaps int, zapAmount int64) int {
	score := float64(comments)*3.0 +
		float64(quotes)*5.0 +
		float64(reposts)*2.0 +
		float64(reactions)*1.0 +
		float64(zaps)*4.0 +
		float64(zapAmount)*0.01

	if score <= 0 {
		return 0
	}

	rank := int((math.Log10(score) / 4.0) * 100)
	if rank > 100 {
		rank = 100
	}
	if rank < 0 {
		rank = 0
	}
	return rank
}

// minInt returns the minimum of two integers
func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
