# Rules Engine Implementation

## Overview

A production-grade rules engine has been successfully implemented for Toko-Mo-Co. The engine enables dynamic request control through configurable rules that can block, rate-limit, override models, inject prompts, or redirect requests based on various conditions.

## Features Implemented

### Core Components

1. **rules/rule.go** - Core types and data structures
   - `Rule` struct with conditions and actions
   - `ConditionSpec` and `ActionSpec` for JSON serialization
   - Support for 11 condition types and 6 action types

2. **rules/engine.go** - Hot-path evaluation engine
   - In-memory evaluation (< 1ms for typical rule sets)
   - Priority-based rule ordering
   - Background reload loop (polls DB every 30 seconds)
   - Thread-safe concurrent access

3. **rules/conditions.go** - Condition implementations
   - String matching (exact, glob, regex) for agent/app/model/provider
   - Numeric comparisons (gt, gte, lt, lte, eq) for tokens/cost
   - Time-window quotas (daily/monthly cost, request count)
   - Prompt content filtering
   - Loop detection integration

4. **rules/actions.go** - Action implementations
   - **Block**: Return HTTP 403/429 immediately
   - **Rate Limit**: Token bucket + quota tracking (per-agent or global)
   - **Override Model**: Swap model ID before upstream call
   - **Inject Prompt**: Prepend system message
   - **Redirect**: Change upstream URL

5. **rules/ratelimiter.go** - In-process rate limiting
   - Token bucket algorithm for request-rate limits
   - Fixed-period quota tracking for cost/token budgets
   - Goroutine-safe with separate mutexes per bucket
   - Zero external dependencies

6. **rules/store.go** - SQLite persistence
   - CRUD operations for rules
   - JSON serialization of conditions/actions
   - Migration-safe schema (added to store/db.go)

7. **rules/api.go** - REST API handlers
   - `GET /api/rules` - List all rules
   - `POST /api/rules` - Create rule
   - `GET /api/rules/{id}` - Get rule by ID
   - `PUT /api/rules/{id}` - Update rule
   - `DELETE /api/rules/{id}` - Delete rule
   - `POST /api/rules/{id}/toggle` - Enable/disable rule

### Integration Points

- **config/config.go**: Added `RulesEnabled` field (default: true)
- **store/db.go**: Added `rules` table migration + `DB()` accessor
- **proxy/handler.go**: Integrated engine after loop detection, before upstream request
- **main.go**: Wired engine initialization and API routes

## Condition Types

| Type | Description | Parameters |
|------|-------------|------------|
| `agent_id` | Match agent ID | value, mode (exact/glob/regex) |
| `app_name` | Match app name | value, mode |
| `model` | Match model name | value, mode |
| `provider` | Match provider (openai/anthropic/gemini) | value, mode |
| `input_tokens` | Token count threshold | threshold, op (gt/gte/lt/lte/eq) |
| `cost_session` | Session cumulative cost | threshold, op |
| `cost_daily` | Daily cost quota | threshold |
| `cost_monthly` | Monthly cost quota | threshold |
| `request_count` | Request rate limit | threshold, window_sec |
| `prompt_content` | Prompt text matching | value, mode |
| `loop_detected` | Loop detection flag | (no parameters) |

## Action Types

| Type | Description | Parameters |
|------|-------------|------------|
| `allow` | Explicitly allow (no-op) | - |
| `block` | Block immediately | block_status, block_message |
| `rate_limit` | Enforce rate limits | rate_limit_* fields, scope |
| `override_model` | Swap model | override_model |
| `inject_prompt` | Add system message | injected_system_prompt |
| `redirect` | Change upstream URL | redirect_url |

## Example Rules

### Block high-cost sessions
```json
{
  "name": "Block expensive sessions",
  "enabled": true,
  "priority": 10,
  "conditions": [
    {"type": "cost_session", "threshold": 10.0, "op": "gte"}
  ],
  "action": {
    "type": "block",
    "block_status": 402,
    "block_message": "Session budget exceeded"
  }
}
```

### Rate limit per agent
```json
{
  "name": "Agent rate limit",
  "enabled": true,
  "priority": 100,
  "scope_agent_id": "agent-123",
  "conditions": [
    {"type": "request_count", "threshold": 10, "window_sec": 60}
  ],
  "action": {
    "type": "rate_limit",
    "rate_limit_requests": 10,
    "rate_limit_window_sec": 60,
    "rate_limit_scope": "agent"
  }
}
```

### Override expensive model
```json
{
  "name": "Downgrade to cheaper model",
  "enabled": true,
  "priority": 50,
  "conditions": [
    {"type": "model", "value": "gpt-4", "mode": "exact"}
  ],
  "action": {
    "type": "override_model",
    "override_model": "gpt-3.5-turbo"
  }
}
```

### Inject safety prompt
```json
{
  "name": "Add safety instructions",
  "enabled": true,
  "priority": 10,
  "conditions": [
    {"type": "prompt_content", "value": ".*unsafe.*", "mode": "regex"}
  ],
  "action": {
    "type": "inject_prompt",
    "injected_system_prompt": "You must prioritize user safety in all responses."
  }
}
```

## Performance

- **Evaluation**: O(rules) with early exit, typically < 1ms for < 1000 rules
- **Reload**: Async background polling (30s interval), synchronous on API mutations
- **Storage**: SQLite with WAL mode, in-memory evaluation cache
- **Concurrency**: RWMutex for rule reloads, separate mutex per rate-limit bucket

## Testing

Comprehensive test suite in `rules/engine_test.go`:
- ✅ Basic rule evaluation
- ✅ Priority ordering
- ✅ Model override
- ✅ Input token thresholds
- ✅ Loop detection integration
- ✅ Session cost tracking
- ✅ Disabled rules
- ✅ Agent-scoped rules
- ✅ Race detection (all tests pass with `-race`)

## Configuration

Enable/disable via config:

```json
{
  "rules_enabled": true
}
```

Or via environment variable:
```bash
export CONFIG_RULES_ENABLED=true
```

## Database Schema

```sql
CREATE TABLE rules (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    name            TEXT    NOT NULL,
    enabled         INTEGER NOT NULL DEFAULT 1,
    priority        INTEGER NOT NULL DEFAULT 0,
    scope_agent_id  TEXT    NOT NULL DEFAULT '',
    conditions_json TEXT    NOT NULL,
    action_json     TEXT    NOT NULL,
    description     TEXT    NOT NULL DEFAULT '',
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL
);

CREATE INDEX idx_rules_enabled  ON rules(enabled);
CREATE INDEX idx_rules_priority ON rules(priority DESC);
```

## Next Steps

1. **Dashboard UI**: Add rules management panel to the web dashboard
2. **More Conditions**: Add time-of-day, IP address, custom headers
3. **Metrics**: Track rule match counts, blocked requests per rule
4. **Webhooks**: Trigger external actions when rules fire
5. **Rule Templates**: Pre-defined rule sets for common use cases

## Files Changed/Added

### New Files
- `rules/rule.go` (116 lines)
- `rules/engine.go` (216 lines)
- `rules/conditions.go` (270 lines)
- `rules/actions.go` (213 lines)
- `rules/ratelimiter.go` (165 lines)
- `rules/store.go` (154 lines)
- `rules/api.go` (163 lines)
- `rules/engine_test.go` (435 lines)

### Modified Files
- `store/db.go` - Added rules table + DB() accessor
- `proxy/handler.go` - Added rules evaluation + model/prompt mutation
- `config/config.go` - Added RulesEnabled field
- `main.go` - Wired engine initialization + API routes

**Total**: ~1,700 lines of production code + tests

## Build Status

✅ Compiles successfully: `go build`
✅ All tests pass: `go test ./...`
✅ Race detector clean: `go test ./... -race`
