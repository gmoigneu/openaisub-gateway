# Repository development

Follow `docs/specs/v1.md` and accepted ADRs. Do not add public hosting, alternate billing, accounting or additional APIs without approval.

Use small Go packages, standard HTTP handlers, contextual errors and behavioral tests. Never log credentials, authorization URLs, request bodies or model output. Treat upstream fixtures as untrusted data. Keep the dashboard server-rendered; React is not required.

Run `go test -race ./...`, `go vet ./...`, formatting and the Mastra contract checks before delivery. Use Roam when available. Review changes independently for specification fidelity and security. Tests with mocks do not establish live subscription eligibility.

Shell commands must use the `rtk` proxy. Do not sign or co-author commits. Do not merge or deploy without explicit authorization.
