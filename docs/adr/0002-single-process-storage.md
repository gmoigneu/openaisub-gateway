# Single-process persistent gateway

Status: accepted, 2026-10-03.

## Context

One owner deploys the gateway beside Mastra in one Docker stack. Rotating refresh tokens must have one writer. A separate database service and frontend build would increase setup and maintenance.

## Decision

We will ship one Go binary with embedded dashboard assets and SQLite on a persistent volume. We will separate inference and administrator listeners, hash client keys, and encrypt OAuth credentials with a separately mounted key. We will serialize refresh and use revision checks for token writes.

## Consequences

Backups consist of the database and separately protected secret. There is no horizontal scaling in v1. Losing the encryption key requires reconnecting OpenAI. The admin secret must remain distinct from inference keys.
