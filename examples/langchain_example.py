#!/usr/bin/env python3
"""
LangChain integration with Toko-Mo-Co.

Supports both Anthropic and OpenAI backends through LangChain.

Usage:
    pip install langchain-anthropic langchain-openai
    export ANTHROPIC_API_KEY="your-key"
    export OPENAI_API_KEY="your-key"
    python langchain_example.py

Proxy must be running at http://localhost:8081
"""

import os

PROXY_URL = os.environ.get("TOKOMOCO_URL", "http://localhost:8081")


# -- 1. LangChain + Anthropic -------------------------------------------------
def langchain_anthropic():
    from langchain_anthropic import ChatAnthropic

    print("=== LangChain + Anthropic ===")
    llm = ChatAnthropic(
        model="claude-sonnet-4-5-20250929",
        anthropic_api_key=os.environ.get("ANTHROPIC_API_KEY", ""),
        anthropic_api_url=PROXY_URL,
        default_headers={
            "X-Session-ID": "langchain-example",
            "X-Agent-ID": "langchain-anthropic",
        },
    )
    response = llm.invoke("What is the capital of France?")
    print(f"Response: {response.content}\n")


# -- 2. LangChain + OpenAI ----------------------------------------------------
def langchain_openai():
    from langchain_openai import ChatOpenAI

    print("=== LangChain + OpenAI ===")
    llm = ChatOpenAI(
        model="gpt-4o",
        api_key=os.environ.get("OPENAI_API_KEY", ""),
        base_url=f"{PROXY_URL}/v1",
        default_headers={
            "X-Session-ID": "langchain-example",
            "X-Agent-ID": "langchain-openai",
        },
    )
    response = llm.invoke("What is the capital of France?")
    print(f"Response: {response.content}\n")


# -- 3. LangChain Streaming ---------------------------------------------------
def langchain_streaming():
    from langchain_anthropic import ChatAnthropic

    print("=== LangChain Streaming ===")
    llm = ChatAnthropic(
        model="claude-sonnet-4-5-20250929",
        anthropic_api_key=os.environ.get("ANTHROPIC_API_KEY", ""),
        anthropic_api_url=PROXY_URL,
        default_headers={
            "X-Session-ID": "langchain-example",
            "X-Agent-ID": "langchain-anthropic",
        },
    )
    for chunk in llm.stream("Count from 1 to 5."):
        print(chunk.content, end="", flush=True)
    print("\n")


# -- 4. LangChain Chain -------------------------------------------------------
def langchain_chain():
    from langchain_anthropic import ChatAnthropic
    from langchain_core.prompts import ChatPromptTemplate

    print("=== LangChain Chain ===")
    llm = ChatAnthropic(
        model="claude-sonnet-4-5-20250929",
        anthropic_api_key=os.environ.get("ANTHROPIC_API_KEY", ""),
        anthropic_api_url=PROXY_URL,
        default_headers={
            "X-Session-ID": "langchain-example",
            "X-Agent-ID": "langchain-chain",
        },
    )
    prompt = ChatPromptTemplate.from_messages([
        ("system", "You are a helpful assistant that answers in one sentence."),
        ("user", "{question}"),
    ])
    chain = prompt | llm
    response = chain.invoke({"question": "Why is the sky blue?"})
    print(f"Response: {response.content}\n")


# -- Main ----------------------------------------------------------------------
if __name__ == "__main__":
    print(f"Proxy: {PROXY_URL}\n")

    if os.environ.get("ANTHROPIC_API_KEY"):
        langchain_anthropic()
        langchain_streaming()
        langchain_chain()
    else:
        print("Skipping Anthropic examples (ANTHROPIC_API_KEY not set)\n")

    if os.environ.get("OPENAI_API_KEY"):
        langchain_openai()
    else:
        print("Skipping OpenAI examples (OPENAI_API_KEY not set)\n")

    print("Done! Check the dashboard at http://localhost:8081")
