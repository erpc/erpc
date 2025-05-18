# Repository Guide for AI Agents

This project hosts **eRPC**, a fault-tolerant EVM RPC proxy with built-in caching, failover logic and a TypeScript CLI. The repository contains Go code for the server and TypeScript packages for tooling and configuration.

## Local Development

### Go code
- Install dependencies with `make setup`.
- Format all Go files with `make fmt`.
- Build the server with `make build` or run it via `make run`.
- Execute the unit tests quickly with `make test-fast` (no race checks).
- Run the full suite with race checks using `make test` when needed.
- To run a single test or pattern, use `LOG_LEVEL=trace go test -run <pattern> ./...`.

### Node/TypeScript packages
- Use `pnpm install` to install dependencies.
- Format TypeScript sources with `pnpm -r run format` (uses [Biome](https://biomejs.dev/)).
- Build all packages with `pnpm -r run build`.


## Commit and PR Guidelines
- Follow the [Conventional Commits](https://www.conventionalcommits.org/) style (e.g. `feat:`, `fix:`) when writing commit messages.
- Create feature branches for new work and open Pull Requests against `main`.
- Ensure `make fmt` and `make test` succeed before pushing your changes.
- Reference relevant issues in the PR description when applicable.

## Additional Notes
- All contributions fall under the [Contributor License Agreement](CLA.md) and require adherence to the [Code of Conduct](CODE_OF_CONDUCT.md).
- Documentation lives under the `docs/` directory. See `README.md` for a project overview and quick start instructions.
