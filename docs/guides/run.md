---
label: Run Cipolin
order: 201
icon: play
---

# Run Cipolin

This guide covers local and Docker setup.

## Prerequisites

- Go 1.22+ (for local build)
- Docker + Docker Compose (for containerized run)
- Neo4j with Graph Data Science plugin enabled

## 1) Configure Environment

Create `.env` from the example:

```bash
cp .env.example .env
```

Important variables:

```env
NIP85_MASTER_KEY=<hex-private-key>
PORT=3334
STORAGE_RELAYS=wss://my-storage-0.relay.example,wss://my-storage-1.relay.example #storage relays store the final computed assertion events
DB_PATH=./data/cipolin.db
FETCH_TTL_SECONDS=60
FETCH_TIMEOUT_SECONDS=30

NEO4J_URI=neo4j://localhost:7687
NEO4J_USERNAME=neo4j
NEO4J_PASSWORD=your_password
NEO4J_DATABASE=neo4j
RANK_CACHE_TTL_SECONDS=15
NEO4J_QUERY_TIMEOUT_SECONDS=20
```

Notes:

- `NEO4J_PASSWORD` is required. Cipolin exits if it is missing.
- If `NIP85_MASTER_KEY` is empty, Cipolin generates a temporary key on boot.

## 2) Start Neo4j (with GDS)

From the repository root:

```bash
docker compose -f neo4j/docker-compose.yaml up -d
```

This publishes:

- Neo4j Browser: `http://localhost:7474`
- Bolt: `neo4j://localhost:7687`

Default credentials from compose example:

- user: `neo4j`
- password: `your_password`

## 3) Run Locally (Go)

```bash
go run ./cmd/cipolin/main.go
```





## Operational Notes

- Local event cache is persisted in BoltDB (`DB_PATH`).
- Finalized assertion events are published to `STORAGE_RELAYS`.
- User rank relies on Neo4j GDS PageRank over ingested follow edges.
