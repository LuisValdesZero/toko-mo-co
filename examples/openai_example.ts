/**
 * OpenAI integration with Toko-Mo-Co (TypeScript/Node.js).
 *
 * Usage:
 *     npm install openai
 *     export OPENAI_API_KEY="your-key"
 *     npx tsx openai_example.ts
 *
 * Proxy must be running at http://localhost:8081
 */

import OpenAI from "openai";

const PROXY_URL = process.env.TOKOMOCO_URL || "http://localhost:8081";

// -- Setup (2 lines changed from normal usage) --------------------------------
// NOTE: OpenAI SDK needs /v1 in the baseURL (unlike Anthropic)
const client = new OpenAI({
  apiKey: process.env.OPENAI_API_KEY,
  baseURL: `${PROXY_URL}/v1`, // <-- point to proxy + /v1
  defaultHeaders: {
    // <-- optional tracking
    "X-Session-ID": "ts-openai-example",
    "X-Agent-ID": "ts-example-agent",
  },
});

// -- 1. Basic request ---------------------------------------------------------
async function basicRequest() {
  console.log("=== Basic Request ===");
  const response = await client.chat.completions.create({
    model: "gpt-4o",
    messages: [{ role: "user", content: "What is the capital of Japan?" }],
  });
  console.log(`Response: ${response.choices[0].message.content}\n`);
}

// -- 2. Streaming request -----------------------------------------------------
async function streamingRequest() {
  console.log("=== Streaming Request ===");
  const stream = await client.chat.completions.create({
    model: "gpt-4o",
    messages: [{ role: "user", content: "Count from 1 to 5." }],
    stream: true,
  });
  for await (const chunk of stream) {
    const content = chunk.choices[0]?.delta?.content;
    if (content) {
      process.stdout.write(content);
    }
  }
  console.log("\n");
}

// -- 3. Multi-turn conversation -----------------------------------------------
async function multiTurn() {
  console.log("=== Multi-turn Conversation ===");
  const messages: OpenAI.ChatCompletionMessageParam[] = [
    { role: "system", content: "You are a helpful assistant." },
    { role: "user", content: "My name is Alice." },
  ];

  const r1 = await client.chat.completions.create({
    model: "gpt-4o",
    messages,
  });
  const reply1 = r1.choices[0].message.content || "";
  console.log(`Assistant: ${reply1}`);

  messages.push({ role: "assistant", content: reply1 });
  messages.push({ role: "user", content: "What is my name?" });

  const r2 = await client.chat.completions.create({
    model: "gpt-4o",
    messages,
  });
  console.log(`Assistant: ${r2.choices[0].message.content}\n`);
}

// -- Main ---------------------------------------------------------------------
async function main() {
  if (!process.env.OPENAI_API_KEY) {
    console.error("Set OPENAI_API_KEY environment variable first.");
    process.exit(1);
  }

  console.log(`Proxy: ${PROXY_URL}/v1\n`);
  await basicRequest();
  await streamingRequest();
  await multiTurn();
  console.log("Done! Check the dashboard at http://localhost:8081");
}

main().catch(console.error);
