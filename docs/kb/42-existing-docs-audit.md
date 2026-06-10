# KB: Audit of existing docs pages (coverage & staleness)

> status: complete
> source-dirs: docs/pages/**/*.mdx, docs/pages/**/_meta.js, docs/theme.config.tsx, docs/components/, docs/next.config.mjs

## L1 — Capability (CTO view)

The eRPC docs site is a Nextra v3/Next.js application hosted at `docs.erpc.cloud`. It contains approximately 35 `.mdx` pages organized into four navigation sections (Config, Deployment, Operations, plus top-level pages). As of June 2026 the site has been substantially rewritten: most pages use the new `ConfigTabs`/`ConfigCode`/`AISection`/`LLMsTxtLink` component system and contain `<AISection>` exhaustive references. However, **several config defaults documented in the pages differ from the actual Go source code**, and the monitoring page covers only ~67 of the ~125 `erpc_*` Prometheus metrics in the codebase census.

## L2 — Mechanics (staff-engineer view)

**Site framework.** Nextra 3 (`nextra` + `nextra-theme-docs`) on Next.js 14. Configuration in `docs/next.config.mjs` and `docs/theme.config.tsx`. Pages are `.mdx` files under `docs/pages/`. Navigation is driven by `_meta.js` files at each directory level.

**Component system (docs/components/).** Four custom components are exported from `docs/components/index.ts`:
- `AISection` — collapsible `<details>` panel for AI-oriented exhaustive references; detected by `.llms.txt` generator via `data-component="ai-section"`.
- `ConfigTabs` — side-by-side YAML/TypeScript tab switcher; tab selection persisted via `localStorage` key `GlobalConfigTypeTabIndex`.
- `ConfigCode` — syntax-highlighted code block with optional breadcrumb path, file label, and focus-line dimming.
- `LLMsTxtLink` — floating "Open as plain markdown for AI" button; derives `.llms.txt` URL from current `router.asPath`.
- `HeroDiagram` — home-page architecture diagram (React-rendered SVG).

**Theme config.** `docs/theme.config.tsx` — Nextra docs theme with dark mode default, `defaultMenuCollapseLevel: 2` in sidebar, banner linking to GitHub, Telegram chat link, OG image from `https://erpc-test.up.railway.app/og?...`. The `docsRepositoryBase` points to `https://github.com/erpc/erpc/tree/main/docs`.

**Navigation structure.** Pages hidden from sidebar but accessible by URL: `presets/*` (set `display: "hidden"`) and `preview-retry` (referenced in `_meta.js` but no corresponding file found in the worktree, so it is either a future placeholder or was deleted).

**Quality tier.** Pages that have been through the new component system include `server`, `example`, `upstreams`, `failsafe/*`, `auth`, `rate-limiters`, `evm-json-rpc-cache`, `shared-state`, `drivers`, `directives`, `admin`, `monitoring`, `tracing`, `production`, `url`, `healthcheck`, `batch`, `cordoning`, `cli`, `cors`, `networks`, `providers`, `selection-policies`, `kubernetes`, `docker`. These pages have `AISection` blocks and `ConfigTabs`. Lower quality / legacy pages: `index` (uses raw MDX without new components), `why` (no component system), `faq` (minimal), `free` (minimal), `railway` (Steps-based walkthrough), `cloud` (pricing table), `presets/dvn-ready`.

**Coverage gaps.** The following subsystems present in the KB layer have no corresponding doc page:
- gRPC BDS server streaming (KB `03-grpc-server-bds.md`)
- Config loading / TypeScript config format (KB `04`, `05`)
- Upstream lifecycle / state poller (KB `08`)
- The full vendor/provider list detail (KB `09` covers more than `providers.mdx`)
- getlogs splitting mechanics (KB `23`)
- Full metrics reference — monitoring page covers ~67/125 metrics

## L3 — Exhaustive reference (agent view)

### Navigation tree

```
docs/pages/
├── _meta.js                    (top-level nav)
│   ├── index             → Quick start / home
│   ├── why               → Why eRPC?
│   ├── free              → Free & Public RPCs
│   ├── faq               → FAQ
│   ├── config/           → [Config section]
│   ├── deployment/       → [Deployment section]
│   ├── operation/        → [Operations section]
│   └── presets/          → [hidden in nav]
│
├── config/_meta.js
│   ├── example           → Complete config example
│   ├── server            → Server config
│   ├── projects/         → [Projects subsection]
│   │   ├── networks      → Networks
│   │   ├── upstreams     → Upstreams
│   │   ├── providers     → Providers / vendor shorthands
│   │   ├── selection-policies → Selection policy
│   │   └── cors          → CORS
│   ├── failsafe/         → [Failsafe subsection]
│   │   ├── timeout       → Timeout policy
│   │   ├── retry         → Retry policy
│   │   ├── hedge         → Hedge policy
│   │   ├── consensus     → Consensus policy
│   │   ├── integrity     → Integrity & Empty Data
│   │   └── circuit-breaker → Circuit breaker (redirects concept to selection-policy)
│   ├── database/         → [Database subsection]
│   │   ├── drivers       → Database drivers
│   │   ├── evm-json-rpc-cache → EVM JSON-RPC cache
│   │   └── shared-state  → Shared state
│   ├── auth              → Authentication
│   ├── rate-limiters     → Rate limiters
│   └── matcher           → Matcher syntax
│
├── deployment/_meta.js
│   ├── docker            → Docker deployment
│   ├── railway           → Railway
│   ├── kubernetes        → Kubernetes
│   └── cloud             → Hosted cloud
│
├── operation/_meta.js
│   ├── url               → URL & routing
│   ├── healthcheck       → Healthcheck
│   ├── batch             → Batch requests
│   ├── directives        → Directives
│   ├── production        → Production guidelines
│   ├── monitoring        → Monitoring & metrics
│   ├── tracing           → OpenTelemetry tracing
│   ├── admin             → Admin API
│   ├── cordoning         → Cordoning
│   └── cli               → CLI & env vars
│
└── presets/_meta.js (hidden)
    └── dvn-ready         → DVN (LayerZero) preset
```

Sources: `docs/pages/_meta.js`, `docs/pages/config/_meta.js`, `docs/pages/config/projects/_meta.js`, `docs/pages/config/failsafe/_meta.js`, `docs/pages/config/database/_meta.js`, `docs/pages/deployment/_meta.js`, `docs/pages/operation/_meta.js`, `docs/pages/presets/_meta.js`.

### Component APIs

#### `AISection` (`docs/components/AISection.tsx`)

Collapsible `<details>` panel. Collapsed by default. Detected by `.llms.txt` generator via `data-component="ai-section"`.

| Prop | Type | Default | Notes |
|---|---|---|---|
| `title` | `string` | `"Copy for your AI assistant"` | Headline in the `<summary>`. |
| `hint` | `string` | `"Expand for every option, default, and edge case — or copy this entire section into your AI assistant."` | Secondary line in `<summary>`. |
| `defaultOpen` | `boolean` | `false` | When `true`, renders expanded. Keep `false` for human-readable pages. |
| `children` | `React.ReactNode` | required | Body content (shown expanded). |

Renders: `<details className="cv-ai" data-component="ai-section" data-title={title}>`.

#### `ConfigTabs` (`docs/components/ConfigTabs.tsx`)

YAML/TypeScript dual-tab wrapper. Tab selection stored in `localStorage` under key `GlobalConfigTypeTabIndex`. Uses Nextra's `Tabs` component.

| Prop | Type | Default | Notes |
|---|---|---|---|
| `yaml` | `string` | required | YAML code body. |
| `ts` | `string` | required | TypeScript code body. |
| `path` | `string` | — | Breadcrumb path applied to both tabs (e.g. `"projects > upstreams[]"`). |
| `focus` | `string` | — | Lines to highlight (1-indexed, comma-sep, range `"6-8,12"`). Applied to both tabs. |
| `focusYaml` | `string` | — | Tab-specific focus override for YAML. |
| `focusTs` | `string` | — | Tab-specific focus override for TS. |
| `filenameYaml` | `string` | `"erpc.yaml"` | Filename label on YAML tab. |
| `filenameTs` | `string` | `"erpc.ts"` | Filename label on TS tab. |
| `showLineNumbers` | `boolean` | — | Show gutter line numbers. |

#### `ConfigCode` (`docs/components/ConfigCode.tsx`)

Single code block with syntax highlighting (prism-react-renderer), optional breadcrumb path, filename label, and line-focus dimming.

| Prop | Type | Default | Notes |
|---|---|---|---|
| `filename` | `string` | — | Filename label in header. |
| `path` | `string` | — | Breadcrumb (`"projects > upstreams[]"`); rendered with `›` separators. Parsed on `>`, `›`, `/`. |
| `language` | `Language \| string` | required | PrismJS language token (`yaml`, `typescript`, `bash`, `json`, etc.). |
| `focus` | `string` | — | Lines at full opacity; others dimmed. Format: `"1,3-5,8"` (1-indexed). |
| `showLineNumbers` | `boolean` | — | Show line numbers in gutter. |
| `code` | `string` | — | Code body (preferred when wiring from React). |
| `children` | `React.ReactNode` | — | Alternative: inline code as JSX children (preferred in MDX). |

Both `code` and `children` are accepted; `code` wins. Leading/trailing newlines are trimmed. Renders a `<div data-component="config-code">` with optional header + prism-highlighted `<pre>`.

#### `LLMsTxtLink` (`docs/components/LLMsTxtLink.tsx`)

Floating `<a>` button. Derives `.llms.txt` path from `router.asPath` (strips `#fragment` and `?query`, removes trailing `/`). Special case: root path `/` → `/llms.txt`; all other paths → `${path}.llms.txt`. Opens in new tab.

Renders with CSS classes: `cv-llms-link`, `cv-llms-badge`, `cv-llms-text`, `cv-llms-arrow`. Attribute `data-component="llms-txt-link"`.

#### `HeroDiagram` (`docs/components/HeroDiagram.tsx`, `docs/components/hero-diagram.internals.ts`)

Animated architecture diagram used on the home page. No configurable props (inspected only structurally, not audited in depth — not a docs-quality risk).

---

### Per-page audit table

#### `index.mdx` — Quick start / home

| Item | Assessment |
|---|---|
| **Topics covered** | Short intro, 6-step quick start (create config, Docker run, test request, monitoring stack, Grafana access, metrics). |
| **Component usage** | `HeroDiagram`, `LLMsTxtLink`, Nextra `Steps`. Does **not** use `ConfigTabs` or `AISection`. |
| **Quality** | Minimal. The code examples are unlabeled raw fences rather than `ConfigTabs`. No AI-expanded reference. |
| **Missing** | No mention of TypeScript config format. No mention of `erpc start -e` shorthand. Grafana screenshot link is broken or placeholder (`/assets/monitoring-example-erpc.png`). |

**Spot-check 1 — Docker port mapping claim.**
Page shows `-p 4000:4000 -p 4001:4001`. The second port is the metrics port. Source: `common/defaults.go:L749-L769` — `MetricsConfig.SetDefaults` sets port `4001` by default. Claim is correct assuming metrics enabled, but the quick-start doesn't configure `metrics.enabled: true`, so the second port mapping is inactive by default. **Potentially confusing.**

**Spot-check 2 — monitoring docker-compose.**
Page instructs `docker-compose up -d`. The `docker-compose.yml` at repo root exists (`erpc/docker-compose.yml`) and does include Prometheus + Grafana. Claim is consistent.

---

#### `why.mdx` — Why eRPC?

| Item | Assessment |
|---|---|
| **Topics covered** | Use-cases (frontend, indexing, self-hosted LB), feature list, case-study links. |
| **Component usage** | `LLMsTxtLink` only. No `AISection`. |
| **Quality** | Adequate overview. Not technical; no config details. |
| **Missing** | No mention of selection policy (mentioned as "circuit breaker" equivalent), gRPC streaming, sharedState, or x402 auth. Feature list is incomplete vs actual codebase. |

**Spot-check 1 — "Selection policy that scores upstreams in real time on response time, error rate, throttling, block lag, and misbehavior".**
Source: `common/defaults.go:L66-L73` — default evalFunc uses `errorRateAbove`, `throttleRateAbove`, `latencyAbove`, `blockNumberLagAbove`, `blockSecondsLagAbove`. Also includes `latencyDeviationAbove`. "Misbehavior" is not a specific signal (consensus misbehavior tracking is a separate feature). Claim is mostly correct but "misbehavior" is vague.

**Spot-check 2 — case study links (`erpc.cloud/case-studies/moonwell`, `/chronicle`).**
External URLs; cannot verify from source. Marked as acceptable risk.

---

#### `free.mdx` — Free & Public RPCs

| Item | Assessment |
|---|---|
| **Topics covered** | `npx start-erpc` and Docker zero-config start, Railway deploy button, public endpoint URL format. |
| **Component usage** | `LLMsTxtLink`, Nextra `Tabs`. |
| **Quality** | Minimal. The URL shown (`/evm/42161`) differs from the standard documented URL pattern (`/main/evm/42161`) — this is the "no project" mode using the auto-generated default project. |
| **Missing** | No reference to how the public chain list is populated (providers repository config). No `AISection`. |

**Spot-check 1 — URL pattern `/evm/42161` (no project segment).**
Source: `erpc/http_server.go` — URL parsing handles `/<architecture>/<chainId>` as a shorthand that uses the first project. This is valid behavior but undocumented in the URL page. **Minor inconsistency with documented URL patterns on `/operation/url`.**

---

#### `faq.mdx` — FAQ

| Item | Assessment |
|---|---|
| **Topics covered** | Env var interpolation, disabling cache, CORS setup, Docker Compose, default ports, auth. |
| **Component usage** | `LLMsTxtLink`, `ConfigTabs`. |
| **Quality** | Adequate for quick Q&A. Each answer is brief. |
| **Missing** | No `AISection`. No FAQ about TypeScript config, rate limiters, failsafe defaults, or selection policy. |

**Spot-check 1 — "Default port 4000 for HTTP, 4001 for metrics".**
Source: `common/defaults.go:L655-L656` (`HttpPortV4 = 4000`) and `common/defaults.go:L753-L759` (`port = 4001`). Correct.

---

#### `config/example.mdx` — Complete config example

| Item | Assessment |
|---|---|
| **Topics covered** | logLevel, server, metrics, database (cache + connectors), projects (networks + upstreams), rateLimiters. |
| **Component usage** | Full `ConfigTabs`, `AISection`, `LLMsTxtLink`. |
| **Quality** | Good overall reference page. The `AISection` has an exhaustive per-field table. |
| **Staleness** | Several config default values are wrong (see spot-checks below). |

**Spot-check 1 — `server.maxTimeout` default claimed as `30s`.**
Page (`AISection` table, line ~393): `"maxTimeout" | "30s" | "Total deadline for one request"`.
Source: `common/defaults.go:L694-L696`: `d := Duration(150 * time.Second); s.MaxTimeout = &d`.
**STALE.** Actual default is `150s`, not `30s`. The minimum-config example and the full-config example both show `maxTimeout: 30s` as a user-provided value, which is fine; but the table claims `30s` is the default. `docs/pages/config/server.mdx` makes the same error in its field table.

**Spot-check 2 — `server.readTimeout` and `server.writeTimeout` defaults claimed as `0` (no timeout).**
Page (`config/server.mdx` AISection): `"readTimeout" | "0 (no timeout)"`, `"writeTimeout" | "0 (no timeout)"`.
Source: `common/defaults.go:L698-L704`: `readTimeout = 30s`, `writeTimeout = 120s`.
**STALE.** Both have non-zero defaults. The `example.mdx` page table repeats this error (shows `—` for both, implying no default).

**Spot-check 3 — `server.includeErrorDetails` default claimed as `false`.**
Page (`config/server.mdx` AISection, line ~172): `"includeErrorDetails" | "*bool" | "false"`.
Source: `common/defaults.go:L717-L719`: `s.IncludeErrorDetails = util.BoolPtr(true)`.
**STALE.** Default is `true`, not `false`. The human-readable advisory ("Leave `false` for public-facing eRPCs") is sound advice but contradicts the actual default. Also `production.mdx` says "leave `false` for public", implying `false` is the default.

**Spot-check 4 — `server.trustedIPHeaders` default claimed as `["X-Forwarded-For"]`.**
Page (`config/server.mdx` AISection, line ~174): `"trustedIPHeaders" | "[]string" | '["X-Forwarded-For"]'`.
Source: `common/defaults.go:L730-L732`: `s.TrustedIPHeaders = []string{}` (empty slice).
**STALE.** Actual default is an empty list, not `["X-Forwarded-For"]`. The header is only consulted when `trustedIPForwarders` is also set and the request comes from a trusted IP.

**Spot-check 5 — `server.waitBeforeShutdown` default claimed as `0s`.**
Page (`config/server.mdx` AISection, line ~170): `"waitBeforeShutdown" | "duration" | "0s"`.
Source: `common/defaults.go:L709-L711`: `d := Duration(10 * time.Second)`.
**STALE.** Actual default is `10s`, not `0s`. Same for `waitAfterShutdown`.

**Spot-check 6 — `server.grpcPortV4` default claimed as `4100`.**
Page (`config/server.mdx` AISection, line ~159): `"grpcPortV4" | "*int" | "4100"`.
Source: `common/defaults.go:L676-L678`: gRPC port is derived from `httpPortV4` (copies the same value). Default is `4000` (same as HTTP), not `4100`.
**STALE.**

**Spot-check 7 — `server.grpcMaxRecvMsgSize` / `grpcMaxSendMsgSize` defaults claimed as `4 MiB (gRPC default)`.**
Page (`config/server.mdx` AISection, line ~162-163): `"4 MiB (gRPC default)"`.
Source: `common/defaults.go:L688-L692`: `100 * 1024 * 1024` = 100 MiB.
**STALE.** Both default to 100 MiB, not 4 MiB.

**Spot-check 8 — `server.enableGzip` default shown inconsistently.**
`config/server.mdx` table says `true`; `config/example.mdx` table says `false`. Source: `common/defaults.go:L706-L707`: `s.EnableGzip = util.BoolPtr(true)`. The `server.mdx` AISection is correct; `example.mdx` table entry is **STALE/WRONG** (`false` shown).

---

#### `config/server.mdx` — Server config

| Item | Assessment |
|---|---|
| **Topics covered** | All `ServerConfig` fields, TLS, gRPC, aliasing, shutdown semantics, custom headers, gzip, trusted-proxy IP. |
| **Component usage** | Full `ConfigTabs`, `AISection`, `LLMsTxtLink`. |
| **Quality** | High quality; very detailed. Best-in-class AISection on this page. |
| **Staleness** | Multiple wrong defaults (see spot-checks in `example.mdx` above — same AISection table is duplicated). |
| **Missing** | `ExecutionHeaders` field (`executionHeaders: all | none | upstream-only | ...`) documented in `common/config.go:L157-L165` but absent from docs. The `ExecutionHeadersAll` constant is set as default in `defaults.go:L720-L723`. |

---

#### `config/projects/networks.mdx` — Networks

| Item | Assessment |
|---|---|
| **Topics covered** | Network lazy-loading, chain identity, finality semantics, failsafe, selection policy, rate limits, directive defaults, getLogs/trace_filter splitting, static responses. |
| **Component usage** | Full `ConfigTabs`, `AISection`, `LLMsTxtLink`. |
| **Quality** | High quality. Covers the network-level feature surface well. |
| **Missing** | `evm.markEmptyAsErrorMethods` described in integrity page but not surfaced here. `fallbackFinalityDepth` default value. |

**Spot-check 1 — "getLogs splitting `splitOnError: true`" vs default.**
Source: `common/defaults.go:L122`: `GetLogsSplitOnError: util.BoolPtr(true)`. Docs state this is the default. Correct.

**Spot-check 2 — Network `GetLogsMaxAllowedRange` default.**
Source: `common/defaults.go:L121`: `GetLogsMaxAllowedRange: 30_000`. Docs mention this field. The value is accurate.

---

#### `config/projects/upstreams.mdx` — Upstreams

| Item | Assessment |
|---|---|
| **Topics covered** | Endpoint types, EVM config, method filters, failsafe, rate limits, selection-policy hooks, transport (batching, gzip, headers), shadow traffic, integrity checks. |
| **Component usage** | Full `ConfigTabs`, `AISection`, `LLMsTxtLink`. |
| **Quality** | High quality; comprehensive. |
| **Staleness** | Comment "eRPC auto-detects the chain ID and applies default failsafe (15s timeout, 2 retries, circuit breaker at 80% failure rate)" is WRONG. |

**Spot-check 1 — Comment claim "default failsafe (15s timeout, 2 retries, circuit breaker at 80% failure rate)".**
Source `common/defaults.go:L148-L160` (upstream defaults): `MaxAttempts: 1` (1 attempt total = 0 retries), `Timeout: 60s`. No circuit breaker by default (circuit breaker is subsumed by selection policy).
**STALE.** Should read: 60s timeout, 1 attempt (no retry), no circuit breaker.

**Spot-check 2 — Proxy pool round-robin claim.**
Page states: "eRPC selects proxies with a lockless atomic counter — each request increments the counter and picks `counter % len(urls)`." Source: need to verify in `erpc/upstream.go` or similar — this is a plausible implementation but not verified from source here. Marked as **unverified**.

---

#### `config/projects/providers.mdx` — Providers

| Item | Assessment |
|---|---|
| **Topics covered** | URL shorthand, long-form provider config, network filtering (`onlyNetworks`/`ignoreNetworks`), per-network overrides, vendor-specific settings. |
| **Component usage** | Full `ConfigTabs`, `AISection`, `LLMsTxtLink`. |
| **Quality** | Good. Vendor table shows supported shorthands. |
| **Missing** | The `providers[]` vs `upstreams[]` distinction for long-form is explained but the `providers[]` config path in YAML is not used in examples (providers are still shown as `upstreams[] { endpoint: vendor://... }`). The `providers[]` top-level array is separate from `upstreams[]` but docs conflate them. The KB `09-providers-vendors.md` has more detail on each vendor's chain list. |

---

#### `config/projects/selection-policies.mdx` — Selection policy

| Item | Assessment |
|---|---|
| **Topics covered** | `evalFunc` JS API, `evalInterval`, `evalScope`, standard library predicates, `sortByScore`, `stickyPrimary`, `probeExcluded`, `preferTag`/`byTag`/`byVendor`/`byType`. |
| **Component usage** | Nextra `Tabs`/`Tab` (raw, not `ConfigTabs`). No `AISection`. |
| **Quality** | Adequate mechanics. Missing the AISection exhaustive reference. Uses raw `Tabs` not `ConfigTabs`. |
| **Staleness** | Default `evalFunc` shown in docs uses `latencyAbove(3000)` but KB `22` and `circuit-breaker.mdx` show a slightly different predicate (`latencyAbove(10_000)` as the absolute cutoff, with `latencyDeviationAbove(3)` as relative). The inline YAML in the page shows `latencyAbove(3000)` (3 seconds) but `circuit-breaker.mdx` shows `latencyAbove(10_000)` (10 seconds). Inconsistency between pages. |

**Spot-check 1 — default evalFunc latency thresholds.**
`selection-policies.mdx` line 69: `.excludeIf(any(all(samplesAbove(20), latencyAbove(3000), latencyDeviationAbove(3, { mode: 'majority' })), latencyAbove(10_000)))`.
`circuit-breaker.mdx` line 19: `.excludeIf(any(all(samplesAbove(20), latencyAbove(3000), latencyDeviationAbove(3)), latencyAbove(30_000)))`.
KB `22-selection-policies-scoring.md` should be the ground truth. The two docs pages differ on the `latencyAbove` absolute fallback (10k vs 30k ms). **INCONSISTENCY between pages.**

---

#### `config/projects/cors.mdx` — CORS

| Item | Assessment |
|---|---|
| **Topics covered** | `allowedOrigins`, `allowedMethods`, `allowedHeaders`, `exposedHeaders`, `allowCredentials`, `maxAge`. |
| **Component usage** | `ConfigTabs`, `AISection`, `LLMsTxtLink`. |
| **Quality** | Good. Clear examples. |
| **Missing** | CORS config path is `projects[].cors`; `admin.cors` is mentioned in `admin.mdx` as independent. |

---

#### `config/failsafe.mdx` — Failsafe overview

| Item | Assessment |
|---|---|
| **Topics covered** | Overview of 5 policies, `matchMethod`/`matchFinality` scoping, evaluation order. |
| **Component usage** | `ConfigTabs`, `AISection`, `LLMsTxtLink`. |
| **Quality** | Good overview. Correctly explains network-vs-upstream scope. |
| **Missing** | Does not mention `matchFinality` in depth — covered in per-policy pages. |

---

#### `config/failsafe/timeout.mdx` — Timeout policy

| Item | Assessment |
|---|---|
| **Topics covered** | Fixed vs adaptive (`AdaptiveDuration`) `base`/`quantile`/`min`/`max`, network vs upstream scope. |
| **Component usage** | `ConfigTabs`, `AISection`, `LLMsTxtLink`. |
| **Quality** | High quality. Adaptive duration is well-explained. |
| **Missing** | Does not document the exact semantics of how the quantile is sampled (rolling-window from health tracker). |

---

#### `config/failsafe/retry.mdx` — Retry policy

| Item | Assessment |
|---|---|
| **Topics covered** | `maxAttempts`, `delay`, `backoffFactor`, `backoffMaxDelay`, `jitter`, `emptyResultAccept`, `emptyResultConfidence`, `emptyResultMaxAttempts`, `emptyResultDelay`, `blockUnavailableDelay`. |
| **Component usage** | `ConfigTabs`, `AISection`, `LLMsTxtLink`. |
| **Quality** | Excellent. Covers the empty-result dimension clearly. |
| **Missing** | Doesn't document the network-scope retry behavior of rotating across upstreams vs upstream-scope retry to the same upstream. Mentioned in intro but not in the AISection field table. |

---

#### `config/failsafe/hedge.mdx` — Hedge policy

| Item | Assessment |
|---|---|
| **Topics covered** | Adaptive `delay` with `quantile`/`min`/`max`, `maxCount`. |
| **Component usage** | `ConfigTabs`, `AISection`, `LLMsTxtLink`. |
| **Quality** | Good. Concise with practical example. |
| **Missing** | No documentation of `hedge.delay` as a fixed scalar (not just adaptive); docs only show the object form. The scalar form `delay: 500ms` is valid and common in examples. |

---

#### `config/failsafe/consensus.mdx` — Consensus policy

| Item | Assessment |
|---|---|
| **Topics covered** | `maxParticipants`, `agreementThreshold`, `disputeBehavior`, `lowParticipantsBehavior`, `ignoreFields`, `preferNonEmpty`, `preferLargerResponses`, `preferHighestValueFor`, `punishMisbehavior`, `misbehaviorsDestination`. |
| **Component usage** | `ConfigTabs`, `AISection`, `LLMsTxtLink`. |
| **Quality** | High quality. The metrics for consensus (`erpc_consensus_*`) are referenced in the AISection. |
| **Missing** | `disputeBehavior` values not fully enumerated. `misbehaviorsDestination` connector path not explained. |

---

#### `config/failsafe/integrity.mdx` — Integrity & Empty Data

| Item | Assessment |
|---|---|
| **Topics covered** | All `directiveDefaults` integrity flags, block tracking, response validation, empty/missing-result handling. |
| **Component usage** | `ConfigTabs`, `AISection`, `LLMsTxtLink`. |
| **Quality** | Good. Comprehensive list of directives. |
| **Missing** | Does not explain the `evm.integrity.*` upstream-level config fields (vs the network-level `directiveDefaults`). These are separate namespaces in the config. |

---

#### `config/failsafe/circuit-breaker.mdx` — Circuit breaker

| Item | Assessment |
|---|---|
| **Topics covered** | Conceptual redirect — explains that circuit breaker is the selection policy. Maps closed/open/half-open states to selection-policy concepts. Tuning example. |
| **Component usage** | Raw markdown. No custom components. No `AISection`. |
| **Quality** | Conceptually correct and useful for operators migrating from other tools. |
| **Missing** | Does not explain the `circuitBreaker` field that still exists on `upstream.failsafe[]` (documented in `upstreams.mdx`). The `circuitBreaker` YAML key at the upstream level is still a valid config option; this page doesn't acknowledge that dual existence. |

**Spot-check 1 — `probeExcluded` parameters shown in docs.**
`circuit-breaker.mdx` shows `.probeExcluded({ sampleRate: 0.1, minSamples: 10 })` as the default. `selection-policies.mdx` shows `.probeExcluded({ sampleRate: 0.1, minSamples: 10, minSamplesWindow: '60s', maxConcurrent: 4, timeout: '10s' })`. The longer form is the full default. **INCONSISTENCY.**

---

#### `config/database/drivers.mdx` — Database drivers

| Item | Assessment |
|---|---|
| **Topics covered** | Memory, Redis, PostgreSQL, DynamoDB drivers with config options. |
| **Component usage** | `ConfigTabs`, `AISection`, `LLMsTxtLink`. |
| **Quality** | Good practical examples for each driver. |
| **Missing** | Does not have a `title` in the frontmatter (`description` only). The `## Drivers` heading is H2 inside the file, not H1 — minor structure issue. |

---

#### `config/database/evm-json-rpc-cache.mdx` — EVM JSON-RPC cache

| Item | Assessment |
|---|---|
| **Topics covered** | Connectors, policies, finality states, empty-response handling, size limits, compression, read/write split (`appliesTo`), per-method behavior overrides. |
| **Component usage** | `ConfigTabs`, `AISection`, `LLMsTxtLink`. |
| **Quality** | Very high quality. One of the more complete pages. |
| **Missing** | The `params` matcher in cache policies is referenced but not documented in depth on this page. The cache key derivation is not explained. |

**Spot-check 1 — "Caching is non-blocking on the critical path".**
Source: `erpc/cache.go` / `erpc/network.go` — cache reads and writes are done in goroutines with `go func()` and errors are logged but not returned to callers. Claim is correct.

**Spot-check 2 — "Sizing: caching 70M blocks + 10M txs + 10M traces on Arbitrum needs ~200 GB".**
Source: no code reference; this is an empirical/operational observation. Not verifiable from code; marked as **unverified operational estimate**.

---

#### `config/database/shared-state.mdx` — Shared state

| Item | Assessment |
|---|---|
| **Topics covered** | `clusterKey`, connector (Redis/PostgreSQL), `fallbackTimeout`, distributed locking (`lockTtl`, `lockMaxWait`, `updateMaxWait`). |
| **Component usage** | `ConfigTabs`, `AISection`, `LLMsTxtLink`. |
| **Quality** | High quality. |
| **Missing** | Does not explain the memory connector (single-instance default). The `database.sharedState.connector.driver: memory` option is valid and is the default. |

---

#### `config/auth.mdx` — Authentication

| Item | Assessment |
|---|---|
| **Topics covered** | `secret`, `network`, `jwt`, `siwe`, `x402` strategies; `ignoreMethods`/`allowMethods`; `rateLimitBudget` per strategy; `database` strategy (internal). |
| **Component usage** | `ConfigTabs`, `AISection`, `LLMsTxtLink`. |
| **Quality** | High quality. Good per-strategy sections. |
| **Missing** | The `database` strategy config path for API-key connector is not explained here (deferred to admin page). The `jwt.verificationId` field and multi-key rotation are not documented. |

---

#### `config/rate-limiters.mdx` — Rate limiters

| Item | Assessment |
|---|---|
| **Topics covered** | `store.driver` (redis/memory), `budgets[]`, `rules[]` with `maxCount`/`period`/`waitTime`/`perIP`/`perUser`/`perNetwork`; auto-tuner (`rateLimitAutoTune`). |
| **Component usage** | `ConfigTabs`, `AISection`, `LLMsTxtLink`. |
| **Quality** | High quality. Covers `perIP`/`perUser`/`perNetwork` scoping well. |
| **Missing** | The `rateLimiters.store` field is documented here as a top-level option but code also shows that budgets can be embedded under `rateLimiters.budgets` without a store (defaults to in-process memory token bucket). The relationship between `store` and per-upstream budgets is not clearly articulated. |

---

#### `config/matcher.mdx` — Matcher syntax

| Item | Assessment |
|---|---|
| **Topics covered** | Wildcards (`*`, empty), OR (`|`), AND (`&`), NOT (`!`), grouping `()`, numeric comparisons (`>`, `<`, `>=`, `<=`, `=`). |
| **Component usage** | `LLMsTxtLink` only. No `ConfigTabs` or `AISection`. |
| **Quality** | Minimal. Covers the basics but lacks deeper examples. |
| **Missing** | Does not explain where matchers are used for `params` matching (cache policies support param-level matching). No hex vs decimal notation depth. No escaping rules. |

---

#### `operation/url.mdx` — URL & routing

| Item | Assessment |
|---|---|
| **Topics covered** | Standard path `/<projectId>/<architecture>/<chainId>`, network alias path, multi-chain batch (with `networkId` in body), domain aliasing, HTTP methods, query-param/header directives. |
| **Component usage** | `ConfigTabs`, `AISection`, `LLMsTxtLink`. |
| **Quality** | High quality. Well-organized with examples. |
| **Missing** | Does not explain the 2-segment path `/<architecture>/<chainId>` (no project, uses first project by default) as used in `free.mdx`. `Content-Type` requirements for POST. |

---

#### `operation/healthcheck.mdx` — Healthcheck

| Item | Assessment |
|---|---|
| **Topics covered** | `healthCheck.mode` (`simple`/`networks`/`verbose`), `defaultEval`, auth, Kubernetes probe examples, eval strategy syntax. |
| **Component usage** | `ConfigTabs`, `AISection`, `LLMsTxtLink`. |
| **Quality** | Good. Kubernetes patterns are practical and accurate. |
| **Missing** | Does not document all `eval` strategy values. The `any:initializedUpstreams` eval string is shown but not explained in detail. |

**Spot-check 1 — Config path `healthCheck` (camelCase).**
Source: `common/config.go` — `HealthCheck *HealthCheckConfig \`yaml:"healthCheck"\``. Page uses `healthCheck`. Correct.

---

#### `operation/batch.mdx` — Batch requests

| Item | Assessment |
|---|---|
| **Topics covered** | Outgoing batch config (`supportsBatch`, `batchMaxSize`, `batchMaxWait`), incoming batch always accepted, multi-chain batches. |
| **Component usage** | `ConfigTabs`, `AISection`, `LLMsTxtLink`. |
| **Quality** | Good. Anti-pattern warning is helpful. |
| **Missing** | Does not document what happens to individual failures within a batch (HTTP 200 with per-item `error` fields). Does not mention multiplexing (deduplication of identical in-flight requests). |

---

#### `operation/directives.mdx` — Directives

| Item | Assessment |
|---|---|
| **Topics covered** | All `X-ERPC-*` headers and `?*=` query param forms; `retryEmpty`, `retryPending`, `skipCacheRead`, `useUpstream`, `skipInterpolation`, block-integrity directives, response validation directives; `directiveDefaults` config. |
| **Component usage** | `ConfigTabs`, `AISection`, `LLMsTxtLink`. |
| **Quality** | Very high quality. The AISection exhaustive table covers all known directives. |
| **Missing** | Does not explain that `skipCacheRead` accepts a wildcard connector-ID pattern (though the example does show `'memory*'`). |

---

#### `operation/monitoring.mdx` — Monitoring & metrics

| Item | Assessment |
|---|---|
| **Topics covered** | Metrics endpoint config, error label mode, histogram buckets, cardinality reduction (`histogramDropLabels`, `histogramLabelOverrides`), ~67 `erpc_*` metric names with types and descriptions. |
| **Component usage** | `ConfigTabs`, `AISection`, `LLMsTxtLink`. |
| **Quality** | Good but **incomplete metric census**. |
| **Staleness** | The metric table has ~67 entries. The KB census at `docs/kb/_census/metrics.md` lists ~125 metrics. **~58 metrics are absent from the monitoring page.** Notable absent metrics: all `erpc_consensus_*`, `erpc_cache_connector_*` block numbers, `erpc_cache_*_duration_seconds`, `erpc_grpc_bds_*`, `erpc_cache_get_age_guard_reject_total`, `erpc_cache_set_compressed_bytes_total`, and many more. |

**Spot-check 1 — `erpc_selection_position` labels described as `project, network, method, upstream`.**
Source: `docs/kb/_census/metrics.md` — labels for `erpc_selection_position` include `project, network, method, upstream`. Claim is consistent.

**Spot-check 2 — `MetricsConfig` field table: `enableGzip` default `false`.**
Source: `common/defaults.go:L706-L708`: `EnableGzip = true`. **STALE.** Page shows `false`; actual default is `true`.

---

#### `operation/tracing.mdx` — OpenTelemetry tracing

| Item | Assessment |
|---|---|
| **Topics covered** | OTLP endpoint, gRPC vs HTTP protocol, `sampleRate`, `forceTraceMatchers`, `serviceName`, `resourceAttributes`, `detailed`, TLS config. |
| **Component usage** | `ConfigTabs`, `AISection`, `LLMsTxtLink`. |
| **Quality** | High quality. `forceTraceMatchers` is a unique eRPC feature explained clearly. |
| **Missing** | Does not document the `detailed: true` attribute list (which high-cardinality fields are added). |

---

#### `operation/admin.mdx` — Admin API

| Item | Assessment |
|---|---|
| **Topics covered** | `erpc_taxonomy`, `erpc_config`, `erpc_project`, `erpc_addApiKey`, `erpc_listApiKeys`, `erpc_updateApiKey`, `erpc_deleteApiKey`; admin auth; admin CORS. |
| **Component usage** | `ConfigTabs`, `AISection`, `LLMsTxtLink`. |
| **Quality** | High quality. Admin API methods are well-documented with request/response examples. |
| **Missing** | Does not document `erpc_cordonUpstream` / `erpc_uncordonUpstream` (those are in `cordoning.mdx`). |

**Spot-check 1 — "admin.cors default: `allowedOrigins: ["*"]`".**
Source: `common/defaults.go:L769-L788` — `AdminConfig.SetDefaults` calls `a.CORS.SetDefaults()`. `CORSConfig.SetDefaults` in `common/defaults.go` sets `AllowedOrigins: ["*"]` when empty. Claim is correct.

---

#### `operation/cordoning.mdx` — Cordoning

| Item | Assessment |
|---|---|
| **Topics covered** | `erpc_cordonUpstream`, `erpc_uncordonUpstream` with method-scoped cordon, use-cases (incident, maintenance, testing), independence from metric-driven exclusion, `.removeCordoned()` in custom `evalFunc`. |
| **Component usage** | Raw markdown with code blocks. No custom components. No `AISection`. |
| **Quality** | Good conceptual and operational content. |
| **Missing** | `AISection` exhaustive reference. No note that cordon state is stored in shared state (Redis) and is cluster-wide when `sharedState` is configured. |

**Spot-check 1 — "Within one evalInterval (default 15s) the selection policy re-evaluates".**
Source: `common/defaults.go:L2519` references `15s` for JS evaluation interval. Page claims default `evalInterval: 15s`. Correct.

---

#### `operation/cli.mdx` — CLI & env vars

| Item | Assessment |
|---|---|
| **Topics covered** | `erpc start`, `erpc validate`, `--config`/`-c`, `--endpoint`/`-e`, `--require-config`, environment variables. |
| **Component usage** | `ConfigTabs`, `AISection`, `LLMsTxtLink`. |
| **Quality** | Good. |
| **Missing** | Env variable list may be incomplete — `LOG_WRITER`, `GOGC`, `GOMEMLIMIT` are mentioned in `production.mdx` but not in `cli.mdx`. |

---

#### `operation/production.mdx` — Production guidelines

| Item | Assessment |
|---|---|
| **Topics covered** | GOGC/GOMEMLIMIT, failsafe policy recommendations, cache selection, shared state, chain ID explicit config, zero-downtime healthcheck drain (Cilium/Envoy pattern), `responseHeaders` for instance ID, `includeErrorDetails`, `trustedIPForwarders`/`trustedIPHeaders`. |
| **Component usage** | `ConfigTabs`, `AISection`, `LLMsTxtLink`. |
| **Quality** | High quality operational guide. |
| **Staleness** | Recommends `includeErrorDetails: false` for production, but the actual default is `true` (see `server.mdx` spot-check 3). The page should note the default and advise to explicitly set `false` in production. |

---

#### `deployment/docker.mdx` — Docker deployment

| Item | Assessment |
|---|---|
| **Topics covered** | Multi-arch image, quick start, docker-compose, custom NPM modules for TS configs, port mapping, env vars, production tuning, healthcheck. |
| **Component usage** | `ConfigTabs`, `AISection`, `LLMsTxtLink`. |
| **Quality** | High quality. docker-compose examples are practical. |
| **Missing** | Does not mention `ghcr.io/erpc/erpc:latest` vs pinned version tags. |

---

#### `deployment/kubernetes.mdx` — Kubernetes

| Item | Assessment |
|---|---|
| **Topics covered** | ConfigMap+Secret, Deployment, Service, readiness/liveness probes, HPA, graceful shutdown, PostgreSQL StatefulSet, Helm chart status. |
| **Component usage** | `ConfigTabs`, `AISection`, `LLMsTxtLink`. |
| **Quality** | High quality. Real manifests. |
| **Missing** | Does not mention `kube/` directory in repo (`docs/pages/deployment/kubernetes.mdx` references it but may be incomplete). |

---

#### `deployment/railway.mdx` — Railway

| Item | Assessment |
|---|---|
| **Topics covered** | One-click deploy via Railway template, config customization, CORS note for frontend usage. |
| **Component usage** | Nextra `Steps`, `Callout`. No `ConfigTabs`, no `AISection`. |
| **Quality** | Minimal walkthrough. |
| **Missing** | TypeScript config flow. No `AISection`. "Config customizaiton" heading has a typo ("customizaiton"). |

---

#### `deployment/cloud.mdx` — Hosted cloud

| Item | Assessment |
|---|---|
| **Topics covered** | Pricing table (compute, storage), example cost breakdown, contact link. |
| **Component usage** | `LLMsTxtLink` only. |
| **Quality** | Minimal. |
| **Missing** | No SLA, no technical details, no `AISection`. Pricing may be stale. |

---

#### `presets/dvn-ready.mdx` — DVN (LayerZero)

| Item | Assessment |
|---|---|
| **Topics covered** | Multi-provider consensus config for DVN operators. |
| **Component usage** | Not fully read; presets are hidden from nav. |
| **Quality** | Specialized use-case preset. |
| **Missing** | Not accessible from nav; hard to discover. |

---

### Behaviors & algorithms (component system specifics)

#### `ConfigTabs` tab persistence

The `storageKey="GlobalConfigTypeTabIndex"` prop is set on every `ConfigTabs` instance. Nextra's `Tabs` component uses `localStorage[storageKey]` to persist the selected index. Any page using raw `<Tabs storageKey="GlobalConfigTypeTabIndex">` (e.g. `selection-policies.mdx`) participates in the same sync. Pages using a different `storageKey` (or no key) are independent.

#### `AISection` and `.llms.txt`

The `.llms.txt` generator detects `data-component="ai-section"` elements and inlines their body fully expanded. The `defaultOpen={false}` prop only affects the browser rendering; AI-side consumers always see the full content. The `title` attribute is exposed as `data-title` for the generator to use as a section heading.

#### `LLMsTxtLink` path derivation

Logic: `path = router.asPath.split("#")[0].split("?")[0].replace(/\/$/, "")`. Then: if `path === "" || path === "/"` → `/llms.txt`; else `${path}.llms.txt`. Source: `docs/components/LLMsTxtLink.tsx:L11-L12`.

### Observability

No Prometheus metrics are emitted by the docs site itself (it is a static Next.js site). The `docs/app/og/route.tsx` generates OG images server-side.

### Edge cases & gotchas

1. **`preview-retry` nav entry references a non-existent page.** `docs/pages/_meta.js` defines `"preview-retry": { display: "hidden" }` but no corresponding `preview-retry.mdx` or `preview-retry/` directory exists in the worktree. This will cause a Nextra build error or a 404 if someone navigates to `/preview-retry`. Source: `docs/pages/_meta.js`.

2. **`config/database/drivers.mdx` has no H1 heading** — opens with `## Drivers` (H2). Nextra uses the first H1 as the page title for OG/SEO. Source: `docs/pages/config/database/drivers.mdx:L12`.

3. **`server.maxTimeout` default discrepancy.** Documented as `30s` in multiple places; actual default is `150s`. Any operator who relies on the documented default will misconfigure their server timeout. Source: `common/defaults.go:L694-L696` vs `docs/pages/config/server.mdx:L164`, `docs/pages/config/example.mdx:L393`.

4. **`includeErrorDetails` default is `true`, not `false`.** The docs strongly recommend setting it to `false` for production while implying `false` is the default. Operators reading the docs may not realize they need to explicitly set it. Source: `common/defaults.go:L717-L719` vs `docs/pages/config/server.mdx:L172`.

5. **`trustedIPHeaders` default is `[]` (empty), not `["X-Forwarded-For"]`.** Operators who don't read carefully may skip setting `trustedIPHeaders` thinking it defaults to X-Forwarded-For. Source: `common/defaults.go:L730-L732` vs `docs/pages/config/server.mdx:L174`.

6. **`gRPC default port` is `4000` (same as HTTP), not `4100`.** Derived at runtime to match `httpPortV4`. The `grpcEnabled: true` example in `server.mdx` shows explicit `grpcPortV4: 4100` which overrides the default — this is probably intentional advice to use a different port, but the default table is wrong. Source: `common/defaults.go:L676-L678` vs `docs/pages/config/server.mdx:L159`.

7. **`grpcMaxRecvMsgSize`/`grpcMaxSendMsgSize` defaults are `100 MiB`, not `4 MiB`.** Source: `common/defaults.go:L688-L692` vs `docs/pages/config/server.mdx:L162-L163`.

8. **`readTimeout` default is `30s`, `writeTimeout` default is `120s`** — docs say both are `0` (no timeout). Source: `common/defaults.go:L698-L704`.

9. **`waitBeforeShutdown`/`waitAfterShutdown` defaults are `10s`, not `0s`.** Source: `common/defaults.go:L709-L715` vs docs tables.

10. **Upstream default failsafe is `1 attempt`, `60s timeout`** — docs comment says "2 retries, circuit breaker at 80%, 15s timeout". Source: `common/defaults.go:L148-L160`.

11. **~58 Prometheus metrics absent from monitoring docs.** Census at `docs/kb/_census/metrics.md` has ~125 metrics; `operation/monitoring.mdx` documents ~67. Absent include all `erpc_consensus_*`, `erpc_cache_connector_*`, timing histograms for cache ops, `erpc_grpc_bds_*`, etc.

12. **`selection-policies.mdx` vs `circuit-breaker.mdx` default `evalFunc` inconsistency.** The absolute latency threshold in `probeExcluded` and the `latencyAbove` fallback value differ between the two pages.

13. **`railway.mdx` has a typo** in the heading "Config customizaiton". Source: `docs/pages/deployment/railway.mdx:L29`.

14. **`free.mdx` URL format `/<architecture>/<chainId>` (2-segment, no project)** is not documented on the URL routing page, creating a discrepancy for users who copy the example.

15. **`MetricsConfig.enableGzip` default shown as `false`** in `monitoring.mdx` AISection field table; actual default is `true`. Source: `common/defaults.go:L706-L708`.

### Source map

- `docs/pages/_meta.js` — Top-level navigation ordering and display modes for all sections.
- `docs/pages/config/_meta.js` — Config section nav (example, server, projects, failsafe, database, auth, rate-limiters, matcher).
- `docs/pages/config/projects/_meta.js` — Projects sub-nav (networks, upstreams, providers, selection-policies, cors).
- `docs/pages/config/failsafe/_meta.js` — Failsafe sub-nav (timeout, retry, hedge, consensus, integrity, circuit-breaker).
- `docs/pages/config/database/_meta.js` — Database sub-nav (drivers, evm-json-rpc-cache). Note: missing `shared-state` from `_meta.js` — it does NOT appear in this file but exists as `shared-state.mdx`. This means it may be excluded from nav. Source: `docs/pages/config/database/_meta.js`.
- `docs/pages/deployment/_meta.js` — Deployment sub-nav (docker, railway, kubernetes, cloud).
- `docs/pages/operation/_meta.js` — Operations sub-nav (url, healthcheck, batch, directives, production, monitoring, tracing, admin, cordoning, cli).
- `docs/pages/presets/_meta.js` — Presets sub-nav (dvn-ready); hidden from sidebar.
- `docs/theme.config.tsx` — Nextra theme: logo, GitHub link, Telegram chat, banner, sidebar collapse level, dark mode default, OG image generator URL, footer.
- `docs/components/AISection.tsx` — Collapsible AI-reference panel component.
- `docs/components/ConfigTabs.tsx` — YAML/TS dual-tab code block.
- `docs/components/ConfigCode.tsx` — Single syntax-highlighted code block with focus/breadcrumb.
- `docs/components/LLMsTxtLink.tsx` — Floating `.llms.txt` link button.
- `docs/components/HeroDiagram.tsx` — Animated architecture diagram (home page only).
- `docs/components/index.ts` — Component barrel export.
- `common/defaults.go` — Ground truth for all config default values (used to verify staleness of docs claims).
