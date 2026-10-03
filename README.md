# OpenAI subscription gateway

A private Go gateway for one owner's Mastra agents. It exposes OpenAI Responses and Models endpoints using the owner's Sign in with ChatGPT credentials. Each app gets a separate revocable gateway key.

**Implementation preview. Live OpenAI sign-in and a subscription-backed Mastra tool round trip still need owner verification.** Fixture tests cannot prove account eligibility or preview availability. The project uses OpenAI's documented open-source sign-in flow, not Codex device credentials.

## Scope

- One owner, one connected OpenAI account, one gateway process.
- Private Docker network, separate localhost dashboard, persistent encrypted credentials.
- Streaming and complete Responses, function tools, structured JSON, live model discovery.
- No Chat Completions, embeddings, token accounting, request-content storage or billing fallback.

See the [canonical specification](docs/specs/v1.md) and [architecture decisions](docs/adr/).

## Start with Docker Compose

You need Docker with Compose. The command examples use the optional [RTK command proxy](https://github.com/rtk-ai/rtk); omit `rtk` if you do not use it.

Build and initialize the two secret files. Initialization does not overwrite existing secrets. The initializer runs as root only to set file ownership for the container's unprivileged UID 10001.

```fish
rtk docker compose build
rtk mkdir -p secrets
rtk docker run --rm --user 0 --volume "$PWD/secrets:/bootstrap" openaisub-gateway:local init --secrets-dir /bootstrap --owner 10001
rtk docker compose up -d
```

Open [the dashboard](http://127.0.0.1:8081). Read the administrator password from the running container and use it to log in:

```fish
rtk docker compose exec gateway /gateway admin password
```

Run this in a private terminal. The files remain readable only by their owner; do not make them world-readable to fix a permissions error.

For a remote server, open an SSH tunnel on your own computer, then use the same dashboard URL:

```fish
rtk ssh -N -L 8081:127.0.0.1:8081 your-server
```

The inference listener is available to sibling containers at `http://gateway:8080/v1`. It is not published on the host. Add your Mastra service to the same Compose project and network. The gateway needs outbound HTTPS access to OpenAI.

## Connect OpenAI

Run the login helper on the computer with your browser. Build the helper with Go 1.26.6 or newer:

```fish
rtk go build -o gateway ./cmd/gateway
```

For a local Docker stack, run the helper from the Compose directory:

```fish
rtk proxy ./gateway auth login
```

For a remote stack, give the SSH destination and absolute Compose directory on that server:

```fish
rtk proxy ./gateway auth login --ssh your-server --directory /absolute/path/to/openaisub-gateway
```

The helper uses a local browser callback and imports credentials into the running container. Remote import uses SSH and standard input. No manual token copying is required. The gateway renews tokens automatically. Revoked or expired refresh credentials require another login.

Create a client key in the dashboard and copy it to your Mastra app's secret store. The full key is shown once. Its name and creation date remain visible, and revocation takes effect for subsequent requests.

## Configure Mastra

Use the Responses model explicitly, disable storage, and send agent instructions as `developer` messages. The subscription route requires these provider options in addition to the gateway URL and key. Select a model returned by authenticated `GET /v1/models`; the gateway does not include a model catalog.

```js
import { createOpenAI } from '@ai-sdk/openai';
import { Agent } from '@mastra/core/agent';

const openai = createOpenAI({
  baseURL: 'http://gateway:8080/v1',
  apiKey: process.env.GATEWAY_API_KEY,
});

const agent = new Agent({
  id: 'my-agent',
  name: 'My agent',
  instructions: 'Answer the user clearly.',
  model: openai.responses(process.env.GATEWAY_MODEL),
  errorProcessorDefaults: false,
});

const result = await agent.generate('Hello', {
  providerOptions: {
    openai: { store: false, systemMessageMode: 'developer' },
  },
  modelSettings: { maxRetries: 0 },
});
```

Mastra owns conversation history and executes function tools. Do not supply `previousResponseId`, stored conversations, hosted tools or subscription-unsupported sampling limits. The gateway reports unsupported options rather than silently dropping them. It always sends upstream streaming requests, collecting the final response when a caller asks for a complete result. It does not retry inference automatically.

The example disables both Mastra retry paths. `modelSettings.maxRetries: 0` disables model-call retries; `errorProcessorDefaults: false` disables the separate default error processors, which otherwise retry transient failures. Choose retries explicitly in your own client. A replay can repeat model work or tool side effects.

The [executable Mastra example](examples/mastra/) pins `@mastra/core` 1.74.0, `@ai-sdk/openai` 4.0.83 and Zod 4.6.5. It checks generation, streaming, a tool round trip and native JSON schema output. See its [instructions](examples/mastra/README.md) for live verification.

## Persistence and recovery

The `gateway-data` named volume holds SQLite state, client key hashes, registration and encrypted OpenAI credentials. Separate host files hold the administrator secret and encryption key. Keep both secret files out of source control and backups accessible only to you.

- Stop the gateway before copying the complete data volume. Back up the encryption key separately and securely. Restore the matching volume and key with UID 10001 ownership before starting one gateway instance.
- Losing the encryption key makes stored OpenAI credentials unreadable. Preserve the old volume while preparing a fresh data directory and new secrets, then reconnect OpenAI and issue new client keys.
- Losing a client key requires creating a replacement and revoking the old one. Keys cannot be recovered from their hashes.
- A reconnect warning requires signing in again. A temporary upstream failure does not require deleting state.
- Disconnect removes local credentials and attempts remote revocation. An unconfirmed revocation means you should also remove access in your OpenAI account.

Do not run multiple gateway processes against the same volume. Do not expose either listener publicly. Local root, Docker administrators and processes that can read the encryption key and database are trusted.

## Develop and verify

```fish
rtk npm --prefix examples/mastra ci --ignore-scripts --no-audit --no-fund
rtk proxy env MASTRA_CONTRACT=1 go test -race ./...
rtk go vet ./...
rtk proxy gofmt -l .
```

Formatting should report no files. CI runs the Go checks and real Mastra client against a controlled upstream fixture. It also runs the native container to check secret initialization, dashboard login, restart persistence and single-process ownership, then builds Linux amd64 and arm64 images without publishing them. Fixture-only cancellation and error cases verify client behavior without sending deliberately invalid live requests.

After building the image locally, run the same container check with Docker running:

```fish
rtk proxy env DOCKER_CONTRACT=1 go test -v ./integration -run TestContainer
```

For a live check, run the smoke example from a container on the same Compose network with `GATEWAY_BASE_URL`, `GATEWAY_API_KEY` and `GATEWAY_MODEL` set. Live checks make inference requests against your subscription. Record the model, package versions and result without tokens or prompt content before claiming deployment readiness.

## Upstream documentation

- [OpenAI open-source sign-in](https://developers.openai.com/siwc/token-sharing-open-source/sign-in)
- [Session renewal](https://developers.openai.com/siwc/token-sharing-open-source/profiles-and-sessions)
- [Models and inference](https://developers.openai.com/siwc/token-sharing-open-source/models-and-inference)
- [Preview limitations](https://developers.openai.com/siwc/token-sharing-open-source/preview-limitations)
- [Remote deployment](https://developers.openai.com/siwc/token-sharing-open-source/self-hosted-vms)
- [Mastra structured output](https://mastra.ai/docs/agents/structured-output)
- [AI SDK OpenAI provider](https://ai-sdk.dev/providers/ai-sdk-providers/openai)

## License

[Apache License 2.0](LICENSE).
