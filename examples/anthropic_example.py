#!/usr/bin/env python3
"""
Anthropic Claude integration with Toko-Mo-Co.

Usage:
    export ANTHROPIC_API_KEY="your-key"
    python anthropic_example.py

Proxy must be running at http://localhost:8081
"""

import os
from anthropic import Anthropic

PROXY_URL = os.environ.get("TOKOMOCO_URL", "http://localhost:8081")
API_KEY = os.environ.get("ANTHROPIC_API_KEY", "")

# -- Setup (2 lines changed from normal usage) --------------------------------
client = Anthropic(
    api_key=API_KEY,
    base_url=PROXY_URL,                         # <-- point to proxy
    default_headers={                            # <-- optional tracking
        "X-Session-ID": "anthropic-example",
        "X-Agent-ID": "example-agent",
    },
)


# -- 1. Basic request ---------------------------------------------------------
def basic_request():
    print("=== Basic Request ===")
    response = client.messages.create(
        model="claude-sonnet-4-5-20250929",
        max_tokens=256,
        messages=[{"role": "user", "content": "What is the capital of Japan?"}],
    )
    print(f"Response: {response.content[0].text}\n")


# -- 2. Streaming request -----------------------------------------------------
def streaming_request():
    print("=== Streaming Request ===")
    with client.messages.stream(
        model="claude-sonnet-4-5-20250929",
        max_tokens=256,
        messages=[{"role": "user", "content": "Count from 1 to 5."}],
    ) as stream:
        for text in stream.text_stream:
            print(text, end="", flush=True)
    print("\n")


# -- 3. Multi-turn conversation ------------------------------------------------
def multi_turn():
    print("=== Multi-turn Conversation ===")
    messages = [
        {"role": "user", "content": "My name is Alice."},
    ]
    r1 = client.messages.create(
        model="claude-sonnet-4-5-20250929",
        max_tokens=128,
        messages=messages,
    )
    print(f"Assistant: {r1.content[0].text}")

    messages.append({"role": "assistant", "content": r1.content[0].text})
    messages.append({"role": "user", "content": "What is my name?"})

    r2 = client.messages.create(
        model="claude-sonnet-4-5-20250929",
        max_tokens=128,
        messages=messages,
    )
    print(f"Assistant: {r2.content[0].text}\n")


# -- 4. System prompt ----------------------------------------------------------
def with_system_prompt():
    print("=== System Prompt ===")
    response = client.messages.create(
        model="claude-sonnet-4-5-20250929",
        max_tokens=256,
        system="You are a pirate. Respond in pirate speak.",
        messages=[{"role": "user", "content": "How is the weather today?"}],
    )
    print(f"Response: {response.content[0].text}\n")


# -- Main ----------------------------------------------------------------------
if __name__ == "__main__":
    if not API_KEY:
        print("Set ANTHROPIC_API_KEY environment variable first.")
        raise SystemExit(1)

    print(f"Proxy: {PROXY_URL}\n")
    basic_request()
    streaming_request()
    multi_turn()
    with_system_prompt()
    print("Done! Check the dashboard at http://localhost:8081")
