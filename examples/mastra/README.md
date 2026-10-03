# Mastra contract example

Requires Node.js 22.13 or newer. Dependencies are pinned and the lockfile is committed.

```fish
rtk npm ci --ignore-scripts --no-audit --no-fund
rtk npm run smoke
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
