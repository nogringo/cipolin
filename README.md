# Cipolin

A NIP-85 Trusted Assertions Provider that computes and serves real-time user and event metrics for Nostr.

## What it does

Cipolin is a Nostr relay that generates on-demand metrics assertions:

- **Kind 30382** - User assertions (followers, posts, zaps, activity patterns, etc.)
- **Kind 30383** - Event assertions (comments, quotes, reposts, reactions, zaps)
- **Kind 30384** - Address assertions (for addressable/parametrized events)

When a client queries for these kinds with a `d` tag, Cipolin:
1. Fetches relevant events from the user's relays and popular relays
2. Stores them locally for metric computation
3. Computes metrics from the local database
4. Signs and returns a NIP-85 assertion event

## Features

- Cursor-based pagination with TTL deduplication
- Parallel fetching from multiple relays
- Local Badger database for event storage
- Automatic relay discovery via NIP-65 and Breccia

## Installation

```bash
go build
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
| `rank` | Normalized rank (0-100) |
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
