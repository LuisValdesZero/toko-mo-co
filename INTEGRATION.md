# Integration Guide

Route your LLM API calls through the proxy to get real-time cost tracking, loop detection, and request monitoring. Integration requires **only 2 changes** to your existing code:

1. Set `base_url` to the proxy (`http://localhost:8081`)
2. Optionally add tracking headers (`X-Session-ID`, `X-Agent-ID`)

The proxy forwards your API key and all headers transparently to the upstream provider. No registration, no accounts, no config files needed.

---

## Quick Start

### Python — Anthropic

```python
from anthropic import Anthropic

client = Anthropic(
    api_key="your-api-key",
    base_url="http://localhost:8081",
    default_headers={
        "X-Session-ID": "my-session",
        "X-Agent-ID": "my-agent",
    },
)

response = client.messages.create(
    model="claude-sonnet-4-5-20250929",
    max_tokens=1024,
    messages=[{"role": "user", "content": "Hello!"}],
)
print(response.content[0].text)
```

### Python — OpenAI

```python
from openai import OpenAI

client = OpenAI(
    api_key="your-api-key",
    base_url="http://localhost:8081/v1",
    default_headers={
        "X-Session-ID": "my-session",
        "X-Agent-ID": "my-agent",
    },
)

response = client.chat.completions.create(
    model="gpt-4o",
    messages=[{"role": "user", "content": "Hello!"}],
)
print(response.choices[0].message.content)
```

### Node.js / TypeScript — Anthropic

```typescript
import Anthropic from "@anthropic-ai/sdk";

const client = new Anthropic({
  apiKey: "your-api-key",
  baseURL: "http://localhost:8081",
  defaultHeaders: {
    "X-Session-ID": "my-session",
    "X-Agent-ID": "my-agent",
  },
});

const response = await client.messages.create({
  model: "claude-sonnet-4-5-20250929",
  max_tokens: 1024,
  messages: [{ role: "user", content: "Hello!" }],
});
console.log(response.content[0].text);
```

### Node.js / TypeScript — OpenAI

```typescript
import OpenAI from "openai";

const client = new OpenAI({
  apiKey: "your-api-key",
  baseURL: "http://localhost:8081/v1",
  defaultHeaders: {
    "X-Session-ID": "my-session",
    "X-Agent-ID": "my-agent",
  },
});

const response = await client.chat.completions.create({
  model: "gpt-4o",
  messages: [{ role: "user", content: "Hello!" }],
});
console.log(response.choices[0].message.content);
```

### curl

```bash
# Anthropic
curl http://localhost:8081/v1/messages \
  -H "x-api-key: $ANTHROPIC_API_KEY" \
  -H "anthropic-version: 2023-06-01" \
  -H "content-type: application/json" \
  -H "X-Session-ID: my-session" \
  -H "X-Agent-ID: my-agent" \
  -d '{
    "model": "claude-sonnet-4-5-20250929",
    "max_tokens": 1024,
    "messages": [{"role": "user", "content": "Hello!"}]
  }'

# OpenAI
curl http://localhost:8081/v1/chat/completions \
  -H "Authorization: Bearer $OPENAI_API_KEY" \
  -H "Content-Type: application/json" \
  -H "X-Session-ID: my-session" \
  -H "X-Agent-ID: my-agent" \
  -d '{
    "model": "gpt-4o",
    "messages": [{"role": "user", "content": "Hello!"}]
  }'
```

---

## Important: base_url Differences

The Anthropic and OpenAI SDKs handle `base_url` differently:

| SDK | `base_url` value | Why |
|-----|-----------------|-----|
| **Anthropic** | `http://localhost:8081` | SDK appends `/v1/messages` automatically |
| **OpenAI** | `http://localhost:8081/v1` | SDK appends `/chat/completions` to the base |
| **Google Gemini** | `http://localhost:8081/v1beta` | SDK appends `/models/{model}:generateContent` |

Getting this wrong is the most common integration mistake.

---

## Async Clients

### Python — Anthropic (async)

```python
from anthropic import AsyncAnthropic

client = AsyncAnthropic(
    api_key="your-api-key",
    base_url="http://localhost:8081",
    default_headers={
        "X-Session-ID": "my-session",
        "X-Agent-ID": "my-agent",
    },
)

response = await client.messages.create(
    model="claude-sonnet-4-5-20250929",
    max_tokens=1024,
    messages=[{"role": "user", "content": "Hello!"}],
)
```

### Python — OpenAI (async)

```python
from openai import AsyncOpenAI

client = AsyncOpenAI(
    api_key="your-api-key",
    base_url="http://localhost:8081/v1",
    default_headers={
        "X-Session-ID": "my-session",
        "X-Agent-ID": "my-agent",
    },
)

response = await client.chat.completions.create(
    model="gpt-4o",
    messages=[{"role": "user", "content": "Hello!"}],
)
```

---

## Streaming

Streaming works identically through the proxy with zero added latency.

### Anthropic Streaming

```python
from anthropic import Anthropic

client = Anthropic(
    api_key="your-api-key",
    base_url="http://localhost:8081",
    default_headers={"X-Session-ID": "my-session"},
)

with client.messages.stream(
    model="claude-sonnet-4-5-20250929",
    max_tokens=1024,
    messages=[{"role": "user", "content": "Tell me a story"}],
) as stream:
    for text in stream.text_stream:
        print(text, end="", flush=True)
```

### OpenAI Streaming

```python
from openai import OpenAI

client = OpenAI(
    api_key="your-api-key",
    base_url="http://localhost:8081/v1",
    default_headers={"X-Session-ID": "my-session"},
)

stream = client.chat.completions.create(
    model="gpt-4o",
    messages=[{"role": "user", "content": "Tell me a story"}],
    stream=True,
)
for chunk in stream:
    if chunk.choices[0].delta.content:
        print(chunk.choices[0].delta.content, end="", flush=True)
```

---

## Framework Integrations

### LangChain — Anthropic

```python
from langchain_anthropic import ChatAnthropic

llm = ChatAnthropic(
    model="claude-sonnet-4-5-20250929",
    anthropic_api_key="your-api-key",
    anthropic_api_url="http://localhost:8081",
    default_headers={
        "X-Session-ID": "my-session",
        "X-Agent-ID": "langchain-agent",
    },
)

response = llm.invoke("What is the capital of France?")
print(response.content)
```

### LangChain — OpenAI

```python
from langchain_openai import ChatOpenAI

llm = ChatOpenAI(
    model="gpt-4o",
    api_key="your-api-key",
    base_url="http://localhost:8081/v1",
    default_headers={
        "X-Session-ID": "my-session",
        "X-Agent-ID": "langchain-agent",
    },
)

response = llm.invoke("What is the capital of France?")
print(response.content)
```

### FastAPI / Backend Service

```python
from contextlib import asynccontextmanager
from fastapi import FastAPI
from anthropic import AsyncAnthropic

# Create the client once at startup, reuse across all requests
ai_client = AsyncAnthropic(
    api_key="your-api-key",
    base_url="http://localhost:8081",
    default_headers={
        "X-Agent-ID": "my-backend-service",
        "X-App-Name": "my-project",
    },
)

app = FastAPI()

@app.post("/chat")
async def chat(message: str):
    response = await ai_client.messages.create(
        model="claude-sonnet-4-5-20250929",
        max_tokens=1024,
        messages=[{"role": "user", "content": message}],
    )
    return {"reply": response.content[0].text}
```

---

## Tracking Headers

All headers are optional. The proxy works without any of them.

| Header | Purpose | Example |
|--------|---------|---------|
| `X-Session-ID` | Group requests into a session for loop detection and cost tracking | `"checkout-flow-abc123"` |
| `X-Agent-ID` | Identify the calling agent in the dashboard | `"legal-agent"` |
| `X-App-Name` | Label the application (falls back to `X-Agent-ID`) | `"my-saas-app"` |

If no headers are provided, the proxy auto-detects the agent name from the `User-Agent` header (e.g., `anthropic-sdk-python` becomes `python`).

---

## Environment Variables

Instead of hardcoding values, use environment variables:

```bash
# Your API keys (unchanged)
export ANTHROPIC_API_KEY="sk-ant-..."
export OPENAI_API_KEY="sk-..."

# Point SDKs to the proxy
export ANTHROPIC_BASE_URL="http://localhost:8081"
export OPENAI_BASE_URL="http://localhost:8081/v1"
```

```python
import os
from anthropic import Anthropic

# SDK reads ANTHROPIC_API_KEY and ANTHROPIC_BASE_URL automatically
client = Anthropic(
    default_headers={"X-Agent-ID": "my-agent"},
)
```

---

## Common Mistakes

### 1. Wrong base_url for Anthropic

```python
# WRONG — results in /v1/v1/messages (404)
client = Anthropic(base_url="http://localhost:8081/v1")

# CORRECT
client = Anthropic(base_url="http://localhost:8081")
```

### 2. Wrong base_url for OpenAI

```python
# WRONG — results in /chat/completions (404)
client = OpenAI(base_url="http://localhost:8081")

# CORRECT
client = OpenAI(base_url="http://localhost:8081/v1")
```

### 3. Using a custom httpx client with conflicting auth

```python
# WRONG — duplicate auth headers cause "Could not resolve authentication method"
custom_client = httpx.AsyncClient(
    base_url="http://localhost:8081",
    headers={"x-api-key": api_key},  # DON'T set auth here
)
client = AsyncAnthropic(
    api_key=api_key,          # SDK already sets X-Api-Key from this
    http_client=custom_client,
)

# CORRECT — let the SDK handle auth, just add your custom headers
client = AsyncAnthropic(
    api_key=api_key,
    base_url="http://localhost:8081",
    default_headers={
        "X-Agent-ID": "my-agent",
        "X-App-Name": "my-project",
    },
)
```

### 4. Proxy not running

If you get `ConnectionRefusedError`, make sure the proxy is running:

```bash
cd "/path/to/toko-mo-co"
go run main.go
```

---

## Verifying It Works

After integrating, open the dashboard at [http://localhost:8081](http://localhost:8081) to verify:

- Requests appear in the live feed
- Cost tracking is accurate
- Your agent ID shows up in the agent summary
- Streaming responses work without added latency

Run the included test app to verify end-to-end:

```bash
cd "/path/to/toko-mo-co"
pip install anthropic
export ANTHROPIC_API_KEY="your-key"
python test_app.py
```
