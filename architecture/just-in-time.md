# Why Just-In-Time vs Pre Compute

Cipolin uses just-in-time (JIT) computation: compute assertions when requested, not through a giant always-on global precompute pipeline.

## Nostr Data Is Distributed and Uneven

Nostr data is fragmented across relay sets and changes continuously. A full precompute strategy has two hard problems:

- high cost to crawl everything all the time
- stale output if crawl frequency is reduced
- stats may not be used, wasting compute resources

JIT addresses this by spending work only where there is active demand.

## Better Freshness-to-Cost Ratio

JIT gives better practical freshness per unit of compute:

- first response from warm local cache
- fresh deltas fetched during the request
- final assertions published for reuse

## User Experience Benefit: Progressive Answers

- quick initial assertion events
- incremental improvements as sync discovers new data
- finalized assertions published to storage relays

This keeps latency low while still converging toward better quality.

## Operational Benefit: Controlled Backpressure

Cipolin combines JIT with explicit controls:

- fetch TTL to avoid repeated relay scans in short windows
- cursor checkpoints and boundary deduplication
- rank cache TTL to avoid rerunning expensive ranking every query

## Tradeoffs and Cipolin Mitigations

- Cold misses can be slower:
  - mitigated with immediate cached emission and async refresh.
- Hot subjects can cause repeated demand:
  - mitigated with TTL, cursors, and storage relay publication.
- Partial relay visibility can create temporary variance:
  - mitigated by multi-source relay selection and progressive updates.
