# Toko-Mo-Co

A high-performance reverse proxy for LLM APIs that adds cost tracking, caching, PII redaction, agent memory, loop detection, and a real-time dashboard — without adding latency to your requests.

Drop it between your agents and OpenAI/Anthropic/Gemini. Every request is logged, costed, and observable. No SDK changes required beyond pointing your `base_url` at the proxy.

## Why

LLM agents burn money silently. A stuck loop can rack up hundreds of dollars before anyone notices. Toko-Mo-Co sits in front of your provider APIs and gives you:

- Visibility into what every agent is spending
- Automatic detection of looping agents
- Response caching that cuts redundant API calls
- PII/secret scanning before prompts leave your network
- Persistent agent memory that learns user preferences across sessions
- A dashboard your whole team can watch

## Features

| Category | What it does |
|----------|-------------|
| **Cost Tracking** | Real-time token counting and cost calculation for 50+ models across OpenAI, Anthropic, and Gemini |
| **Response Cache** | LRU cache with SHA-256 content hashing — identical prompts return instantly from cache |
| **Loop Detection** | Levenshtein similarity detects agents stuck in loops; injects cost warnings into responses |
| **PII Redaction** | Scans prompts for emails, phone numbers, SSNs, credit cards (Luhn validated), IBANs, API keys, private keys, connection strings — 16 categories |
| **Agent Memory** | Automatically extracts and remembers user preferences, project context, and technical facts across sessions — with conflict resolution, recency-weighted retrieval, and per-agent eviction quotas |
| **Rules Engine** | Route models, set rate limits, block patterns, override parameters — all configurable via UI or API |
| **Retry & Fallback** | Exponential backoff with jitter; automatic failover to alternate providers on error |
| **API Key Auth** | Issue scoped proxy keys (SHA-256 hashed, constant-time validated) to control access |
| **Custom Providers** | Route to Ollama, vLLM, or any OpenAI-compatible local endpoint with prefix-based model routing |
| **Live Dashboard** | WebSocket-powered real-time feed, cost charts, session tracking, and settings management |
| **Zero Latency** | Streaming pass-through — the proxy adds no measurable latency to responses |

## Quick Start

```bash
# Build
go build -o tokomoco .

# Set at least one provider key
export OPENAI_API_KEY=sk-...
# export ANTHROPIC_API_KEY=sk-ant-...
# export GEMINI_API_KEY=AI...

# Run
./tokomoco
```

Open **http://localhost:8081** for the dashboard.

### Keep it running with the watchdog

A watchdog script is included that monitors port 8081 and automatically restarts the proxy if it goes down:

```bash
# Start the watchdog in the background
./watchdog.sh &

# Logs are written to watchdog.log
tail -f watchdog.log
```

The watchdog checks every 5 seconds. On crash it relaunches `./tokomoco` and appends to `tokomoco.log`.

> **After a code update or `go build`**, the watchdog will pick up the new binary automatically on the next restart cycle (within 5 seconds). No manual intervention needed.

### Docker

```bash
docker build -t tokomoco .

docker run -d \
  -p 8081:8081 \
  -e OPENAI_API_KEY=sk-... \
  -v proxy-data:/app/data \
  tokomoco
```

## Usage

Point your LLM client's base URL at the proxy. No other code changes needed.

```python
# OpenAI
from openai import OpenAI
client = OpenAI(
    base_url="http://localhost:8081/v1",
    default_headers={"X-Session-ID": "my-agent", "X-Agent-ID": "researcher"},
)

# Anthropic
from anthropic import Anthropic
client = Anthropic(
    base_url="http://localhost:8081",
    default_headers={"X-Session-ID": "my-agent"},
)
```

```typescript
// OpenAI (TypeScript)
import OpenAI from "openai";
const client = new OpenAI({
  baseURL: "http://localhost:8081/v1",
  defaultHeaders: { "X-Session-ID": "my-agent" },
});
```

```bash
# curl
curl http://localhost:8081/v1/chat/completions \
  -H "Authorization: Bearer $OPENAI_API_KEY" \
  -H "X-Session-ID: my-agent" \
  -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"Hello"}]}'
```

See **[INTEGRATION.md](INTEGRATION.md)** for complete examples including Python, TypeScript, LangChain, curl, and async patterns.

## Architecture

```
┌─────────────┐         ┌──────────────────────────────────────┐         ┌───────────┐
│  Your Agent │ ──────> │            Toko-Mo-Co                 │ ──────> │  OpenAI   │
│  or CLI     │         │                                      │         │  Anthropic│
│             │ <────── │  Auth > PII Scan > Cache > Rules     │ <────── │  Gemini   │
└─────────────┘         │  > Retry/Fallback > Cost Track       │         │  Ollama   │
                        └──────────────────────────────────────┘         └───────────┘
                                         │
                                    WebSocket
                                         │
                                  ┌──────────────┐
                                  │  Dashboard   │
                                  │  (built-in)  │
                                  └──────────────┘
```

**Request flow:** Auth check → PII redaction → Cache lookup → Memory injection → Rules evaluation → Proxy to provider → Retry/fallback on error → Fact extraction → Cost calculation → WebSocket broadcast → SQLite persistence

## API Endpoints

### Proxy (drop-in replacement)

| Endpoint | Provider |
|----------|----------|
| `POST /v1/chat/completions` | OpenAI |
| `POST /v1/messages` | Anthropic |
| `POST /v1beta/models/{model}:generateContent` | Gemini |
| `POST /v1beta/models/{model}:streamGenerateContent` | Gemini |

### Management

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/api/settings` | GET / PUT | Runtime configuration |
| `/api/cache` | GET | Cache statistics |
| `/api/cache/flush` | POST | Clear the response cache |
| `/api/analytics/savings` | GET | Cost savings breakdown |
| `/api/security/pii` | GET | PII redaction statistics |
| `/api/memories` | GET/POST | Memory stats and manual fact storage |
| `/api/memories/list` | GET | List stored memories |
| `/api/memories/search` | POST | Semantic memory search |
| `/api/memories/flush` | POST | Clear all memories |
| `/api/memories/agents` | GET | Per-agent memory breakdown |
| `/api/memories/top` | GET | Most-accessed memories |
| `/api/fallback-configs` | CRUD | Fallback chain management |
| `/api/rules` | CRUD | Rules engine management |
| `/api/keys` | CRUD | API key management |
| `/ws` | WebSocket | Real-time request feed |
| `/health` | GET | Health check |

## Configuration

The proxy works out of the box with zero configuration. Everything can be tuned via:

1. **Dashboard UI** — Settings page with live updates
2. **Config file** — `config.json` (optional)
3. **Environment variables** — `CONFIG_PORT`, `CONFIG_CACHE_ENABLED`, etc.

See **[USER_GUIDE.md](USER_GUIDE.md)** for the full configuration reference.

### Key Settings

| Setting | Default | Description |
|---------|---------|-------------|
| `port` | 8081 | HTTP listen port |
| `cache_enabled` | true | Response cache on/off |
| `cache_ttl_minutes` | 60 | Cache entry lifetime |
| `pii_enabled` | false | PII/secret redaction |
| `pii_mode` | redact | `redact`, `hash`, or `placeholder` |
| `auth_enabled` | false | Require API keys for proxy endpoints |
| `retry_enabled` | true | Automatic retries on provider errors |
| `retry_max_attempts` | 3 | Max retry count |
| `memory_enabled` | false | Agent memory extraction and injection |
| `memory_threshold` | 0.7 | Similarity threshold for memory retrieval |
| `memory_recency_lambda` | 0.01 | Time decay rate for recency-weighted scoring |
| `memory_conflict_threshold` | 0.85 | Similarity threshold for conflict detection |
| `memory_ttl_days` | 90 | Days before unused memories are eviction candidates |
| `loop_threshold` | 3 | Similar prompts before warning |
| `loop_similarity` | 0.8 | Levenshtein similarity threshold (0-1) |

## Project Structure

```
scrollypedia/
├── main.go                  # Entry point, routing, startup
├── auth/                    # API key authentication middleware
├── cache/                   # LRU response cache + SQLite persistence
├── config/                  # Configuration loading + runtime settings API
├── dashboard/               # HTML/JS/CSS frontend + WebSocket hub
├── detector/                # Loop detection (Levenshtein similarity)
├── injector/                # Warning injection into LLM responses
├── memory/                  # Agent memory — fact extraction, semantic search, conflict resolution
├── providers/               # Custom provider management (Ollama, vLLM)
├── proxy/                   # Core reverse proxy handler
├── redactor/                # PII & secret redaction engine (16 categories)
├── reliability/             # Retry with exponential backoff & fallback
├── rules/                   # Rules engine (model routing, rate limits)
├── store/                   # SQLite database layer
├── tracker/                 # Session tracking, token counting, cost calc
├── examples/                # Client examples (Python, TypeScript, curl)
├── docs/                    # Additional documentation
├── Dockerfile               # Multi-stage production build
├── QUICKSTART.md            # 2-minute setup guide
├── USER_GUIDE.md            # Complete reference
└── INTEGRATION.md           # Client library examples
```

## Testing

```bash
# Run all tests
go test ./...

# With race detector
go test -race ./...

# Verbose output
go test -v ./...
```

| Package | Tests | What's covered |
|---------|-------|----------------|
| auth | 18 | Key generation, hashing, middleware, header extraction |
| cache | 18 | Hash determinism, LRU eviction/promotion, TTL, stats |
| config | 20 | Defaults, file loading, env overrides, validation |
| detector | 17 | Similarity, request store, loop detection, severity |
| memory | 40 | Fact extraction, search, conflict resolution, eviction, recency scoring |
| proxy | 7 | Usage extraction (OpenAI, Anthropic, Gemini, streaming) |
| redactor | 21 | All 16 PII categories, Luhn/IBAN validators, request body |
| rules | 48+ | Conditions, actions, priorities, engine lifecycle |
| store | 6 | Schema creation, persistence |
| tracker | 23 | Cost calculation, prefix matching, cached tokens |

## Documentation

| Document | Description |
|----------|-------------|
| [QUICKSTART.md](QUICKSTART.md) | 2-minute setup guide |
| [USER_GUIDE.md](USER_GUIDE.md) | Complete reference — all features, API, troubleshooting |
| [INTEGRATION.md](INTEGRATION.md) | Client examples — Python, TypeScript, LangChain, curl |
| [RULES_ENGINE.md](RULES_ENGINE.md) | Rules engine design and API reference |
| [CONTRIBUTING.md](CONTRIBUTING.md) | How to contribute |

## Tech Stack

- **Go 1.24** — single binary, no runtime dependencies
- **SQLite** (WAL mode) — zero-config embedded persistence
- **WebSocket** — real-time dashboard updates
- **Chart.js** — cost visualization (loaded via CDN)

## License

MIT — see [LICENSE.md](LICENSE.md)

## Contributing

Issues and PRs welcome. See [CONTRIBUTING.md](CONTRIBUTING.md) for development setup and guidelines.

---

Built by [Scrollypedia](https://scrollypedia.com)
