# AGENTS.md

Guidance for coding agents (Codex, etc.) working in this repository.

The authoritative rules — architecture, layering, commands, and conventions — live in [CLAUDE.md](./CLAUDE.md). Read it first and follow it exactly.

Key non-negotiables:
- Clean/Hexagonal layering: `transport → usecase → domain ← infrastructure`.
- This is a **public** project: no private/internal dependencies. Public modules + Go stdlib only.
- Run `gofmt -w . && go vet ./...` before finishing any change.
