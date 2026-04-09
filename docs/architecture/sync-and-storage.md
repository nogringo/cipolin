---
label: Sync And Storage
order: 153
icon: database
---

# Sync And Storage

Cipolin computes assertions from local persisted source events, while continuously improving that local dataset through sync.

## Relay Selection Strategy

Depending on metric subject, Cipolin pulls from:

- user NIP-65 relays (for user-centric source data)
- popular relays (for interactions/reports)
- configured storage relays (baseline + previously computed assertions)

This reduces blind network crawling and improves chance of high-value deltas.

## Fetch Mechanics

Key fetch characteristics:

- cursor-based pagination with oldest/newest boundaries
- boundary ID deduplication to avoid double-counting same timestamp pages
- TTL skip window for short-term fetch suppression
- bounded sync deadlines per request flow

Together, these mechanics keep sync practical for on-demand usage.

## Local Persistence

Fetched source events are persisted into BoltDB.

Metric computation always reads from local BoltDB first

## Assertion Replication

During streaming updates, clients receive events immediately. On final sync state, Cipolin republishes finalized assertions to storage relays.

