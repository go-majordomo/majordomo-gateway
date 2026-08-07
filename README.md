# majordomo-gateway

A self-hosted, open-source LLM gateway for **cost and usage tracking**. Point your
apps at it instead of the provider, and every request is priced and logged to your
own Postgres. Then ask **Claude Code** (via the bundled MCP server) or the CLI what
your agents actually cost — sliced by model, provider, API key, or metadata, and
rolled up into per-run cost waterfalls.

- **Provider-agnostic proxy** — OpenAI, Anthropic, Gemini, Bedrock, and
  OpenAI-compatible providers (Fireworks, Together, DeepSeek).
- **Your keys, relayed** — the gateway forwards the caller's own provider key
  upstream. It never stores provider credentials.
- **Agent-run observability** — stamp requests with a trace id and span path and the
  gateway reconstructs the run's cost waterfall.
- **No UI to run** — the interface is a CLI and an MCP server. Nothing phones home.

```
your app ──X-Majordomo-Key──▶ gateway (:6560) ──your provider key──▶ OpenAI / Anthropic / …
                                   │
                                   ▼
                              PostgreSQL  ◀── gateway-cli / gateway-mcp (usage queries)
```

## Quickstart

New here? **[GETTING_STARTED.md](GETTING_STARTED.md)** is a ~10-minute hands-on
walkthrough (Docker or local Go): run the gateway, mint a key, send your first request,
see the cost, and wire it into Claude Code. The short version:

```bash
# 1. Postgres + gateway (migrations run on startup; ADMIN_TOKEN enables the /api/v1 API)
docker run -d --name gateway-db -e POSTGRES_USER=majordomo -e POSTGRES_PASSWORD=secret -e POSTGRES_DB=majordomo_gateway -p 5432:5432 postgres:16
docker build -t majordomo-gateway . && docker run -d --name gateway -p 6560:6560 \
  -e POSTGRES_HOST=host.docker.internal -e POSTGRES_USER=majordomo -e POSTGRES_PASSWORD=secret \
  -e POSTGRES_DB=majordomo_gateway -e ADMIN_TOKEN=$(openssl rand -hex 32) majordomo-gateway

# 2. Mint a key (shown once), then send a request relaying your own provider key
export GATEWAY_TOKEN=<the ADMIN_TOKEN you set>
gateway-cli keys create --name billing-service
curl http://localhost:6560/v1/chat/completions \
  -H "X-Majordomo-Key: mdm_sk_..." -H "Authorization: Bearer $OPENAI_API_KEY" \
  -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"Hello"}]}'

# 3. See what it cost
gateway-cli usage summary
```

The rest of this README is the configuration + feature reference.

## Grouping usage: metadata and agent runs

Add `X-Majordomo-*` headers to tag requests; the `x-majordomo-` prefix is stripped to
form the metadata key:

```
X-Majordomo-Feature: onboarding     →  metadata key "feature"
X-Majordomo-Team: growth            →  metadata key "team"
```

Every tag is stored, and the gateway auto-discovers each key and tracks its approximate
**cardinality** (distinct-value count). But a key only becomes **queryable** once you
*activate* it — this keeps the metadata index bounded, so a high-cardinality tag like
`user_id` can't blow it up. Discover, inspect cardinality, then activate the low-card
dimensions you want to slice by:

```bash
gateway-cli metadata list
# API KEY          METADATA KEY  ~CARDINALITY  INDEXED  REQUESTS  API KEY ID
# billing-service  feature       3             false    1240      6f2c...
# billing-service  user_id       48213         false    1240      6f2c...

gateway-cli metadata activate --api-key 6f2c... feature   # low cardinality → safe to index
```

Activating backfills existing rows, so past usage becomes queryable too. Then:

```bash
gateway-cli usage summary --metadata feature=onboarding
```

Leave high-cardinality keys (like `user_id`) un-activated — they stay in the request's
raw metadata (visible in `usage requests`/`get_request`) but never hit the index.

To group the many LLM calls of one agent run, send a shared trace id and a span path
describing where in the run the call happened:

```
X-Majordomo-Trace-Id: run_8f2c...
X-Majordomo-Span-Path: planner/tool:search_db
```

Then render the run's cost waterfall:

```bash
gateway-cli usage runs                 # list runs
gateway-cli usage run run_8f2c...      # the waterfall for one run
```

## Ask Claude Code about your usage (MCP)

`gateway-mcp` is a stdio MCP server that exposes the usage/cost queries as tools.
Register it with Claude Code, pointing it at your gateway:

```bash
claude mcp add gateway -- gateway-mcp --api-url http://localhost:6560 --token $GATEWAY_TOKEN
```

Then ask in natural language — e.g. *"what did the billing-service spend on gpt-4o
last week?"* or *"show me the most expensive agent run yesterday."* The server exposes
`usage_summary`, `usage_by_model`, `usage_by_provider`, `usage_by_api_key`,
`usage_by_metadata`, `list_requests`, `get_request`, `error_summary`,
`recent_errors`, `latency_stats`, `list_runs`, and `get_run_tree`.

## Configuration

| Variable | Required | Description |
|---|---|---|
| `POSTGRES_HOST` | yes | PostgreSQL host |
| `POSTGRES_PORT` | | Port (default `5432`) |
| `POSTGRES_USER` | yes | PostgreSQL user |
| `POSTGRES_PASSWORD` | yes | PostgreSQL password |
| `POSTGRES_DB` | yes | Database name (default `majordomo_gateway`) |
| `ADMIN_TOKEN` | | Enables the `/api/v1` admin + usage query API (needed for the CLI and MCP server) |
| `PORT` | | Server port (default `6560`) |
| `BODY_STORAGE` | | `none` (default), `s3`, or `gcs` — see "Archiving bodies" below |
| `LOG_LEVEL` | | `debug`, `info`, `warn`, `error` (default `info`) |

Provider base URLs (`OPENAI_BASE_URL`, `ANTHROPIC_BASE_URL`, …) can be overridden to
route through a local proxy or mock. OpenAI-compatible providers (Fireworks, Together,
DeepSeek) share OpenAI's request paths — select them with the `X-Majordomo-Provider`
header (e.g. `X-Majordomo-Provider: deepseek`).

## Archiving request/response bodies

By default the gateway stores token counts and cost but **not** the request/response
payloads — you don't need them to track spend. To capture bodies for debugging or
audit, point `BODY_STORAGE` at an object store (S3, an S3-compatible store like MinIO
or R2, or GCS). Credentials come from the cloud SDK's default chain (`AWS_*` env / IAM
role, or `GOOGLE_APPLICATION_CREDENTIALS`) — the gateway stores no credentials itself.

```bash
BODY_STORAGE=s3
BODY_S3_BUCKET=my-gateway-bodies
BODY_S3_REGION=us-east-1
# BODY_S3_ENDPOINT=https://...   # for MinIO / R2
```

Each request's bodies are gzipped into one object keyed by request id; the row keeps a
`body_s3_key` pointer. Retrieve them on demand:

```bash
gateway-cli usage request <id> --body     # or the get_request_body MCP tool
```

Object storage (rather than the database) keeps large payloads out of Postgres, so the
usage tables stay lean. Bodies are archived in full — leave `BODY_STORAGE=none` to store
no bodies at all.

## Development

```bash
make build   # builds gateway, gateway-cli, gateway-mcp into bin/
make test    # run tests
make run     # run the server (localhost:6560)
```
