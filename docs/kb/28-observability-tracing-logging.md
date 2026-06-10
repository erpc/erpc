# KB: Tracing (OpenTelemetry) & logging

> status: complete
> source-dirs: common/tracing_core.go, common/tracing_util.go, common/config.go (TracingConfig, LogLevel), common/console.go, util/redact.go, common/defaults.go, cmd/erpc/main.go, cmd/erpc/initflags.go, erpc/erpc.go, erpc/init.go, erpc/http_server.go

## L1 — Capability (CTO view)

eRPC embeds a full OpenTelemetry distributed tracing pipeline (OTLP export over gRPC or HTTP) and a structured JSON logging system (zerolog). Tracing provides end-to-end visibility from inbound HTTP request to every upstream call, cache lookup, and database operation, including configurable sampling and a "force-trace" override that bypasses sampling for specific networks/methods or per-request headers. Logging outputs newline-delimited JSON by default (with an optional human-readable console mode), supports five verbosity levels, and redacts all secrets from log output before they can leak.

## L2 — Mechanics (staff-engineer view)

**Tracing initialization.** On `NewERPC` (`erpc/erpc.go:L31`), `common.InitializeTracing` is called once (guarded by `sync.Once`) with the `TracingConfig`. If `enabled: false` or config is nil, it sets two package globals — `IsTracingEnabled = false` and `IsTracingDetailed = false` — and returns immediately (`common/tracing_core.go:L53-L58`). When enabled, it builds an OTLP exporter (gRPC via `otlptracegrpc` or HTTP via `otlptracehttp`), constructs a `TracerProvider` with a batcher and a custom sampler, configures W3C TraceContext + Baggage propagation, and stores `forceTraceMatchers` for later per-request evaluation (`common/tracing_core.go:L60-L148`). The tracer is registered with `otel.SetTracerProvider` and is accessible via the package-level `tracer` variable. Shutdown is triggered by a goroutine watching `appCtx.Done` with a 5-second grace period (`erpc/erpc.go:L92-L99`).

**Two tiers of spans.** All call sites use one of two helpers in `common/tracing_util.go`:
- `StartSpan` — emits a span whenever `IsTracingEnabled`; used for major external interactions (cache, upstreams, connectors, rate limiters, consensus). These are the "data plane" spans you always want.
- `StartDetailSpan` — emits a span only when both `IsTracingEnabled` and `IsTracingDetailed`; used for eRPC-internal operations and any span carrying high-cardinality attributes (request params, block numbers). Enabling `detailed: true` roughly doubles the span count and includes spans for JSON parsing, lock contention, hook execution, and state-poller polls.

When tracing is disabled, both helpers return the caller's unchanged context and a no-op `noopSpan` (singleton, `common/tracing_util.go:L29`). No allocation happens on the hot path when tracing is off.

**Span hierarchy for a single HTTP request.** A typical trace (normal mode, not detailed) looks like:

```
Http.ReceivedRequest  (SpanKindServer)
  └─ Request.Handle   (SpanKindInternal, started via tracer.Start directly)
       └─ Network.Forward
            └─ Network.forwardAttempt
                 ├─ Cache.Get
                 └─ Upstream.Forward
                      └─ HttpJsonRpcClient.sendSingleRequest
```

With `detailed: true`, additional spans appear:
- `Http.ReadBody`, `Http.ParseRequests`, `HttpServer.WriteResponse` inside the HTTP layer
- `Project.Forward`, `Network.TryForward`, `Network.UpstreamLoop` between Forward and forwardAttempt
- `RateLimiter.TryAcquirePermit`, `Upstream.tryForward.PreRequest`, `Upstream.tryForward.SendRequest` inside upstream execution
- `Cache.FindGetPolicies`, `Cache.GetForPolicy` inside cache lookup
- `Request.ResolveJsonRpc`, `Request.Lock`/`RLock`, `Response.ResolveJsonRpc` for lock/parse ops
- Method-specific hooks: `Project.PreForwardHook.eth_blockNumber`, `Network.PostForward.eth_getBlockByNumber`, etc.

**Force-trace mechanism.** Three paths can bypass the sampler and force a span to be recorded regardless of `sampleRate`:
1. HTTP header `X-ERPC-Force-Trace: true` (or `1`, `yes`) — checked in `StartHTTPServerSpan` (`common/tracing_util.go:L107-L118`).
2. Query parameter `?force-trace=true` — same check.
3. Config-driven `forceTraceMatchers` — evaluated in `StartRequestSpan` after the URL is parsed and network is known (`common/tracing_util.go:L148-L169`). The network (in `architecture:chainId` format, e.g., `evm:1`) is stored in context at URL-parse time (`erpc/http_server.go:L287`) and retrieved later.

In all cases, the force-trace decision is communicated to the OTel sampler by setting the span attribute `erpc.force_trace = true` at span creation time. The custom `forceTraceSampler` wraps the base sampler and intercepts `ShouldSample`; when it sees this attribute it returns `RecordAndSample` unconditionally (`common/tracing_core.go:L257-L268`).

**Sampler logic.** `createTracingSampler` builds the base sampler from `sampleRate`:
- `<= 0`: `NeverSample()`
- `>= 1.0`: `AlwaysSample()`
- Otherwise: `ParentBased(TraceIDRatioBased(sampleRate))` with all four parent-sampling options configured for consistency (no orphan spans) (`common/tracing_core.go:L229-L249`).

The `forceTraceSampler` always wraps the base sampler regardless of rate setting (`common/tracing_core.go:L249`).

**Context propagation.** Incoming HTTP requests have W3C `traceparent`/`tracestate` extracted via `propagation.TraceContext{}` carrier on the request headers (`common/tracing_util.go:L72-L74`, `StartHTTPServerSpan`). After the handler writes the response, `InjectHTTPResponseTraceContext` injects the active span's context back into response headers (`erpc/http_server.go:L701`, `common/tracing_util.go:L58-L65`). This enables downstream callers to correlate their own traces with eRPC's internal ones.

**Span attributes on `Request.Handle`.** The `StartRequestSpan`/`EndRequestSpan` pair (called around each individual JSON-RPC request within a batch) sets:
- Always (if recording): `request.method`, optionally `erpc.forced_trace_reason`
- If `detailed`: `request.id`, `request.jsonrpc.params` (serialized), `network.id`, `user.id`, `request.finality`, `response.finality`, `execution.attempts`, `execution.retries`, `execution.hedges`, `response.result_size`, `upstream.id`, `cache.hit` (`common/tracing_util.go:L140-L234`)

**Error recording.** `SetTraceSpanError` checks `span.IsRecording()` before doing anything, then for `StandardError` types it also sets `error.code` (the full code chain) as a span attribute and records the error as an event; for plain `error` it calls `span.RecordError` + `SetStatus(codes.Error, ...)` (`common/tracing_core.go:L163-L183`).

**Logging system.** zerolog is the structured JSON logger used everywhere. On startup, `cmd/erpc/main.go` sets:
- `zerolog.TimeFieldFormat = zerolog.TimeFormatUnixMs` — timestamps are Unix milliseconds
- `zerolog.ErrorMarshalFunc` — passes the `error` interface directly (not `.Error()` string), so error values can carry richer JSON when the zerolog encoder supports it
- If `LOG_WRITER=console`: wraps `log.Logger` with `zerolog.ConsoleWriter` for human-readable output with `04:05.000ms` time format (`cmd/erpc/main.go:L53-L57`)
- If `LOG_LEVEL` env var is set at startup, the global zerolog level is set before `cfg.LogLevel` is applied

The per-instance level is then resolved in `erpc/init.go:L27-L33`: `zerolog.ParseLevel(cfg.LogLevel)` — on parse error it defaults to `debug`.

**Log level precedence.** Config `logLevel` has the final say on the `logger` instance used throughout eRPC. But there are two override mechanisms:
1. `LOG_LEVEL` env var at startup (applied to global zerolog level before `Init` is called, `cmd/erpc/main.go:L59-L66`)
2. `LOG_LEVEL` env var at `getConfig` time (overrides `cfg.LogLevel` so it persists through `Init`, `cmd/erpc/main.go:L354-L363`)

The `ERPC_NOLOGS=1` env var silences all logging at the binary level (zerolog global = `Disabled`, writer = `io.Discard`, `cmd/erpc/initflags.go:L16-L19`). This is a compile-time-tagged init (`//go:build !test`).

**Config logging.** When the resolved log level is `<= Info`, eRPC serializes the entire loaded config as a JSON field in an `info` log line (`erpc/init.go:L35-L42`). Because `MarshalJSON` is overridden on sensitive types, this config dump is safe.

**Secret redaction.** Secrets are redacted in two layers:
1. `MarshalJSON`/`MarshalYAML` on config types — called whenever config is serialized (e.g., the startup config dump): `RedisConnectorConfig.password` → `"REDACTED"`, `AwsAuthConfig.secretAccessKey` → `"REDACTED"`, `ProviderConfig.settings` → `"REDACTED"`, `SecretStrategyConfig.value` → `"REDACTED"` (`common/config.go:L397-L503`, `common/config.go:L2467-L2477`).
2. `util.RedactEndpoint` — strips path/query/credentials from endpoint URLs, preserving only scheme + host + a 5-char SHA-256 hash suffix as a stable identifier. Applied to `UpstreamConfig.Endpoint` in both `MarshalJSON` and `MarshalYAML` (`common/config.go:L1033-L1048`), to Redis/PostgreSQL URIs, and in log messages for Redis rate-limiter connections.

**JavaScript console logging.** When TypeScript/JS config files use the `console.debug/info/log/trace/warn` global, calls route through `common/console.go` which maps each method to the corresponding zerolog level on `log.Logger`. `console.error` is absent — only debug/info/log/trace/warn are provided (`common/console.go:L15-L21`). The console object is installed into every sobek runtime via `common/runtime.go:L39`. Output respects the configured global log level; calls below the current level return early without allocating strings (`common/console.go:L26-L29`).

**OTel error handling.** Export failures (transient network issues sending spans to the collector) are logged at trace level via a custom `otel.ErrorHandlerFunc`: `"open telemetry export error"`. They do not propagate to callers (`common/tracing_core.go:L125-L127`).

**Standard `OTEL_*` env vars and the eRPC config block.** The OTel Go SDK honors standard `OTEL_*` environment variables at the package level, but most are overridden or bypassed by eRPC's explicit SDK option calls. `OTEL_EXPORTER_OTLP_ENDPOINT` is always overridden (eRPC always passes `WithEndpoint`). `OTEL_TRACES_SAMPLER` and `OTEL_PROPAGATORS` are ignored because eRPC supplies its own sampler and propagator. `OTEL_EXPORTER_OTLP_HEADERS` may take effect only when `tracing.headers` is empty in config. `OTEL_SDK_DISABLED` is the only env var that can globally disable the OTel SDK regardless of eRPC config. See the config-fields table below for per-env-var details and footguns.

**OTel debug logging.** When zerolog's global level is `<= Debug`, eRPC installs a `zerologr.New(logger)` adapter as the OTel SDK's internal logger (`common/tracing_core.go:L119-L122`). This logs OTel SDK internals (batch flush timing, etc.) at the zerolog debug level.

## L3 — Exhaustive reference (agent view)

### Config fields

#### Top-level `logLevel`

| YAML path | Type | Default | Valid values | Behavior |
|-----------|------|---------|-------------|----------|
| `logLevel` | string | `"INFO"` (`common/defaults.go:L50-L51`) | `trace`, `debug`, `info`, `warn`, `error`, `fatal`, `panic`, `disabled` (zerolog levels, case-insensitive) | Sets the log level for the logger instance passed into all eRPC subsystems. Invalid values cause a warning and fall back to `debug` (`erpc/init.go:L27-L32`). Can be overridden by the `LOG_LEVEL` environment variable at runtime (`cmd/erpc/main.go:L354-L363`). |

#### `tracing.*`

Full struct: `common/config.go:L218-L235`.

| YAML path | Type | Default | Behavior / notes |
|-----------|------|---------|------------------|
| `tracing.enabled` | bool | `false` (no SetDefaults override; Go zero value) | Master switch. When `false`, `IsTracingEnabled = false` globally, all span calls are no-ops (`common/tracing_core.go:L53-L58`). |
| `tracing.endpoint` | string | gRPC: `"localhost:4317"`, HTTP: `"http://localhost:4318"` — derived from protocol in `SetDefaults` (`common/defaults.go:L622-L628`) | OTLP collector endpoint. For gRPC, no scheme — just `host:port`. For HTTP, full URL including scheme. |
| `tracing.protocol` | TracingProtocol | `"grpc"` (`common/defaults.go:L619-L621`) | `"grpc"` uses `otlptracegrpc`; `"http"` uses `otlptracehttp`. Any other value → error at init (`common/tracing_core.go:L71-L78`). |
| `tracing.sampleRate` | float64 | `1.0` (`common/defaults.go:L629-L631`) | `0` → NeverSample, `1.0` → AlwaysSample, `0 < x < 1` → ParentBased+TraceIDRatioBased (`common/tracing_core.go:L229-L249`). Note: SetDefaults sets `1.0` only if the value is exactly `0`; setting `sampleRate: 0` in YAML means "100%" is the default, not "never sample". To never sample, set `enabled: false`. |
| `tracing.detailed` | bool | `false` (no default override) | When `true`, sets `IsTracingDetailed = true`. Enables `StartDetailSpan` calls (internal ops, high-cardinality attributes). Roughly 2× span count vs. normal mode. |
| `tracing.serviceName` | string | `"erpc"` (`common/defaults.go:L632-L634`) | OTel `service.name` resource attribute. |
| `tracing.headers` | map[string]string | nil (no default) | Additional HTTP/gRPC metadata headers sent with every export batch. Used for auth to managed OTLP collectors (e.g., Grafana Cloud, Honeycomb). |
| `tracing.tls.enabled` | bool | `false` (Go zero value; no `SetDefaults` override — `common/config.go:L376`) | Master switch for TLS on the OTLP exporter connection. When `false`, both gRPC and HTTP exporters are created with `WithInsecure()` (`common/tracing_core.go:L187`, `L208`). When `true`, `CreateTLSConfig` builds a `tls.Config` from the sub-fields and passes it to the exporter via `WithTLSCredentials` (gRPC) or `WithTLSClientConfig` (HTTP) (`common/tracing_core.go:L189-L193`, `L210-L214`). |
| `tracing.tls.certFile` | string | `""` (Go zero value; no `SetDefaults` override — `common/config.go:L377`) | Path to a PEM-encoded client certificate file. Used together with `keyFile` to load an `x509.KeyPair` for **mutual TLS (mTLS)** to the OTLP collector — this authenticates eRPC to the collector, not the other way around. Both `certFile` and `keyFile` must be non-empty for the client cert to be loaded; supplying one without the other silently skips mutual TLS (`common/tls.go:L16-L22`). |
| `tracing.tls.keyFile` | string | `""` (Go zero value; no `SetDefaults` override — `common/config.go:L378`) | Path to the PEM-encoded private key corresponding to `certFile`. Required alongside `certFile` for mTLS. |
| `tracing.tls.caFile` | string | `""` (Go zero value; no `SetDefaults` override — `common/config.go:L379`) | Path to a PEM-encoded CA certificate file. When set, a new `x509.CertPool` is created from this file and assigned to `tls.Config.RootCAs`, so the TLS handshake validates the OTLP **collector's** server certificate against this CA instead of the system CA pool (`common/tls.go:L24-L32`). Use this to trust a private/self-signed certificate on the OTLP collector. If `caFile` is empty and `insecureSkipVerify` is false, the system CA pool is used (standard Go TLS behavior). |
| `tracing.tls.insecureSkipVerify` | bool | `false` (Go zero value; no `SetDefaults` override — `common/config.go:L380`) | When `true`, sets `tls.Config.InsecureSkipVerify = true` (`common/tls.go:L13`), which **disables verification of the OTLP collector's server certificate** — not the client certificate. This is the same semantics as `crypto/tls`'s `InsecureSkipVerify`: the TLS handshake succeeds regardless of whether the collector's cert is valid, expired, or signed by an unknown CA. It does NOT affect mTLS client-cert loading (which is controlled by `certFile`/`keyFile`). Only use in development or on a trusted private network. |
| `tracing.resourceAttributes` | map[string]string | nil (no default) | Custom OTel resource attributes (e.g., `deployment.environment: production`). Environment variables in values are expanded at config load time (YAML `os.ExpandEnv`). Empty values are silently skipped (`common/tracing_core.go:L93-L97`). |
| `tracing.forceTraceMatchers` | []*ForceTraceMatcher | nil (no default) | List of matchers that force sampling regardless of `sampleRate`. Each matcher can have `network` and/or `method` pattern fields. |
| `tracing.forceTraceMatchers[].network` | string | — | Network pattern in `architecture:chainId` form, e.g. `"evm:1"`, `"evm:1|evm:42161"`, `"evm:*"`. Pipe (`\|`) = OR. Wildcards via `WildcardMatch`. |
| `tracing.forceTraceMatchers[].method` | string | — | JSON-RPC method pattern, e.g. `"eth_call"`, `"debug_*\|trace_*"`. Pipe = OR. Wildcards supported. |

**ForceTraceMatcher AND/OR semantics:** if both `network` and `method` are specified, both must match (AND). If only one is specified, only that field is checked. An empty matcher (neither field set) never matches (`common/tracing_core.go:L306-L330`).

#### Standard `OTEL_*` environment variables (OTel Go SDK layer)

The OTel Go SDK itself honors a set of standard environment variables independently of the eRPC `tracing.*` config block. These env vars are processed by the SDK packages at the time the exporter or provider is constructed — eRPC does not explicitly read or suppress them. The interaction with eRPC's own config options depends on whether eRPC passes an explicit SDK option for the same setting:

| Env var | Scope | Interaction with eRPC config |
|---------|-------|------------------------------|
| `OTEL_EXPORTER_OTLP_ENDPOINT` | OTLP exporter | **Overridden by eRPC.** eRPC always calls `WithEndpoint(cfg.Endpoint)` (`common/tracing_core.go:L197`, `L217`); explicit SDK options take precedence over the env var, so this env var is effectively ignored when `tracing.endpoint` is set in config. |
| `OTEL_EXPORTER_OTLP_HEADERS` | OTLP exporter | **Partially overridden.** eRPC calls `WithHeaders(cfg.Headers)` only when `len(cfg.Headers) > 0` (`common/tracing_core.go:L200-L202`, `L222-L224`). If `tracing.headers` is empty/nil, the SDK falls back to this env var; if non-empty, eRPC's explicit option takes precedence. |
| `OTEL_SERVICE_NAME` | Resource | **Not overridden.** eRPC builds its OTel resource using only `resource.WithAttributes(attrs...)` (`common/tracing_core.go:L100-L102`), not `resource.WithFromEnv()`. However, the OTel SDK's `resource.New` merges multiple detectors including the environment detector by default in some SDK versions. If the SDK does read `OTEL_SERVICE_NAME` in its default detectors, it may set `service.name` a second time, but eRPC's explicit `semconv.ServiceNameKey.String(cfg.ServiceName)` attribute is also present — the last-writer-wins semantics of OTel resource merging apply. |
| `OTEL_TRACES_SAMPLER` / `OTEL_TRACES_SAMPLER_ARG` | Sampler | **Ignored.** eRPC always constructs its own sampler via `createTracingSampler(cfg)` and passes it with `sdktrace.WithSampler(...)` (`common/tracing_core.go:L109`). The SDK sampler env vars are only respected when using the SDK's environment-configured sampler factory, which eRPC does not invoke. |
| `OTEL_PROPAGATORS` | Propagator | **Ignored.** eRPC explicitly sets `TraceContext{}` + `Baggage{}` as the composite propagator via `otel.SetTextMapPropagator(...)` (`common/tracing_core.go:L114-L117`). This call always overwrites any SDK-default propagator. |
| `OTEL_SDK_DISABLED` | SDK global | Honored by the OTel SDK at the package level before `InitializeTracing` runs. Setting this to `true` disables all OTel SDK operations globally even if `tracing.enabled: true` in eRPC config. |

**Key footgun:** if `OTEL_EXPORTER_OTLP_ENDPOINT` is set in the environment (e.g., by a container orchestration platform injecting OTel sidecar config) and `tracing.endpoint` has its default value (`localhost:4317` for gRPC), the config-file default wins because eRPC always passes `WithEndpoint`. Operators who expect the env var to route spans to a remote collector must set `tracing.endpoint` explicitly in the config file, or the spans will go to localhost and be silently dropped.

### Behaviors & algorithms

#### Log format

Default output is newline-delimited JSON via zerolog. Field layout example:
```json
{"level":"info","time":1718000000123,"message":"...","key":"value"}
```
The `time` field is Unix milliseconds (`zerolog.TimeFieldFormat = zerolog.TimeFormatUnixMs`, `cmd/erpc/main.go:L48`). Error values are embedded as the raw `error` interface (not forced to string), allowing rich JSON error types from zerolog encoders.

When `LOG_WRITER=console` env var is set, output switches to a human-readable format:
```
04:05.000ms INF message key=value
```
The console writer uses `04:05.000ms` time format (`cmd/erpc/main.go:L54-L56`).

#### Tracing initialization flow

1. `NewERPC` calls `common.InitializeTracing(appCtx, logger, cfg.Tracing)`.
2. `initOnce.Do(...)` ensures it runs exactly once per process. This means re-creating an ERPC instance (e.g., in tests) does NOT reinitialize tracing.
3. If `cfg == nil || !cfg.Enabled`: sets globals to false, logs `"OpenTelemetry tracing is disabled"` at info level, returns.
4. Logs init parameters at info: endpoint, protocol, serviceName, sampleRate, detailed.
5. Creates OTLP exporter based on protocol.
6. Builds OTel resource with `service.name`, `service.version` (= `ErpcVersion`), `commit.sha` (= `ErpcCommitSha`), plus any `resourceAttributes`.
7. Creates `TracerProvider` with batcher exporter, sampler (see above), and resource.
8. Registers provider globally via `otel.SetTracerProvider`.
9. Configures composite propagator: TraceContext + Baggage.
10. If zerolog level `<= Debug`: installs zerologr adapter as OTel logger.
11. Installs error handler: logs export failures at `trace` level.
12. Stores force-trace matchers in package var.
13. Sets `IsTracingEnabled = true` and `IsTracingDetailed = cfg.Detailed`.

#### Span attribute reference for `Request.Handle`

Attributes set by `StartRequestSpan` / `EndRequestSpan` (`common/tracing_util.go:L140-L234`):

| Attribute | Mode | Value |
|-----------|------|-------|
| `request.method` | always | JSON-RPC method name |
| `erpc.force_trace` | always, when forced | `true` (bool, internal — used by sampler, not exported as visible attribute) |
| `erpc.forced_trace_reason` | always, when forced | `"header_or_query"` for header/query-param force; `"network:<n>,method:<m>"`, `"network:<n>"`, or `"method:<m>"` for matcher-based force |
| `request.id` | detailed only | JSON-RPC request ID as string |
| `request.jsonrpc.params` | detailed only | JSON-serialized params array |
| `network.id` | detailed only, on response | Network ID string |
| `user.id` | detailed only, on response | Auth user ID |
| `request.finality` | detailed only, on response | Finality label (e.g., `"realtime"`, `"finalized"`) |
| `response.finality` | detailed only, on response | Response finality |
| `execution.attempts` | detailed only, on response | Total attempts |
| `execution.retries` | detailed only, on response | Number of retries |
| `execution.hedges` | detailed only, on response | Number of hedges |
| `response.result_size` | detailed only, on response | Length of JSON-RPC result field |
| `upstream.id` | always (if non-nil), on response | Upstream identifier |
| `cache.hit` | always (if non-nil), on response | bool: whether response was served from cache |

#### `Http.ReceivedRequest` span attributes

Set in `StartHTTPServerSpan` (`common/tracing_util.go:L80-L103`):

| Attribute | Value |
|-----------|-------|
| `http.method` | HTTP method (semconv) |
| `http.url` | Full URL |
| `http.scheme` | URL scheme |
| `http.user_agent` | User-Agent header |
| `erpc.force_trace` | `true` (if forced via header/query) |
| `erpc.forced_trace_reason` | `"header_or_query"` (if forced) |

Set in `EnrichHTTPServerSpan` on response (`common/tracing_util.go:L121-L138`):
- `http.status_code` (semconv)
- Status `Ok` or `Error`

#### W3C trace context propagation

- **Incoming**: `traceparent` + `tracestate` headers on HTTP requests are extracted via `propagation.TraceContext{}` in `StartHTTPServerSpan`. If present, the eRPC request becomes a child of the upstream caller's trace.
- **Outgoing**: After response is written, `InjectHTTPResponseTraceContext` injects the current span's W3C context into the response headers (`erpc/http_server.go:L701`). Allows eRPC callers to correlate their own spans with the eRPC-internal trace.
- **gRPC**: No trace context extraction/injection is implemented for gRPC requests (no equivalent of `StartHTTPServerSpan` in `erpc/grpc_server.go`).

#### `ForceFlushTraces`

`common.ForceFlushTraces(ctx)` calls `tracerProvider.ForceFlush(ctx)`, which flushes all batched but not-yet-exported spans to the collector. Not called automatically except on graceful shutdown. Exposed for callers that need guaranteed delivery of critical traces (`common/tracing_core.go:L237-L245`).

#### Secret redaction in logs

The following config types implement custom `MarshalJSON` and/or `MarshalYAML` that replace secrets with static strings before any serialization:

| Config type | Field redacted | Replacement | Source |
|-------------|---------------|-------------|--------|
| `RedisConnectorConfig` | `password` | `"REDACTED"` | `common/config.go:L397-L413` |
| `RedisConnectorConfig` | `uri` | `util.RedactEndpoint(r.URI)` | `common/config.go:L404` |
| `PostgreSQLConnectorConfig` | `connectionUri` | `util.RedactEndpoint(p.ConnectionUri)` | `common/config.go:L455-L470` |
| `AwsAuthConfig` | `secretAccessKey` | `"REDACTED"` | `common/config.go:L487-L503` |
| `ProviderConfig` | `settings` | `"REDACTED"` | `common/config.go:L679-L694` |
| `UpstreamConfig` | `endpoint` | `util.RedactEndpoint(u.Endpoint)` | `common/config.go:L1033-L1048` |
| `SecretStrategyConfig` | `value` | `"REDACTED"` | `common/config.go:L2467-L2477` |

`util.RedactEndpoint` algorithm (`util/redact.go:L10-L36`):
1. Compute SHA-256 of the full original endpoint; take first 5 hex chars as `hash`.
2. Parse URL; if unparseable → return `"redacted=<hash>"`.
3. For native protocols (alchemyv2, erigon, etc.): `scheme://host#redacted=<hash>`
4. For envio-suffix schemes: `scheme://host` (no hash needed — no secrets in path)
5. For repository-suffix schemes: `scheme://host#redacted=<hash>`
6. For everything else (standard http/https RPC endpoints): `scheme#redacted=<hash>` — note: **host is also dropped** for non-native, non-special-case URLs

This means two different endpoints with the same scheme but different paths produce different hashes, allowing correlation without exposing API keys.

### Observability

#### Prometheus metrics

There are **no Prometheus metrics emitted by the tracing or logging subsystem itself**. The tracing pipeline is a pure OTel concern; logging has no counters. Export errors from OTLP are logged at trace level only.

#### OTel span name inventory (all 126 spans)

"mode" = `normal` (emitted when `tracing.enabled`); `detail` (emitted when `tracing.enabled && tracing.detailed`); `direct` (calls `tracer.Start` directly, same as normal).

| Span name | Mode | Call site |
|-----------|------|-----------|
| `Cache.FindGetPolicies` | detail | `architecture/evm/json_rpc_cache.go:L173` |
| `Cache.Get` | normal | `architecture/evm/json_rpc_cache.go:L153` |
| `Cache.GetForPolicy` | detail | `architecture/evm/json_rpc_cache.go:L241` |
| `Cache.Set` | normal | `architecture/evm/json_rpc_cache.go:L572` |
| `ConnectorFailsafe.Delete` | detail | `data/failsafe.go:L286` |
| `ConnectorFailsafe.Get` | detail | `data/failsafe.go:L224` |
| `ConnectorFailsafe.Set` | detail | `data/failsafe.go:L253` |
| `Consensus.CollectResponses` | detail | `consensus/executor.go:L189` |
| `Consensus.Run` | normal | `consensus/executor.go:L1282` |
| `CounterInt64.TryUpdate` | normal | `data/shared_state_variable.go:L365` |
| `CounterInt64.TryUpdateIfStale` | normal | `data/shared_state_variable.go:L391` |
| `CounterInt64.TryUpdateIfStale.AcquireMutex` | normal | `data/shared_state_variable.go:L405` |
| `CounterInt64.TryUpdateIfStale.ExecuteRefresh` | normal | `data/shared_state_variable.go:L430` |
| `DynamoDBConnector.Delete` | normal | `data/dynamodb.go:L833` |
| `DynamoDBConnector.Get` | normal | `data/dynamodb.go:L414` |
| `DynamoDBConnector.List` | normal | `data/dynamodb.go:L874` |
| `DynamoDBConnector.Lock` | normal | `data/dynamodb.go:L579` |
| `DynamoDBConnector.Set` | normal | `data/dynamodb.go:L355` |
| `DynamoDBConnector.Unlock` | normal | `data/dynamodb.go:L691` |
| `DynamoDBConnector.getSimpleValue` | detail | `data/dynamodb.go:L766` |
| `Evm.ExtractBlockReferenceFromRequest` | detail | `architecture/evm/block_ref.go:L19` |
| `Evm.ExtractBlockReferenceFromResponse` | detail | `architecture/evm/block_ref.go:L118` |
| `Evm.ExtractBlockTimestampFromResponse` | detail | `architecture/evm/block_ref.go:L191` |
| `Evm.PickHighestBlock` | detail | `architecture/evm/eth_getBlockByNumber.go:L336` |
| `Evm.extractRefFromJsonRpcRequest` | detail | `architecture/evm/block_ref.go:L240` |
| `Evm.extractRefFromJsonRpcResponse` | detail | `architecture/evm/block_ref.go:L314` |
| `EvmStatePoller.PollFinalizedBlockNumber` | detail | `architecture/evm/evm_state_poller.go:L491` |
| `EvmStatePoller.PollLatestBlockNumber` | detail | `architecture/evm/evm_state_poller.go:L394` |
| `GrpcBdsClient.GetBlockByHash` | detail | `clients/grpc_bds_client.go:L365` |
| `GrpcBdsClient.GetBlockByNumber` | detail | `clients/grpc_bds_client.go:L422` |
| `GrpcBdsClient.GetLogs` | detail | `clients/grpc_bds_client.go:L632` |
| `GrpcBdsClient.QueryBlocks` | detail | `clients/grpc_bds_client.go:L1097` |
| `GrpcBdsClient.QueryLogs` | detail | `clients/grpc_bds_client.go:L1173` |
| `GrpcBdsClient.QueryTraces` | detail | `clients/grpc_bds_client.go:L1209` |
| `GrpcBdsClient.QueryTransactions` | detail | `clients/grpc_bds_client.go:L1138` |
| `GrpcBdsClient.QueryTransfers` | detail | `clients/grpc_bds_client.go:L1245` |
| `GrpcBdsClient.SendRequest` | normal | `clients/grpc_bds_client.go:L194` |
| `Http.ParseRequests` | detail | `erpc/http_server.go:L397` |
| `Http.ReadBody` | detail | `erpc/http_server.go:L373` |
| `Http.ReceivedRequest` | normal | `common/tracing_util.go:L96` (via `StartHTTPServerSpan`) |
| `HttpJsonRpcClient.sendSingleRequest` | normal | `clients/http_json_rpc_client.go:L675` |
| `HttpServer.WriteResponse` | detail | `erpc/http_server.go:L680` |
| `JsonRpcRequest.Lock` | detail | `common/json_rpc.go:L1229` |
| `JsonRpcRequest.RLock` | detail | `common/json_rpc.go:L1235` |
| `JsonRpcResponse.IsResultEmptyish` | detail | `common/json_rpc.go:L799` |
| `JsonRpcResponse.ParseFromStream` | detail | `common/json_rpc.go:L288` |
| `JsonRpcResponse.PeekBytesByPath` | detail | `common/json_rpc.go:L486` |
| `JsonRpcResponse.PeekStringByPath` | detail | `common/json_rpc.go:L464` |
| `Multiplexer.Close` | detail | `erpc/multiplexer.go:L37` |
| `Network.EnrichStatePoller` | detail | `erpc/networks.go:L2146` |
| `Network.EvmHighestFinalizedBlockNumber` | detail | `erpc/networks.go:L679` |
| `Network.EvmHighestLatestBlockNumber` | detail | `erpc/networks.go:L518` |
| `Network.EvmLowestFinalizedBlockNumber` | detail | `erpc/networks.go:L855` |
| `Network.Forward` | normal | `erpc/networks.go:L943` |
| `Network.GetFinality` | detail | `erpc/networks.go:L1646` |
| `Network.NormalizeResponse` | detail | `erpc/networks.go:L2235` |
| `Network.PostForward.eth_getBlockByNumber` | detail | `architecture/evm/eth_getBlockByNumber.go:L44` |
| `Network.PostForward.eth_sendRawTransaction` | detail | `architecture/evm/eth_sendRawTransaction.go:L280` |
| `Network.PostForwardHook` | detail | `architecture/evm/hooks.go:L64` |
| `Network.PreForwardHook` | detail | `architecture/evm/hooks.go:L41` |
| `Network.PreForwardHook.eth_chainId` | detail | `architecture/evm/eth_chainId.go:L79` |
| `Network.TryForward` | detail | `erpc/networks.go:L1137` |
| `Network.UpstreamLoop` | detail | `erpc/networks.go:L1227` |
| `Network.WaitForMultiplexResult` | normal | `erpc/networks.go:L2058` |
| `Network.forwardAttempt` | normal | `erpc/networks.go:L1188` |
| `PolicyEngine.GetOrdered` | detail | `erpc/networks.go:L1023` |
| `PostgreSQLConnector.Delete` | normal | `data/postgresql.go:L1130` |
| `PostgreSQLConnector.Get` | normal | `data/postgresql.go:L466` |
| `PostgreSQLConnector.List` | normal | `data/postgresql.go:L1165` |
| `PostgreSQLConnector.Lock` | normal | `data/postgresql.go:L526` |
| `PostgreSQLConnector.PublishCounterInt64` | normal | `data/postgresql.go:L683` |
| `PostgreSQLConnector.Set` | normal | `data/postgresql.go:L409` |
| `PostgreSQLConnector.Unlock` | normal | `data/postgresql.go:L589` |
| `PostgreSQLConnector.getCurrentValue` | detail | `data/postgresql.go:L974` |
| `PostgreSQLConnector.getWithWildcard` | detail | `data/postgresql.go:L1006` |
| `Project.Forward` | detail | `erpc/projects.go:L103` |
| `Project.PreForwardHook` | detail | `architecture/evm/hooks.go:L14` |
| `Project.PreForwardHook.eth_blockNumber` | detail | `architecture/evm/eth_blockNumber.go:L16` |
| `Project.PreForwardHook.eth_chainId` | detail | `architecture/evm/eth_chainId.go:L30` |
| `Project.executeShadowRequest` | detail | `erpc/shadow.go:L82` |
| `Query.Execute` | detail | `erpc/query_executor.go:L45,L78,L111,L144,L177` |
| `Query.ForwardSubrequest` | detail | `erpc/query_shim.go:L427` |
| `Query.ResolveQueryBounds` | detail | `erpc/query_executor.go:L277` |
| `Query.ShimBlocks` | detail | `erpc/query_shim.go:L18` |
| `Query.ShimLogs` | detail | `erpc/query_shim.go:L112` |
| `Query.ShimTraces` | detail | `erpc/query_shim.go:L187` |
| `Query.ShimTransactions` | detail | `erpc/query_shim.go:L55` |
| `QueryStream.Handle` | normal | `erpc/request_processor.go:L76` |
| `RateLimiter.DoLimit` | normal | `upstream/ratelimiter_budget.go:L279` |
| `RateLimiter.TryAcquirePermit` | detail | `upstream/ratelimiter_budget.go:L161` |
| `RedisConnector.Delete` | normal | `data/redis.go:L636` |
| `RedisConnector.Get` | normal | `data/redis.go:L341` |
| `RedisConnector.List` | normal | `data/redis.go:L682` |
| `RedisConnector.Lock` | normal | `data/redis.go:L435` |
| `RedisConnector.PublishCounterInt64` | normal | `data/redis.go:L558` |
| `RedisConnector.Set` | normal | `data/redis.go:L281` |
| `RedisConnector.Unlock` | normal | `data/redis.go:L612` |
| `Request.GenerateCacheHash` | detail | `common/json_rpc.go:L1387` |
| `Request.Handle` | direct (fires whenever tracing enabled) | `common/tracing_util.go:L165` (via `StartRequestSpan`) |
| `Request.Lock` | detail | `common/request.go:L925` |
| `Request.RLock` | detail | `common/request.go:L931` |
| `Request.ResolveJsonRpc` | detail | `common/request.go:L939` |
| `Response.IsObjectNull` | detail | `common/response.go:L420` |
| `Response.Lock` | detail | `common/response.go:L84` |
| `Response.RLock` | detail | `common/response.go:L90` |
| `Response.ResolveJsonRpc` | detail | `common/response.go:L293` |
| `Upstream.Forward` | normal | `upstream/upstream.go:L416` |
| `Upstream.PostForwardHook` | detail | `architecture/evm/hooks.go:L110` |
| `Upstream.PostForwardHook.eth_getBlockByNumber` | detail | `architecture/evm/eth_getBlockByNumber.go:L435` |
| `Upstream.PostForwardHook.eth_getBlockReceipts` | detail | `architecture/evm/eth_getBlockReceipts.go:L31` |
| `Upstream.PostForwardHook.eth_getLogs` | detail | `architecture/evm/eth_getLogs.go:L350` |
| `Upstream.PostForwardHook.eth_sendRawTransaction` | detail | `architecture/evm/eth_sendRawTransaction.go:L59` |
| `Upstream.PostForwardHook.trace_filter` | detail | `architecture/evm/trace_filter.go:L362` |
| `Upstream.PreForwardHook` | detail | `architecture/evm/hooks.go:L87` |
| `Upstream.PreForwardHook.eth_chainId` | detail | `architecture/evm/eth_chainId.go:L124` |
| `Upstream.PreForwardHook.eth_getLogs` | detail | `architecture/evm/eth_getLogs.go:L289` |
| `Upstream.PreForwardHook.trace_filter` | detail | `architecture/evm/trace_filter.go:L298` |
| `Upstream.tryForward.PreRequest` | detail | `upstream/upstream.go:L572` |
| `Upstream.tryForward.SendRequest` | detail | `upstream/upstream.go:L630` |
| `UpstreamsRegistry.GetNetworkUpstreams` | detail | `upstream/registry.go:L349` |
| `UpstreamsRegistry.GetSortedUpstreams` | detail | `upstream/registry.go:L387` |
| `UpstreamsRegistry.buildProviderBootstrapTask` | detail | `upstream/registry.go:L485` |
| `UpstreamsRegistry.buildUpstreamBootstrapTask` | detail | `upstream/registry.go:L431` |
| `createSyntheticSuccessResponse` | detail | `architecture/evm/eth_sendRawTransaction.go:L179` |
| `extractTxHashFromSendRawTransaction` | detail | `architecture/evm/eth_sendRawTransaction.go:L136` |
| `verifyAndHandleNonceTooLow` | detail | `architecture/evm/eth_sendRawTransaction.go:L206` |

OTel instrumentation name: `"github.com/erpc/erpc"` (`common/tracing_core.go:L26`).

#### Notable log messages

| Level | Message / pattern | Where | When |
|-------|------------------|-------|------|
| info | `"OpenTelemetry tracing is disabled"` | `common/tracing_core.go:L55` | `tracing.enabled = false` |
| info | `"initializing OpenTelemetry tracing"` + endpoint/protocol/sampleRate/detailed fields | `common/tracing_core.go:L61-L67` | before exporter creation |
| info | `"OpenTelemetry debug logging enabled"` | `common/tracing_core.go:L121` | only if zerolog level ≤ debug |
| info | `"OpenTelemetry tracing initialized successfully"` | `common/tracing_core.go:L146` | after successful init |
| info | `"force-trace matcher configured"` + index/network/method | `common/tracing_core.go:L138-L144` | once per configured matcher |
| trace | `"open telemetry export error"` | `common/tracing_core.go:L126` | OTLP export failure (network errors, etc.) |
| error | `"failed to create span exporter"` | `common/tracing_core.go:L81-L83` | exporter init failure |
| error | `"failed to create resource"` | `common/tracing_core.go:L103-L105` | OTel resource build failure |
| error | `"failed to initialize tracing"` | `erpc/erpc.go:L32` | from `NewERPC` when `InitializeTracing` errors |
| error | `"failed to shutdown tracer provider"` | `erpc/erpc.go:L97` | on shutdown |
| warn | `"invalid log level '%s', defaulting to 'debug'"` | `erpc/init.go:L29` | unrecognized `logLevel` value in config |
| info | `""` (empty message, `config` JSON field) | `erpc/init.go:L40-L41` | at startup when level ≤ info: full redacted config dump |

### Edge cases & gotchas

1. **`initOnce.Do` means tracing config cannot change after first `NewERPC`.** If tests or multi-tenant code creates multiple ERPC instances, only the first one's tracing config takes effect. All subsequent calls to `InitializeTracing` are silently skipped (`common/tracing_core.go:L53`).

2. **`sampleRate: 0` in YAML does NOT mean "never sample" — it means "use the default (1.0)".** `SetDefaults` checks `if c.SampleRate == 0 { c.SampleRate = 1.0 }`. To effectively disable sampling while keeping tracing enabled (e.g., to use only force-trace), you'd need `sampleRate: 0.0000001` or a very small positive value (`common/defaults.go:L629-L631`).

3. **Force-trace network matching requires URL parsing to have happened first.** `SetForceTraceNetwork` is only called after `parseUrlPath` successfully extracts `architecture` and `chainId` from the URL (`erpc/http_server.go:L285-L288`). Admin/healthcheck paths bypass this, so force-trace matchers with network patterns never match admin or healthcheck requests.

4. **`Request.Handle` span is started via `tracer.Start` directly** (not `StartSpan`), meaning it always fires when `IsTracingEnabled` regardless of `IsTracingDetailed`. This span is the per-JSON-RPC-request root inside the `Http.ReceivedRequest` parent. It is the correct attach point for distributed callers to link their traces (`common/tracing_util.go:L165`).

5. **For batch requests, each item in the batch gets its own `Request.Handle` span** as a child of the single `Http.ReceivedRequest` parent (`erpc/http_server.go:L465`). The batch fan-out is concurrent; all child spans may be in-flight simultaneously.

6. **W3C traceparent injection into HTTP responses is unconditional** if tracing is enabled — even for error responses. An attacker could observe trace IDs in error responses. If this is a security concern, `InjectHTTPResponseTraceContext` can be removed, but it is currently always called at `erpc/http_server.go:L701`.

7. **gRPC server has no OTel trace extraction.** Unlike the HTTP server which calls `StartHTTPServerSpan` (which extracts `traceparent`), the gRPC request handler in `erpc/grpc_server.go` has no equivalent. gRPC-originated requests start with a fresh root span, not as children of the caller's trace.

8. **TLS on the OTLP exporter is opt-in and off by default.** The default `secureOption` is `WithInsecure()` for both gRPC and HTTP exporters (`common/tracing_core.go:L187`, `common/tracing_core.go:L208`). Production deployments sending spans to a remote collector over the internet should set `tracing.tls.enabled: true`.

9. **`util.RedactEndpoint` drops the entire host for standard http/https URLs** (the `else` branch at `util/redact.go:L32-L34` produces `scheme#redacted=<hash>`). This means the log entry for an upstream endpoint like `https://eth-mainnet.alchemyapi.io/v2/SECRET_KEY` becomes `https#redacted=ab3c4`. The hostname is not preserved. Only native-protocol and repository-type endpoints keep `scheme://host`.

10. **`console.error` is not available in TypeScript/JS config scripts.** Only `debug`, `info`, `log`, `trace`, and `warn` are registered. A call to `console.error(...)` in user scripts will throw `TypeError: console.error is not a function` at script evaluation time (`common/console.go:L15-L21`).

11. **The `LOG_LEVEL` environment variable is applied twice**: once globally in `initflags.go`/`main()` before config load, and again in `getConfig` where it overrides `cfg.LogLevel`. The second application is what actually persists into the running instance since `erpc/init.go:L27` calls `zerolog.ParseLevel(cfg.LogLevel)`. If you set `LOG_LEVEL=trace`, the effective level is trace regardless of what `logLevel` says in the YAML (`cmd/erpc/main.go:L354-L363`).

12. **OTel debug logging (zerologr bridge) only fires when zerolog global level is `<= Debug` at tracing init time.** If you start with log level info and later switch to debug (via admin API), the OTel SDK's internal logger is not retroactively wired up (`common/tracing_core.go:L119-L122`).

13. **`OTEL_EXPORTER_OTLP_ENDPOINT` env var is silently overridden by the eRPC config default.** eRPC always passes `WithEndpoint(cfg.Endpoint)` to the OTel SDK exporter constructor (`common/tracing_core.go:L197` for gRPC, `L217` for HTTP). This means the standard OTel env var for the OTLP endpoint is ignored — even if a container platform injects it to route spans to a sidecar collector. Operators must set `tracing.endpoint` in the YAML config explicitly; they cannot rely on `OTEL_EXPORTER_OTLP_ENDPOINT` to override the default `localhost:4317`.

14. **`OTEL_TRACES_SAMPLER` and `OTEL_PROPAGATORS` env vars have no effect.** eRPC constructs its own sampler (`common/tracing_core.go:L109`) and propagator (`common/tracing_core.go:L114-L117`) via explicit SDK options, bypassing the SDK's environment-based configuration for both. Setting these env vars does nothing.

15. **`OTEL_EXPORTER_OTLP_HEADERS` is the one env var that CAN take effect** — but only when `tracing.headers` is nil/empty in config. eRPC only calls `WithHeaders` conditionally (`common/tracing_core.go:L200-L202`). If `tracing.headers` is non-empty in config, the env var is ignored; if it is empty, the SDK may pick up `OTEL_EXPORTER_OTLP_HEADERS` as a fallback for authentication headers.

### Source map

- `common/tracing_core.go` — tracer initialization, exporter creation (gRPC+HTTP), sampler construction, forceTraceSampler, ShouldForceTrace/matchesForceTraceMatcher, SetTraceSpanError, ForceFlushTraces, global `IsTracingEnabled`/`IsTracingDetailed` flags
- `common/tracing_util.go` — noopSpan singleton, StartSpan/StartDetailSpan helpers, ExtractHTTPRequestTraceContext, InjectHTTPResponseTraceContext, StartHTTPServerSpan, shouldForceTrace (header/query), EnrichHTTPServerSpan, StartRequestSpan/EndRequestSpan
- `common/config.go` — TracingConfig struct, TracingProtocol constants, ForceTraceMatcher struct, TLSConfig struct, MarshalJSON redaction methods for all secret-bearing types
- `common/defaults.go:L618-L637` — TracingConfig.SetDefaults (protocol, endpoint, sampleRate, serviceName)
- `common/console.go` — JS/TS console.{debug,info,log,trace,warn} → zerolog bridge for Sobek runtimes
- `common/runtime.go:L39` — installs `console` object into sobek global
- `util/redact.go` — RedactEndpoint: SHA-256-based URL sanitization with scheme-aware rules
- `cmd/erpc/main.go:L48-L66` — zerolog global setup: TimeFieldFormat, ErrorMarshalFunc, LOG_WRITER console mode, LOG_LEVEL env override
- `cmd/erpc/initflags.go` — ERPC_NOLOGS=1 → zerolog.Disabled + io.Discard writer
- `erpc/erpc.go:L31-L99` — calls InitializeTracing on startup, registers shutdown goroutine
- `erpc/init.go:L27-L42` — parses cfg.LogLevel into zerolog level, logs full config at info level
- `erpc/http_server.go:L701,L749-L752,L285-L288` — per-request span start/end, response traceparent injection, network context for force-trace
- `docs/kb/_census/metrics.md` — complete authoritative census of all 126 span names and their call sites
