/**
 * Anthropic Claude integration with Toko-Mo-Co (TypeScript/Node.js).
 *
 * Usage:
 *     npm install @anthropic-ai/sdk
 *     export ANTHROPIC_API_KEY="your-key"
 *     npx tsx anthropic_example.ts
 *
 * Proxy must be running at http://localhost:8081
 */

import Anthropic from "@anthropic-ai/sdk";

const PROXY_URL = process.env.TOKOMOCO_URL || "http://localhost:8081";

// -- Setup (2 lines changed from normal usage) --------------------------------
const client = new Anthropic({
  apiKey: process.env.ANTHROPIC_API_KEY,
  baseURL: PROXY_URL, // <-- point to proxy
  defaultHeaders: {
    // <-- optional tracking
    "X-Session-ID": "ts-anthropic-example",
    "X-Agent-ID": "ts-example-agent",
  },
});

// -- 1. Basic request ---------------------------------------------------------
async function basicRequest() {
  console.log("=== Basic Request ===");
  const response = await client.messages.create({
    model: "claude-sonnet-4-5-20250929",
    max_tokens: 256,
    messages: [{ role: "user", content: "What is the capital of Japan?" }],
  });
  if (response.content[0].type === "text") {
    console.log(`Response: ${response.content[0].text}\n`);
  }
}

// -- 2. Streaming request -----------------------------------------------------
async function streamingRequest() {
  console.log("=== Streaming Request ===");
  const stream = client.messages.stream({
    model: "claude-sonnet-4-5-20250929",
    max_tokens: 256,
    messages: [{ role: "user", content: "Count from 1 to 5." }],
  });
  for await (const event of stream) {
    if (
      event.type === "content_block_delta" &&
      event.delta.type === "text_delta"
    ) {
      process.stdout.write(event.delta.text);
    }
  }
  console.log("\n");
}

// -- 3. System prompt ---------------------------------------------------------
async function withSystemPrompt() {
  console.log("=== System Prompt ===");
  const response = await client.messages.create({
    model: "claude-sonnet-4-5-20250929",
    max_tokens: 256,
    system: "You are a pirate. Respond in pirate speak.",
    messages: [{ role: "user", content: "How is the weather today?" }],
  });
  if (response.content[0].type === "text") {
    console.log(`Response: ${response.content[0].text}\n`);
  }
}

// -- Main ---------------------------------------------------------------------
async function main() {
  if (!process.env.ANTHROPIC_API_KEY) {
    console.error("Set ANTHROPIC_API_KEY environment variable first.");
    process.exit(1);
  }

  console.log(`Proxy: ${PROXY_URL}\n`);
  await basicRequest();
  await streamingRequest();
  await withSystemPrompt();
  console.log("Done! Check the dashboard at http://localhost:8081");
}

main().catch(console.error);
