# API Key Authentication

Toko-Mo-Co includes built-in API key authentication to control access to your proxy endpoints. This guide covers how it works, how to set it up, and how to configure your client applications.

---

## How It Works

Toko-Mo-Co uses a **two-tier authentication model**:

```
Client App ──── tc_ key ────► Toko-Mo-Co ──── provider key ────► OpenAI / Anthropic / Gemini
             (proxy auth)                  (upstream auth)
```

| Layer | Key | Purpose |
|-------|-----|---------|
| **Client → Proxy** | `tc_*` key | Controls who can use your proxy instance |
| **Proxy → Provider** | `sk-*`, `anthropic-*`, etc. | Authenticates with the upstream LLM provider |

These are **completely independent**. The proxy's `tc_` key validates that a client is authorized to send requests through the proxy. The upstream provider API key (OpenAI, Anthropic, Gemini) is still passed through by the client in the standard headers, and the proxy forwards it to the provider unchanged.

---

## Quick Start

### 1. Create an API Key

**Via the Dashboard UI:**

1. Open the Settings page (`http://localhost:8081/settings`)
2. Find the **API Key Authentication** section at the top
3. Click **Create Key**
4. Enter a name (e.g., "cursor-dev", "ci-pipeline", "team-backend")
5. Select scopes and expiry
6. Click **Create**
7. **Copy the key immediately** — it is shown only once and cannot be retrieved later

**Via the API:**

```bash
curl -X POST http://localhost:8081/api/keys \
  -H "Content-Type: application/json" \
  -d '{"name": "my-app", "scopes": "proxy", "expires_in_days": 90}'
```

Response:
```json
{
  "id": 1,
  "name": "my-app",
  "key": "tc_a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2",
  "prefix": "tc_a1b2c3d4...",
  "scopes": "proxy",
  "message": "Save this key — it cannot be retrieved again."
}
```

### 2. Enable Authentication

**Via the Dashboard UI:**

1. On the Settings page, toggle **Authentication Enabled** to ON
2. The toggle requires at least one API key to exist before enabling

**Via config.json:**

```json
{
  "auth_enabled": true
}
```

**Via environment variable:**

```bash
CONFIG_AUTH_ENABLED=true ./tokomoco
```

### 3. Send Requests with Your Key

Once auth is enabled, all proxy endpoints require a valid `tc_` key. See [Client Configuration](#client-configuration) below.

---

## Protected vs. Public Endpoints

| Endpoint | Auth Required | Purpose |
|----------|---------------|---------|
| `POST /v1/chat/completions` | Yes | OpenAI-compatible proxy |
| `POST /v1/messages` | Yes | Anthropic-compatible proxy |
| `POST /v1beta/models/{model}:generateContent` | Yes | Gemini-compatible proxy |
| `POST /v1beta/models/{model}:streamGenerateContent` | Yes | Gemini streaming proxy |
| `GET /health` | No | Health check (for load balancers) |
| `GET /` | No | Dashboard UI |
| `GET /settings` | No | Settings UI |
| `GET /ws` | No | WebSocket (dashboard live feed) |
| `* /api/keys/*` | No | Key management API |
| `* /api/settings` | No | Settings API |
| `* /api/fallback-configs/*` | No | Fallback config API |
| `* /api/rules/*` | No | Rules API |

> **Note:** Dashboard and management APIs are intentionally open so you can always create keys and manage settings, even when auth is enabled. To restrict dashboard access, use network-level controls (firewall, VPN, reverse proxy).

---

## Client Configuration

The proxy accepts the `tc_` key in four locations, checked in priority order:

### Option 1: X-Proxy-Key Header (Recommended)

Use the dedicated `X-Proxy-Key` header. This is the cleanest approach — it never collides with any upstream provider's auth headers (Anthropic SDK uses `X-Api-Key`, OpenAI uses `Authorization: Bearer`):

```bash
# Anthropic endpoint — zero header conflicts
curl -X POST http://localhost:8081/v1/messages \
  -H "X-Proxy-Key: tc_YOUR_PROXY_KEY" \
  -H "x-api-key: sk-ant-YOUR_ANTHROPIC_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "claude-sonnet-4-20250514",
    "max_tokens": 1024,
    "messages": [{"role": "user", "content": "Hello"}]
  }'
```

```bash
# OpenAI endpoint
curl -X POST http://localhost:8081/v1/chat/completions \
  -H "X-Proxy-Key: tc_YOUR_PROXY_KEY" \
  -H "Authorization: Bearer sk-YOUR_OPENAI_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt-4o",
    "messages": [{"role": "user", "content": "Hello"}]
  }'
```

### Option 2: Authorization: Bearer (OpenAI clients)

For OpenAI-compatible clients that only support a single API key field:

```bash
curl -X POST http://localhost:8081/v1/chat/completions \
  -H "Authorization: Bearer tc_YOUR_PROXY_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model": "gpt-4o", "messages": [{"role": "user", "content": "Hello"}]}'
```

The proxy intercepts the `tc_` prefixed token. Your upstream provider API key should be passed via the provider's alternative header.

### Option 3: X-API-Key Header

Also works, but **avoid this with the Anthropic SDK** — the SDK uses `X-Api-Key` for its own auth, and HTTP headers are case-insensitive, so they will collide.

```bash
# Safe for OpenAI/Gemini, NOT safe for Anthropic SDK
curl -X POST http://localhost:8081/v1/chat/completions \
  -H "X-API-Key: tc_YOUR_PROXY_KEY" \
  -H "Authorization: Bearer sk-YOUR_OPENAI_KEY" \
  -H "Content-Type: application/json" \
  -d '...'
```

> **Note:** Query parameter auth (`?api_key=...`) is **not supported** because query strings appear in server logs, browser history, and referrer headers. Use one of the header-based methods above.

---

## Configuring Common Clients

### Cursor / VS Code AI Extensions

Most OpenAI-compatible clients let you set a custom base URL and API key:

```
Base URL: http://localhost:8081/v1
API Key:  tc_YOUR_PROXY_KEY
```

The client will send `Authorization: Bearer tc_...` which the proxy intercepts. Configure the upstream provider key via the proxy's environment or config, or use the `X-API-Key` header if the client supports custom headers.

### Python (OpenAI SDK)

```python
import openai

client = openai.OpenAI(
    base_url="http://localhost:8081/v1",
    api_key="tc_YOUR_PROXY_KEY",            # Proxy auth
    default_headers={
        "X-Upstream-Key": "sk-YOUR_OPENAI_KEY"  # If needed
    }
)

response = client.chat.completions.create(
    model="gpt-4o",
    messages=[{"role": "user", "content": "Hello"}]
)
```

### Python (Anthropic SDK)

```python
import anthropic

# X-Proxy-Key avoids collision with SDK's built-in X-Api-Key header
client = anthropic.Anthropic(
    base_url="http://localhost:8081",
    api_key="sk-ant-YOUR_ANTHROPIC_KEY",     # Upstream key (SDK sends as X-Api-Key)
    default_headers={
        "X-Proxy-Key": "tc_YOUR_PROXY_KEY"   # Proxy auth (dedicated header)
    }
)
```

> **Important:** Do NOT use `X-API-Key` for proxy auth with the Anthropic SDK — the SDK uses `X-Api-Key` internally (case-insensitive match), so your proxy key would overwrite the upstream API key. Always use `X-Proxy-Key`.

### JavaScript / TypeScript

```javascript
const response = await fetch("http://localhost:8081/v1/chat/completions", {
  method: "POST",
  headers: {
    "Content-Type": "application/json",
    "X-Proxy-Key": "tc_YOUR_PROXY_KEY",         // Proxy auth
    "Authorization": "Bearer sk-YOUR_OPENAI_KEY" // Upstream auth
  },
  body: JSON.stringify({
    model: "gpt-4o",
    messages: [{ role: "user", content: "Hello" }]
  })
});
```

### cURL

```bash
curl -X POST http://localhost:8081/v1/messages \
  -H "X-Proxy-Key: tc_YOUR_PROXY_KEY" \
  -H "x-api-key: sk-ant-YOUR_ANTHROPIC_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "claude-sonnet-4-20250514",
    "max_tokens": 1024,
    "messages": [{"role":"user","content":"Hello"}]
  }'
```

---

## Key Management

### Scopes

Each key has a scope that controls what it can access:

| Scope | Access |
|-------|--------|
| `proxy` | Proxy endpoints only (`/v1/*`) |
| `dashboard` | Dashboard APIs only (`/api/*`) |
| `proxy,dashboard` | Full access (default) |

### Expiration

Keys can be created with an optional expiry:
- **No expiry** (default) — key is valid until manually revoked
- **30 / 60 / 90 / 365 days** — key auto-expires after the set period

Expired keys are automatically rejected. The key remains in the database for audit purposes but will not authenticate.

### Managing Keys

**List all keys:**
```bash
curl http://localhost:8081/api/keys
```

**Create a key:**
```bash
curl -X POST http://localhost:8081/api/keys \
  -H "Content-Type: application/json" \
  -d '{"name": "my-key", "scopes": "proxy", "expires_in_days": 90}'
```

**Disable a key (without deleting):**
```bash
curl -X POST http://localhost:8081/api/keys/1/toggle \
  -H "Content-Type: application/json" \
  -d '{"enabled": false}'
```

**Re-enable a key:**
```bash
curl -X POST http://localhost:8081/api/keys/1/toggle \
  -H "Content-Type: application/json" \
  -d '{"enabled": true}'
```

**Delete a key permanently:**
```bash
curl -X DELETE http://localhost:8081/api/keys/1
```

**Check auth status:**
```bash
curl http://localhost:8081/api/keys/auth-status
# {"auth_enabled":true,"key_count":3}
```

---

## Security Design

### Key Format
- **Prefix:** `tc_` (Toko-Mo-Co) for easy identification
- **Entropy:** 32 random bytes (256 bits) hex-encoded = 64 character random string
- **Full key:** `tc_` + 64 hex chars = 67 characters total
- **Example:** `tc_a1b2c3d4e5f6789012345678abcdef0123456789abcdef0123456789abcdef01`

### Storage
- Only a **SHA-256 hash** of the key is stored in the database
- The raw key is returned **exactly once** at creation time
- There is no "show key" or "recover key" operation
- If a key is lost, delete it and create a new one

### Validation
- **Constant-time comparison** (`crypto/subtle.ConstantTimeCompare`) prevents timing attacks
- **In-memory cache** with 60-second TTL avoids hitting the database on every request
- Cache is **immediately invalidated** when keys are created, deleted, or toggled
- Background goroutine cleans expired cache entries every 5 minutes

### What Happens on Invalid Key
- Missing key: `401 Unauthorized` with hint about how to provide the key
- Invalid/expired/disabled key: `401 Unauthorized` with "invalid API key" message
- Internal validation error: `500 Internal Server Error`

---

## Error Responses

When auth is enabled and a request fails authentication:

**No key provided:**
```json
{
  "error": "missing API key",
  "hint": "Provide via Authorization: Bearer <key> or X-API-Key header"
}
```
HTTP Status: `401`

**Invalid, expired, or disabled key:**
```json
{
  "error": "invalid API key"
}
```
HTTP Status: `401`

---

## Configuration Reference

| Method | Setting | Default | Description |
|--------|---------|---------|-------------|
| config.json | `auth_enabled` | `false` | Enable/disable API key auth |
| Environment | `CONFIG_AUTH_ENABLED` | `false` | Override via env var |

Auth is **disabled by default** for backward compatibility. Existing deployments continue working without any changes. Enable auth only after creating at least one API key.

---

## FAQ

**Q: If I enable auth, will my existing clients break?**
A: Yes, any client that doesn't include a valid `tc_` key will receive `401 Unauthorized`. Create keys for all clients before enabling auth.

**Q: Do I need different keys for OpenAI vs. Anthropic endpoints?**
A: No. A single `tc_` key with `proxy` scope works for all proxy endpoints regardless of provider.

**Q: Can I use the proxy without auth?**
A: Yes. Auth is disabled by default. If your proxy is on a private network or behind a VPN, you may not need it.

**Q: What if I lose a key?**
A: The raw key cannot be recovered (only the hash is stored). Delete the lost key and create a new one.

**Q: Does enabling auth affect the dashboard?**
A: No. The dashboard UI and all management APIs remain accessible without a key. Auth only protects the proxy endpoints (`/v1/*`).

**Q: How do I rotate keys?**
A: Create a new key, update your clients to use it, then delete the old key. Both keys work simultaneously during the transition.

**Q: Is there rate limiting per key?**
A: Not yet. Rate limiting per key is on the roadmap. Currently, all keys have equal access within their scope.
