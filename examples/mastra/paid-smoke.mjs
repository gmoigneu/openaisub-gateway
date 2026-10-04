// Exercise the pinned OpenAI provider without printing paid request or response content.
import assert from 'node:assert/strict';
import { createOpenAI } from '@ai-sdk/openai';

async function main() {
  for (const name of ['GATEWAY_BASE_URL', 'GATEWAY_API_KEY']) {
    assert.ok(process.env[name], `${name} is required`);
  }
  const provider = createOpenAI({
    baseURL: process.env.GATEWAY_BASE_URL,
    apiKey: process.env.GATEWAY_API_KEY,
  });
  const embeddingModel = process.env.GATEWAY_EMBEDDING_MODEL || 'text-embedding-3-small';
  const speechModel = process.env.GATEWAY_SPEECH_MODEL || 'gpt-4o-mini-tts';
  const transcriptionModel = process.env.GATEWAY_TRANSCRIPTION_MODEL || 'gpt-4o-mini-transcribe';

  const embedding = await provider.embeddingModel(embeddingModel).doEmbed({ values: ['Gateway check'] });
  assert.ok(embedding.embeddings?.[0]?.length > 0);
  console.log('PASS embedding');

  const speech = await provider.speech(speechModel).doGenerate({
    text: 'Gateway check',
    voice: 'alloy',
    outputFormat: 'mp3',
  });
  const audio = typeof speech.audio === 'string' ? Buffer.from(speech.audio, 'base64') : speech.audio;
  assert.ok(audio.length > 0);
  console.log('PASS speech');

  const transcript = await provider.transcription(transcriptionModel).doGenerate({
    audio,
    mediaType: 'audio/mpeg',
  });
  assert.ok(transcript.text?.length > 0);
  console.log('PASS transcription');
}

main().catch(() => {
  // SDK exceptions may contain credentials or content.
  console.error('FAIL paid OpenAI contract. Check the case after the last PASS.');
  process.exitCode = 1;
});
