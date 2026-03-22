#!/bin/bash
# =============================================================================
# curl examples for Toko-Mo-Co integration.
#
# Usage:
#     export ANTHROPIC_API_KEY="your-key"
#     export OPENAI_API_KEY="your-key"
#     bash curl_examples.sh
#
# Proxy must be running at http://localhost:8081
# =============================================================================

PROXY="http://localhost:8081"

echo "=== Toko-Mo-Co - curl Examples ==="
echo "Proxy: $PROXY"
echo

# -- 1. Anthropic (non-streaming) ---------------------------------------------
echo "1. Anthropic — Basic Request"
curl -s "$PROXY/v1/messages" \
  -H "x-api-key: $ANTHROPIC_API_KEY" \
  -H "anthropic-version: 2023-06-01" \
  -H "content-type: application/json" \
  -H "X-Session-ID: curl-example" \
  -H "X-Agent-ID: curl-agent" \
  -d '{
    "model": "claude-sonnet-4-5-20250929",
    "max_tokens": 128,
    "messages": [{"role": "user", "content": "What is 2+2?"}]
  }' | python3 -m json.tool 2>/dev/null || cat
echo
echo

# -- 2. Anthropic (streaming) -------------------------------------------------
echo "2. Anthropic — Streaming Request"
curl -s -N "$PROXY/v1/messages" \
  -H "x-api-key: $ANTHROPIC_API_KEY" \
  -H "anthropic-version: 2023-06-01" \
  -H "content-type: application/json" \
  -H "X-Session-ID: curl-example" \
  -H "X-Agent-ID: curl-agent" \
  -d '{
    "model": "claude-sonnet-4-5-20250929",
    "max_tokens": 128,
    "stream": true,
    "messages": [{"role": "user", "content": "Count from 1 to 3."}]
  }'
echo
echo

# -- 3. OpenAI (non-streaming) ------------------------------------------------
echo "3. OpenAI — Basic Request"
curl -s "$PROXY/v1/chat/completions" \
  -H "Authorization: Bearer $OPENAI_API_KEY" \
  -H "Content-Type: application/json" \
  -H "X-Session-ID: curl-example" \
  -H "X-Agent-ID: curl-agent" \
  -d '{
    "model": "gpt-4o",
    "messages": [{"role": "user", "content": "What is 2+2?"}]
  }' | python3 -m json.tool 2>/dev/null || cat
echo
echo

# -- 4. OpenAI (streaming) ----------------------------------------------------
echo "4. OpenAI — Streaming Request"
curl -s -N "$PROXY/v1/chat/completions" \
  -H "Authorization: Bearer $OPENAI_API_KEY" \
  -H "Content-Type: application/json" \
  -H "X-Session-ID: curl-example" \
  -H "X-Agent-ID: curl-agent" \
  -d '{
    "model": "gpt-4o",
    "stream": true,
    "messages": [{"role": "user", "content": "Count from 1 to 3."}]
  }'
echo
echo

# -- 5. Health check -----------------------------------------------------------
echo "5. Health Check"
curl -s "$PROXY/health" | python3 -m json.tool 2>/dev/null || cat
echo

echo
echo "=== Done! Check the dashboard at $PROXY ==="
