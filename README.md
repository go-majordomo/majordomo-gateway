# majordomo-gateway

A self-hosted, open-source LLM gateway for **cost and usage tracking**. Point your
apps at it instead of the provider, and every request is priced and logged to your
own Postgres. Then ask **Claude Code** (via the bundled MCP server) or the CLI what
your agents actually cost — sliced by model, provider, API key, or metadata, and
rolled up into per-run cost waterfalls.

- **Provider-agnostic proxy** — OpenAI, Anthropic, Gemini, Bedrock, and
  OpenAI-compatible providers (Fireworks, Together, Baseten).
- **Your keys, relayed** — by default the gateway forwards the caller's own provider
  key upstream and stores no provider credentials.
- **Provider routing (optional)** — opt in and the gateway routes a virtual model slug
  to the cheapest healthy provider that can serve it — the OpenRouter model,
  self-hosted. This is the one feature that stores credentials: an AES-encrypted,
  Postgres-backed provider-key store you manage with the CLI.
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
  -d '{"model":"gpt-5-nano","messages":[{"role":"user","content":"Hello"}]}'

# 3. See what it cost
gateway-cli usage summary
```

The rest of this README is the configuration + feature reference.

## Provider routing (self-hosted OpenRouter)

Optionally, the gateway can pick the provider for you. Opt in per request and a
**virtual model slug** (e.g. `glm-5.2`) is routed to the cheapest healthy provider
endpoint that can serve it. Routing is **off by default** — without it the gateway is a
pure pass-through and the model catalog only powers cost attribution.

Routing needs stored provider credentials (you can't choose among providers with a
single relayed key), so it is the one feature that stores secrets: keys are held in an
**AES-256-GCM encrypted, Postgres-backed** store, encrypted with `ENCRYPTION_KEY`. The
plaintext is never persisted and never returned by the API.

```bash
# 1. Enable routing and set the encryption key (32 bytes, hex or base64)
export ENCRYPTION_KEY=$(openssl rand -hex 32)
export ROUTING_ENABLED=true

# 2. Load a provider key (encrypted server-side; never echoed back)
gateway-cli provider-keys add --provider fireworks --key sk-...
gateway-cli provider-keys list      # PROVIDER  CREATED  (never the key material)

# 3. Route a request: opt in with x-majordomo-provider: majordomo, request a slug,
#    and send NO upstream Authorization — the gateway injects the stored credential.
curl http://localhost:6560/v1/chat/completions \
  -H "X-Majordomo-Key: mdm_sk_..." -H "x-majordomo-provider: majordomo" \
  -d '{"model":"glm-5.2","messages":[{"role":"user","content":"Hello"}]}'
```

The response carries `X-Majordomo-Routed-Provider` and `X-Majordomo-Routed-Model`, so
you can see which upstream served the request and the provider-native model id the slug
was rewritten to. The decision (provider, reason, original slug) is recorded on the
request log and surfaced by `gateway-cli usage request <id>`.

Routable slugs: `kimi-k3`, `glm-5.2`, `glm-5.3`, `glm-5.3-flash`,
`deepseek-v4-pro-0813`, `deepseek-v4.1-flash`, `qwen3.8-2.4t-a95b`, and `inkling`,
served by Fireworks, Together, and Baseten.

**How an endpoint is chosen.** Candidates for the slug are hard-filtered — a stored
credential first, then the request's data policy, then recent health (error rate over
the request log) — and the survivors are cost-weighted (cheaper wins more often). The
routable slugs and their candidate providers live in `model_catalog.json`.

**Request defaults.** A route entry in `model_catalog.json` may carry
`request_defaults`: body fields the gateway adds when it routes a request to that
endpoint.

```json
"glm-5.3": [
  {"provider": "together", "model": "zai-org/GLM-5.3",
   "request_defaults": {"reasoning_effort": "low"}},
  {"provider": "baseten", "model": "zai-org/GLM-5.3",
   "request_defaults": {"reasoning_effort": "low"}}
]
```

These exist because the hosts run most open-weight models with reasoning on at the
highest tier and bill it as output tokens, and a routing client cannot know which
host will serve it. Defaults are set per endpoint because the same value behaves
differently on each host and model, so an entry with no `request_defaults` is a
deliberate choice, not an omission. Baseten's GLM-5.2, for example, reasons only
when a reasoning field is sent, so it carries none.

- The caller always wins. A default is added only when the request body does not
  already set that field; an explicit `null` counts as set.
- `reasoning_effort`, `reasoning`, and `thinking` are treated as one decision. A
  request that sets any of them receives no reasoning default, even if the default
  uses a different one of the three.
- Values can be any JSON, so `{"thinking": {"type": "disabled"}}` is valid.
- A default may not set `model`; such an entry is dropped from the route.
- Defaults apply only to routed requests. A request that names a concrete provider
  is forwarded as sent.

When defaults are added, the response carries `X-Majordomo-Routed-Defaults` with the
field names, and the routing reason on the request log records them.

**Data policy (optional).** Tighten which endpoints are eligible with per-request
headers, on top of the deployment defaults (`ROUTING_DEFAULT_REQUIRE_ZDR`,
`ROUTING_DEFAULT_DATA_COLLECTION=deny`). Headers can only tighten, never relax:

```
X-Majordomo-ZDR: true                 → only zero-data-retention endpoints
X-Majordomo-Data-Collection: deny     → only endpoints that don't train on your data
```

Routing applies only to the OpenAI-compatible surface (`/v1/chat/completions` etc.). A
concrete provider pin (`x-majordomo-provider: fireworks`) or no header at all is
unchanged pass-through. A non-routable model returns **400**; a routable model with no
usable credentialed endpoint returns **502**.

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

Then ask in natural language — e.g. *"what did the billing-service spend on gpt-5-nano
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
| `ENCRYPTION_KEY` | | 32-byte key (hex or base64) enabling the encrypted provider-key store; required for provider routing |
| `ROUTING_ENABLED` | | `true` turns on provider routing (requires `ENCRYPTION_KEY`); default `false` |
| `ROUTING_DEFAULT_REQUIRE_ZDR` | | Default-require zero-data-retention endpoints for routed requests (default `false`) |
| `ROUTING_DEFAULT_DATA_COLLECTION` | | Set `deny` to default-require no-data-collection endpoints for routed requests |
| `PORT` | | Server port (default `6560`) |
| `BODY_STORAGE` | | `none` (default), `s3`, or `gcs` — see "Archiving bodies" below |
| `LOG_LEVEL` | | `debug`, `info`, `warn`, `error` (default `info`) |

Provider base URLs (`OPENAI_BASE_URL`, `ANTHROPIC_BASE_URL`, `GEMINI_BASE_URL`,
`FIREWORKS_BASE_URL`, `TOGETHER_BASE_URL`, `BASETEN_BASE_URL`) can be overridden to
route through a local proxy or mock. OpenAI-compatible providers (Fireworks,
Together, Baseten) share OpenAI's request paths — select them with the
`X-Majordomo-Provider` header (e.g. `X-Majordomo-Provider: together`).

### Pricing

Cost is computed per request from two sources:

| Variable | Description |
|---|---|
| `MODEL_CATALOG_FILE` | Local catalog of prices and routes (default `./model_catalog.json`) |
| `PRICING_REMOTE_URL` | Remote price feed (default `https://www.llm-prices.com/current-v1.json`) |
| `PRICING_REFRESH_INTERVAL` | How often both sources are reloaded (default `1h`) |
| `PRICING_ALIASES_FILE` | Maps model names returned by providers to catalog names (default `./model_aliases.json`) |
| `DEPRECATED_MODELS_FILE` | Maps retired model ids to their replacements (default `./deprecated_models.json`) |

The catalog is authoritative. A model listed in `model_catalog.json` is always
priced from the catalog, even when the remote feed lists the same model at a
different price. The remote feed only supplies prices for models the catalog does
not list, so traffic to a model you have not registered can still be costed.

- Remote feed unreachable at startup: prices come from the catalog alone.
- Remote feed unreachable on a later refresh: the prices already loaded are kept.
- Catalog unreadable: prices come from the remote feed alone and the routes already
  loaded are kept.

A request for a model that neither source prices is still proxied and logged, with
no cost attached. To add or reprice a model, edit its entry under
`providers.<provider>.models`; to make it routable, add a `routes` entry with at
least two endpoints.

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
