# KB: Deployment — Docker, Kubernetes, Monitoring (Prometheus + Grafana)

> status: complete
> source-dirs: Dockerfile, Dockerfile.fake, docker-compose.yml, kube/, monitoring/, prometheus.yaml, Makefile

## L1 — Capability (CTO view)

eRPC ships a multi-stage Dockerfile that produces a minimal distroless image (~30 MB compressed) containing both the main server binary and a pprof-instrumented variant, plus compiled TypeScript config support. Kubernetes manifests, a combined Prometheus+Grafana monitoring container, and Makefile targets provide a complete local-to-production deployment story. The monitoring stack includes a pre-built Grafana dashboard with 80+ panels covering request rates, latency, failover events, consensus, cache, selection policy, and block-head lag.

## L2 — Mechanics (staff-engineer view)

**Dockerfile build stages.** Five named build stages: `go-builder` compiles both `erpc-server` (production) and `erpc-server-pprof` (pprof build tag); `ts-core` installs pnpm globally on Node 20 alpine; `ts-dev` installs all dependencies and runs `pnpm build` to compile the TypeScript config SDK; `ts-prod` installs production-only dependencies; `symlink` creates `/root/erpc-server → /erpc-server` for backwards compatibility with older mount paths; `final` is a `gcr.io/distroless/static-debian12:nonroot` image that receives only the two Go binaries, the symlink, and the compiled TypeScript files (`Dockerfile:L70-L88`). CGO is disabled (`CGO_ENABLED=0`), `-ldflags="-w -s"` strips debug info and symbol table, and `VERSION`/`COMMIT_SHA` build args are injected via `-X` ldflags into `common.ErpcVersion` and `common.ErpcCommitSha` (`Dockerfile:L24-L34`). The pprof binary is built with `-tags pprof` so profiling endpoints are only present in the explicit pprof variant (`Dockerfile:L33-L34`).

**Ports exposed.** The final image exposes three ports (`Dockerfile:L84`):
- `4000` — JSON-RPC HTTP ingress (default `server.httpPortV4`)
- `4001` — Prometheus metrics (default `metrics.port`)
- `6060` — pprof (only active in `erpc-server-pprof`)

**Image registry and tags.** The official registry is `ghcr.io/erpc/erpc`. The Kubernetes manifest pins `ghcr.io/erpc/erpc:latest` (`kube/erpc.yml:L28`). Docker Compose references `ghcr.io/erpc/erpc:main` for the (commented-out) eRPC service (`docker-compose.yml:L4`). No pinned digest in the kube manifest — operators should pin a specific tag or SHA for production.

**docker-compose services.** The compose file (`docker-compose.yml`) provides:
1. `monitoring` — builds `./monitoring`, exposes `3000:3000` (Grafana) and `9090:9090` (Prometheus). Accepts env vars `SERVICE_ENDPOINT` and `SERVICE_PORT` (default target: `host.docker.internal:4001`) substituted into the Prometheus scrape target at entrypoint time.
2. `redis` — `redis:latest`, port `6379:6379`, restart always, on the `erpc` bridge network. Used as a cache connector backend.
3. The eRPC container itself is commented out; operators mount `./erpc.yaml` at `/erpc.yaml`.
4. Commented-out optional services: Jaeger (OTLP gRPC+HTTP, Jaeger UI), PostgreSQL, ScyllaDB (Alternator mode).

**Volumes.** `prometheus_data` (Prometheus TSDB) and `grafana_data` (Grafana state) are named Docker volumes (`docker-compose.yml:L84-L86`). Grafana provisioning config is mounted from `./monitoring/grafana/`.

**`make docker-build`.** Wraps `docker build` with `--platform $(platform)` (default `linux/amd64`); passes `VERSION=local` and `COMMIT_SHA=$(git rev-parse --short HEAD)`. Only `linux/amd64` and `linux/arm64` are supported via this target (`Makefile:L121-L123`). No `docker buildx` multi-arch bake in the Makefile — multi-arch builds for the GHCR release image are done via CI.

**`make docker-run`.** Runs the local image with ports `4000:4000` and `4001:4001`, mounting `./erpc.yaml:/erpc.yaml`, and explicitly executes `/erpc-server --config /erpc.yaml` (`Makefile:L127-L129`).

**Kubernetes manifests.** `kube/erpc.yml` defines: Namespace `erpc`, Deployment, ConfigMap `erpc-config`, ClusterIP Service (port 80 → 4000), and a `PodMonitor` for Prometheus Operator. `kube/postgres.yml` defines: PersistentVolumeClaim (500 Gi), Deployment, Service, and Secret for a companion PostgreSQL cache store.

**Resource sizing.** The kube manifest embeds example resource specs; these are the only code-grounded recommendations:
- eRPC pod: requests and limits both `3Gi` memory, `2` CPU (`kube/erpc.yml:L33-L36`).
- PostgreSQL pod: requests and limits both `8Gi` memory, `4` CPU (`kube/postgres.yml:L31-L34`).
- PostgreSQL PVC: `500Gi` (`kube/postgres.yml:L9`).

No `GOMEMLIMIT` environment variable is set in any manifest or Dockerfile. The Go runtime's default GC behavior applies; operators using containers with hard memory limits should set `GOMEMLIMIT` themselves to avoid OOM kills at the container limit rather than a Go-managed soft limit.

**Prometheus scrape in Kubernetes.** The Deployment pod template carries annotations (`kube/erpc.yml:L20-L23`):
```yaml
prometheus.io/scrape: "true"
prometheus.io/port: "4001"
prometheus.io/path: "/metrics"
```
Additionally, a `PodMonitor` (`monitoring.coreos.com/v1`) is created in the `erpc` namespace, labeled `release: monitoring`, scraping port named `http` every 10s with a 5s timeout (`kube/erpc.yml:L145-L160`). The production `prometheus.yaml` file (used in-cluster via Prometheus Operator) shows the eRPC podMonitor scraping at `scrape_interval: 10s`, `scrape_timeout: 5s`, matching pods labeled `dev.flair/component: erpc` in the `erpc` namespace (`prometheus.yaml:L461-L509`).

**Monitoring container internals.** `monitoring/Dockerfile` pulls Prometheus `v3.9.1` and Grafana `12.3.3` (both pinned by digest), copies binaries into an `ubuntu:22.10` base, and runs both processes via `monitoring/scripts/entrypoint.sh` (`monitoring/Dockerfile:L1-L28`). The entrypoint substitutes `SERVICE_ENDPOINT:SERVICE_PORT` into `prometheus.yml` and starts both processes in the background, then tails `/dev/null` to keep the container alive (`monitoring/scripts/entrypoint.sh`). This single-container design is useful for local dev; for production, Prometheus and Grafana should run as separate services.

**Prometheus local config.** `monitoring/prometheus/prometheus.yml` defines two scrape jobs: `prometheus` (self at `localhost:9090`) and `erpc` (dual targets: `host.docker.internal:4001` and a `REPLACE_SERVICE_ENDPOINT_HERE:REPLACE_SERVICE_PORT_HERE` placeholder replaced at container start) with `scrape_interval: 10s` and `evaluation_interval: 10s` (`monitoring/prometheus/prometheus.yml`).

**Grafana provisioning.** `monitoring/grafana/grafana.ini` configures: `http_addr=0.0.0.0`, `http_port=3000`, anonymous access disabled, sign-up disabled, `publicDashboards` feature toggle enabled, `default_home_dashboard_path` pointing at `/etc/grafana/provisioning/dashboards/erpc.json` (`monitoring/grafana/grafana.ini`). The datasource provisioning (`monitoring/grafana/datasources/prometheus.yml`) points at `http://localhost:9090` as the default Prometheus datasource. The dashboard provider (`monitoring/grafana/dashboards/default.yml`) loads all JSON files from `/etc/grafana/dashboards` every 10 seconds with UI editing allowed.

**Alert rules.** `monitoring/prometheus/alert.rules` defines five alerts in the `eRPC_Alerts` group:
1. `HighErrorRate` — upstream error rate > 5% for 5 min (warning)
2. `SlowRequests` — upstream p95 latency > 1s for 5 min (uses `erpc_upstream_request_duration_seconds_budget` histogram)
3. `HighRateLimiting` — upstream self-rate-limiting > 10% for 5 min
4. `NetworkRateLimiting` — network self-rate-limiting > 10 req/s for 5 min
5. `HighRequestRate` — upstream request rate > 1000 req/s for 5 min (warning)
6. `LowRequestRate` — upstream request rate < 1 req/s for 15 min (warning; fire condition is 15 min `for:`)

**Grafana dashboard sections.** The `erpc.json` dashboard (15,534 lines) includes these collapsible row groups and panels (extracted from title fields):
- **Overall / Share Breakdown** — Share Breakdown (Top 20), Vendors/Upstreams/Networks/Method/Finality/Cache Connector Share, Total Responses by Region and User-Agent
- **Overall** — Incoming Requests, Incoming RPS, Successful Responses RPS, Network-level Critical Errors/Warnings/Notices
- **Networks - Requests** — per-network request metrics
- **Networks - Latency** — Network-method P50/P90/P99 response times, Dynamic Timeout Duration, Hedge Quantile/RPS/Effectiveness, Multiplexed RPS, Response Finality Share
- **Networks - Advanced** — Block Timestamp Distance (State Poller + Network Response), Block Time, Poll Interval/Block Time Ratio, Block Head Lag, Active Primary Upstream, Failover Retries by Reason, Upstream Errors by Type
- **Served Tip** — Served Tip, Tip age, Tip read p95, Primary lag, Served-tip lag, Finalized lag, Requests waiting %, Served tip/lane (latest/finalized), Tip lag/lane, Catch-up wait p95, Upstream heads, Tip culprits, Other culprits
- **Vendors** — Vendors Share (finality=latest/finalized), Vendor Requests/RPS, Vendor P50/P90/P99, Vendor warnings
- **Upstreams** — Upstream-level critical errors/warnings/notices, Upstream-level Requests/RPS/P50/P90/P99, Position per Upstream, Score per Upstream, Sticky Holds per Upstream, Primary Switches per Network, Readmit Age Distribution, Top Excluded (current duration)
- **Selection Policy** — Shadow Traffic, Shadow Errors, Shadow Identical/Mismatches, Shadow Exclusions by Reason, Stale latest/finalized block by Upstream, Out-of-sync block lag, Upstream stale lower/upper bound, Upstream wrong empty responses
- **Rate Limiters** — Rate-limit budget MaxCount, Upstream self/remote RPS limits, Project-level RPS limits
- **evm eth_getLogs** — eth_getLogs P50/P99 ranges, Top Block Buckets (Cacheable/Non-cacheable)
- **Consensus** — Total consensus operations/responses/errors/cancellations/misbehaviors/short-circuits

**Production Prometheus config.** The `prometheus.yaml` at repo root is the in-cluster Prometheus Operator config used for `erpc.cloud` production. It uses `remote_write` to Mimir at `http://mimir-distributor.telemetry.svc.cluster.local:8080/api/v1/push` with `X-Scope-OrgID: erpc`, external labels for `cluster: eu-central.dch.erpc.cloud`, a `__replica__` from `$(POD_NAME)`, and sharding via `hashmod` (`prometheus.yaml:L1-L20`, `prometheus.yaml:L608-L616`). This file is the operator's config snapshot and is NOT used by the in-repo monitoring container.

**Fake RPC server.** `Dockerfile.fake` builds the test fake-RPC server from `test/cmd/main.go` into a standalone image on alpine, exposing ports `9081-9185` (`Dockerfile.fake:L30`). Used by `make run-fake-rpcs` for integration testing without real upstream providers.

## L3 — Exhaustive reference (agent view)

### Config fields

The deployment layer has no eRPC `common.Config` fields of its own. Config is mounted at `/erpc.yaml` (or `/erpc.yml`, `/erpc.ts`, `/erpc.js`, `/root/erpc.yaml`, etc. — see `cmd/erpc/main.go:L279-L294`). The following environment variables affect the running process:

| # | Variable | Effect | Source |
|---|----------|--------|--------|
| 1 | `LOG_LEVEL` | Overrides `cfg.LogLevel`; parsed as zerolog level | `cmd/erpc/main.go:L354-L363` |
| 2 | `LOG_WRITER` | If `"console"` → zerolog console writer with `04:05.000ms` time format | `cmd/erpc/main.go:L53-L57` |
| 3 | `SERVICE_ENDPOINT` | Injected into Prometheus config at container startup (monitoring container only) | `monitoring/scripts/entrypoint.sh` |
| 4 | `SERVICE_PORT` | Same | `monitoring/scripts/entrypoint.sh` |
| 5 | `GOMEMLIMIT` | Not set by any manifest; operators must set manually for memory-bounded containers | n/a |

Kubernetes ConfigMap `erpc-config` in `kube/erpc.yml:L54-L130` contains an embedded example `erpc.yaml` demonstrating a realistic Arbitrum deployment with PostgreSQL cache, hedge failsafe, and three upstreams (Alchemy, Blastapi, QuickNode).

### Behaviors & algorithms

**Config file search order** (`cmd/erpc/main.go:L279-L294`): `--config <path>` flag → positional first arg → first match in: `./erpc.yaml`, `./erpc.yml`, `./erpc.ts`, `./erpc.js`, `/erpc.yaml`, `/erpc.yml`, `/erpc.ts`, `/erpc.js`, `/root/erpc.yaml`, `/root/erpc.yml`, `/root/erpc.ts`, `/root/erpc.js`. If `--require-config` is set (or any explicit config path was given) and no file is found, startup fails.

**CLI subcommands** (`cmd/erpc/main.go`):
- `erpc [config]` or `erpc start` — start the server
- `erpc validate [--format json|md]` — parse config, run `erpc.GenerateValidationReport`, exit non-zero on errors
- `erpc dump [--format yaml|json]` — parse config, resolve effective selection policies, dump to stdout

**pprof binary.** `erpc-server-pprof` is built with `-tags pprof` enabling HTTP profiling endpoints. Run via `make run-pprof` which runs `./cmd/erpc/main.go ./cmd/erpc/pprof.go`. Exposed on port 6060 (the third `EXPOSE` in the Dockerfile). In the image, the pprof binary is at `/erpc-server-pprof` and the default CMD still runs `/erpc-server` — operators must override CMD to use pprof.

**Multi-stage layer cache.** `go.mod`/`go.sum` are copied first and `go mod download` runs before the source copy (`Dockerfile:L16-L21`). pnpm uses a mount-cache (`--mount=type=cache,id=pnpm,target=/pnpm/store`) for both dev and prod stages (`Dockerfile:L51`, `L63`). The symlink stage is an independent alpine layer so COPY with `--link` can be parallelized (`Dockerfile:L66-L77`).

**Graceful shutdown.** The Go process handles `SIGINT`/`SIGTERM` via `signal.NotifyContext` (`cmd/erpc/main.go:L72-L74`). Context cancellation triggers the HTTP server's drain phase (healthcheck → 503, then `Shutdown` after `server.waitBeforeShutdown`).

### Observability

The monitoring container bundles the full Grafana dashboard at `monitoring/grafana/dashboards/erpc.json`. All metrics consumed by that dashboard are emitted by eRPC's Prometheus endpoint at port 4001 (`/metrics`). Key alert expressions:

- `erpc_upstream_request_errors_total` — used in `HighErrorRate` alert (rate over 5m by upstream)
- `erpc_upstream_request_duration_seconds_budget` — used in `SlowRequests` alert (p95 histogram_quantile by upstream)
- `erpc_upstream_request_self_rate_limited_total` — used in `HighRateLimiting` alert
- `erpc_network_request_self_rate_limited_total` — used in `NetworkRateLimiting` alert
- `erpc_upstream_request_total` — used in `HighRequestRate` and `LowRequestRate` alerts

The Grafana dashboard template variable `$datasource` selects the Prometheus datasource; `$finality` filters Vendor Share panels.

### Edge cases & gotchas

1. **distroless/nonroot base**: The final image runs as nonroot (no shell, no package manager). Mounting files, exec-ing, or attaching debug tools requires privileged sidecars. The CMD is `/erpc-server` — not a shell wrapper.

2. **TS config on distroless**: TypeScript config files (`erpc.ts`) require Node.js to evaluate. The distroless image does NOT include Node; however the compiled TypeScript SDK (`/typescript`) and `node_modules` are present. TypeScript config loading in eRPC actually invokes a bundled Node execution path using the node_modules included in the image. If a customer strips node_modules, TS configs fail silently and fall back to YAML.

3. **Port collision between gRPC and HTTP**: By default `server.grpcPortV4 = server.httpPortV4 = 4000`. If an operator overrides the gRPC port to differ from HTTP, a second listener is bound (`erpc/init.go:L125-L136`). The Dockerfile exposes only 4000, 4001, 6060 — an operator-changed gRPC port needs an explicit Docker `-p` flag.

4. **prometheus.yaml at repo root is NOT the monitoring container's config**: It is the production Prometheus Operator config. The monitoring container uses `monitoring/prometheus/prometheus.yml`. They have different scrape configs and targets.

5. **Monitoring container is single-process using `tail -f /dev/null`**: This is a development convenience. Prometheus and Grafana do not restart independently if one crashes. Not suitable for production.

6. **`SERVICE_ENDPOINT` substitution is sed-based**: If `SERVICE_ENDPOINT` contains characters special to sed (e.g. `/`), the substitution will break. The placeholder in the prometheus config is `REPLACE_SERVICE_ENDPOINT_HERE:REPLACE_SERVICE_PORT_HERE` (`monitoring/prometheus/prometheus.yml:L19`).

7. **No GOMEMLIMIT in kube manifests**: With memory limit 3Gi, Go's GC may allow heap to approach 3Gi before triggering, causing container OOM kills. Operators should set `GOMEMLIMIT=2700MiB` (90% of limit) as a container env var.

8. **erpc-server-pprof binary in image but not default CMD**: The pprof binary is always present at `/erpc-server-pprof` in every image build but is never invoked unless the operator overrides the container command or uses `make run-pprof`.

9. **`docker-compose down --volumes` drops prometheus_data and grafana_data**: `make down` passes `--volumes` (`Makefile:L114`), destroying all monitoring state. Use `docker compose down` (without `--volumes`) to preserve data.

10. **kube/postgres.yml uses `postgres:latest`**: No version pin. Production deployments should pin a specific PostgreSQL version and digest.

### Source map

- `Dockerfile` — five-stage multi-arch build: go-builder, ts-core, ts-dev, ts-prod, symlink, final (distroless)
- `Dockerfile.fake` — standalone fake-RPC test server image (ports 9081-9185)
- `docker-compose.yml` — local dev compose: monitoring, redis; eRPC/Jaeger/PG/Scylla commented out
- `kube/erpc.yml` — Namespace + Deployment + ConfigMap + Service + PodMonitor for eRPC
- `kube/postgres.yml` — PVC + Deployment + Service + Secret for PostgreSQL cache store
- `monitoring/Dockerfile` — combined Prometheus v3.9.1 + Grafana 12.3.3 on ubuntu:22.10
- `monitoring/scripts/entrypoint.sh` — substitutes SERVICE_ENDPOINT into prometheus.yml, starts both processes
- `monitoring/prometheus/prometheus.yml` — local scrape config (self + erpc at host.docker.internal:4001)
- `monitoring/prometheus/alert.rules` — 6 Prometheus alerting rules for error rate, latency, rate limiting
- `monitoring/grafana/grafana.ini` — Grafana server config (port 3000, provisioning paths, publicDashboards)
- `monitoring/grafana/datasources/prometheus.yml` — auto-provisions Prometheus datasource at localhost:9090
- `monitoring/grafana/dashboards/default.yml` — dashboard file provider scanning /etc/grafana/dashboards every 10s
- `monitoring/grafana/dashboards/erpc.json` — 15,534-line pre-built Grafana dashboard with 80+ panels
- `prometheus.yaml` — production Prometheus Operator config (remote_write to Mimir, in-cluster job discovery)
- `Makefile` — build/run/test/docker/compose targets; `docker-build` defaults to `linux/amd64`
- `cmd/erpc/main.go` — CLI entry point: config discovery, start/validate/dump subcommands, LOG_LEVEL/LOG_WRITER env handling
