# Toko-Mo-Co — User Guide

Toko-Mo-Co sits between your apps and the LLM providers (OpenAI, Anthropic, Google Gemini). Every request flows through it, giving you one place to see costs, enforce rules, cache responses, build persistent agent memory, and handle failures — without changing any application code.

## Why Use It

If you're running LLM-powered apps in production, you've probably run into these problems:

- **No idea what you're spending.** Each app calls the API directly, costs are scattered across provider dashboards.
- **No control.** A runaway agent can burn through your budget in minutes. There's no way to set limits or swap models without changing code.
- **No reliability.** When OpenAI goes down, your app goes down. No fallback, no retries.
- **Duplicate calls.** The same question gets asked hundreds of times, and you pay for each one.

Toko-Mo-Co fixes all of these by acting as a single gateway.

---

## Quick Start

### Run Locally

```bash
./tokomoco
```

Dashboard opens at `http://localhost:8081/`. Point your apps at the proxy instead of the provider directly.

### Run with Docker

```bash
docker run -d \
  -p 8080:8080 \
  -v proxy-data:/app/data \
  tokomoco:latest
```

The `-v proxy-data:/app/data` flag keeps your database across container restarts.

### Provider API Keys

Set your provider keys as environment variables. The proxy forwards them to the upstream provider:

```bash
# Pass provider keys when running locally
OPENAI_API_KEY=sk-... ANTHROPIC_API_KEY=sk-ant-... GEMINI_API_KEY=AI... ./tokomoco

# Or with Docker
docker run -d \
  -e OPENAI_API_KEY=sk-... \
  -e ANTHROPIC_API_KEY=sk-ant-... \
  -e GEMINI_API_KEY=AI... \
  -p 8080:8080 -v proxy-data:/app/data \
  tokomoco:latest
```

---

## Connecting Your Apps

Point your app at the proxy instead of the provider. No SDK changes needed — just swap the base URL.

### OpenAI / GPT models

```python
# Before
client = OpenAI()

# After — just add base_url
client = OpenAI(base_url="http://localhost:8081/v1")
```

Endpoint: `POST /v1/chat/completions`

### Anthropic / Claude models

```python
# Before
client = Anthropic()

# After
client = Anthropic(base_url="http://localhost:8081")
```

Endpoint: `POST /v1/messages`

### Google Gemini

Endpoint: `POST /v1beta/models/{model}:generateContent`

### curl Example

```bash
curl -X POST http://localhost:8081/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model": "gpt-4o-mini", "messages": [{"role": "user", "content": "Hello"}]}'
```

### Agent Identification

Add these headers so the proxy can track which agent, session, and app is making requests:

| Header | Required | Description |
|--------|----------|-------------|
| `X-Agent-ID` | Recommended | Identifies the specific agent or component making the call (e.g. `summarizer`, `code-reviewer`) |
| `X-Session-ID` | Recommended | Groups related requests into a session. All requests with the same session ID appear together in the Sessions view |
| `X-App-Name` | Optional | The application or product name (e.g. `customer-support-bot`). Useful when multiple apps share the proxy |

```python
# Python (OpenAI SDK)
response = client.chat.completions.create(
    model="gpt-4o",
    messages=[...],
    extra_headers={
        "X-Agent-ID": "summarizer",
        "X-Session-ID": "job-abc-123",
        "X-App-Name": "doc-processor",
    }
)

# Python (Anthropic SDK)
response = client.messages.create(
    model="claude-sonnet-4-5-20250929",
    messages=[...],
    extra_headers={
        "X-Agent-ID": "analyst",
        "X-Session-ID": "scan-2026-02-23",
        "X-App-Name": "trading-system",
    }
)
```

If `X-Agent-ID` is not set, the proxy infers one from the User-Agent header (e.g. `python-requests`, `curl`). If `X-App-Name` is not set, it defaults to the agent ID.

**Multi-agent sessions.** When several agents share the same `X-Session-ID`, the proxy tracks all of them. The Sessions view shows every agent that participated and aggregates the total cost and tokens across the group. This is the typical pattern for orchestrator apps that fan out to specialist agents:

```
Orchestrator
  ├─ MarketAnalyst   → X-Session-ID: scan-001, X-Agent-ID: MarketAnalyst
  ├─ SentimentBot    → X-Session-ID: scan-001, X-Agent-ID: SentimentBot
  └─ DecisionMaker   → X-Session-ID: scan-001, X-Agent-ID: DecisionMaker
```

The dashboard session row will show all three agent names. Each individual request is still tracked and filterable by its own agent ID. Rules, memory, and feedback all operate at the per-request agent level, not the session level.

**Avoid commas in agent IDs.** Commas are used internally as a delimiter in session grouping. An agent ID like `Agent,v2` will be stored as `Agent_v2` in the session view (the original value is preserved in individual request records).

---

## Features

### 1. Real-Time Dashboard

Open the dashboard at `/` to see:

- **Live request feed** — every LLM call as it happens, with model, tokens, cost, and latency
- **Per-agent breakdown** — which apps are spending the most
- **Cost chart** — spending over time
- **Cost savings card** — total money saved by caching, rules, and fallback (updates every 30 seconds)

All data is stored in SQLite and survives restarts.

### 2. Response Cache

**What it does:** When the same question is asked twice, the proxy returns the cached answer instead of calling the provider again. You pay nothing for the second request.

**How it works:**
- The proxy hashes the request (provider, model, messages, temperature) using SHA-256
- If a matching response exists in the cache and hasn't expired, it's returned immediately
- Cache hits include `X-Cache: HIT` in the response headers; misses show `X-Cache: MISS`
- Only non-streaming requests with `temperature=0` are cached by default (deterministic responses only)

**What you save:** In testing, cached responses return in ~2ms vs 200-900ms from the provider. Cost per cached response: $0.

**Settings (all changeable at runtime):**

| Setting | Default | Description |
|---------|---------|-------------|
| Cache enabled | On | Toggle caching on/off |
| Max entries | 1,000 | How many responses to keep in memory |
| TTL | 60 minutes | How long before a cached response expires |
| Only temp=0 | On | Only cache deterministic requests |

**Opt-out per request:** Add `X-Cache-Control: no-cache` header to skip the cache for a specific call.

### 3. Retry and Fallback

**Retries:** When a provider returns a 5xx error or times out, the proxy automatically retries with exponential backoff. Default: 3 attempts, starting at 1 second delay.

**Fallback:** When retries are exhausted, the proxy can route the request to a different provider entirely. For example, if Claude is down, fall back to GPT-4o.

**Fallback strategies:**

| Strategy | Behavior |
|----------|----------|
| `same_tier` | Only fall back to models at a similar price point (default) |
| `cheaper` | Fall back to same tier or cheaper models |
| `any` | Fall back to any available model |

**Default fallback mappings** (pre-configured):

- GPT-4o-mini fails → Claude Haiku → Gemini Flash
- GPT-4o fails → Claude Sonnet → Gemini Flash
- Claude Sonnet fails → GPT-4o → Gemini Flash
- Claude Haiku fails → GPT-4o-mini → Gemini Flash

You can customize these in Settings → Fallback Configuration. Each mapping shows how many times it has been triggered.

### 4. Rules Engine

Rules let you control requests before they reach the provider. They're evaluated in priority order — the first match wins.

**Actions:**

| Action | What it does | Use case |
|--------|-------------|----------|
| **Block** | Rejects the request with a custom error | Budget caps, content filtering |
| **Rate Limit** | Throttles requests per time window | Preventing runaway agents |
| **Override Model** | Silently swaps to a different model | Cost optimization |
| **Inject Prompt** | Adds a system instruction the user doesn't see | Compliance, safety guardrails |
| **Redirect** | Routes to a different provider endpoint | A/B testing, self-hosted models |

**Example: Daily spend cap**
```
Name:       Daily $50 Cap
Priority:   100
Conditions: Daily Cost >= 50.00
Action:     Block (402, "Daily spending limit reached.")
```

**Example: Downgrade expensive models for simple questions**
```
Name:       Auto-Downgrade Short Queries
Priority:   50
Conditions: Input Tokens < 200 AND Model = claude-opus-4-6
Action:     Override Model → claude-haiku-4-5
```

**Example: Add compliance disclaimer**
```
Name:       Medical Disclaimer
Scope:      health-bot
Action:     Inject Prompt
Prompt:     "You MUST end every response with: 'Note: This is not medical
            advice. Consult a qualified healthcare provider.'"
```

**Priority guide:**

| Range | Use |
|-------|-----|
| 200+ | Security (block injection attempts, content filtering) |
| 100-199 | Budget and rate limits |
| 50-99 | Model overrides |
| 10-49 | Prompt injection (disclaimers, tone) |

### 5. Loop Detection

Detects when an agent is sending the same (or very similar) prompt repeatedly — a common sign of an agent stuck in a loop.

**How it works:** The proxy compares each prompt against recent prompts in the same session using text similarity. If 3+ similar prompts appear within 5 minutes, a loop is flagged.

**What happens:** Loop events appear in the request feed. You can combine this with a rule to automatically block looping agents:

```
Conditions: Loop Detected
Action:     Block (429, "Repetitive request pattern detected.")
```

**Settings:**

| Setting | Default | Description |
|---------|---------|-------------|
| Threshold | 3 | How many similar prompts before flagging |
| Similarity | 0.8 | How similar prompts need to be (0.0 to 1.0) |
| Window | 5 minutes | Time window for comparison |

### 6. API Key Authentication

When enabled, all proxy requests require an API key. This lets you control who can use the proxy.

**Key format:** Keys start with `tc_` (e.g., `tc_a1b2c3d4...`). The raw key is shown once at creation — only the hash is stored.

**How to use:** Add the key to your requests via any of these methods:

```bash
# Recommended — dedicated header (won't collide with provider keys)
curl -H "X-Proxy-Key: tc_your_key_here" ...

# Also works
curl -H "Authorization: Bearer tc_your_key_here" ...
```

**Management:** Create, revoke, and toggle keys from Settings → API Key Authentication. Keys can have expiry dates.

### 7. Custom Providers (Self-Hosted Models)

Connect your own LLM endpoints — Ollama, vLLM, LiteLLM, DeepSeek, Mistral, Groq, or any OpenAI-compatible API. Custom providers appear in the dashboard, rules engine, fallback editor, and pricing settings just like the built-in providers.

**How it works:**

1. Register a provider in Settings → Custom Providers with a name and base URL
2. Prefix your model name with the provider name when sending requests: `provider-name/model-name`
3. The proxy strips the prefix, routes to your endpoint, and tracks everything

**Setting up a provider:**

Go to Settings → Custom Providers → Add Provider and fill in:

| Field | Description | Example |
|-------|-------------|---------|
| **Name** | Routing prefix (lowercase, alphanumeric + hyphens) | `ollama` |
| **Display Name** | Friendly label for the dashboard | `Ollama (Local)` |
| **Base URL** | Where the provider is running | `http://localhost:11434` |
| **API Format** | Protocol the provider speaks | `openai` or `anthropic` |
| **API Path** | Chat completions endpoint path | `/v1/chat/completions` |
| **API Key** | Auth token if required (optional) | `sk-...` |
| **Env Var** | Or read the key from an environment variable | `OLLAMA_API_KEY` |

**Connection test:** Click "Test Connection" to verify the URL is reachable. If the provider exposes a model list endpoint (`/v1/models` or Ollama's `/api/tags`), the proxy auto-discovers available models and fills them in.

**Using it from your app:**

```python
from openai import OpenAI

client = OpenAI(base_url="http://localhost:8081/v1", api_key="not-needed")
response = client.chat.completions.create(
    model="ollama/llama3.2",   # provider-name/model-name
    messages=[{"role": "user", "content": "What is 2+2?"}]
)
```

```bash
curl -X POST http://localhost:8081/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model": "ollama/llama3.2", "messages": [{"role": "user", "content": "Hello"}]}'
```

**How prefix routing works:**

- Request comes in with model `ollama/llama3.2`
- Proxy splits on `/` → prefix = `ollama`
- Checks if `ollama` is a registered, enabled custom provider → **yes**
- Strips the prefix, rewrites model to `llama3.2`
- Forwards to the provider's base URL + API path
- If the prefix doesn't match any registered provider, the model string passes through unchanged

**Model validation:** If you send a model that doesn't match any built-in model or custom provider (e.g., `llama3.2` without a prefix, or `deepseek/coder` without registering "deepseek"), the proxy returns a clear error immediately instead of forwarding a doomed request:

```json
{
  "error": {
    "message": "Unknown model \"llama3.2\". Register a custom provider in Settings → Custom Providers and prefix the model name (e.g., provider-name/model-name).",
    "type": "invalid_request_error",
    "code": "model_not_found"
  }
}
```

**Auth handling:** The proxy replaces the client's Authorization header with the provider's configured API key (or env var). If no auth is configured (common for local Ollama), the auth header is removed entirely.

**Enabling and disabling:** Toggle a provider on/off without deleting it. Disabled providers are ignored during routing.

### 8. Cost Tracking and Savings

The proxy tracks the cost of every request using built-in pricing tables for 50+ models across OpenAI, Anthropic, and Google.

**Cost savings are tracked from four sources:**

| Source | How it saves money |
|--------|-------------------|
| **Response Cache** | Identical requests served from cache at $0 |
| **Rules (blocked)** | Requests blocked by budget rules don't reach the provider |
| **Fallback** | Failed requests recovered by cheaper fallback models |
| **Memory** | Agents get context without re-asking — fewer round-trips, better first-try responses |

The dashboard shows a savings card with the total and per-feature breakdown. It updates every 30 seconds.

You can also customize model pricing in Settings → Model Pricing if your negotiated rates differ from the defaults.

### 9. Agent Memory

**What it does:** The proxy learns from your agents' conversations. It automatically extracts user preferences, project context, and technical facts from requests and responses, then injects relevant memories into future requests as system-prompt context. Your agents get smarter over time — without any code changes.

**How it works:**

1. **Extraction:** After each request, the proxy scans user messages and assistant responses for memorable facts — preferences ("I prefer Go"), technical context ("Our app runs on Kubernetes"), and project details ("We use PostgreSQL 15")
2. **Storage:** Facts are embedded as vectors (using the same embedding infrastructure as the semantic cache) and stored in SQLite with per-agent scoping
3. **Retrieval:** Before each request, the proxy searches for relevant memories based on the current prompt and injects them as a system-prompt block
4. **Conflict resolution:** When a user's preference changes ("now prefers Python" → "switched to Go"), the old fact is automatically replaced instead of accumulating duplicates

**What gets extracted:**
- User preferences ("I prefer dark mode", "Use TypeScript for frontend")
- Technical stack details ("We deploy on AWS", "Running Go 1.24")
- Project context ("Our database is PostgreSQL", "API uses REST")
- Team workflows ("We use trunk-based development")

**What gets ignored:**
- Questions ("How do I deploy?")
- Trivial messages ("Hello", "Thanks")
- System prompts (only user and assistant messages are scanned)

**Intelligent memory management:**

| Feature | What it does |
|---------|-------------|
| **Recency-weighted scoring** | Recent memories rank higher in search results. A 30-day-old memory retains ~74% weight; 90-day-old ~41%. Configurable via the decay lambda parameter |
| **Conflict resolution** | Detects when a new fact contradicts an existing one (similarity 0.85–0.95) using keyword signals like "now", "switched to", "no longer". The old fact is replaced in-place |
| **Per-agent eviction quotas** | Each agent gets a fair share of memory slots (`max_entries / num_agents`, minimum 100). One chatty agent can't monopolize all the memory |
| **TTL-based eviction** | Memories not accessed within the TTL window (default: 90 days) are evicted first when space is needed |
| **Access tracking** | Every search hit increments an access counter and updates the last-accessed timestamp. Frequently-used memories are protected from eviction |

**Settings (all changeable at runtime):**

| Setting | Default | Description |
|---------|---------|-------------|
| Memory enabled | Off | Toggle memory extraction and injection |
| Max entries | 10,000 | Maximum stored facts across all agents |
| Similarity threshold | 0.7 | How similar a memory must be to the current query to be injected (0–1) |
| Max results | 5 | Maximum memories injected per request |
| Recency decay (lambda) | 0.01 | Time decay rate for recency-weighted scoring. Higher = more aggressive decay (0–0.1) |
| Conflict threshold | 0.85 | Similarity threshold for conflict detection. Facts above this are checked for replacement signals (0.5–0.99) |
| TTL (days) | 90 | Days of inactivity before a memory becomes an eviction candidate (7–365) |

**Dashboard observability:**

- **Savings card** shows memory hit count, total lookups, and hit rate alongside cache and rules savings
- **Settings page** shows a stats bar with total memories, hit rate, number of agents, and stale memory count
- **Flush button** clears all stored memories with one click

**API endpoints:**

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/api/memories` | GET | Memory stats (total, lookups, hits, hit rate, per-agent breakdown) |
| `/api/memories` | POST | Manually store a fact |
| `/api/memories/list` | GET | List stored memories with access metadata |
| `/api/memories/search` | POST | Semantic search across memories |
| `/api/memories/flush` | POST | Clear all stored memories |
| `/api/memories/agents` | GET | Per-agent memory breakdown (counts, access totals, last activity) |
| `/api/memories/top` | GET | Top 10 most-accessed memories |
| `/api/memories/{id}` | DELETE | Delete a specific memory |
| `/api/memories/agent/{agent_id}` | DELETE | Delete all memories for a specific agent |

**Requirements:** Agent Memory requires an embedding provider. It reuses the same embedder as the semantic cache (configured via `EMBEDDING_PROVIDER` and `EMBEDDING_API_KEY` environment variables). If no embedding provider is configured, memory is silently disabled.

---

## Settings

All settings are accessible from the Settings page (`/settings`). Most can be changed at runtime without restarting the proxy.

**Runtime settings** (take effect immediately):
- Retry configuration (enabled, max attempts, delays)
- Fallback configuration (enabled, strategy, model mappings)
- Loop detection (threshold, similarity, window)
- Response cache (enabled, max entries, TTL, temp-0 only)
- Agent memory (enabled, thresholds, recency decay, TTL, conflict detection)
- Prompt injection mode

**Restart-required settings** (set via environment variables or config.json):
- Port, database path, CORS origins
- API key auth (enabled/disabled)
- Rules engine (enabled/disabled)
- Upstream timeout

### Config File

Create a `config.json` in the working directory to override defaults:

```json
{
  "port": "8081",
  "db_path": "proxy.db",
  "db_keep_days": 30,
  "upstream_timeout_sec": 300,
  "auth_enabled": false,
  "rules_enabled": true,
  "cache_enabled": true,
  "cache_ttl_minutes": 60,
  "retry_enabled": true,
  "retry_max_attempts": 3,
  "fallback_enabled": false,
  "fallback_strategy": "same_tier",
  "memory_enabled": false,
  "memory_max_entries": 10000,
  "memory_threshold": 0.7,
  "memory_recency_lambda": 0.01,
  "memory_conflict_threshold": 0.85,
  "memory_ttl_days": 90
}
```

Environment variables override config.json. Use the `CONFIG_` prefix:

```bash
CONFIG_CACHE_TTL_MINUTES=120 CONFIG_RETRY_MAX_ATTEMPTS=5 ./tokomoco
```

---

## API Reference

**Proxy endpoints:**

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/v1/chat/completions` | POST | OpenAI proxy |
| `/v1/messages` | POST | Anthropic proxy |
| `/v1beta/models/{model}:generateContent` | POST | Gemini proxy |

**Dashboard & management** (always accessible):

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/health` | GET | Health check |
| `/api/settings` | GET/PUT | Runtime settings |
| `/api/cache` | GET | Cache statistics |
| `/api/cache/flush` | POST | Clear all cached responses |
| `/api/analytics/savings` | GET | Cost savings breakdown (includes memory stats) |
| `/api/memories` | GET | Memory stats (total, lookups, hits, hit rate) |
| `/api/memories` | POST | Manually store a memory fact |
| `/api/memories/list` | GET | List stored memories with access metadata |
| `/api/memories/search` | POST | Semantic search across memories |
| `/api/memories/flush` | POST | Clear all stored memories |
| `/api/memories/agents` | GET | Per-agent memory breakdown |
| `/api/memories/top` | GET | Top 10 most-accessed memories |
| `/api/memories/{id}` | DELETE | Delete a specific memory |
| `/api/memories/agent/{id}` | DELETE | Delete all memories for an agent |
| `/api/sessions` | GET | List sessions (supports `?app_name=`, `?limit=`, `?offset=`) |
| `/api/sessions/{id}/requests` | GET | All requests in a session with aggregate stats |
| `/api/feedback` | POST | Record outcome feedback for a request or session |
| `/api/feedback` | GET | Query feedback (supports `?agent_id=`, `?outcome=`, `?from=`, `?to=`) |
| `/api/timeline/{agent_id}` | GET | Request timeline for an agent |
| `/api/analytics/agents/{id}/trends` | GET | Cost and usage trends for an agent |
| `/api/prompts/{agent_id}/history` | GET | System prompt version history |
| `/api/prompts/{agent_id}/diff/{a}/{b}` | GET | Diff between two prompt versions |
| `/api/keys` | GET/POST | API key management |
| `/api/keys/{id}` | DELETE | Revoke an API key |
| `/api/fallback-configs` | GET/POST | Fallback mappings |
| `/api/rules` | GET/POST | Rules management |
| `/api/rules/templates` | GET | Built-in rule templates by category |
| `/api/rules/from-template` | POST | Create a rule from a template |
| `/api/providers` | GET/POST | Custom provider management |
| `/api/providers/{id}` | GET/PUT/DELETE | Custom provider CRUD |
| `/api/providers/{id}/toggle` | POST | Enable/disable a provider |
| `/api/providers/{id}/test` | POST | Test connection to a saved provider |
| `/api/providers/test` | POST | Test connection (unsaved, from editor) |
| `/api/pricing` | GET/POST | Model pricing entries |
| `/api/pricing/{id}` | PUT/DELETE | Update/delete pricing entry |
| `/api/pricing/unknown` | GET | List unknown models detected |
| `/ws` | WebSocket | Live dashboard events |

---

## Troubleshooting

### Proxy returns 400 "model_not_found"
The model you sent isn't recognized. This happens when:
- The model name has a typo (e.g., `gpt-4o-mni` instead of `gpt-4o-mini`)
- You're using a self-hosted model without registering a custom provider. Register it in Settings → Custom Providers, then prefix the model name: `provider-name/model-name`
- You're using a provider prefix (e.g., `ollama/llama3.2`) but the provider isn't registered or is disabled

### Proxy returns 401
API key authentication is enabled and your request is missing a valid key. Add `X-Proxy-Key: tc_...` header.

### Requests are slow
Check if caching is enabled in Settings. For repeated queries with `temperature=0`, caching can cut response times from 500ms+ to ~2ms.

### Provider returns errors but fallback doesn't kick in
Fallback is disabled by default. Enable it in Settings → Fallback Configuration and make sure fallback mappings exist for your models.

### Custom provider returns errors
- Verify the base URL is correct and the provider is running — use "Test Connection" in the provider editor
- Check the API path — Ollama uses `/v1/chat/completions` (with the OpenAI compatibility layer) or `/api/chat`
- If auth is required, make sure the API key or env var is set correctly
- Check the API format matches what the provider expects (`openai` for most, `anthropic` for Anthropic-compatible)

### Rule doesn't fire
- Check that the rule is enabled (green badge)
- Verify the scope matches the agent ID (check the Live Feed for actual agent names)
- Higher-priority rules match first — a rule at priority 200 blocks before one at priority 50

### Memory is enabled but no facts are being stored
- Make sure an embedding provider is configured (`EMBEDDING_PROVIDER` and `EMBEDDING_API_KEY` environment variables). Without embeddings, memory is silently disabled
- Check that requests contain user messages with meaningful content — trivial messages ("hello", "thanks") and pure questions ("how do I...?") are intentionally skipped
- The proxy only extracts facts from user and assistant messages, not system prompts

### Memory shows stale entries
Stale memories are those not accessed within the configured TTL (default: 90 days). They're evicted automatically when space is needed. You can reduce the TTL in Settings → Agent Memory to evict stale entries sooner, or click "Flush Memories" to clear everything.

### Old preferences aren't being updated
The conflict resolution system detects updates when new facts contain signals like "now prefers", "switched to", "no longer", or "changed to". If a preference changed but doesn't include these keywords, the old fact may persist alongside the new one. You can manually delete outdated memories via the API (`DELETE /api/memories/{id}`).

### Dashboard shows no data
Make sure your apps are sending requests through the proxy, not directly to the provider. Check that the base URL points to the proxy.
