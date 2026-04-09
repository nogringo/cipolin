---
label: Use Cipolin with nak
order: 202
icon: terminal
---

# Use Cipolin with nak

Cipolin is queried like a relay. Use NIP-85 kinds and `d` tags to request assertions.

Assume relay URL:

```bash
RELAY=ws://localhost:3334
```

## Install nak

Repository: `github.com/fiatjaf/nak`

```bash
go install github.com/fiatjaf/nak@latest
```

## Query User Assertions (Kind 30382)

Subject is a hex pubkey in `d`.

```bash
PUBKEY=<64-hex-pubkey>
nak req -k 30382 -d "$PUBKEY" "$RELAY" --stream
```

Expected response characteristics:

- one event per metric tag (for example `followers`, `rank`, `post_cnt`, `zap_amt_recd`)
- each event includes `d` and `p` tags for the subject
- metrics stream progressively as sync fetches additional source events

## Query Event Assertions (Kind 30383)

Subject is a hex event id in `d`.

```bash
EVENT_ID=<64-hex-event-id>
nak req -k 30383 -d "$EVENT_ID" "$RELAY"
```

Common tags emitted:

- `comment_cnt`, `quote_cnt`, `repost_cnt`, `reaction_cnt`, `zap_cnt`, `zap_amount`, `rank`

## Query Address Assertions (Kind 30384)

Subject is an address string in `d` using `kind:pubkey:d-tag` format.

```bash
ADDR="30023:0123456789abcdef...:my-article-slug"
nak req -k 30384 -d "$ADDR" "$RELAY"
```

## Filter by Metric Pubkey (NIP-85 style)

Each metric is signed by a metric-specific pubkey. Fetch those keys from `/keys`:

```bash
curl -s http://localhost:3334/keys | jq '.pubkeys'
```

Then request only one metric by author filter:

```bash
RANK_PUBKEY=$(curl -s http://localhost:3334/keys | jq -r '.pubkeys.rank')
nak req -k 30382 -d "$PUBKEY" -a "$RANK_PUBKEY" "$RELAY"
```

## Generate Kind 10040 Helper Data

Cipolin exposes preformatted `kind10040` tuples at `/keys`:

```bash
curl -s http://localhost:3334/keys | jq '.kind10040'
```

Use those tuples to build your client-side `kind:10040` metric configuration.

## Troubleshooting

- No events returned:
  - verify `d` subject format and kind pairing
  - check relay URL and websocket connectivity
- Rank always `0`:
  - confirm Neo4j is reachable and GDS plugin is enabled
  - ensure follow events (kind `3`) are being ingested
- Missing specific metrics:
  - if using author filters, confirm you used the correct metric pubkey from `/keys`
