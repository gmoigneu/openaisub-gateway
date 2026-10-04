# Paid embeddings and voice expansion

Status: proposed, 2026-10-04. Governing issue: [#7](https://github.com/gmoigneu/openaisub-gateway/issues/7). Owner decisions: use a separately billed OpenAI API key; support embeddings, speech-to-text and text-to-speech; allow every valid gateway client key to call the paid endpoints.

This specification extends [v1](v1.md). The subscription sign-in, Models and Responses behavior in v1 remain in force. Its exclusion of embeddings applies only to the original subscription-only release. The accepted [subscription ADR](../adr/0001-subscription-responses.md) remains the decision for Responses. [ADR 0004](../adr/0004-separate-paid-api-credential.md) proposes the paid route architecture.

## Outcome and boundaries

Mastra clients use the same gateway base URL and revocable client key for three additional OpenAI-compatible endpoints:

- `POST /v1/embeddings` for text embeddings.
- `POST /v1/audio/transcriptions` for speech-to-text.
- `POST /v1/audio/speech` for text-to-speech.

These calls use the owner's separately billed OpenAI Platform API key. Responses and the account-specific `GET /v1/models` catalog continue to use the existing ChatGPT plan grant. The Models catalog does not claim to list paid embedding or audio models; clients select those models explicitly. No Chat Completions, Realtime, audio in Responses, translations, custom voices, alternate provider, billing fallback, usage accounting, quotas or content storage are in scope.

Every existing and future valid gateway client key may use these paid endpoints. Revocation blocks subsequent calls. A key holder can incur paid usage; the operator must manage OpenAI project limits and key access. The gateway does not silently switch a failed subscription request to paid billing.

## Credential and route design

Read the Platform API key from a separate, owner-supplied, read-only secret file. Never put it in an environment variable, command argument, URL, dashboard response, log or database. The paid routes are optional: without a configured key, authenticated calls return a clear unavailable error. The key is never sent to gateway clients. Document secret setup and replacement for both private Compose and Portainer deployment; replacement takes effect after a gateway restart.

Keep the fixed official OpenAI origin and exact upstream paths. Replace the caller's Authorization header with the Platform API key only on paid routes. Do not forward caller-supplied organization, project, routing, cookies or arbitrary credential headers. Reject redirects, query strings and unsupported methods. Do not retry a request automatically. Preserve upstream status, error body and request ID where safe.

Authenticate every new route with the existing gateway key middleware, including through Caddy. Expand the public Caddy allowlist by exactly these three paths. Keep `/healthz`, administration, credential import and all other routes inaccessible through the public proxy.

## Endpoint behavior

Accept standard JSON embedding and speech requests and multipart transcription uploads. Pass supported OpenAI request fields through without changing model choice or response data. Preserve JSON results for embeddings and transcriptions, speech media types and binary audio, and documented streaming formats when requested. Flush streamed bytes or events promptly and propagate cancellation. An upstream interruption is not a successful completed response.

Apply separate, documented size bounds suitable for text JSON, multipart audio and generated audio. Bound request headers and upload time; do not impose a short global response deadline on a speech stream. Reject malformed or oversized requests before sending them upstream when practical. Never retain request bodies, recordings, transcripts, vectors or generated audio, and never log them.

The dashboard may show whether the paid key is configured, but must not reveal it. Existing OAuth connection status must remain distinct from paid API-key status.

## Verification

- Behavioral tests prove route and method allowlists, gateway-key authentication and revocation, credential separation, header stripping, no redirects or retries, missing-key errors, size limits, error and request-ID forwarding, and client cancellation.
- Contract checks exercise the pinned Mastra/OpenAI provider against controlled embeddings, transcription and speech fixtures, including binary audio and relevant streaming modes. Fixtures are untrusted data and cannot prove paid-account eligibility.
- Container and Caddy checks prove the three routes are exposed only through the authenticated inference path, the paid secret is mounted read-only, and admin routes remain private.
- Run the repository's Go race tests, vet, formatting, Mastra contract checks and relevant container checks. Independently review specification fidelity and security. Before claiming live paid support verified, the owner runs one real request for each endpoint with a configured paid key and records outcomes and model IDs without credentials or content.

Expected SemVer impact: MINOR for the added backward-compatible endpoints. No release or deployment is part of this change.

## Sources checked 2026-10-04

- https://developers.openai.com/siwc/token-sharing-open-source/models-and-inference
- https://developers.openai.com/siwc/token-sharing-open-source/preview-limitations
- https://developers.openai.com/api/docs/guides/embeddings
- https://platform.openai.com/docs/api-reference/audio
- https://ai-sdk.dev/providers/ai-sdk-providers/openai
