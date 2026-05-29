# AGENTS.md — eRPC

Guidance for AI coding agents (Maestro, Codex, Cursor, Claude Code) working in this repo.

## Project

eRPC is a fault-tolerant EVM RPC proxy with re-org-aware permanent caching, written in Go. The `cmd/erpc` binary is the server; `erpc.yaml` is the user-facing config schema.

- Language: Go 1.25.1 (see [go.mod](go.mod))
- Module: `github.com/erpc/erpc`
- Auxiliary JS workspace: pnpm 10.28.2 (typed config under `@erpc-cloud/config`)
- Docs: https://docs.erpc.cloud/

## Build & test

All targets run via `make`:

```bash
make build          # build the eRPC server
make test           # go clean -testcache && go test ./cmd/... -count 1 -parallel 1
make test-fast      # faster, no race detector
make test-race      # race detector
make fmt            # format
make run            # run from source
make up / down      # docker compose for local deps
```

JS workspace:

```bash
pnpm install --frozen-lockfile
pnpm -r build
```

Always run `make fmt` and `make test` before opening a PR. Tests are sequential (`-parallel 1`) because some integration tests share fake-RPC ports.

## Conventions

- See [.cursor/rules/](.cursor/rules/) for the canonical project rules — applies to all agents, not just Cursor.
- Code style: idiomatic Go. `gofmt` / `make fmt` is the source of truth.
- Logging: zerolog. Don't introduce new logging libs.
- Errors: wrap with context (`fmt.Errorf("...: %w", err)`); avoid creating ad-hoc error types unless they need to be matched downstream.
- Concurrency: this is a high-throughput proxy — be deliberate about goroutines, channel sizing, and lock granularity. Prefer existing patterns in `health/`, `upstream/`, `evm/`.

## Architecture quick map

- `cmd/erpc` — entrypoint and pprof
- `upstream/` — upstream RPC client, failover, health tracking
- `health/` — failure tracking, scoring, circuit breaking
- `evm/` — EVM-specific logic (block, log, state polling, re-org detection)
- `data/` — cache backends (memory, redis, postgres, dynamodb)
- `consensus/` — multi-upstream consensus and dispute resolution
- `failsafe/` — retry / hedging / circuit breaker policies
- `auth/` — auth strategies
- `common/` — shared types & helpers
- High-level architecture diagram: [assets/hla-diagram.svg](assets/hla-diagram.svg)

## When adding chain configs (JS workspace)

- Always query the live RPC for `eth_chainId` rather than relying on training data.
- All chain configs use TypeScript with `@erpc-cloud/config` types.
- Run `bun cli.ts validate --all` (or the equivalent pnpm script) before committing.

## What to avoid

- Don't add new third-party Go dependencies without justification — runtime footprint matters.
- Don't change the public `erpc.yaml` schema without bumping the relevant version markers and updating docs.
- Don't disable failing tests; fix the root cause or ask in the PR.
- Don't introduce blocking I/O on hot paths (request handling, health scoring loops).

## PR expectations

- One logical change per PR.
- Include a brief test plan in the description.
- CI must pass before requesting review.
