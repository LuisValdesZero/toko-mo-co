#!/usr/bin/env python3
"""
Toko-Mo-Co Feature Demo — Generate traffic that showcases all proxy features.

Demonstrates:
  1. CACHE HITS        — Send identical requests → 2nd call is free (served from cache)
  2. LOOP DETECTION    — Send similar prompts rapidly → proxy flags the pattern
  3. PII REDACTION     — Send prompts containing emails/SSNs → proxy strips them
  4. RULES ENGINE      — A rule blocks requests from "blocked-agent"
  5. COST TRACKING     — All requests tracked with per-agent cost breakdown

Usage:
    export ANTHROPIC_API_KEY="sk-ant-..."
    python scripts/demo_traffic.py
"""

import json
import os
import sys
import time
import requests

PROXY_URL = os.environ.get("TOKOMOCO_URL", "http://localhost:8081")
API_KEY = os.environ.get("ANTHROPIC_API_KEY", "")

if not API_KEY:
    print("ERROR: Set ANTHROPIC_API_KEY environment variable")
    sys.exit(1)

HEADERS = {
    "Content-Type": "application/json",
    "X-Api-Key": API_KEY,
    "anthropic-version": "2023-06-01",
}

ENDPOINT = f"{PROXY_URL}/v1/messages"


def send_request(agent_id, session_id, prompt, model="claude-haiku-4-5-20251001", temperature=0, max_tokens=200):
    """Send a request through the proxy and return (status, cache_header, cost_info)."""
    headers = {
        **HEADERS,
        "X-Agent-ID": agent_id,
        "X-Session-ID": session_id,
        "X-App-Name": "feature-demo",
    }
    body = {
        "model": model,
        "max_tokens": max_tokens,
        "temperature": temperature,
        "messages": [{"role": "user", "content": prompt}],
    }
    try:
        resp = requests.post(ENDPOINT, json=body, headers=headers, timeout=60)
        cache_status = resp.headers.get("X-Cache", "—")
        return resp.status_code, cache_status, resp.text[:120]
    except Exception as e:
        return 0, "ERROR", str(e)[:120]


def banner(title):
    print(f"\n{'═' * 60}")
    print(f"  {title}")
    print(f"{'═' * 60}")


def demo_cache():
    """Send identical requests to demonstrate exact-match caching."""
    banner("1. CACHE HITS — Identical requests served from cache")

    prompt = "What is the capital of France? Answer in one word."
    agent = "cache-demo-agent"
    session = "cache-demo-session"

    # First request — will be a MISS (hits upstream API)
    print("\n  [1/3] Sending first request (cold — cache MISS expected)...")
    status, cache, _ = send_request(agent, session, prompt)
    print(f"        Status: {status}  Cache: {cache}")

    # Second request — identical body → should be a HIT
    print("  [2/3] Sending identical request (cache HIT expected)...")
    status, cache, _ = send_request(agent, session, prompt)
    print(f"        Status: {status}  Cache: {cache}")
    if cache == "HIT":
        print("        ✓ Cache HIT — $0.00 cost, served instantly!")

    # Third request — slightly different prompt → MISS
    print("  [3/3] Sending different prompt (cache MISS expected)...")
    status, cache, _ = send_request(agent, session, "What is the capital of Germany? Answer in one word.")
    print(f"        Status: {status}  Cache: {cache}")

    print("\n  → Dashboard will show cache hit rate and cost savings.")


def demo_loop_detection():
    """Send similar prompts rapidly to trigger loop detection."""
    banner("2. LOOP DETECTION — Rapid similar prompts flagged")

    session = "loop-demo-session"
    agent = "loop-demo-agent"

    prompts = [
        "Analyze the stock price trend for NVDA over the past week.",
        "Analyze the stock price trend for NVDA over the past week.",
        "Analyze the stock price trend for NVDA over the past week.",
        "Analyze the stock price trend for NVDA over the past week.",
    ]

    for i, prompt in enumerate(prompts, 1):
        print(f"  [{i}/{len(prompts)}] Sending similar prompt #{i}...")
        status, cache, _ = send_request(agent, session, prompt)
        print(f"        Status: {status}  Cache: {cache}")

    print("\n  → Dashboard Security tab will show loop detection events.")


def demo_pii_redaction():
    """Send prompts containing PII to demonstrate redaction."""
    banner("3. PII REDACTION — Sensitive data stripped before upstream")

    agent = "pii-demo-agent"
    session = "pii-demo-session"

    prompts = [
        "Summarize this customer record: John Smith, email john.smith@example.com, SSN 123-45-6789, phone 555-0123.",
        "Process payment for card 4111-1111-1111-1111 belonging to jane.doe@company.org.",
        "Update address for patient ID 98765, DOB 01/15/1990, at 123 Main St, Springfield IL.",
    ]

    for i, prompt in enumerate(prompts, 1):
        print(f"  [{i}/{len(prompts)}] Sending prompt with PII...")
        status, cache, _ = send_request(agent, session, prompt, temperature=1)
        print(f"        Status: {status}")

    print("\n  → Dashboard will show PII redaction counts per request.")


def demo_rules():
    """Create a blocking rule, then trigger it."""
    banner("4. RULES ENGINE — Block requests from specific agents")

    # Create a rule via the API
    rule_payload = {
        "name": "Block demo-blocked-agent",
        "enabled": True,
        "priority": 100,
        "conditions": [
            {"type": "agent_id", "value": "demo-blocked-agent", "mode": "exact"}
        ],
        "action": {
            "type": "block",
            "block_status": 403,
            "block_message": "Blocked by demo rule: this agent is not allowed."
        },
        "description": "Demo rule — blocks requests from demo-blocked-agent",
    }

    print("  Creating blocking rule via API...")
    resp = requests.post(f"{PROXY_URL}/api/rules", json=rule_payload)
    if resp.status_code in (200, 201):
        rule_id = resp.json().get("id", "?")
        print(f"        ✓ Rule created (ID: {rule_id})")
    else:
        print(f"        ✗ Failed to create rule: {resp.status_code} {resp.text[:80]}")

    time.sleep(1)  # Wait for rule reload

    # Send a request from the blocked agent
    print("\n  Sending request from 'demo-blocked-agent' (should be blocked)...")
    status, cache, body = send_request("demo-blocked-agent", "rules-demo", "Hello, world!")
    print(f"        Status: {status}")
    if status == 403:
        print("        ✓ Blocked by rule! No upstream cost incurred.")

    # Send from an allowed agent — should pass through
    print("  Sending request from 'demo-allowed-agent' (should pass)...")
    status, cache, _ = send_request("demo-allowed-agent", "rules-demo", "Hello, world!")
    print(f"        Status: {status}  Cache: {cache}")
    if status == 200:
        print("        ✓ Allowed through — rule only blocks matching agents.")

    print("\n  → Dashboard Rules tab will show the rule and hit count.")


def demo_multi_agent():
    """Simulate a multi-agent pipeline with different models."""
    banner("5. MULTI-AGENT COST TRACKING — Per-agent cost breakdown")

    session = "pipeline-demo"

    agents = [
        ("planner-agent",   "Plan a 3-step approach to analyze market sentiment. Be brief."),
        ("researcher-agent", "List 3 key economic indicators for Q1 2026. Be brief."),
        ("summarizer-agent", "Summarize: markets are driven by inflation data, Fed policy, and earnings. One sentence."),
    ]

    for agent, prompt in agents:
        print(f"  Sending as '{agent}'...")
        status, cache, _ = send_request(agent, session, prompt)
        print(f"        Status: {status}  Cache: {cache}")

    print("\n  → Dashboard Agents tab will show per-agent cost breakdown.")


def enable_pii():
    """Enable PII redaction via settings API."""
    print("\n  Enabling PII redaction via settings API...")
    resp = requests.get(f"{PROXY_URL}/api/settings")
    if resp.status_code != 200:
        print("  ✗ Could not load settings")
        return
    settings = resp.json()
    settings["pii_enabled"] = True
    settings["pii_mode"] = "redact"
    resp = requests.put(f"{PROXY_URL}/api/settings", json=settings)
    if resp.status_code == 200:
        print("  ✓ PII redaction enabled")
    else:
        print(f"  ✗ Failed: {resp.status_code}")


def main():
    print("╔══════════════════════════════════════════════════════════╗")
    print("║       Toko-Mo-Co Feature Demo — Live Traffic Test       ║")
    print("╠══════════════════════════════════════════════════════════╣")
    print(f"║  Proxy:  {PROXY_URL:<48}║")
    print(f"║  API Key: {API_KEY[:8]}...{API_KEY[-4:]:<41}║")
    print("╚══════════════════════════════════════════════════════════╝")

    # 1. Cache
    demo_cache()

    # 2. Loop detection
    demo_loop_detection()

    # 3. PII (enable first, then send)
    enable_pii()
    demo_pii_redaction()

    # 4. Rules
    demo_rules()

    # 5. Multi-agent
    demo_multi_agent()

    banner("DEMO COMPLETE")
    print(f"\n  Open the dashboard: {PROXY_URL}/")
    print("  Check each tab to see the features in action:")
    print("    • Request Feed  — cost totals, cache hit rate")
    print("    • Agents        — per-agent cost breakdown")
    print("    • Sessions      — multi-request pipeline tracking")
    print("    • Security      — loop detection events, PII redaction")
    print("    • Rules         — active rules with hit counts")
    print()


if __name__ == "__main__":
    main()
