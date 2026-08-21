# Framework & library recipes

Every recipe uses the same two environment variables from the main skill:
`MAJORDOMO_GATEWAY_URL` and `MAJORDOMO_API_KEY`. All `X-Majordomo-*` headers from the
main skill (metadata like `X-Majordomo-Feature`, and the agent-run `X-Majordomo-Trace-Id`
/ `X-Majordomo-Span-Path`) work in every recipe — attach them wherever the recipe shows
`extra_headers`.

Refresh model names as needed. Examples use `claude-sonnet-4-6`, `gpt-5-mini`,
`gemini-2.5-flash`.

---

## majordomo-llm

The library takes the gateway root as `base_url` (no `/v1`) and a `default_headers`
dict. It handles per-provider routing internally. See the **majordomo-llm** skill for
the library's own API (structured output, streaming, logging).

```python
import os
from majordomo_llm import get_llm_instance, LLMCascade

GATEWAY = os.environ["MAJORDOMO_GATEWAY_URL"]
HEADERS = {
    "X-Majordomo-Key": os.environ["MAJORDOMO_API_KEY"],
    "X-Majordomo-Feature": "document-classification",   # stable metadata
}

llm = get_llm_instance(
    "anthropic", "claude-sonnet-4-6",
    base_url=GATEWAY,
    default_headers=HEADERS,
)

# Per-call values (agent-run trace/span, current user, etc.) via extra_headers:
resp = await llm.get_response(
    "Classify this: ...",
    extra_headers={
        "X-Majordomo-User-Id": user_id,
        "X-Majordomo-Trace-Id": trace_id,
        "X-Majordomo-Span-Path": "planner/tool:classify",
    },
)

# Cascade — base_url + default_headers propagate to every provider in the list:
cascade = LLMCascade(
    [("anthropic", "claude-sonnet-4-6"), ("openai", "gpt-5-mini")],
    base_url=GATEWAY,
    default_headers=HEADERS,
)
resp = await cascade.get_response("Hello!")
```

---

## Pydantic AI

Point the model's provider at the gateway; pass `X-Majordomo-*` headers through
`extra_headers` on the model settings. **OpenAI and Gemini** go through the
OpenAI-compatible endpoint (`base_url` ends in `/v1`); **Anthropic** uses its native
provider (`base_url` is the gateway root, no `/v1`).

```python
import os
from openai import AsyncOpenAI
from pydantic_ai import Agent
from pydantic_ai.models.openai import OpenAIChatModel
from pydantic_ai.models.anthropic import AnthropicModel
from pydantic_ai.providers.openai import OpenAIProvider
from pydantic_ai.providers.anthropic import AnthropicProvider
from pydantic_ai.settings import AnthropicModelSettings, OpenAIChatModelSettings

GATEWAY = os.environ["MAJORDOMO_GATEWAY_URL"]

def mdm_headers(**extra):
    return {"X-Majordomo-Key": os.environ["MAJORDOMO_API_KEY"], **extra}

# --- OpenAI ---
openai_model = OpenAIChatModel(
    "gpt-5-mini",
    provider=OpenAIProvider(openai_client=AsyncOpenAI(
        base_url=f"{GATEWAY}/v1",
        api_key=os.environ["OPENAI_API_KEY"],
    )),
)
openai_settings = OpenAIChatModelSettings(
    extra_headers=mdm_headers(**{"X-Majordomo-Feature": "research-agent"})
)

# --- Anthropic (native provider; gateway root, no /v1) ---
anthropic_model = AnthropicModel(
    "claude-sonnet-4-6",
    provider=AnthropicProvider(
        base_url=GATEWAY,
        api_key=os.environ["ANTHROPIC_API_KEY"],
    ),
)
anthropic_settings = AnthropicModelSettings(
    extra_headers=mdm_headers(**{"X-Majordomo-Feature": "research-agent"})
)

# --- Gemini (OpenAI-compatible endpoint; add the provider routing header) ---
gemini_model = OpenAIChatModel(
    "gemini-2.5-flash",
    provider=OpenAIProvider(openai_client=AsyncOpenAI(
        base_url=f"{GATEWAY}/v1",
        api_key=os.environ["GEMINI_API_KEY"],
    )),
)
gemini_settings = OpenAIChatModelSettings(
    extra_headers=mdm_headers(**{"X-Majordomo-Provider": "gemini-openai"})
)

agent = Agent(anthropic_model, system_prompt="You are a helpful assistant.")
result = await agent.run("Summarize this document: ...", model_settings=anthropic_settings)
```

**Agent runs:** for a single call you can drop the run's `X-Majordomo-Trace-Id` /
`X-Majordomo-Span-Path` into `extra_headers` on the model settings. But for a real
multi-step agent — nested tool calls, `asyncio.gather` — threading `model_settings`
through every call is painful. Use **context-scoped injection** instead (see
[Agent runs](#agent-runs-propagate-traces-automatically) below); it propagates the
current step's headers to every model call automatically.

---

## Agno

Agno routes every provider through the gateway's OpenAI-compatible API. Use `OpenAIChat`
for OpenAI and `OpenAILike` for the rest, with the gateway `base_url` (ends in `/v1`)
and headers baked into the model via `extra_headers`. Non-OpenAI providers need an
`X-Majordomo-Provider` routing header so the gateway translates the format.

```python
import os
from agno.agent import Agent
from agno.models.openai import OpenAIChat
from agno.models.openai.like import OpenAILike

GATEWAY = os.environ["MAJORDOMO_GATEWAY_URL"]

def mdm_headers(**extra):
    return {"X-Majordomo-Key": os.environ["MAJORDOMO_API_KEY"], **extra}

# --- OpenAI ---
openai_model = OpenAIChat(
    id="gpt-5-mini",
    api_key=os.environ["OPENAI_API_KEY"],
    base_url=f"{GATEWAY}/v1",
    extra_headers=mdm_headers(**{"X-Majordomo-Feature": "support-agent"}),
)

# --- Anthropic (via OpenAI-compatible translation) ---
anthropic_model = OpenAILike(
    id="claude-sonnet-4-6",
    api_key=os.environ["ANTHROPIC_API_KEY"],
    base_url=f"{GATEWAY}/v1",
    extra_headers=mdm_headers(**{
        "X-Majordomo-Provider": "anthropic-openai",
        "X-Majordomo-Feature": "support-agent",
    }),
)

# --- Gemini (via OpenAI-compatible translation) ---
gemini_model = OpenAILike(
    id="gemini-2.5-flash",
    api_key=os.environ["GEMINI_API_KEY"],
    base_url=f"{GATEWAY}/v1",
    extra_headers=mdm_headers(**{
        "X-Majordomo-Provider": "gemini-openai",
        "X-Majordomo-Feature": "support-agent",
    }),
)

agent = Agent(model=anthropic_model)
agent.print_response("Hello!")
```

**Agent runs:** Agno bakes `extra_headers` into the model instance, so the trace/span
can't vary per call on one model. Either build the model once per conversation with the
trace id in `extra_headers`, or give the model a custom OpenAI-compatible client and use
context-scoped injection (below).

---

## Agent runs: propagate traces automatically

For a whole agent/conversation you want one `X-Majordomo-Trace-Id` on every call, with
the `X-Majordomo-Span-Path` changing per step — without passing headers into each call.
Route through the gateway's OpenAI-compatible endpoint (`/v1`) and attach a single
request hook that reads the current step's headers from a context store. One hook covers
every provider; add the `X-Majordomo-Provider` translation header (`anthropic-openai`,
`gemini-openai`) to the static headers when the model isn't OpenAI.

**Pydantic AI (Python)** — `contextvars` + an httpx request hook on the provider's
`http_client`:

```python
import contextvars, os, httpx
from pydantic_ai import Agent
from pydantic_ai.models.openai import OpenAIChatModel
from pydantic_ai.providers.openai import OpenAIProvider

_run_ctx: contextvars.ContextVar[dict[str, str]] = contextvars.ContextVar("majordomo_run", default={})

async def _inject(request: httpx.Request) -> None:
    for k, v in _run_ctx.get().items():
        if v:
            request.headers[k] = v

http_client = httpx.AsyncClient(
    headers={"X-Majordomo-Key": os.environ["MAJORDOMO_API_KEY"]},
    event_hooks={"request": [_inject]},
)
model = OpenAIChatModel(
    "gpt-5-mini",
    provider=OpenAIProvider(
        base_url=f"{os.environ['MAJORDOMO_GATEWAY_URL']}/v1",
        api_key=os.environ["OPENAI_API_KEY"],
        http_client=http_client,
    ),
)
agent = Agent(model)

# Set before each step; every model call in the run inherits the headers.
# Under asyncio.gather, call _run_ctx.set(...) inside each task (contextvars are per-task).
_run_ctx.set({
    "X-Majordomo-Trace-Id": trace_id,
    "X-Majordomo-Span-Path": "planner/tool:search_db",
    "X-Majordomo-Span-Name": "summarize",
})
await agent.run("Summarize these rows: ...")
```

**Mastra (TypeScript)** — `AsyncLocalStorage` + a custom `fetch` on `createOpenAI`:

```typescript
import { AsyncLocalStorage } from 'node:async_hooks';
import { createOpenAI } from '@ai-sdk/openai';
import { Agent } from '@mastra/core/agent';

const runContext = new AsyncLocalStorage<Record<string, string>>();

const majordomoFetch = (input: RequestInfo | URL, init: RequestInit = {}): Promise<Response> => {
  const headers = new Headers(init.headers);
  headers.set('X-Majordomo-Key', process.env.MAJORDOMO_API_KEY!);
  for (const [k, v] of Object.entries(runContext.getStore() ?? {})) headers.set(k, v);
  return fetch(input, { ...init, headers });
};

const openai = createOpenAI({
  baseURL: `${process.env.MAJORDOMO_GATEWAY_URL}/v1`,
  apiKey: process.env.OPENAI_API_KEY,
  fetch: majordomoFetch,
});
const agent = new Agent({ name: 'support-copilot', instructions: '...', model: openai('gpt-5-mini') });

// Run each step inside the store; every model call inherits the headers.
await runContext.run(
  { 'X-Majordomo-Trace-Id': traceId, 'X-Majordomo-Span-Path': 'planner/tool:search_db', 'X-Majordomo-Span-Name': 'summarize' },
  () => agent.generate('Summarize these rows: ...'),
);
```

Verified against current provider APIs: `OpenAIProvider(base_url, api_key, http_client)`
and `createOpenAI({ baseURL, apiKey, fetch })`. Pydantic AI's current model class is
`OpenAIChatModel` (older versions used `OpenAIModel`).

---

## Provider routing header values

| To reach | via native format | via OpenAI-compatible translation |
|---|---|---|
| OpenAI | (default on `/v1/chat/completions`) | — |
| Anthropic | `anthropic` (native `/v1/messages`) | `anthropic-openai` |
| Gemini | `gemini` | `gemini-openai` |
| Fireworks / Together / DeepSeek | — | `fireworks` / `together` / `deepseek` |
| Moonshot (Kimi) / Baseten / Nebius | — | `moonshot` / `baseten` / `nebius` |
| DeepInfra / Novita | — | `deepinfra` / `novita` |

Set `X-Majordomo-Provider` explicitly whenever the request path can't disambiguate the
provider (i.e. anything going through the OpenAI-compatible `/v1/chat/completions` path).
