# Request Lifecycle

This page explains what happens from a NIP-85 query to final assertion publication.

## Lifecycle Diagram

```text
Client (nak/app)
	|
	| REQ kind 30382/30383/30384 + d tag
	v
+-------------------+
| Cipolin Relay     |
| NIP-85 Handler    |
+-------------------+
	|
	| parse + validate subject
	v
+-------------------+
| Local BoltDB      |
| compute metrics   |
+-------------------+
	|
	| stream initial assertions
	v
Client receives first results
	^
	|
	| updated assertions (progressive)
	|
+-------------------+         +-----------------------+
| Async Syncer      |-------> | Relay Sources         |
| (bounded window)  |         | NIP-65/popular/store  |
+-------------------+         +-----------------------+
	|
	| persist new source events
	v
+-------------------+
| Local BoltDB      |
| recompute metrics |
+-------------------+
	|
	| final assertions
	v
+-------------------+
| Storage Relays    |
| publish finalized |
+-------------------+
```

## 1) Client Query Arrives

A client sends a query to Cipolin relay (`/`) for one of:

- `30382` user assertions (`d=<pubkey>`)
- `30383` event assertions (`d=<event_id>`)
- `30384` address assertions (`d=<kind:pubkey:d-tag>`)

The handler intercepts these kinds before normal relay DB query flow.

## 2) Subject Resolution

Cipolin extracts the subject from `d` and validates shape:

- user: 64-char hex pubkey
- event: 64-char hex id
- address: Nostr address string

Invalid subjects are ignored rather than forcing expensive downstream work.

## 3) Initial Assertion Emission

Cipolin computes metrics from locally cached events and emits assertion events immediately.

This first wave gives low-latency results even before fresh network sync completes.

## 4) Async Sync Starts

In parallel, Cipolin starts bounded async sync:

- user-related filters for posts, reactions, followers, zaps, reports
- event/address interaction filters for comments, quotes, reposts, reactions, zaps
- relay sets selected from user NIP-65 relays, popular relays, and storage relays

## 5) Progressive Recompute and Stream

As new source events are fetched and persisted, Cipolin recomputes metrics and streams updated assertions.

The client sees progressively improving results during one query session.

## 6) Finalization and Replication

When sync completes for the request window, Cipolin emits final metrics and publishes those finalized assertion events to configured storage relays.

This makes subsequent reads faster and increases availability of recently computed assertions across relay infrastructure.
