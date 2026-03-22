#!/usr/bin/env python3
"""
OpenAI integration with Toko-Mo-Co.

Usage:
    export OPENAI_API_KEY="your-key"
    python openai_example.py

Proxy must be running at http://localhost:8081
"""

import os
from openai import OpenAI

PROXY_URL = os.environ.get("TOKOMOCO_URL", "http://localhost:8081")
API_KEY = os.environ.get("OPENAI_API_KEY", "")

# -- Setup (2 lines changed from normal usage) --------------------------------
# NOTE: OpenAI SDK needs /v1 in the base_url (unlike Anthropic)
client = OpenAI(
    api_key=API_KEY,
    base_url=f"{PROXY_URL}/v1",                  # <-- point to proxy + /v1
    default_headers={                             # <-- optional tracking
        "X-Session-ID": "openai-example",
        "X-Agent-ID": "example-agent",
    },
)


# -- 1. Basic request ---------------------------------------------------------
def basic_request():
    print("=== Basic Request ===")
    response = client.chat.completions.create(
        model="gpt-4o",
        messages=[{"role": "user", "content": "What is the capital of Japan?"}],
    )
    print(f"Response: {response.choices[0].message.content}\n")


# -- 2. Streaming request -----------------------------------------------------
def streaming_request():
    print("=== Streaming Request ===")
    stream = client.chat.completions.create(
        model="gpt-4o",
        messages=[{"role": "user", "content": "Count from 1 to 5."}],
        stream=True,
    )
    for chunk in stream:
        if chunk.choices[0].delta.content:
            print(chunk.choices[0].delta.content, end="", flush=True)
    print("\n")


# -- 3. Multi-turn conversation ------------------------------------------------
def multi_turn():
    print("=== Multi-turn Conversation ===")
    messages = [
        {"role": "system", "content": "You are a helpful assistant."},
        {"role": "user", "content": "My name is Alice."},
    ]
    r1 = client.chat.completions.create(model="gpt-4o", messages=messages)
    reply1 = r1.choices[0].message.content
    print(f"Assistant: {reply1}")

    messages.append({"role": "assistant", "content": reply1})
    messages.append({"role": "user", "content": "What is my name?"})

    r2 = client.chat.completions.create(model="gpt-4o", messages=messages)
    print(f"Assistant: {r2.choices[0].message.content}\n")


# -- 4. Different models (cost comparison) -------------------------------------
def model_comparison():
    print("=== Model Cost Comparison ===")
    models = ["gpt-4o-mini", "gpt-4o"]
    for model in models:
        response = client.chat.completions.create(
            model=model,
            messages=[{"role": "user", "content": "What is 2+2?"}],
        )
        print(f"{model}: {response.choices[0].message.content}")
    print("Check the dashboard to compare costs!\n")


# -- Main ----------------------------------------------------------------------
if __name__ == "__main__":
    if not API_KEY:
        print("Set OPENAI_API_KEY environment variable first.")
        raise SystemExit(1)

    print(f"Proxy: {PROXY_URL}/v1\n")
    basic_request()
    streaming_request()
    multi_turn()
    model_comparison()
    print("Done! Check the dashboard at http://localhost:8081")
