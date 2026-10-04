// Exercise real Mastra serialization against a gateway; print checks, never model content.
import assert from 'node:assert/strict';
import { createOpenAI } from '@ai-sdk/openai';
import { Agent } from '@mastra/core/agent';
import { Mastra } from '@mastra/core/mastra';
import { createTool } from '@mastra/core/tools';
import { z } from 'zod';

async function main() {
  for (const name of ['GATEWAY_BASE_URL', 'GATEWAY_API_KEY', 'GATEWAY_MODEL']) {
    assert.ok(process.env[name], `${name} is required`);
  }

  const provider = createOpenAI({
    baseURL: process.env.GATEWAY_BASE_URL,
    apiKey: process.env.GATEWAY_API_KEY,
  });
  const model = provider.responses(process.env.GATEWAY_MODEL);
  const options = {
    providerOptions: { openai: { store: false, systemMessageMode: 'developer' } },
    modelSettings: { maxRetries: 0 },
    maxSteps: 4,
  };
  const agent = new Agent({
    id: 'gateway-contract',
    name: 'Gateway contract',
    instructions: 'Follow the requested output format exactly. Keep answers brief.',
    model,
    errorProcessorDefaults: false,
  });
  new Mastra({ agents: { agent }, logger: false });

  const generated = await agent.generate('CONTRACT_GENERATE: Reply with only OK.', options);
  assert.equal(generated.text.trim(), 'OK');
  console.log('PASS generate');

  const streamed = await agent.stream('CONTRACT_STREAM: Reply with only OK.', options);
  let text = '';
  for await (const chunk of streamed.textStream) text += chunk;
  assert.equal(text.trim(), 'OK');
  console.log('PASS stream');

  let executions = 0;
  const getWeather = createTool({
    id: 'getWeather',
    description: 'Get the current temperature for a city.',
    inputSchema: z.object({ city: z.string() }),
    outputSchema: z.object({ temperature: z.number() }),
    execute: async ({ city }) => {
      assert.equal(city, 'Paris');
      executions++;
      return { temperature: 21 };
    },
  });
  const toolAgent = new Agent({
    id: 'gateway-tools',
    name: 'Gateway tools',
    instructions: 'Use getWeather for the requested city. Reply with only the temperature number.',
    model,
    errorProcessorDefaults: false,
    tools: { getWeather },
  });
  new Mastra({ agents: { toolAgent }, logger: false });
  const toolResult = await toolAgent.generate('CONTRACT_TOOL: What is the temperature in Paris? Call getWeather.', options);
  assert.equal(executions, 1);
  assert.equal(toolResult.text.trim(), '21');
  assert.ok(toolResult.steps.length >= 2, 'tool result must reach a second model step');
  console.log('PASS tool round trip');

  const structured = await agent.generate('CONTRACT_JSON: Return an object with ok set to true.', {
    ...options,
    structuredOutput: { schema: z.object({ ok: z.boolean() }), jsonPromptInjection: false },
  });
  assert.deepEqual(structured.object, { ok: true });
  console.log('PASS structured JSON');

  if (process.env.MASTRA_FIXTURE === '1') {
    const controller = new AbortController();
    const pending = await agent.stream('CONTRACT_CANCEL: Stream until the client cancels.', {
      ...options,
      abortSignal: controller.signal,
    });
    let received = false;
    try {
      for await (const chunk of pending.textStream) {
        if (chunk) {
          received = true;
          controller.abort();
          break;
        }
      }
    } catch (error) {
      if (!controller.signal.aborted) throw error;
    }
    assert.ok(received, 'cancellation must happen after stream output');
    console.log('PASS cancellation requested');

    let failed = false;
    try {
      const result = await agent.generate('CONTRACT_ERROR: Return the fixture upstream error.', options);
      failed = Boolean(result.error);
    } catch {
      failed = true;
    }
    assert.ok(failed, 'upstream failure must not become a successful response');
    console.log('PASS upstream failure');
  }
}

main().catch(() => {
  // SDK exceptions can contain full requests, so keep failures out of shared logs.
  console.error('FAIL Mastra contract. Check the case after the last PASS and gateway connection status.');
  process.exitCode = 1;
});
