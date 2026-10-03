# Official subscription Responses integration

Status: accepted, 2026-10-03.

## Context

The owner needs subscription inference for private Mastra agents with low maintenance. Codex device authentication and private backend endpoints do not establish compatibility with the documented OSS subscription grant.

## Decision

We will use official OSS Sign in with ChatGPT and the public Responses and Models endpoints. We will provide a local browser login helper with SSH-assisted transfer for remote Docker. We will expose the supported Responses subset and reject unsupported semantics explicitly.

## Consequences

Dynamic discovery avoids maintaining a model catalog. Public documentation supplies the authentication contract. The preview still requires compatibility tests and may change. Remote onboarding requires a helper beside the browser. Chat Completions and embeddings remain outside v1.
