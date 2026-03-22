#!/bin/bash
# Examples of using the Rules Engine REST API

BASE_URL="http://localhost:8081"

echo "=== Rules Engine API Examples ==="
echo

# 1. List all rules
echo "1. List all rules:"
curl -X GET "$BASE_URL/api/rules" | jq
echo
echo

# 2. Create a rule to block expensive sessions
echo "2. Create a rule to block expensive sessions:"
curl -X POST "$BASE_URL/api/rules" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "Block expensive sessions",
    "enabled": true,
    "priority": 10,
    "description": "Block requests when session cost exceeds $10",
    "conditions": [
      {
        "type": "cost_session",
        "threshold": 10.0,
        "op": "gte"
      }
    ],
    "action": {
      "type": "block",
      "block_status": 402,
      "block_message": "Session budget exceeded. Please contact support."
    }
  }' | jq
echo
echo

# 3. Create a rate-limit rule
echo "3. Create a rate-limit rule (10 req/min per agent):"
curl -X POST "$BASE_URL/api/rules" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "Agent rate limit",
    "enabled": true,
    "priority": 100,
    "description": "Limit agents to 10 requests per minute",
    "conditions": [],
    "action": {
      "type": "rate_limit",
      "rate_limit_requests": 10,
      "rate_limit_window_sec": 60,
      "rate_limit_scope": "agent",
      "block_status": 429,
      "block_message": "Rate limit exceeded. Try again in a minute."
    }
  }' | jq
echo
echo

# 4. Create a model override rule
echo "4. Create a model override rule (downgrade gpt-4 to gpt-3.5-turbo):"
curl -X POST "$BASE_URL/api/rules" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "Downgrade expensive model",
    "enabled": true,
    "priority": 50,
    "description": "Automatically downgrade gpt-4 requests to gpt-3.5-turbo",
    "conditions": [
      {
        "type": "model",
        "value": "gpt-4",
        "mode": "exact"
      }
    ],
    "action": {
      "type": "override_model",
      "override_model": "gpt-3.5-turbo"
    }
  }' | jq
echo
echo

# 5. Create an agent-scoped rule
echo "5. Create an agent-scoped rule (block test-agent):"
curl -X POST "$BASE_URL/api/rules" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "Block test agent",
    "enabled": true,
    "priority": 200,
    "scope_agent_id": "test-agent",
    "description": "Block all requests from test-agent",
    "conditions": [],
    "action": {
      "type": "block",
      "block_status": 403,
      "block_message": "Agent blocked by administrator."
    }
  }' | jq
echo
echo

# 6. Create a prompt injection rule
echo "6. Create a prompt injection rule:"
curl -X POST "$BASE_URL/api/rules" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "Add safety instructions",
    "enabled": true,
    "priority": 30,
    "description": "Inject safety prompt for sensitive topics",
    "conditions": [
      {
        "type": "prompt_content",
        "value": ".*medical|legal|financial.*",
        "mode": "regex"
      }
    ],
    "action": {
      "type": "inject_prompt",
      "injected_system_prompt": "IMPORTANT: You must not provide medical, legal, or financial advice. Recommend users consult qualified professionals."
    }
  }' | jq
echo
echo

# 7. Create a loop detection rule
echo "7. Create a loop detection rule:"
curl -X POST "$BASE_URL/api/rules" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "Block on loop detected",
    "enabled": true,
    "priority": 500,
    "description": "Immediately block requests when loop is detected",
    "conditions": [
      {
        "type": "loop_detected"
      }
    ],
    "action": {
      "type": "block",
      "block_status": 429,
      "block_message": "Loop detected. Your agent appears to be stuck."
    }
  }' | jq
echo
echo

# 8. Get a specific rule by ID (replace {id} with actual ID)
echo "8. Get rule by ID (example: ID 1):"
curl -X GET "$BASE_URL/api/rules/1" | jq
echo
echo

# 9. Update a rule (replace {id} with actual ID)
echo "9. Update rule (example: disable rule 1):"
curl -X PUT "$BASE_URL/api/rules/1" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "Block expensive sessions (UPDATED)",
    "enabled": false,
    "priority": 10,
    "conditions": [
      {
        "type": "cost_session",
        "threshold": 10.0,
        "op": "gte"
      }
    ],
    "action": {
      "type": "block",
      "block_status": 402,
      "block_message": "Session budget exceeded."
    }
  }' | jq
echo
echo

# 10. Toggle rule enabled status
echo "10. Toggle rule (example: enable rule 1):"
curl -X POST "$BASE_URL/api/rules/1/toggle" \
  -H "Content-Type: application/json" \
  -d '{"enabled": true}' | jq
echo
echo

# 11. Delete a rule
echo "11. Delete rule (example: delete rule 1):"
curl -X DELETE "$BASE_URL/api/rules/1" | jq
echo
echo

echo "=== Complete ==="
