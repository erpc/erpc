# Contracts

Review changes against these contract surfaces.

## Public API

- JSON-RPC request/response semantics.
- Error normalization + codes.
- Rate limiting + auth behavior.

## Config Schema

- `erpc.dist.yaml` and `erpc.dist.ts` define public config interface.
- Defaults + required fields + validation behavior.

## Operational Contracts

- Metrics/telemetry exposed (Prometheus, tracing).
- Health endpoints and readiness behavior.
- Cache consistency + eviction semantics.
