# OpenAI subscription gateway

A Go gateway for one owner's Mastra agents. It exposes OpenAI Responses and Models endpoints using the owner's Sign in with ChatGPT credentials. Each app gets a separate revocable gateway key.

**Implementation preview. Live OpenAI sign-in and a subscription-backed Mastra tool round trip still need owner verification.** Fixture tests cannot prove account eligibility or preview availability. The project uses OpenAI's documented open-source sign-in flow, not Codex device credentials.

## Scope

- One owner, one connected OpenAI account, one gateway process.
- Private Docker network by default, optional localhost API port for testing, or public inference behind an existing HTTPS proxy. The dashboard stays on localhost; credentials stay encrypted in persistent storage.
- Streaming and complete Responses, function tools, structured JSON, live model discovery.
- No Chat Completions, embeddings, token accounting, request-content storage or billing fallback.

See the [canonical specification](docs/specs/v1.md) and [architecture decisions](docs/adr/).

## Deploy with Portainer and Caddy

Follow the [Portainer deployment guide](docs/portainer.md) to run the gateway independently behind your existing Caddy HTTPS proxy. Use [compose.portainer.yaml](compose.portainer.yaml) for Docker Standalone and [examples/Caddyfile](examples/Caddyfile) for the public route. The project does not run another proxy or manage certificates.

Only Models and Responses are public, and both require a valid gateway client key. Administration stays on host loopback through SSH. Public clients use `https://your-gateway-domain/v1` as their `baseURL`. The guide covers the image build, secrets, OpenAI login, keys, checks and updates.

## How it works

Mastra sends a gateway client key to the API. The gateway checks that key, obtains a current OpenAI bearer token, and forwards the request to the official OpenAI API. OpenAI credentials stay in the gateway. Mastra supplies conversation history and executes tools.

One Go process serves the API and dashboard on separate listeners. SQLite stores client key hashes and encrypted OpenAI credentials in a Docker volume. The encryption key is a separate Docker secret. The gateway discovers models from OpenAI and refreshes credentials when needed.

## Start with Docker Compose

You need Docker with Compose. Clone the repository and run the setup commands from its root:

```fish
git clone https://github.com/gmoigneu/openaisub-gateway.git
cd openaisub-gateway
```

The implementation preview is on `main`. Use that branch before building:

```fish
git switch main
```

Build and initialize the two secret files. Initialization does not overwrite existing secrets. The initializer runs as root only to set file ownership for the container's unprivileged UID 10001.

```fish
docker compose build
mkdir -p secrets
docker run --rm --user 0 --volume "$PWD/secrets:/bootstrap" openaisub-gateway:local init --secrets-dir /bootstrap --owner 10001
docker compose up -d
```

Open [the dashboard](http://127.0.0.1:8081). Read the administrator password from the running container and use it to log in:

```fish
docker compose exec gateway /gateway admin password
```

Run this in a private terminal. The files remain readable only by their owner; do not make them world-readable to fix a permissions error.

For a remote server, open an SSH tunnel on your own computer, then use the same dashboard URL:

```fish
ssh -N -L 8081:127.0.0.1:8081 your-server
```

The inference listener is available to sibling containers at `http://gateway:8080/v1`. It is not published on the host by default. Add your Mastra service to the same Compose project and network. The gateway needs outbound HTTPS access to OpenAI.

### Optional host access for testing

After setup, enable the host port with the supplied override:

```fish
docker compose -f compose.yaml -f compose.host.yaml up -d
```

Host clients can now use `http://127.0.0.1:8080/v1` with a gateway client key. For Mastra running on the host, use that URL as `baseURL`. Container clients still use `http://gateway:8080/v1`. The binding accepts connections only from the Docker host; the dashboard stays at `http://127.0.0.1:8081`.

To use a different host port, set `GATEWAY_HOST_PORT` for the Compose command, or put it in the repository's `.env` file:

```fish
env GATEWAY_HOST_PORT=18080 docker compose -f compose.yaml -f compose.host.yaml up -d
```

The host URL is then `http://127.0.0.1:18080/v1`. The container port stays 8080. Keep the same file flags and port setting for later Compose updates while host access is enabled.

Check that the default host port responds:

```fish
curl --fail http://127.0.0.1:8080/healthz
```

Health checks need no key. Models and Responses requests still require `Authorization: Bearer <gateway-client-key>`. For a remote Docker host, use an SSH tunnel to reach the published port from your computer:

```fish
ssh -N -L 8080:127.0.0.1:8080 your-server
```

Disable host access by applying only the base configuration. Compose recreates the gateway with its private API; saved credentials and client keys remain in the volume:

```fish
docker compose -f compose.yaml up -d
```

Changing port mappings recreates the container and can interrupt active requests.

## Connect OpenAI

Run the login helper on the computer with your browser. Build the helper with Go 1.26.6 or newer:

```fish
go build -o gateway ./cmd/gateway
```

For a local Docker stack, run the helper from the Compose directory:

```fish
./gateway auth login
```

For a remote stack, give the SSH destination and absolute Compose directory on that server:

```fish
./gateway auth login --ssh your-server --directory /absolute/path/to/openaisub-gateway
```

For remote login, the SSH user must be able to run Docker Compose in that directory without an interactive privilege prompt. Build and run the helper on your own computer; the gateway must already be running on the server.

The helper uses a local browser callback and imports credentials into the running container. Remote import uses SSH and standard input. No manual token copying is required. The gateway renews tokens automatically. Revoked or expired refresh credentials require another login.

The issued registration is saved before token exchange, so a failed first attempt can reuse it. Transfer retries reuse the same credentials safely. If transfer still fails, the helper attempts to revoke the unused session and reports whether cleanup succeeded.

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

The gateway wraps flat function definitions in the upstream `gateway` namespace. It adds that namespace to function calls in input history, removes it from returned calls, and flattens echoed tool definitions. Function names, arguments, results and call IDs stay unchanged. Caller-defined namespaces are rejected. Scalar text input becomes a user-message array; omitted storage becomes `store:false`.

The example disables both Mastra retry paths. `modelSettings.maxRetries: 0` disables model-call retries; `errorProcessorDefaults: false` disables the separate default error processors, which otherwise retry transient failures. Choose retries explicitly in your own client. A replay can repeat model work or tool side effects.

The [executable Mastra example](examples/mastra/) pins `@mastra/core` 1.74.0, `@ai-sdk/openai` 4.0.83 and Zod 4.6.5. It checks generation, streaming, a tool round trip and native JSON schema output. Follow [live verification](#verify-with-your-subscription) below to run it against your account.

## API and configuration

Clients send `Authorization: Bearer <gateway-client-key>` to `http://gateway:8080`, to `http://127.0.0.1:8080` when host access is enabled, or to the public HTTPS domain in the [Portainer setup](docs/portainer.md). Use a key created in the dashboard, not the administrator password or an OpenAI token.

- `GET /v1/models` returns the connected account's models in OpenAI-compatible `data[].id` format. Results are cached briefly; there is no bundled model list.
- `POST /v1/responses` accepts complete or streaming inference requests. Set `stream:true` for server-sent events. Clients must send the full conversation history on each request.
- `GET /healthz` returns `ok` without authentication on the private listener. The supplied Caddy route returns 404 for this path publicly. It checks that the process responds, not that OpenAI is connected or available.

The shipped Compose configuration sets these variables. Defaults work for the supplied container:

- `GATEWAY_DATA_DIR`: `/data`, the persistent state directory.
- `GATEWAY_API_ADDR`: `:8080`, the private inference listener.
- `GATEWAY_ADMIN_ADDR`: `:8081`, the administrator listener, published only on host loopback.
- `GATEWAY_ADMIN_SECRET_FILE`: `/run/secrets/admin_secret`, the administrator password file.
- `GATEWAY_ENCRYPTION_KEY_FILE`: `/run/secrets/encryption_key`, the credential encryption key file.

Expose public inference only through the [documented HTTPS proxy route](docs/portainer.md); keep the dashboard on host loopback. The administrator password cannot authenticate inference requests; client keys cannot access the dashboard.

`GATEWAY_HOST_PORT` is a Compose setting for `compose.host.yaml`, not a gateway process variable. It defaults to 8080 and does not affect the internal API or administrator port.

## Verify with your subscription

Connect OpenAI and create a client key first. This check makes real inference requests and uses subscription capacity. Fixture tests alone do not establish account eligibility or preview availability.

Run the check on the Docker host, from the repository root. Create a private environment file, then use your editor to fill in the values shown below. The `secrets/` directory is excluded from Git:

```fish
touch secrets/mastra.env
chmod 600 secrets/mastra.env
```

```dotenv
GATEWAY_API_KEY=replace-with-your-gateway-client-key
GATEWAY_MODEL=replace-with-a-model-id
```

Save this optional service as `compose.check.yaml` beside `compose.yaml`. It joins the gateway's Compose network and uses Node.js 24. Node.js is not required on the host:

```yaml
services:
  mastra-check:
    image: node:24-bookworm-slim
    working_dir: /work
    volumes:
      - ./examples/mastra:/work:ro
      - mastra-check-deps:/work/node_modules
    env_file:
      - ./secrets/mastra.env
    environment:
      GATEWAY_BASE_URL: http://gateway:8080/v1
    command: [sh, -c, "npm ci --ignore-scripts --no-audit --no-fund && npm run smoke"]

volumes:
  mastra-check-deps:
```

List the models available to your connected account, then replace `GATEWAY_MODEL` in `secrets/mastra.env` with one of the returned IDs:

```fish
docker compose -f compose.yaml -f compose.check.yaml run --rm mastra-check node --input-type=module -e '
const response = await fetch(process.env.GATEWAY_BASE_URL + "/models", {
  headers: { Authorization: "Bearer " + process.env.GATEWAY_API_KEY },
});
if (!response.ok) throw new Error("Models request failed: HTTP " + response.status);
for (const model of (await response.json()).data) console.log(model.id);
'
```

Run the live check:

```fish
docker compose -f compose.yaml -f compose.check.yaml run --rm mastra-check
```

Success prints `PASS generate`, `PASS stream`, `PASS tool round trip` and `PASS structured JSON`. The script does not print prompts, responses or credentials. Record the model, package versions and result before claiming deployment readiness. Leave `MASTRA_FIXTURE` unset for live checks.

If a check fails, inspect the dashboard connection status and the last printed `PASS`. An invalid gateway key returns HTTP 401; replace or recreate that client key. A reconnect warning requires another `auth login`. Upstream quota or availability failures require waiting or choosing an available model; the gateway does not retry inference or switch billing methods.

## Persistence and recovery

The `gateway-data` named volume holds SQLite state, client key hashes, registration and encrypted OpenAI credentials. Separate host files hold the administrator secret and encryption key. Keep both secret files out of source control and backups accessible only to you.

- Stop the gateway before copying the complete data volume. Back up the encryption key separately and securely. Restore the matching volume and key with UID 10001 ownership before starting one gateway instance.
- Losing the encryption key makes stored OpenAI credentials unreadable. Preserve the old volume while preparing a fresh data directory and new secrets, then reconnect OpenAI and issue new client keys.
- Losing a client key requires creating a replacement and revoking the old one. Keys cannot be recovered from their hashes.
- A reconnect warning requires signing in again. A temporary upstream failure does not require deleting state.
- Disconnect removes local credentials and attempts remote revocation. An unconfirmed revocation means you should also remove access in your OpenAI account.

Do not run multiple gateway processes against the same volume. Do not publish either listener directly to the internet. The public setup routes only inference through the existing HTTPS proxy. Local root, Docker administrators, containers on the shared proxy network, and processes that can read the encryption key and database are trusted. The Portainer stack uses the separate named volume `openaisub-gateway-data`; it does not automatically import an existing private Compose deployment.

## Develop and verify

```fish
npm --prefix examples/mastra ci --ignore-scripts --no-audit --no-fund
env MASTRA_CONTRACT=1 go test -race ./...
go vet ./...
gofmt -l .
```

Formatting should report no files. CI runs the Go checks and real Mastra client against a controlled upstream fixture. It also runs the native container to check secret initialization, dashboard login, restart persistence and single-process ownership, then builds Linux amd64 and arm64 images without publishing them. Fixture-only cancellation and error cases verify client behavior without sending deliberately invalid live requests.

After building the image locally, run the same container check with Docker running:

```fish
env DOCKER_CONTRACT=1 go test -v ./integration -run TestContainer
```

The example needs Node.js 22.13 or newer when run outside Docker. See [live verification](#verify-with-your-subscription) for the Docker procedure and [fixture protocol](examples/mastra/README.md#fixture-protocol) for the controlled upstream checks.

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
