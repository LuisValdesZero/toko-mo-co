# Toko-Mo-Co

One gateway between your apps and OpenAI, Anthropic, Google Gemini — and your own self-hosted models.

## What It Does

You point your apps at the proxy instead of the provider. Every LLM call flows through it. You get:

- **See what you're spending** — real-time dashboard showing costs per app, per model, per day
- **Stop paying for duplicate calls** — response cache serves repeated questions for $0 in ~2ms
- **Stay up when providers go down** — automatic retries + fallback to a different provider
- **Set limits** — budget caps, rate limits, model overrides — no code changes needed
- **Catch runaway agents** — loop detection flags agents stuck sending the same prompt
- **Bring your own models** — connect Ollama, vLLM, or any OpenAI-compatible endpoint via Custom Providers

## Setup (2 minutes)

### 1. Start the Proxy

**Docker (recommended):**

```bash
docker run -d \
  -p 8080:8080 \
  -e OPENAI_API_KEY=sk-... \
  -v proxy-data:/app/data \
  tokomoco:latest
```

**Local binary:**

```bash
./tokomoco
```

**From source:**

```bash
go run .
```

Open `http://localhost:8080` (Docker) or `http://localhost:8081` (local) for the dashboard.

### 2. Connect Your App

Change one line — the base URL:

```python
# OpenAI
client = OpenAI(base_url="http://localhost:8080/v1")

# Anthropic
client = Anthropic(base_url="http://localhost:8080")
```

Everything else stays the same. Your app doesn't know it's going through a proxy.

## Custom Providers (Self-Hosted Models)

Connect your own LLM endpoints — Ollama, vLLM, LiteLLM, or any OpenAI-compatible API.

**1. Register in the dashboard:** Go to Settings → Custom Providers → Add Provider.

**2. Fill in the details:**

| Field | Example |
|-------|---------|
| Name | `ollama` |
| Base URL | `http://localhost:11434` |
| API Format | OpenAI-compatible |
| API Path | `/v1/chat/completions` |

**3. Use it with a prefix:** Add the provider name before the model:

```python
client = OpenAI(base_url="http://localhost:8081/v1")
response = client.chat.completions.create(
    model="ollama/llama3.2",       # provider-name/model-name
    messages=[{"role": "user", "content": "Hello"}]
)
```

The proxy strips the prefix, routes to your endpoint, and tracks costs just like any other provider.

## What's Included

| Feature | What it does |
|---------|-------------|
| **Dashboard** | Live request feed, per-agent costs, spending chart |
| **Response Cache** | Same question twice? Served from cache, $0 cost, 2ms latency |
| **Retry + Fallback** | Provider down? Auto-retry, then fall back to another provider |
| **Rules Engine** | Block requests, enforce budgets, swap models, inject prompts |
| **Loop Detection** | Flags agents sending the same prompt on repeat |
| **API Key Auth** | Control who can use the proxy |
| **Custom Providers** | Connect Ollama, vLLM, or any OpenAI-compatible endpoint |
| **Model Validation** | Unknown models are rejected immediately with a clear error |
| **Cost Savings** | Dashboard shows exactly how much money the proxy saved you |

## Supported Models (50+ with built-in pricing)

**OpenAI**
- GPT-5.2, GPT-5.2 Pro, GPT-5 Mini
- GPT-4o, GPT-4o Mini
- o4-mini, o3, o3-mini, o1, o1-mini

**Anthropic**
- Claude Opus 4.6, 4.5, 4.1, 4
- Claude Sonnet 4.5, 4, 3.7, 3.5
- Claude Haiku 4.5, 3.5, 3

**Google Gemini**
- Gemini 3 Pro, 3 Flash (preview)
- Gemini 2.5 Pro, 2.5 Flash
- Gemini 2.0 Flash

**Custom:** Any model from a registered Custom Provider (Ollama, vLLM, etc.)

Works with streaming and non-streaming requests. Custom models can be added via Settings → Custom Providers.

## How It Saves Money

Three ways, tracked automatically:

1. **Caching** — identical requests (same model, same prompt, temperature=0) return cached responses instead of calling the provider
2. **Rules** — block requests that exceed daily/monthly budgets before they cost anything
3. **Model overrides** — silently route simple questions to cheaper models

The dashboard shows a running total of savings with a breakdown by feature.
