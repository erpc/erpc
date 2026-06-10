# Docs IA Proposal C — Conservative Evolution

> Lens: keep every existing URL intact (zero redirects), fix grouping problems the audit
> found, add NEW pages only for uncovered capabilities, apply the 3-level page anatomy
> everywhere. Cheapest migration, zero link rot, full brief delivery.

---

## 1. Nav Tree

Legend: **KEPT** = existing page at same URL, **NEW** = no MDX exists today.

```
docs.erpc.cloud/
│
├── /                          Quick start                   KEPT  index.mdx
├── /why                       Why eRPC?                     KEPT
├── /free                      Free & Public RPCs            KEPT
├── /faq                       FAQ                           KEPT
│
│── [separator] Config ─────────────────────────────────────────────────────────
│
├── /config/
│   ├── /config/example        Full config example           KEPT
│   ├── /config/server         Server                        KEPT
│   │
│   ├── /config/projects/      [group: Projects]
│   │   ├── /config/projects/networks       Networks         KEPT
│   │   ├── /config/projects/upstreams      Upstreams        KEPT
│   │   ├── /config/projects/providers      Providers        KEPT
│   │   ├── /config/projects/selection-policies  Selection policy  KEPT
│   │   └── /config/projects/cors           CORS             KEPT
│   │
│   ├── /config/failsafe/      [group: Failsafe]
│   │   ├── /config/failsafe/timeout        Timeout          KEPT
│   │   ├── /config/failsafe/retry          Retry            KEPT
│   │   ├── /config/failsafe/hedge          Hedge            KEPT
│   │   ├── /config/failsafe/consensus      Consensus        KEPT
│   │   ├── /config/failsafe/integrity      Integrity        KEPT
│   │   └── /config/failsafe/circuit-breaker  Circuit breaker  KEPT
│   │
│   ├── /config/database/      [group: Database]
│   │   ├── /config/database/drivers        Drivers          KEPT
│   │   ├── /config/database/evm-json-rpc-cache  EVM Cache   KEPT
│   │   └── /config/database/shared-state   Shared state     KEPT  (add to _meta.js)
│   │
│   ├── /config/auth           Auth                          KEPT
│   ├── /config/rate-limiters  Rate limiters                 KEPT
│   └── /config/matcher        Matcher syntax                KEPT
│
│── [separator] Deployment ──────────────────────────────────────────────────────
│
├── /deployment/
│   ├── /deployment/docker     Docker                        KEPT
│   ├── /deployment/kubernetes Kubernetes                    KEPT
│   ├── /deployment/railway    Railway                       KEPT
│   └── /deployment/cloud      Cloud                         KEPT
│
│── [separator] Operations ──────────────────────────────────────────────────────
│
├── /operation/
│   ├── /operation/url         URL & routing                 KEPT
│   ├── /operation/healthcheck Healthcheck                   KEPT
│   ├── /operation/batch       Batching & multiplexing       KEPT  (rename title only)
│   ├── /operation/directives  Directives                    KEPT
│   ├── /operation/production  Production checklist          KEPT
│   ├── /operation/monitoring  Monitoring & metrics          KEPT
│   ├── /operation/tracing     Tracing & logging             KEPT
│   ├── /operation/admin       Admin API                     KEPT
│   ├── /operation/cordoning   Cordoning                     KEPT
│   └── /operation/cli         CLI & env vars                KEPT
│
│── [separator] Capabilities ────────────────────────────────────────────────────
│
├── NEW top-level section: "Capabilities" (display: "children")
│   Added as `capabilities` key in docs/pages/_meta.js after the Operations separator.
│   All pages are NEW MDX files under docs/pages/capabilities/.
│
│   ├── /capabilities/request-lifecycle     How a request flows    NEW
│   ├── /capabilities/evm-method-handlers   EVM method handling    NEW
│   ├── /capabilities/getlogs-splitting     getLogs auto-splitting NEW
│   ├── /capabilities/static-responses      Static responses       NEW
│   ├── /capabilities/shadow-upstreams      Shadow upstreams       NEW
│   ├── /capabilities/lanes                 Lanes & concurrency    NEW
│   ├── /capabilities/errors                Error taxonomy         NEW
│   └── /capabilities/simulator             Simulator              NEW
│
│── [separator] Reference ───────────────────────────────────────────────────────
│
└── NEW top-level section: "Reference" (display: "children")
    Added as `reference` key in docs/pages/_meta.js.
    All pages are NEW MDX files under docs/pages/reference/.

    ├── /reference/config-formats   Config formats (YAML vs TS)  NEW
    ├── /reference/grpc-bds         gRPC / BDS streaming         NEW
    └── /reference/metrics          Full metrics reference        NEW
```

### Notes on zero-redirect enforcement

- `/config/database/shared-state` already has an MDX file (`shared-state.mdx`). It was
  accidentally omitted from `docs/pages/config/database/_meta.js`. Fix: add
  `"shared-state": { title: "Shared state" }` to that file. URL unchanged.
- The `presets/dvn-ready` page stays hidden (`display: "hidden"`) — its URL is
  unchanged, just not promoted in sidebar.
- `preview-retry` entry in `_meta.js` remains `display: "hidden"` (dead entry, but
  removing it is safe since no MDX exists; however leaving it avoids any hypothetical
  Nextra warning about unexpected keys — leave it as-is or silently remove it).
- No existing page is moved. Title-only renames (e.g. "Batching" → "Batching &
  multiplexing") do NOT change URLs.

---

## 2. Page List — KB File Mapping

Every KB file (01–42) must map to at least one page. Below: page slug → KB sources.
Pages marked `(existing)` are KEPT pages that absorb additional KB context.

### Top-level pages

| Page slug | Status | Primary KB sources |
|---|---|---|
| `/` | KEPT | `01-request-lifecycle.md` (architecture intro), `39-evm-architecture-overview.md` |
| `/why` | KEPT | `01-request-lifecycle.md` (L1 capabilities), `39-evm-architecture-overview.md` |
| `/free` | KEPT | `04-cli-bootstrap-config-loading.md` (npx/quickstart), `32-url-structure-routing.md` |
| `/faq` | KEPT | general — pick from multiple KB files as needed |

### Config section

| Page slug | Status | Primary KB sources |
|---|---|---|
| `/config/example` | KEPT | `04-cli-bootstrap-config-loading.md`, `05-config-formats-typescript.md` |
| `/config/server` | KEPT | `02-http-server.md`, `37-clients-proxy-pools.md` |
| `/config/projects/networks` | KEPT | `07-networks.md`, `23-evm-getlogs-splitting.md`, `34-static-responses.md` |
| `/config/projects/upstreams` | KEPT | `08-upstreams-lifecycle.md`, `09-providers-vendors.md`, `35-shadow-upstreams.md`, `37-clients-proxy-pools.md` |
| `/config/projects/providers` | KEPT | `09-providers-vendors.md` |
| `/config/projects/selection-policies` | KEPT | `22-selection-policies-scoring.md`, `26-health-tracker-scoring-inputs.md`, `36-lanes-concurrency.md` |
| `/config/projects/cors` | KEPT | `06-projects-cors.md` |
| `/config/failsafe/timeout` | KEPT | `12-failsafe-timeout.md` |
| `/config/failsafe/retry` | KEPT | `10-failsafe-retry.md` |
| `/config/failsafe/hedge` | KEPT | `11-failsafe-hedge.md` |
| `/config/failsafe/consensus` | KEPT | `14-consensus.md` |
| `/config/failsafe/integrity` | KEPT | `15-failsafe-integrity.md` |
| `/config/failsafe/circuit-breaker` | KEPT | `13-failsafe-circuit-breaker.md`, `22-selection-policies-scoring.md` |
| `/config/database/drivers` | KEPT | `18-cache-connectors.md` |
| `/config/database/evm-json-rpc-cache` | KEPT | `17-cache-policies.md`, `18-cache-connectors.md` |
| `/config/database/shared-state` | KEPT (fix nav) | `19-shared-state.md` |
| `/config/auth` | KEPT | `20-auth.md` |
| `/config/rate-limiters` | KEPT | `16-rate-limiters.md` |
| `/config/matcher` | KEPT | `21-matchers.md` |

### Deployment section

| Page slug | Status | Primary KB sources |
|---|---|---|
| `/deployment/docker` | KEPT | `40-deployment-docker-k8s-monitoring.md` |
| `/deployment/kubernetes` | KEPT | `40-deployment-docker-k8s-monitoring.md` |
| `/deployment/railway` | KEPT | `40-deployment-docker-k8s-monitoring.md` |
| `/deployment/cloud` | KEPT | `40-deployment-docker-k8s-monitoring.md` |

### Operations section

| Page slug | Status | Primary KB sources |
|---|---|---|
| `/operation/url` | KEPT | `32-url-structure-routing.md` |
| `/operation/healthcheck` | KEPT | `30-healthcheck.md` |
| `/operation/batch` | KEPT | `33-batching-multiplexing.md` |
| `/operation/directives` | KEPT | `31-request-directives.md` |
| `/operation/production` | KEPT | `40-deployment-docker-k8s-monitoring.md`, `02-http-server.md` |
| `/operation/monitoring` | KEPT | `27-observability-metrics.md` |
| `/operation/tracing` | KEPT | `28-observability-tracing-logging.md` |
| `/operation/admin` | KEPT | `29-admin-api.md` |
| `/operation/cordoning` | KEPT | `22-selection-policies-scoring.md` (cordon integration), `29-admin-api.md` |
| `/operation/cli` | KEPT | `04-cli-bootstrap-config-loading.md` |

### Capabilities section (all NEW)

| Page slug | Status | Primary KB sources |
|---|---|---|
| `/capabilities/request-lifecycle` | NEW | `01-request-lifecycle.md` |
| `/capabilities/evm-method-handlers` | NEW | `24-evm-method-handlers.md`, `39-evm-architecture-overview.md`, `25-evm-state-poller-served-tip.md` |
| `/capabilities/getlogs-splitting` | NEW | `23-evm-getlogs-splitting.md` |
| `/capabilities/static-responses` | NEW | `34-static-responses.md` |
| `/capabilities/shadow-upstreams` | NEW | `35-shadow-upstreams.md` |
| `/capabilities/lanes` | NEW | `36-lanes-concurrency.md`, `22-selection-policies-scoring.md` |
| `/capabilities/errors` | NEW | `38-errors-taxonomy.md` |
| `/capabilities/simulator` | NEW | `41-erpc-simulator.md` |

### Reference section (all NEW)

| Page slug | Status | Primary KB sources |
|---|---|---|
| `/reference/config-formats` | NEW | `04-cli-bootstrap-config-loading.md`, `05-config-formats-typescript.md` |
| `/reference/grpc-bds` | NEW | `03-grpc-server-bds.md` |
| `/reference/metrics` | NEW | `27-observability-metrics.md` (full 122-metric census from `docs/kb/_census/metrics.md`) |

### Coverage verification

All 42 KB files covered:

```
01 → /capabilities/request-lifecycle, /              ✓
02 → /config/server                                  ✓
03 → /reference/grpc-bds                             ✓
04 → /operation/cli, /reference/config-formats, /config/example  ✓
05 → /reference/config-formats, /config/example      ✓
06 → /config/projects/cors                           ✓
07 → /config/projects/networks                       ✓
08 → /config/projects/upstreams                      ✓
09 → /config/projects/providers, /config/projects/upstreams  ✓
10 → /config/failsafe/retry                          ✓
11 → /config/failsafe/hedge                          ✓
12 → /config/failsafe/timeout                        ✓
13 → /config/failsafe/circuit-breaker                ✓
14 → /config/failsafe/consensus                      ✓
15 → /config/failsafe/integrity                      ✓
16 → /config/rate-limiters                           ✓
17 → /config/database/evm-json-rpc-cache             ✓
18 → /config/database/drivers, /config/database/evm-json-rpc-cache  ✓
19 → /config/database/shared-state                   ✓
20 → /config/auth                                    ✓
21 → /config/matcher                                 ✓
22 → /config/projects/selection-policies, /operation/cordoning, /capabilities/lanes  ✓
23 → /capabilities/getlogs-splitting, /config/projects/networks  ✓
24 → /capabilities/evm-method-handlers               ✓
25 → /capabilities/evm-method-handlers               ✓
26 → /config/projects/selection-policies             ✓
27 → /operation/monitoring, /reference/metrics       ✓
28 → /operation/tracing                              ✓
29 → /operation/admin                                ✓
30 → /operation/healthcheck                          ✓
31 → /operation/directives                           ✓
32 → /operation/url                                  ✓
33 → /operation/batch                                ✓
34 → /capabilities/static-responses, /config/projects/networks  ✓
35 → /capabilities/shadow-upstreams, /config/projects/upstreams  ✓
36 → /capabilities/lanes                             ✓
37 → /config/server, /config/projects/upstreams      ✓
38 → /capabilities/errors                            ✓
39 → /capabilities/request-lifecycle, /why           ✓
40 → /deployment/docker, /deployment/kubernetes, /deployment/railway, /operation/production  ✓
41 → /capabilities/simulator                         ✓
42 → (audit doc — informs all pages, not a content source itself)  ✓
```

---

## 3. Page Anatomy — Standard 3-Level Layout

Every MDX page (KEPT and NEW) applies this structure. The anatomy is mandatory; existing
pages that do not yet follow it must be updated as part of normal maintenance after this
IA is approved.

### 3.1 Skeleton

```mdx
---
title: "<Page title>"
description: "<One-sentence L1 description — used in OG and llms.txt>"
---

import { AISection, ConfigTabs, ConfigCode, LLMsTxtLink } from "@/components";

<LLMsTxtLink />

# Page Title

{/* ── L1 ── */}
{/* 2–4 sentences. What the feature is, what problem it solves, what operators gain.
    No config fields. No code blocks unless a single illustrative snippet is essential.
    Target: 60–120 words. */}

<short paragraph(s)>

{/* ── L2 ── */}
{/* One or more H2 sections. Interesting "how": nuances, design choices, behavioral
    edge cases. May include focused ConfigTabs / ConfigCode snippets.
    NOT exhaustive — leave field-level minutiae to L3. */}

## How it works

<H2 sections with optional ConfigTabs>

## Configuration quick-reference

<ConfigTabs showing the minimal meaningful config>

{/* ── L3 ── */}
{/* Single AISection at the bottom of the page. Collapsed by default.
    Contains: full field table, all defaults (verified against common/defaults.go),
    all edge cases, exact metric names, source code links. */}

<AISection title="Full reference for AI / agents">

### Config fields

| Field | Type | Default | Notes |
|---|---|---|---|
| ... | ... | ... | ... |

### Source links

- Config struct: [github.com/erpc/erpc/blob/main/common/config.go#L...](...)
- Defaults: [github.com/erpc/erpc/blob/main/common/defaults.go#L...](...)
- Implementation: [github.com/erpc/erpc/blob/main/<path>.go#L...](...)

### Metrics

| Metric | Type | Labels | Description |
|---|---|---|---|
| erpc_... | counter | ... | ... |

### Edge cases

<bulleted list>

</AISection>
```

### 3.2 L1 — presentation rules

- Default visible on page load. No `<details>`, no collapsing.
- No component wrappers needed (plain MDX prose + optional one small `ConfigTabs`).
- Word budget: 60–120 words of prose per page. The 5-minute test (Section 6) is computed
  against this budget.
- Callout boxes (`<Callout>`) are permitted for critical warnings (staleness, security).

### 3.3 L2 — presentation rules

- Rendered as standard H2 (`##`) sections below the L1 block.
- For deeply optional deep-dives (e.g., quantile sampling internals), use a Nextra
  `<Tabs>` group labeled "Overview / Deep-dive" rather than a full `<details>`.
- `ConfigTabs` and `ConfigCode` blocks go here, annotated with `path=` breadcrumb and
  `focus=` line highlighting for the relevant fields.
- H2 headings are included in the page's right-hand TOC (Nextra default) — keep them
  scannable: 3–6 words each.

### 3.4 L3 — presentation rules

- Wrapped in `<AISection>` with `defaultOpen={false}`.
- `data-component="ai-section"` is present automatically (rendered by `AISection`).
- Subsections inside the AISection use H3 (`###`) so they do not pollute the TOC.
- Source-code links format: `github.com/erpc/erpc/blob/main/<path>#L<n>` (no https:// —
  the build-llms.mjs script treats links with `/blob/main/` as source refs and keeps
  them as-is).
- Metric names use the exact `erpc_*` string from the codebase census
  (`docs/kb/_census/metrics.md`).
- Config field defaults must be cross-checked against `common/defaults.go`. The audit
  (KB `42`) identified 11 stale defaults — these MUST be corrected in each page's
  AISection field table before the page is considered complete.

---

## 4. Component API Spec

### 4.1 Reuse without modification

All four existing components (`AISection`, `ConfigTabs`, `ConfigCode`, `LLMsTxtLink`)
are usable as-is. No props need to change.

### 4.2 Required `data-component` attributes (for build-llms.mjs)

The build script detects three values of `data-component`:

| Attribute value | Rendered by | build-llms.mjs behavior |
|---|---|---|
| `"ai-section"` | `AISection` | Inlines body fully expanded; uses `data-title` as a heading |
| `"config-code"` | `ConfigCode` | Emits fenced code block preceded by `> Config path: <path>` |
| `"llms-txt-link"` | `LLMsTxtLink` | Stripped (UI only, not included in .llms.txt) |

No new `data-component` values are needed. The existing `ConfigTabs` component wraps
two `ConfigCode` blocks; the script emits both tabs sequentially in the .llms.txt output.

### 4.3 New component: `SourceLink`

A small inline component for L3 source-code cross-references. **Not strictly required**
(plain markdown links work fine), but recommended for consistent formatting and for the
`build-llms.mjs` script to emit a consistent "Source:" prefix.

```tsx
// docs/components/SourceLink.tsx
interface SourceLinkProps {
  path: string;   // e.g. "common/defaults.go"
  line?: number;  // e.g. 694
  children: React.ReactNode;
}
// Renders: <a href={`https://github.com/erpc/erpc/blob/main/${path}${line ? `#L${line}` : ''}`}
//              data-component="source-link"
//              target="_blank" rel="noreferrer">{children}</a>
```

build-llms.mjs addition needed: detect `data-component="source-link"` and emit the URL
in the .llms.txt body as `[<children> — github.com/erpc/erpc/blob/main/<path>#L<line>]`.

### 4.4 `AISection` — recommended usage pattern for L3 field tables

```mdx
<AISection title="Full reference — <FeatureName>">

### Config fields

<ConfigCode language="typescript" path="<yaml path>" showLineNumbers>
{`// Annotated type definition`}
</ConfigCode>

| Field | Type | Default | Notes |
|---|---|---|---|
| `field` | `type` | `value` | Description. Source: defaults.go:Lxxx |

### Defaults correction notes
<!-- Cross-ref KB 42 staleness list; fix every wrong default here -->

### Source links
<!-- SourceLink components or plain markdown links -->

### Related metrics

| Metric | Type | Labels |
|---|---|---|
| `erpc_*` | counter | project, network, upstream |

</AISection>
```

### 4.5 `ConfigTabs` — required props for every L2 snippet

Every `ConfigTabs` block in an L2 section MUST include:
- `path=` — the dot-path in the config tree (e.g. `"projects[] > upstreams[]"`)
- `focus=` or `focusYaml=`/`focusTs=` — line numbers of the fields being discussed
- `yaml=` and `ts=` — synchronized minimal examples (not full config dumps)

This ensures the .llms.txt output prefixes every code block with its config location,
making it navigable for agents without reading surrounding prose.

---

## 5. llms.txt Scheme

### 5.1 Per-page .llms.txt

The existing build pipeline (`docs/scripts/build-llms.mjs`) already generates
`docs/public/<page-path>.llms.txt` for every `.mdx` file. The per-page file contains:

1. The page's `title` and `description` frontmatter as a markdown H1 + blockquote.
2. All L1 prose verbatim (not wrapped — plain markdown).
3. All L2 `##` sections verbatim, with `ConfigTabs`/`ConfigCode` blocks rendered as
   fenced markdown preceded by `> Config path: <path>`.
4. The `AISection` body fully expanded (the script detects `data-component="ai-section"`
   and inlines all children, regardless of `defaultOpen`).
5. Internal links (`/foo/bar`) rewritten to `/foo/bar.llms.txt` — this guarantees agents
   following links from any page stay in the machine-readable surface.

**No changes to the generator are needed for existing page types.** The only addition
needed is detection of the new `SourceLink` component (see Section 4.3).

### 5.2 Root llms.txt

`docs/public/llms.txt` is rebuilt by `build-llms.mjs` on every build. Its structure:

```
# eRPC Documentation

> eRPC is an EVM RPC proxy with …  (← one-line description from theme.config.tsx)

## Navigation

- Quick start: https://docs.erpc.cloud/
- Why eRPC?: https://docs.erpc.cloud/why
… (every page in sidebar order) …

## All pages

/                  → /llms.txt
/why               → /why.llms.txt
/free              → /free.llms.txt
/faq               → /faq.llms.txt
/config/example    → /config/example.llms.txt
… (every page, in full) …
/capabilities/request-lifecycle → /capabilities/request-lifecycle.llms.txt
… (all NEW pages) …
/reference/config-formats → /reference/config-formats.llms.txt
/reference/grpc-bds       → /reference/grpc-bds.llms.txt
/reference/metrics        → /reference/metrics.llms.txt
```

The flat "All pages" section guarantees transitive reachability: an agent that fetches
`/llms.txt` sees every page URL and its `.llms.txt` counterpart, so it can load any page
without needing to follow sidebar nav links.

### 5.3 Transitive reachability guarantee

An agent starting at root achieves full coverage in at most **2 hops**:

- Hop 0: `GET /llms.txt` → sees all page URLs.
- Hop 1: `GET /<page>.llms.txt` → sees the full L1 + L2 + L3 content for that page,
  plus links to related pages (rewritten to `.llms.txt` form).

L2 `ConfigTabs` blocks in the `.llms.txt` output carry their `path=` annotation, so an
agent reading a page can immediately locate where a config snippet belongs in the full
config tree without needing to understand the surrounding prose.

L3 source links (`github.com/erpc/erpc/blob/main/…`) point to the Go source. Agents that
need ground truth beyond the docs can follow these links in one more hop.

### 5.4 Build pipeline additions

1. Walk `docs/pages/capabilities/*.mdx` and `docs/pages/reference/*.mdx` in the
   `walkMdx` function — no code change needed, the walker is already recursive.
2. Add `SourceLink` component handling in `renderJsx` (see Section 4.3 addition spec).
3. The root `llms.txt` nav section must be ordered to match the `_meta.js` sidebar order
   (including the new `capabilities` and `reference` sections). Extend the
   `buildNavSection` function (or equivalent) to read from the new `_meta.js` files.

---

## 6. The 4–5 Minute Test

### 6.1 L1 word budget per page

Target: every page's L1 section 60–120 words. At comfortable reading pace (~200 wpm):

| Count | Per-page words | Total words | Read time |
|---|---|---|---|
| 4 top-level pages | 80 avg | 320 | ~1 min 36 s |
| 20 config pages | 70 avg | 1,400 | ~7 min |
| 4 deployment pages | 60 avg | 240 | ~1 min 12 s |
| 10 operations pages | 70 avg | 700 | ~3 min 30 s |
| 8 capability pages | 75 avg | 600 | ~3 min |
| 3 reference pages | 60 avg | 180 | ~54 s |

**Grand total: ~3,440 words across 49 pages ≈ 17 minutes if every page is read.**

That is not the 5-minute test — the test is the **human learning path**, not full
coverage. The human path described below covers eRPC end-to-end at L1 in 4–5 minutes by
reading only the highest-value pages.

### 6.2 The 5-minute human path (exact page sequence)

A staff engineer evaluating eRPC for the first time reads these pages in order, L1 only:

| # | Page | URL | Key question answered | Approx words | Cum. time |
|---|---|---|---|---|---|
| 1 | Quick start | `/` | What is eRPC and how do I start it? | 80 | 24 s |
| 2 | Why eRPC? | `/why` | Why not just a load balancer? | 80 | 48 s |
| 3 | Request lifecycle | `/capabilities/request-lifecycle` | How does a request actually flow? | 100 | 1 min 18 s |
| 4 | Upstreams | `/config/projects/upstreams` | How do I add RPCs? | 80 | 1 min 42 s |
| 5 | Selection policy | `/config/projects/selection-policies` | How does routing / circuit-breaking work? | 90 | 2 min 9 s |
| 6 | Failsafe overview | (failsafe/ index or `/config/failsafe/retry`) | What resilience can I configure? | 80 | 2 min 33 s |
| 7 | EVM Cache | `/config/database/evm-json-rpc-cache` | What does caching look like? | 80 | 2 min 57 s |
| 8 | Directives | `/operation/directives` | How does a caller control per-request behavior? | 80 | 3 min 21 s |
| 9 | Monitoring | `/operation/monitoring` | What can I observe? | 70 | 3 min 42 s |
| 10 | Production | `/operation/production` | What are the go-live checklist items? | 90 | 4 min 9 s |
| 11 | URL & routing | `/operation/url` | How do clients address eRPC? | 70 | 4 min 30 s |

**Total: ~830 words, ~4 min 30 s at 185 wpm.** Well within the 5-minute ceiling.

### 6.3 Design constraints that enforce the budget

- L1 word limit is a **hard spec constraint**: page authors must keep L1 ≤ 120 words.
  Any content exceeding that belongs in L2 (human detail) or L3 (agent exhaustive).
- The `LLMsTxtLink` floating button is always visible, so an agent reading a page that
  wants more detail can immediately jump to the `.llms.txt` version (which includes the
  fully expanded AISection) without any prose navigation.
- The "Capabilities" section acts as an optional accelerator for humans who want the
  interesting behavioral content (getLogs splitting, shadow upstreams, lanes) without
  wading through the full Config reference.

---

## 7. Migration Checklist

The following changes are required to implement this proposal. All are additive or
minimal surgical edits — no existing page is deleted or moved.

### 7.1 Nav / _meta.js changes (no URL impact)

1. `docs/pages/config/database/_meta.js` — add `"shared-state": { title: "Shared state" }`.
2. `docs/pages/operation/_meta.js` — rename `batch` title from `"Batching"` to
   `"Batching & multiplexing"` (title only, no URL change).
3. `docs/pages/_meta.js` — add two new sections after the `"-- Operations"` separator:
   ```js
   "-- Capabilities": { type: "separator", title: "Capabilities" },
   capabilities: { display: "children", title: "Capabilities" },
   "-- Reference": { type: "separator", title: "Reference" },
   reference: { display: "children", title: "Reference" },
   ```
4. Create `docs/pages/capabilities/_meta.js` with entries for all 8 new pages.
5. Create `docs/pages/reference/_meta.js` with entries for all 3 new pages.

### 7.2 New MDX files to create (11 files)

Under `docs/pages/capabilities/`:
- `request-lifecycle.mdx` — feeds from KB `01`
- `evm-method-handlers.mdx` — feeds from KB `24`, `25`, `39`
- `getlogs-splitting.mdx` — feeds from KB `23`
- `static-responses.mdx` — feeds from KB `34`
- `shadow-upstreams.mdx` — feeds from KB `35`
- `lanes.mdx` — feeds from KB `36`, `22`
- `errors.mdx` — feeds from KB `38`
- `simulator.mdx` — feeds from KB `41`

Under `docs/pages/reference/`:
- `config-formats.mdx` — feeds from KB `04`, `05`
- `grpc-bds.mdx` — feeds from KB `03`
- `metrics.mdx` — feeds from KB `27` + `docs/kb/_census/metrics.md` (full 122-metric table)

### 7.3 Existing pages to update (anatomy compliance)

These KEPT pages have specific deficiencies identified in the audit (KB `42`) that must
be corrected. They do not require structural changes — only content fixes within the
existing L3 AISection field tables:

| Page | Deficiency | Fix |
|---|---|---|
| `/config/server` | `maxTimeout` default wrong (30s → 150s); `readTimeout` (0 → 30s); `writeTimeout` (0 → 120s); `includeErrorDetails` (false → true); `trustedIPHeaders` (["X-Forwarded-For"] → []); `waitBeforeShutdown`/`waitAfterShutdown` (0s → 10s); `grpcPortV4` (4100 → 4000); gRPC msg sizes (4 MiB → 100 MiB) | Correct field table in AISection |
| `/config/example` | Same defaults mirrored from server page | Correct field table |
| `/config/projects/upstreams` | "default failsafe (15s timeout, 2 retries, circuit breaker at 80%)" → "60s timeout, 1 attempt (no retry), no circuit breaker" | Fix comment and AISection |
| `/config/projects/selection-policies` | Add AISection; fix latency threshold inconsistency vs circuit-breaker page (use KB `22` as ground truth); switch from raw `<Tabs>` to `<ConfigTabs>` | Update component usage + add AISection |
| `/config/failsafe/circuit-breaker` | Fix `probeExcluded` params inconsistency; add AISection | Add AISection with correct defaults |
| `/operation/monitoring` | Add ~58 missing metrics (all `erpc_consensus_*`, `erpc_cache_connector_*` block numbers, `erpc_cache_*_duration_seconds`, `erpc_grpc_bds_*`, etc.); fix `enableGzip` default (false → true) | Update AISection metric table; promote `/reference/metrics` as the full-census page |
| `/operation/cordoning` | Add AISection noting cordon state is stored in shared state | Add AISection |
| `/config/failsafe/circuit-breaker` | Raw markdown; no custom components | Add ConfigTabs + AISection |
| `/deployment/railway` | Fix "customizaiton" typo; add AISection | Typo + AISection |
| `/operation/batch` | Add note about per-item error isolation in batch responses; add multiplexing behavior | Update L2 section |

### 7.4 Optional: new `SourceLink` component

Create `docs/components/SourceLink.tsx` (see Section 4.3 spec) and add handling in
`docs/scripts/build-llms.mjs`. This is optional — plain markdown links in AISection
bodies work today. Recommended for phase 2.

---

## 8. Design Rationale vs. Competing Proposals

### Why conservative evolution wins on migration sanity

Zero redirects. Every operator who bookmarked `/config/projects/selection-policies` or
linked to `/operation/monitoring` in runbooks gets the same URL forever. The cost of
link rot in production infrastructure docs is disproportionately high; this proposal
accepts a slightly larger nav tree in exchange for zero breakage.

### Why the new "Capabilities" section is correct grouping

The capabilities are not config pages (there is nothing to configure on the getLogs
splitting page beyond what appears on the networks page). They are not operations pages
(they do not describe runtime behaviors an operator monitors). They are features that
make eRPC worth choosing — the "why" made concrete. Putting them in a distinct
"Capabilities" section means a CTO scanning the sidebar immediately sees the capability
surface, while the Config section remains a clean reference.

### Why "Reference" is separate from "Capabilities"

`/reference/metrics` is a pure lookup table (122 rows). `/reference/config-formats`
and `/reference/grpc-bds` are deep technical specifications. Mixing them with the
capability-overview pages would dilute both. A "Reference" section signals to operators:
"come here when you know what you want, not when you are discovering."

### Why the selection-policy page keeps its URL

`/config/projects/selection-policies` is referenced extensively in production runbooks
and third-party tutorials (DVN preset, etc.). Moving it to `/capabilities/` would be the
highest-impact redirect of any option. The conservative choice is to keep it in Config
where it is, add a proper AISection, and let `/capabilities/lanes` serve as the
behavioral companion page.

### Agent navigability

An agent following links from root llms.txt → any capabilities page → AISection source
links achieves 3-hop access to Go source. The `.llms.txt` rewriting of internal links
keeps the agent on the machine-readable surface at every step. The `path=` annotations on
every `ConfigTabs` block let an agent locate config fields in the full tree without
parsing YAML — this is the primary ergonomic improvement over the current docs for
tool-calling agents.
