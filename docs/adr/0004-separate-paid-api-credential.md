# Separate paid API credential for embeddings and voice

Status: accepted, 2026-10-04. Governing issue: [#7](https://github.com/gmoigneu/openaisub-gateway/issues/7).

## Context

The gateway uses the open-source Sign in with ChatGPT grant for subscription-backed Models and Responses calls. OpenAI documents embeddings and audio as Platform API operations, but does not document them for this grant; its preview explicitly excludes audio input and transcription. The owner wants OpenAI embeddings, speech-to-text and text-to-speech through the same gateway and accepts separate API billing. Existing gateway client keys should work on the new routes.

## Decision

We will add exact embeddings, transcription and speech routes backed only by an owner-supplied OpenAI Platform API key in a separate read-only secret file. We will keep subscription credentials for Models and Responses and will never switch a request between billing routes. The existing gateway client-key check will protect every route, and the public Caddy allowlist will expose only the supported inference paths.

## Consequences

Clients keep one base URL and one revocable gateway key. The owner must provide, protect and pay for a Platform API key, then restart the gateway after replacement. Every valid client key can incur paid usage; the gateway adds no per-key quotas or usage accounting. Separate credential paths reduce the chance of sending an OAuth token to a paid route or billing a failed subscription request. More request types and binary streams increase the handler, test and proxy review scope.
