# Public inference behind HTTPS

Status: accepted, 2026-10-04. The owner approved public inference and specified their existing Caddy proxy for HTTPS termination. Governing issue: [#3](https://github.com/gmoigneu/openaisub-gateway/issues/3).

## Context

The owner needs a gateway that runs independently of agent applications and accepts internet requests with a valid client API key. Portainer must manage deployment. Existing client keys are random, hashed and revocable. Administration and OAuth onboarding already use a separate listener and SSH.

## Decision

We will provide a Docker Standalone Portainer stack with one gateway behind the owner's existing Caddy HTTPS proxy. We will not bundle a proxy or add TLS handling to the project. Caddy routes only Models and Responses to the inference listener; the dashboard, credential import and health check stay private. The gateway remains responsible for API-key validation, including revocation.

We will keep administrator access on host loopback through SSH and add a login-helper option to target a named container, without requiring Portainer's Compose directory. Persistent state and separate secret files survive restarts. Secrets stay out of stack environment variables.

We will preserve the private Compose default, single-process storage, streaming and cancellation. Container-based Caddy shares an external Docker network with the gateway. Host-based Caddy can use a loopback-only inference port on the same host. Public deployment requires a domain served by the existing HTTPS proxy, outbound HTTPS from the gateway, and one trusted owner. It does not add accounts, billing or shared-service hosting.

## Consequences

The owner manages DNS, certificates and Caddy configuration through their existing deployment. The gateway publishes no additional internet ports. Portainer Swarm is outside this deployment recipe. Containers sharing the proxy network are trusted because they can reach container listeners. A valid key permits use of the owner's subscription capacity; this change adds no per-key quota or rate limit. Network denial-of-service protection remains outside the gateway.

Acceptance requires rejected missing, invalid and revoked keys; no public admin routes; persistent container state; safe SSH argument handling; proxy streaming/cancellation; and the Go, Mastra and Docker checks. Live subscription eligibility still requires the owner check in the canonical specification.
