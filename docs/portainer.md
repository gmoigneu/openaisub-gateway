# Deploy with Portainer and your existing Caddy proxy

Run one gateway container on a Docker Standalone host. Your existing Caddy proxy handles HTTPS and exposes only the Models and Responses endpoints. Every request to those endpoints needs a valid gateway client key. The dashboard stays on host loopback and is reached through SSH.

This guide uses [compose.portainer.yaml](../compose.portainer.yaml) and [examples/Caddyfile](../examples/Caddyfile). It does not install Caddy or manage certificates. Portainer Swarm is outside this recipe.

**Live OpenAI sign-in and a subscription-backed Mastra tool round trip still need owner verification.** Fixture checks do not prove account eligibility. A valid client key can consume the owner's subscription capacity. There are no per-key quotas, rate limits or network denial-of-service protections in this change.

## 1. Prepare the Docker host

You need:

- A Docker Standalone environment managed by Portainer, with SSH access and permission to run Docker.
- An existing Caddy HTTPS proxy on the same host, plus a domain such as `gateway.example.com` pointed at that proxy.
- Outbound HTTPS access from the gateway to OpenAI.
- Go 1.26.6 or newer on the computer with your browser, for the local login helper.

Commands use the `rtk` command proxy and are compatible with fish. Replace the sample domain and SSH destination before use.

Run the following on the **actual Docker host selected in Portainer**, not on the Portainer server if it manages another host. No gateway registry image is published. Build the preview image locally:

```fish
rtk git clone --branch main https://github.com/gmoigneu/openaisub-gateway.git
rtk proxy docker build -t openaisub-gateway:public-preview ./openaisub-gateway
```

If you already have a checkout, fetch and select that branch before building. Keep your current changes before switching branches. Later instructions that refer to repository files use this checkout.

Initialize the two secret files at their default absolute host path:

```fish
rtk proxy sudo install -d -m 700 /opt/openaisub-gateway/secrets
rtk proxy docker run --rm --user 0 --volume /opt/openaisub-gateway/secrets:/bootstrap openaisub-gateway:public-preview init --secrets-dir /bootstrap --owner 10001
```

Initialization preserves existing files. New files belong to UID 10001 and have mode 0600. The serving container runs as that unprivileged user; root is used only for this setup command. Do not make the secrets world-readable or paste their contents into Portainer environment variables.

For an existing gateway, preserve its data volume and matching encryption key. This recipe creates `openaisub-gateway-data` by default; it does not migrate a private Compose deployment. Never run two gateway processes against the same volume.

## 2. Connect to the existing Caddy network

For **Caddy running in Docker**, find the network attached to its container:

```fish
rtk proxy docker inspect caddy --format '{{json .NetworkSettings.Networks}}'
```

Replace `caddy` with the actual Caddy container name. Set `GATEWAY_PROXY_NETWORK` in the next step to an existing user-defined network from this result. The default is `caddy`. Preserve Caddy's attachment in its own stack configuration so a later Caddy update does not remove it. Docker documents [how separate Compose projects share an external network](https://docs.docker.com/compose/how-tos/networking/).

Containers on this network are trusted: they can reach the gateway's container listeners. Do not attach untrusted workloads to it. The gateway stack publishes no host inference port in this configuration.

For **Caddy running directly on the host**, skip the network setup and use the host-based variation in step 4. This guide does not expose an unencrypted API port to a proxy on another machine.

## 3. Create the Portainer stack

In your Docker Standalone environment, select **Stacks → Add stack → Web editor**, name the stack `openaisub-gateway`, and paste [compose.portainer.yaml](../compose.portainer.yaml). See Portainer's [stack creation instructions](https://docs.portainer.io/user/docker/stacks/add).

Under environment variables, set any values that differ from these defaults:

```dotenv
GATEWAY_IMAGE=openaisub-gateway:public-preview
GATEWAY_CONTAINER_NAME=openaisub-gateway
GATEWAY_PROXY_NETWORK=caddy
GATEWAY_SECRETS_DIR=/opt/openaisub-gateway/secrets
GATEWAY_DATA_VOLUME=openaisub-gateway-data
GATEWAY_ADMIN_PORT=8081
```

These variables select the image, paths, network and host admin port. They contain no passwords or client keys. Secret paths must exist on the target Docker host. Both secret files are mounted separately and read-only; the stack rejects missing source files.

If Caddy runs directly on the host, make the changes described in step 4 before deployment. Otherwise, select **Deploy the stack**. Keep any option to force an image pull disabled because the preview image exists only on this Docker host. If Portainer reports an unavailable image, verify that the image was built in this environment.

The stack starts one container with automatic restart, a read-only root filesystem, dropped capabilities, a persistent data volume, and the dashboard bound to `127.0.0.1:8081` on the host. No OpenAI connection or client key exists yet.

## 4. Add the public route to Caddy

For Caddy in Docker, add this site to its existing Caddyfile and replace the domain:

```caddyfile
gateway.example.com {
    @inference path /v1/models /v1/responses
    handle @inference {
        reverse_proxy openaisub-gateway:8080
    }
    handle {
        respond 404
    }
}
```

The stack sets `openaisub-gateway` as a network alias. That upstream name remains valid if you change `GATEWAY_CONTAINER_NAME`. Use a distinct alias if you deploy another gateway on the same network.

Validate and reload through your existing Caddy deployment process. For a Caddy container named `caddy` with its file at `/etc/caddy/Caddyfile`:

```fish
rtk proxy docker exec caddy caddy validate --config /etc/caddy/Caddyfile
rtk proxy docker exec caddy caddy reload --config /etc/caddy/Caddyfile
```

Leave request buffering and stream timeouts unset, and keep proxy retries disabled. Caddy flushes server-sent events immediately with its normal settings. Do not add `flush_interval -1`: that mode keeps the upstream request running after the client disconnects. See Caddy's [reverse proxy streaming behavior](https://caddyserver.com/docs/caddyfile/directives/reverse_proxy#streaming).

Do not enable request-body or response-body logging, and do not configure Caddy to log Authorization headers or credentials. Keep the catch-all 404 route. Do not proxy port 8081, `/healthz`, `/admin`, or a wildcard path to the gateway.

### If Caddy runs directly on the host

In the Portainer stack, remove the gateway service's `networks` block and the top-level `networks` block. Replace the service's `ports` block with:

```yaml
    ports:
      - "127.0.0.1:${GATEWAY_ADMIN_PORT:-8081}:8081"
      - "127.0.0.1:8080:8080"
```

Keep the rest of the stack unchanged. Change only the upstream in the Caddy site to:

```caddyfile
        reverse_proxy 127.0.0.1:8080
```

Deploy the stack, then validate and reload your existing host Caddy service. Do not remove `127.0.0.1` from either mapping. Ports 8080 and 8081 must never become public host bindings. Your existing Caddy deployment remains responsible for HTTPS.

## 5. Open the private dashboard

On your own computer, start an SSH tunnel and leave it open:

```fish
rtk proxy ssh -N -L 8081:127.0.0.1:8081 your-server
```

If you changed `GATEWAY_ADMIN_PORT`, change the second `8081`, the remote destination port. The first is the local browser port.

In another private terminal on your computer, read the administrator password:

```fish
rtk proxy ssh your-server docker exec openaisub-gateway /gateway admin password
```

Use the configured container name if you changed it. Open [http://127.0.0.1:8081](http://127.0.0.1:8081) and sign in. Keep the password out of screenshots, logs and shared terminals. It is only for administration and cannot authorize inference.

## 6. Connect your OpenAI account

On your own computer, get the same source branch and run this from the repository root:

```fish
rtk proxy go build -o gateway ./cmd/gateway
rtk proxy ./gateway auth login --ssh your-server --container openaisub-gateway
```

The helper opens your browser locally and receives the local callback. SSH transfers registration and credentials to the running container through standard input. Tokens are not placed in command arguments, stack variables or output. The SSH account must run Docker without an interactive privilege prompt. The gateway renews its own tokens after import.

If Docker and your browser run on the same computer, omit SSH:

```fish
rtk proxy ./gateway auth login --container openaisub-gateway
```

`--container` accepts a running Docker container name or ID. Do not combine it with `--directory`. Container targeting avoids depending on Portainer's internal Compose directory. If the dashboard later requests reconnection, run the same login command again.

## 7. Create a key and verify public access

In the dashboard, create a client key with an app-specific name. Save it in that app's secret store when displayed; the full key is shown only once. The gateway stores its hash. Create a separate key for each app so you can revoke one without affecting the others.

From outside the Docker host, run these checks with your domain:

```fish
rtk proxy curl -sS -o /dev/null -w '%{http_code}\n' https://gateway.example.com/v1/models
rtk proxy curl -sS -o /dev/null -w '%{http_code}\n' -H 'Authorization: Bearer invalid' https://gateway.example.com/v1/models
rtk proxy curl -sS -o /dev/null -w '%{http_code}\n' -X POST https://gateway.example.com/v1/responses
rtk proxy curl -sS -o /dev/null -w '%{http_code}\n' https://gateway.example.com/healthz
rtk proxy curl -sS -o /dev/null -w '%{http_code}\n' https://gateway.example.com/admin
```

Expect `401`, `401`, `401`, `404`, and `404`, in that order. Stop and correct the routing if health or admin paths succeed publicly. An unreachable domain or TLS failure must be fixed in DNS or your existing proxy before continuing.

Use this fish command to check authenticated models. It prompts for a key without saving it in shell history or placing it in curl's arguments; only the HTTP status is printed:

```fish
rtk proxy fish -c '
    read --silent --prompt-str "Gateway client key: " gateway_key
    printf "Authorization: Bearer %s\n" "$gateway_key" | rtk proxy curl -sS -o /dev/null -w "%{http_code}\n" --header @- https://gateway.example.com/v1/models
    set --erase gateway_key
'
```

Expect `200` after successful OpenAI connection. To see available model IDs, repeat the command without `-o /dev/null`; models appear in `data[].id`. For a revocation check, create a temporary key, verify `200`, revoke it in the dashboard, and repeat with that key. Expect `401` immediately for subsequent requests. Revocation does not cancel inference already in progress.

Configure your app with:

```dotenv
GATEWAY_BASE_URL=https://gateway.example.com/v1
GATEWAY_API_KEY=your-apps-gateway-client-key
GATEWAY_MODEL=a-model-id-from-the-authenticated-models-endpoint
```

Use the [Mastra configuration](../README.md#configure-mastra), setting the provider's `baseURL` to the HTTPS URL. Keep `store:false`, developer instructions, and the Responses model. The administrator password and an OpenAI token are not gateway client keys.

Before relying on the deployment, run the [Mastra smoke example](../examples/mastra/README.md) against the public URL with your key and an available model. Verify generation, streaming, a tool round trip and structured JSON. These checks use real subscription capacity. Record the model, package versions and outcome without prompts or credentials.

## Updates, backups and recovery

Keep `GATEWAY_DATA_VOLUME`, `GATEWAY_SECRETS_DIR`, and both secret files stable across updates. Never delete the data volume as part of a normal redeployment.

For an update, build the reviewed revision on the same Docker host with a new image tag. Set `GATEWAY_IMAGE` to that tag in Portainer and update the stack without forcing a pull. A new tag makes the selected image explicit and retains the previous image for rollback. Restarting or recreating the container can interrupt active requests. Repeat the access checks after an update.

Stop the gateway before backing up the complete `openaisub-gateway-data` volume. Back up both secret files separately with restricted access. Restore the matching volume and encryption key with UID 10001 ownership before starting one instance. Root and Docker administrators can access these files and are trusted.

- Lost client key: create a replacement and revoke the old key. A hash cannot recover the original.
- Lost encryption key: saved OpenAI credentials cannot be decrypted. Preserve the old state before preparing fresh state and reconnecting.
- Reconnect required: rerun the local login helper. Do not delete state for a temporary upstream failure.
- Public `401`: check that the app uses a current gateway client key.
- Public `502` or another upstream error: check the dashboard connection status, Caddy's upstream address, and the shared network. Never disable key checks to diagnose connectivity.

The gateway does not retry inference automatically or switch billing methods. Keep the last working image and a matching backup until an update passes your live checks.
