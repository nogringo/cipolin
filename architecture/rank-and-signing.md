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

- `NIP85_MASTER_KEY` + NIP-85 kind + metric name

The derivation uses:

```
SHA256(masterKey + ":nip85:" + kind + ":" + metricName)
```

So that the same metric name on different kinds produces **distinct** pubkeys:

| Derivation | Example |
|---|---|
| user `rank` (kind 30382) | `SHA256(master + ":nip85:30382:rank")` |
| event `rank` (kind 30383) | `SHA256(master + ":nip85:30383:rank")` |
| address `rank` (kind 30384) | `SHA256(master + ":nip85:30384:rank")` |

Consequences:

- different metrics are signed by different pubkeys
- **same metric name across different kinds gets distinct pubkeys** — no ambiguity
- clients can request specific metrics by author pubkey filter
- provenance is explicit at metric granularity
- adding a new metric costs nothing — just derive a new pubkey, no schema change or migration needed

### Why one pubkey per metric?

This design is central to cipolin's scalability. Because each metric has a distinct pubkey, clients use the NIP-85 `author` filter (`-a`) to request **only the metrics they need**. This triggers a cascade of optimizations:

1. **Selective relay queries** — if a client only requests `followers`, cipolin only queries relays for kind 3 events (contact lists). It skips fetching kind 1 posts, kind 7 reactions, kind 9735 zaps, etc.
2. **Reduced computation** — unrequested metrics are never computed, serialized, or signed
3. **Linear scalability** — one cipolin instance can serve hundreds of different metrics without proportional cost increase, because each request only touches what is explicitly asked for
4. **Stateless metric addition** — adding a new metric does not degrade existing request performance; it is simply another derivable pubkey that only gets exercised when requested

## `/keys` Endpoint

Cipolin exposes:

- `"kind:metric" -> pubkey` map (e.g. `"30382:rank"`, `"30383:rank"`, `"30384:rank"`)
- `kind10040` tags with the full tag list for client configuration
- `relay_url`, `user_metrics`, and `event_metrics` lists
