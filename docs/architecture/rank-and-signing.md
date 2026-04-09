---
label: Rank And Signing
order: 154
icon: graph
---

# Rank And Signing

This page covers user rank computation and the per-metric signing model.

## Neo4j Graph Rank

Cipolin ingests kind `3` contact lists into a directed follow graph:

- nodes: `User {pubkey}`
- edges: `(:User)-[:FOLLOWS]->(:User)`

It computes rank with Neo4j Graph Data Science PageRank stream and normalizes outputs to `0..100` for NIP-85 `rank` tags.

## Cache and Invalidation

To avoid expensive recomputation on every read:

- rank values are cached with TTL
- cache is invalidated when new follow edges are ingested
- graph projection is refreshed when marked dirty


## Deterministic Metric Keys

Each metric has its own deterministic key pair derived from:

- `NIP85_MASTER_KEY` + metric name

Consequences:

- different metrics are signed by different pubkeys
- clients can request specific metrics by author pubkey filter
- provenance is explicit at metric granularity

## `/keys` Endpoint

Cipolin exposes:

- `metric -> pubkey` map


