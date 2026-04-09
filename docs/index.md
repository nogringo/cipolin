---
label: Home
order:  100
icon: home
---

# Cipolin

Cipolin is a NIP-85 trusted assertions provider for Nostr.

It computes assertions just in time when queried and stores computed assertion events on storage relays. For user rank, Cipolin ingests follow graph updates into Neo4j and runs Graph Data Science PageRank.

## What Cipolin Serves

- `kind:30382`: User assertions (`d=<pubkey>`)
- `kind:30383`: Event assertions (`d=<event_id>`)
- `kind:30384`: Address assertions (`d=<kind:pubkey:d-tag>`)

Each assertion event is signed with a deterministic metric-specific key derived from one master key, matching the NIP-85 model.

## What is diffrent to other NIP-85 providers

Cipolin uses a just-in-time approach; metrics are only calculated when requested. Because this can take a long time, preliminary results are streamed to the client. The underlying assumption is that early results are already good enough for most use cases. For algorithms like PageRank that work iteratively, this works really well, according to our testing, even with incomplete follow graph data, the scores are within ~10% of their final value. For the Nostr use case where you are mostly interested if the account is an imposter or not, this can be acceptable.
To cache the finalized value, storage relays are used where the final trusted assertion event gets published.

This architecture allows for a lot of flexibility; in theory, values can be calculated ahead of time by invoking Cipolin; however, an invoker engine is not part of Cipolin (yet).

## Documentation

- [Architecture](architecture/index.md): End-to-end request flow, fetch/sync pipeline, storage, and rank engine.
- [Guides](guides/index.md): Practical guides for running and querying Cipolin.

## Quick Start

1. Configure `.env` (especially `NEO4J_PASSWORD`, `NIP85_MASTER_KEY`, `STORAGE_RELAYS`).
2. Start Neo4j with Graph Data Science plugin.
3. Run Cipolin.
4. Query assertions with `nak req` using `kinds 30382/30383/30384` and a `d` tag.


