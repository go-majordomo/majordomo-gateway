---
name: majordomo-gateway
description: |
  How to route LLM calls through a Majordomo gateway — Managed Cloud, self-hosted
  Steward, or the open-source majordomo-gateway — for cost tracking, usage logging,
  metadata attribution, and agent-run observability. Works with ANY client: the raw
  OpenAI or Anthropic SDK, curl, majordomo-llm, Pydantic AI, or Agno.
  Load this skill when the user wants to send LLM requests through the Majordomo
  gateway, add cost/usage tracking to existing LLM code, tag requests with metadata
  (feature, team, project, user), or group an agent's or conversation's calls into
  one run with a nested cost waterfall (trace/span headers).
  Do NOT load for majordomo-llm standalone usage without a gateway (use the
  majordomo-llm skill instead), or for Go gateway internals.
allowed-tools: Read, Write, Bash
---

The Majordomo gateway is a transparent HTTP proxy. Point any client at it instead of
the provider, add one header, and every request is priced and logged. The integration
code is the **same** whether you use Managed Cloud, self-hosted Steward, or the
open-source gateway — only the URL, where the key comes from, and where you read
results differ.

## Step 1 — Ask which gateway they're running

**"Are you on Majordomo Cloud, self-hosting Steward, or running the open-source gateway?"**

| Target | `MAJORDOMO_GATEWAY_URL` | Get an API key | Read usage |
|---|---|---|---|
| **Managed Cloud** | `https://gateway.gomajordomo.com` | dashboard → API Keys | dashboard (`app.gomajordomo.com`) |
| **Self-hosted Steward** | wherever you deployed it — **port 7680** by default (`http://localhost:7680` for local dev) | dashboard → API Keys | dashboard |
| **Open-source `majordomo-gateway`** | wherever you deployed it — **port 6560** by default (`http://localhost:6560` for local dev) | `gateway-cli keys create` | `gateway-cli usage` / MCP |

Everything below is identical across the three. Only Managed Cloud has a fixed URL; a
self-hosted Steward or open-source gateway lives at whatever host you deployed it to (a
server, a container, a Kubernetes ingress) — `localhost` is just the local-dev case.
**Always ask for the actual base URL; the ports above are only the defaults.**

## Step 2 — Environment

Always read config from the environment; never hardcode URLs or keys.

```bash
MAJORDOMO_GATEWAY_URL=https://your-gateway.example.com   # your deploy's base URL (local dev: http://localhost:7680 Steward, :6560 OSS)
MAJORDOMO_API_KEY=mdm_sk_your_key_here             # always required

# Provider keys — whichever you use. They are relayed upstream by the gateway.
ANTHROPIC_API_KEY=sk-ant-...
OPENAI_API_KEY=sk-...
GEMINI_API_KEY=...
```

## Step 3 — Connect a client

The gateway speaks the providers' own wire formats. Change the base URL, add the
`X-Majordomo-Key` header, and pass your provider key through as usual.

```python
# OpenAI SDK — base_url ends in /v1
from openai import OpenAI

client = OpenAI(
    base_url=f"{os.environ['MAJORDOMO_GATEWAY_URL']}/v1",
    api_key=os.environ["OPENAI_API_KEY"],
    default_headers={"X-Majordomo-Key": os.environ["MAJORDOMO_API_KEY"]},
)
resp = client.chat.completions.create(
    model="gpt-5-mini",
    messages=[{"role": "user", "content": "Hello!"}],
)
```

```python
# Anthropic SDK — base_url has NO /v1 suffix (the SDK appends /v1/messages)
import anthropic

client = anthropic.Anthropic(
    base_url=os.environ["MAJORDOMO_GATEWAY_URL"],
    api_key=os.environ["ANTHROPIC_API_KEY"],
)
resp = client.messages.create(
    model="claude-sonnet-4-6",
    max_tokens=1024,
    messages=[{"role": "user", "content": "Hello!"}],
    extra_headers={"X-Majordomo-Key": os.environ["MAJORDOMO_API_KEY"]},
)
```

```bash
# curl — any HTTP client works
curl "$MAJORDOMO_GATEWAY_URL/v1/chat/completions" \
  -H "Authorization: Bearer $OPENAI_API_KEY" \
  -H "X-Majordomo-Key: $MAJORDOMO_API_KEY" \
  -d '{"model": "gpt-5-mini", "messages": [{"role": "user", "content": "Hello!"}]}'
```

Using **majordomo-llm, Pydantic AI, or Agno**? The base_url + header pattern is the
same, but each has its own wiring — see **[references/frameworks.md](references/frameworks.md)**.

## Metadata headers — tag requests for cost attribution

Any `X-Majordomo-*` header you add is logged and becomes a filterable dimension.
Headers are user-defined; there is no fixed list. The `X-Majordomo-` prefix is
stripped, so `X-Majordomo-Feature` is stored as `Feature`.

**Before writing code, look at what the call does and recommend concrete headers.**
Consider:

- What is this call doing? → a feature/workflow name (`X-Majordomo-Feature`)
- Part of a larger product? → a project name (`X-Majordomo-Project`)
- Serves multiple teams? → a team (`X-Majordomo-Team`)
- Serves multiple users/tenants? → an opaque id (`X-Majordomo-User-Id`), never PII
- Which environment? → `X-Majordomo-Environment`

Propose names and values from what you can infer, then confirm with the user. Set
stable dimensions once in `default_headers`; send per-call values (like the current
user's id) in `extra_headers` on the individual request.

## Agent run tracking — group calls into one run with a cost waterfall

When one logical task (a conversation or an agent/workflow run) makes several LLM
calls, three reserved headers group them into a single **run** with a rolled-up cost
and a nested waterfall. They are consumed by the gateway (not stored as metadata):

| Header | Required | Value |
|---|---|---|
| `X-Majordomo-Trace-Id` | To join a run | One id per conversation / run. Generate once at the start, send on every call in the run (any opaque string). |
| `X-Majordomo-Span-Path` | No | `/`-joined ancestor step names down to this call's parent, e.g. `planner/tool:search_db`. Omit to hang directly under the run root. `/` is the separator — percent-encode a literal `/`. |
| `X-Majordomo-Span-Name` | No | Label for this call in the waterfall. Defaults to the model name. |

Graceful degradation: **trace id alone** → a flat run rollup (all calls + total cost);
**trace id + span path** → the nested waterfall (which step drove which calls, and what
each cost). No SDK required — any client that can set headers works.

```python
import uuid
trace_id = str(uuid.uuid4())   # one per conversation / run, reused across every call

resp = client.chat.completions.create(
    model="gpt-5-mini",
    messages=[...],
    extra_headers={
        "X-Majordomo-Key": os.environ["MAJORDOMO_API_KEY"],
        "X-Majordomo-Trace-Id": trace_id,
        "X-Majordomo-Span-Path": "planner/tool:search_db",
        "X-Majordomo-Span-Name": "summarize",
    },
)
```

On Cloud/Steward the dashboard's **Runs** view opens the waterfall; on the open-source
gateway, `gateway-cli usage runs` and `gateway-cli usage run <id>` show the same tree.

## Notes

- Reserved headers (not stored as metadata): `X-Majordomo-Key`, `X-Majordomo-Provider`,
  `X-Majordomo-Provider-Alias`, `X-Majordomo-Client`, `X-Majordomo-Trace-Id`,
  `X-Majordomo-Span-Path`, `X-Majordomo-Span-Name`.
- `X-Majordomo-*` headers are consumed by the gateway and stripped before the request
  is proxied upstream — providers never see them.
- Self-host / deploy the open-source gateway: see the gateway README and GETTING_STARTED.
  Managed Cloud and self-hosted Steward setup: https://docs.gomajordomo.com
