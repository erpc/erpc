# AGENTS.md — eRPC

Pointer file for cross-tool AI coding agents (Maestro, Codex, Cursor,
Claude Code). The canonical project rules live in
[`.cursor/rules/erpc.md`](.cursor/rules/erpc.md) — read that file first;
it applies to all agents, not just Cursor.

This file only repeats the bare minimum needed to bootstrap.

## Project

- **eRPC** — fault-tolerant EVM RPC proxy with re-org-aware permanent caching.
- Go server (`cmd/erpc`) + TypeScript packages under `typescript/` (`@erpc-cloud/config`, `@erpc-cloud/cli`).
- Module: `github.com/erpc/erpc`. Docs: <https://docs.erpc.cloud/>.

## Bootstrap commands

```bash
make setup           # go mod tidy
pnpm install --ignore-scripts   # JS workspace (skip postinstall, matches CI)

make fmt             # format Go (also Biome for TS via pnpm)
make build           # build the eRPC server
make test-fast       # parallel-sharded unit tests (daily-driver)
make test            # full suite with -race (slow, run before merge / CI runs this)
make test-race       # race detector on a slimmer scope
```

Single test: `LOG_LEVEL=trace go test -run <pattern> ./...`

## PR expectations

- One logical change per PR.
- `make fmt` clean.
- `make test-fast` green locally; `make test` (race) green in CI.
- Brief test plan in the description.

## Hard rules from `.cursor/rules/erpc.md`

These bite hard if missed; read the cursor rules for the full version:

- **Test logger init:** every test file that logs must include
  `func init() { util.ConfigureTestLogger() }`.
- **Gock setup order:** set up *all* gock mocks **before** initializing any
  network components. Always `util.ResetGock()` + `defer util.ResetGock()`.
- **Logging:** zerolog only (`log.Logger` from `github.com/rs/zerolog/log`).
- **Errors:** wrap with context (`fmt.Errorf("...: %w", err)`).
- **Don't** change the public `erpc.yaml` schema without version markers + docs.
- **Don't** disable failing tests — fix the root cause or surface it in the PR.
