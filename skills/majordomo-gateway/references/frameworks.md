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
from pydantic_ai.models.openai import OpenAIModel
from pydantic_ai.models.anthropic import AnthropicModel
from pydantic_ai.providers.openai import OpenAIProvider
from pydantic_ai.providers.anthropic import AnthropicProvider
from pydantic_ai.settings import AnthropicModelSettings, OpenAIChatModelSettings

GATEWAY = os.environ["MAJORDOMO_GATEWAY_URL"]

def mdm_headers(**extra):
    return {"X-Majordomo-Key": os.environ["MAJORDOMO_API_KEY"], **extra}

# --- OpenAI ---
openai_model = OpenAIModel(
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
gemini_model = OpenAIModel(
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

**Agent runs:** `model_settings` is per-`run`, so put the run's `X-Majordomo-Trace-Id`
(and `X-Majordomo-Span-Path`) into `extra_headers` for that run:

```python
settings = AnthropicModelSettings(extra_headers=mdm_headers(**{
    "X-Majordomo-Trace-Id": trace_id,
    "X-Majordomo-Span-Path": "planner",
}))
await agent.run("Which tool?", model_settings=settings)
```

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

**Agent runs:** Agno bakes `extra_headers` into the model instance, so a per-run
`X-Majordomo-Trace-Id` means **building the model once per conversation** with the
trace id in `extra_headers` (rather than mutating it per call).

---

## Provider routing header values

| To reach | via native format | via OpenAI-compatible translation |
|---|---|---|
| OpenAI | (default on `/v1/chat/completions`) | — |
| Anthropic | `anthropic` (native `/v1/messages`) | `anthropic-openai` |
| Gemini | `gemini` | `gemini-openai` |
| Fireworks / Together / DeepSeek | — | `fireworks` / `together` / `deepseek` |

Set `X-Majordomo-Provider` explicitly whenever the request path can't disambiguate the
provider (i.e. anything going through the OpenAI-compatible `/v1/chat/completions` path).
