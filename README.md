# Cipolin

A NIP-85 Trusted Assertions Provider that computes and serves real-time user and event metrics for Nostr.

**Live instance:** `wss://nip85.uid.ovh`

## What it does

Cipolin is a Nostr relay that generates on-demand metrics assertions:

- **Kind 30382** - User assertions (followers, posts, zaps, activity patterns, etc.)
- **Kind 30383** - Event assertions (comments, quotes, reposts, reactions, zaps)
- **Kind 30384** - Address assertions (for addressable/parametrized events)

When a client queries for these kinds with a `d` tag, Cipolin:
1. Fetches relevant events from the user's relays and popular relays
2. Stores them locally for metric computation
3. Ingests follow edges into Neo4j and computes user rank via PageRank
4. Signs and returns a NIP-85 assertion event

User rank keeps the existing progressive stream behavior: initial cached values are emitted first, then updated ranks are streamed as newly fetched follow data is ingested.

## Features

- Cursor-based pagination with TTL deduplication
- Parallel fetching from multiple relays
- Local Badger database for event storage
- Automatic relay discovery via NIP-65 and Breccia

## Installation

```bash
go build
```

## Docker

### Using pre-built image

```bash
docker compose -f docker-compose.ghcr.yml up -d
```

### Building locally

```bash
docker compose up -d
```

To rebuild the image after code changes:

```bash
docker compose up -d --build
```

### Manual Docker run

```bash
docker build -t cipolin .
docker run -d --name cipolin -p 3334:3334 --env-file .env -v ./data:/app/data cipolin
```

## Configuration

Copy `.env.example` to `.env` and configure:

```env
# Hex private key for signing assertions (leave empty to auto-generate)
NIP85_PRIVATE_KEY=

# Relay port
PORT=3334

# Storage relays for fetching/publishing (comma-separated)
STORAGE_RELAYS=wss://relay1.example.com,wss://relay2.example.com

# Local database path
DB_PATH=./data/cipolin.db

# Fetch TTL - skip re-fetching if within this duration (seconds)
FETCH_TTL_SECONDS=60

# Fetch timeout - timeout per relay connection (seconds)
FETCH_TIMEOUT_SECONDS=10

# Neo4j settings for PageRank user ranking
NEO4J_URI=neo4j://localhost:7687
NEO4J_USERNAME=neo4j
NEO4J_PASSWORD=your_password
NEO4J_DATABASE=neo4j

# Rank cache TTL and Neo4j query timeout (seconds)
RANK_CACHE_TTL_SECONDS=15
NEO4J_QUERY_TIMEOUT_SECONDS=20

# Require NIP-42 AUTH for REQ/COUNT requests
ENABLE_NIP42_AUTH=false

# Optional request policy plugin command
REQUEST_POLICY_PLUGIN=
REQUEST_POLICY_TIMEOUT_MS=1500
REQUEST_POLICY_FAIL_OPEN=false
```

### Request Policy Plugin Protocol

When `REQUEST_POLICY_PLUGIN` is set, Cipolin starts the plugin command and sends one JSON line per request (`REQ`/`COUNT`) to stdin.

Input JSON fields:

- `type`: always `new`
- `id`: request correlation id (must be echoed back)
- `filter`: full Nostr filter from the client
- `receivedAt`: unix timestamp (seconds)
- `sourceType`: `IP4`, `IP6`, or `Unknown`
- `sourceInfo`: source IP when available
- `authed`: present only when the connection completed NIP-42 AUTH (hex pubkey)
- `requestType`: `REQ` or `COUNT`

Output JSON fields:

- `id`: same as input id
- `action`: `accept`, `reject`, or `shadowReject`
- `msg`: optional NIP-20 message for `reject`

This lets you implement per-pubkey allow/deny and custom rate limiting in any language.

Example bundled plugin:

```bash
chmod +x ./scripts/request-policy.js
REQUEST_POLICY_PLUGIN=./scripts/request-policy.js \
ALLOW_PUBKEYS=<hex-pubkey-1>,<hex-pubkey-2> \
RATE_LIMIT_PER_MIN=120 \
```

## Usage

Start the relay:

```bash
./cipolin
```

Query user metrics:

```bash
nak req -k 30382 -d <pubkey> ws://localhost:3334
```

Query event metrics:

```bash
nak req -k 30383 -d <event_id> ws://localhost:3334
```

Query address metrics:

```bash
nak req -k 30384 -d <kind:pubkey:d-tag> ws://localhost:3334
```

## User Metrics (Kind 30382)

| Tag | Description |
|-----|-------------|
| `followers` | Number of followers |
| `rank` | Neo4j GDS PageRank over follow graph, normalized to 0-100 |
| `first_created_at` | Timestamp of first known event |
| `post_cnt` | Number of posts |
| `reply_cnt` | Number of replies |
| `reactions_cnt` | Number of reactions sent |
| `zap_amt_recd` | Total sats received via zaps |
| `zap_amt_sent` | Total sats sent via zaps |
| `zap_cnt_recd` | Number of zaps received |
| `zap_cnt_sent` | Number of zaps sent |
| `zap_avg_amt_day_recd` | Average sats received per day |
| `zap_avg_amt_day_sent` | Average sats sent per day |
| `reports_cnt_recd` | Reports received |
| `reports_cnt_sent` | Reports sent |
| `active_hours_start` | Start of active hours (UTC) |
| `active_hours_end` | End of active hours (UTC) |
| `t` | Top topics (multiple tags) |

## Event/Address Metrics (Kind 30383/30384)

| Tag | Description |
|-----|-------------|
| `comment_cnt` | Number of comments/replies |
| `quote_cnt` | Number of quotes |
| `repost_cnt` | Number of reposts |
| `reaction_cnt` | Number of reactions |
| `zap_cnt` | Number of zaps |
| `zap_amount` | Total sats zapped |
| `rank` | Normalized rank (0-100) |
