# Getting started

A hands-on walkthrough: stand up the gateway, send your first LLM request through it,
see what it cost, and wire it into Claude Code. Should take about 10 minutes.

By the end you'll have:

- the gateway running against Postgres,
- an API key your apps use to authenticate,
- real cost/usage data you can query from the CLI and from Claude Code (via MCP).

**Prerequisites:** Docker (for Postgres, and optionally the gateway itself). To run the
gateway locally or use the CLI/MCP without `docker exec`, you'll also want Go 1.25+.

Throughout, `$OPENAI_API_KEY` is your own provider key — the gateway relays it upstream
and never stores it.

---

## 1. Get the code

```bash
git clone https://github.com/go-majordomo/majordomo-gateway.git
cd majordomo-gateway
```

## 2. Start Postgres

```bash
docker run -d --name gateway-db \
  -e POSTGRES_USER=majordomo -e POSTGRES_PASSWORD=secret -e POSTGRES_DB=majordomo_gateway \
  -p 5432:5432 postgres:16
```

(Already have a Postgres? Just create an empty database and point the env vars in the
next step at it.)

## 3. Pick an admin token

The `/api/v1` management + query API (used by the CLI and MCP server) is guarded by a
single admin token. Generate one and keep it handy:

```bash
export ADMIN_TOKEN=$(openssl rand -hex 32)
echo "$ADMIN_TOKEN"
```

## 4. Run the gateway

Migrations run automatically on startup. Pick **one** of the following.

### Option A — Docker

```bash
docker build -t majordomo-gateway .

docker run -d --name gateway -p 6560:6560 \
  -e POSTGRES_HOST=host.docker.internal -e POSTGRES_USER=majordomo \
  -e POSTGRES_PASSWORD=secret -e POSTGRES_DB=majordomo_gateway \
  -e ADMIN_TOKEN=$ADMIN_TOKEN \
  majordomo-gateway
```

### Option B — local (Go)

```bash
cp .env.example .env    # then edit: set POSTGRES_* and ADMIN_TOKEN

# quick run:
go run ./cmd/gateway
# …or build all three binaries into ./bin and run the server:
make build && ADMIN_TOKEN=$ADMIN_TOKEN bin/gateway
```

Verify it's up:

```bash
curl -s localhost:6560/health      # -> ok
```

You should see a log line: `admin/query API enabled at /api/v1`. (If instead you see a
warning that `ADMIN_TOKEN` isn't set, the gateway is serving proxy traffic only — go
back and set it.)

## 5. Get the CLI

The `gateway-cli` and `gateway-mcp` binaries are thin HTTP clients for the gateway.

- **Local (Go):** `make build` puts them in `./bin`. Add `./bin` to your `PATH`, or
  call them as `bin/gateway-cli`.
- **Docker only (no Go):** the image bundles them — run e.g.
  `docker exec gateway /app/gateway-cli keys list`.

Point the CLI at the gateway once (or pass `--api-url` / `--token` per call):

```bash
export GATEWAY_URL=http://localhost:6560
export GATEWAY_TOKEN=$ADMIN_TOKEN
```

## 6. Mint an API key

Your apps authenticate to the gateway with an `mdm_sk_…` key (separate from the admin
token). The plaintext is shown **once** — copy it.

```bash
gateway-cli keys create --name billing-service
```

```bash
export MDM_KEY=mdm_sk_...   # paste the key from the output
```

## 7. Send your first request

Send a request exactly as you would to the provider, with two extra headers: your
gateway key, and your own provider key (which the gateway relays upstream).

```bash
curl http://localhost:6560/v1/chat/completions \
  -H "X-Majordomo-Key: $MDM_KEY" \
  -H "Authorization: Bearer $OPENAI_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"Hello!"}]}'
```

You get the provider's normal response back. Behind the scenes the gateway priced the
call and logged it.

> Using the OpenAI/Anthropic SDKs instead of curl? Point the SDK's base URL at
> `http://localhost:6560` and add the `X-Majordomo-Key` header; keep your provider key
> where it already is.

## 8. See what it cost

```bash
gateway-cli usage summary      # totals: requests, tokens, cost
gateway-cli usage models       # broken down by provider + model
gateway-cli usage keys         # broken down by API key
```

Send a few more requests and watch the numbers move.

## 9. Group usage by metadata

Tag requests with `X-Majordomo-*` headers (the `x-majordomo-` prefix becomes the key):

```bash
curl http://localhost:6560/v1/chat/completions \
  -H "X-Majordomo-Key: $MDM_KEY" -H "Authorization: Bearer $OPENAI_API_KEY" \
  -H "X-Majordomo-Feature: onboarding" \
  -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}'
```

The gateway auto-discovers each metadata key and tracks its approximate cardinality,
but a key only becomes queryable once you **activate** it — this keeps the metadata
index bounded, so a high-cardinality tag like `user_id` can't blow it up.

```bash
gateway-cli metadata list
# API KEY          METADATA KEY  ~CARDINALITY  INDEXED  REQUESTS  API KEY ID
# billing-service  feature       3             false    120       6f2c...

gateway-cli metadata activate --api-key 6f2c... feature   # low-card -> safe to index
gateway-cli usage summary --metadata feature=onboarding   # now queryable (past rows too)
```

Leave high-cardinality keys un-activated: they stay in the request's raw metadata but
never hit the index.

## 10. Track agent runs

To group the many LLM calls of one agent run, send a shared trace id plus a span path
describing where in the run each call happened:

```bash
TRACE=run_$(openssl rand -hex 6)
# call 1 — planning step
curl http://localhost:6560/v1/chat/completions \
  -H "X-Majordomo-Key: $MDM_KEY" -H "Authorization: Bearer $OPENAI_API_KEY" \
  -H "X-Majordomo-Trace-Id: $TRACE" -H "X-Majordomo-Span-Path: planner" \
  -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"plan"}]}'
# call 2 — a tool step under the planner
curl http://localhost:6560/v1/chat/completions \
  -H "X-Majordomo-Key: $MDM_KEY" -H "Authorization: Bearer $OPENAI_API_KEY" \
  -H "X-Majordomo-Trace-Id: $TRACE" -H "X-Majordomo-Span-Path: planner/tool:search" \
  -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"search"}]}'
```

Then render the run's cost waterfall:

```bash
gateway-cli usage runs           # list runs, newest first
gateway-cli usage run $TRACE     # the waterfall for one run
```

## 11. Query from Claude Code (MCP)

`gateway-mcp` exposes the usage/cost queries as tools. Register it, pointing at your
gateway:

```bash
claude mcp add gateway -- gateway-mcp --api-url http://localhost:6560 --token $ADMIN_TOKEN
```

(Use the full path to the binary — e.g. `.../majordomo-gateway/bin/gateway-mcp` — if
it isn't on your `PATH`.)

Now ask in natural language: *"what did the billing-service spend on gpt-4o-mini this
week?"* or *"show me the most expensive agent run today."* Claude calls the gateway's
tools — `usage_summary`, `usage_by_model`, `usage_by_metadata`, `list_runs`,
`get_run_tree`, and more.

## 12. (Optional) Archive request/response bodies

By default the gateway stores cost and tokens but **not** the payloads. To capture
bodies for debugging or audit, point `BODY_STORAGE` at an object store (S3, an
S3-compatible store like MinIO/R2, or GCS). Credentials come from the cloud SDK's
default chain (`AWS_*` / `GOOGLE_APPLICATION_CREDENTIALS` / instance role) — the
gateway stores none itself. Add to the gateway's environment:

```bash
BODY_STORAGE=s3
BODY_S3_BUCKET=my-gateway-bodies
BODY_S3_REGION=us-east-1
# BODY_S3_ENDPOINT=https://...   # for MinIO / R2
```

Each request's bodies are gzipped into one object; the row keeps a `body_s3_key`
pointer. Fetch them on demand:

```bash
gateway-cli usage request <request-id> --body
```

## Next steps

- Full configuration reference and provider notes: [README.md](README.md).
- Point every service at the gateway and give each its own key (`gateway-cli keys
  create --name <service>`) so you can attribute cost per service.
- Teardown for this walkthrough: `docker rm -f gateway gateway-db`.
