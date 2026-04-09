---
label: Architecture
order: 80
icon: cpu
expanded: true
---

# Architecture

Cipolin is a Nostr relay that intercepts NIP-85 metric queries, computes assertions from synchronized source events, signs them with per-metric keys, and returns them in the same request flow.

## At A Glance

- Relay server (`khatru`) with WebSocket endpoint on `/`
- NIP-85 query handler for kinds `30382`, `30383`, `30384`
- Async syncer and fetcher with cursor + TTL deduplication
- Local event store (BoltDB)
- Neo4j graph rank engine (GDS PageRank)
- Metric key manager (deterministic per-metric signing keys)
- Storage relay publisher for finalized assertion events

## Read The Architecture

- [Request Lifecycle](request-lifecycle.md): Step-by-step path from query to final assertion publication.
- [Why Just-In-Time](just-in-time.md): Deep rationale for Cipolin's progressive on-demand computation model.
- [Sync And Storage](sync-and-storage.md): Relay selection, fetch controls, local persistence, and replication behavior.
- [Rank And Signing](rank-and-signing.md): Neo4j PageRank pipeline, cache invalidation, and metric-specific keys.

