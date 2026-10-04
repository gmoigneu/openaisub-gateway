# Mastra contract example

Requires Node.js 22.13 or newer. Dependencies are pinned and the lockfile is committed.

For setup, model discovery and a live check inside Docker, use the main [README](../../README.md#verify-with-your-subscription). To run directly, use these commands from `examples/mastra` after setting the environment variables below:

```fish
npm ci --ignore-scripts --no-audit --no-fund
npm run smoke
```

Set these environment variables through your app's secret configuration before running:

- `GATEWAY_BASE_URL`: `http://gateway:8080/v1` on the Compose network.
- `GATEWAY_API_KEY`: a client key created in the dashboard.
- `GATEWAY_MODEL`: a model ID from the gateway's authenticated Models endpoint.

The script checks `generate()`, `stream()`, one function tool round trip and native structured JSON output. It prints check names, not response text. Use it for an opt-in live test only after connecting your OpenAI account. These requests consume subscription capacity.

For offline CI, the Go integration test launches a local gateway and upstream fixture, sets the variables and invokes this script. `MASTRA_FIXTURE=1` adds deterministic cancellation and upstream-error cases. Do not set that variable for live tests. The Go fixture must verify that upstream sees cancellation and no automatic request replay.

## Fixture protocol

Inspect the user input for these markers. Each success stream needs standard Responses SSE lifecycle events and a final response with an `output` array.

- `CONTRACT_GENERATE` and `CONTRACT_STREAM`: return assistant text `OK`.
- `CONTRACT_TOOL`: emit a `function_call` for `getWeather` with `{"city":"Paris"}`. On the next request, verify matching `function_call_output` with `{"temperature":21}` and return `21`.
- `CONTRACT_JSON`: verify `text.format.type` is `json_schema` and return text `{"ok":true}`.
- `CONTRACT_CANCEL`: emit a text delta, then wait for request cancellation.
- `CONTRACT_ERROR`: return a non-success upstream status with an error envelope. The client must not return a successful result.

All requests use `store:false`, a full input history and an explicitly selected Responses model. Set `providerOptions.openai.systemMessageMode` to `developer` so Mastra agent instructions use the role required by the subscription route.

Client retries are disabled with both call-time `modelSettings.maxRetries: 0` and agent-level `errorProcessorDefaults: false`. Mastra's default error processors otherwise retry transient errors independently of model-call retries. The gateway itself never replays inference. Configure deliberate retries in your own app only after considering repeated model work and tool side effects.

## Paid provider check

The pinned OpenAI provider also calls the gateway's paid embeddings, speech and transcription endpoints. With the optional Platform API key configured, set `GATEWAY_BASE_URL` and `GATEWAY_API_KEY` as above and run:

```fish
rtk npm run smoke:paid
```

The script embeds a short test phrase, turns it into speech, then transcribes that generated audio. It prints only check names. Each successful live run makes three separately billed Platform API calls. Defaults are `text-embedding-3-small`, `gpt-4o-mini-tts` and `gpt-4o-mini-transcribe`; override them with `GATEWAY_EMBEDDING_MODEL`, `GATEWAY_SPEECH_MODEL` and `GATEWAY_TRANSCRIPTION_MODEL` when needed. The subscription `GET /v1/models` catalog does not list paid models.

Offline CI runs the same script against controlled upstream fixtures with `TestPaidProviderContract`. Fixture success does not establish paid-account eligibility.
