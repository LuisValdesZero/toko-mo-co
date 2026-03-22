#!/usr/bin/env python3
"""
Async Anthropic Claude integration with Toko-Mo-Co.

This is the recommended pattern for backend services (FastAPI, Django, etc.)
where you want a single shared client across all request handlers.

Usage:
    export ANTHROPIC_API_KEY="your-key"
    python async_anthropic_example.py

Proxy must be running at http://localhost:8081
"""

import asyncio
import os
from anthropic import AsyncAnthropic

PROXY_URL = os.environ.get("TOKOMOCO_URL", "http://localhost:8081")
API_KEY = os.environ.get("ANTHROPIC_API_KEY", "")

# -- Setup: single shared client (create once, reuse everywhere) ---------------
client = AsyncAnthropic(
    api_key=API_KEY,
    base_url=PROXY_URL,
    default_headers={
        "X-Session-ID": "async-example",
        "X-Agent-ID": "async-agent",
        "X-App-Name": "my-backend-service",
    },
)


# -- 1. Basic async request ---------------------------------------------------
async def basic_request():
    print("=== Async Basic Request ===")
    response = await client.messages.create(
        model="claude-sonnet-4-5-20250929",
        max_tokens=256,
        messages=[{"role": "user", "content": "What is the capital of Japan?"}],
    )
    print(f"Response: {response.content[0].text}\n")


# -- 2. Async streaming -------------------------------------------------------
async def streaming_request():
    print("=== Async Streaming ===")
    async with client.messages.stream(
        model="claude-sonnet-4-5-20250929",
        max_tokens=256,
        messages=[{"role": "user", "content": "Count from 1 to 5."}],
    ) as stream:
        async for text in stream.text_stream:
            print(text, end="", flush=True)
    print("\n")


# -- 3. Concurrent requests (parallel API calls) ------------------------------
async def concurrent_requests():
    print("=== Concurrent Requests ===")
    questions = [
        "What is the capital of France?",
        "What is the capital of Germany?",
        "What is the capital of Italy?",
    ]

    async def ask(question: str) -> str:
        response = await client.messages.create(
            model="claude-sonnet-4-5-20250929",
            max_tokens=64,
            messages=[{"role": "user", "content": question}],
        )
        return response.content[0].text

    results = await asyncio.gather(*[ask(q) for q in questions])
    for question, answer in zip(questions, results):
        print(f"  Q: {question}")
        print(f"  A: {answer}")
    print()


# -- Main ----------------------------------------------------------------------
async def main():
    if not API_KEY:
        print("Set ANTHROPIC_API_KEY environment variable first.")
        raise SystemExit(1)

    print(f"Proxy: {PROXY_URL}\n")
    await basic_request()
    await streaming_request()
    await concurrent_requests()
    print("Done! Check the dashboard at http://localhost:8081")


if __name__ == "__main__":
    asyncio.run(main())
