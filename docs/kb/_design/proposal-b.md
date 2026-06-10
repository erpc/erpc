# Docs IA Proposal B — Operator Journey First

> Authored against: brief.md (north star), KB 01–42, existing nav (_meta.js files), component audit (KB 42).
> Design lens: **operator lifecycle** — Get Started → Understand → Configure → Operate → Reference.

---

## 1. Nav Tree

### Design rationale

The current tree groups by _config object_ (Config / Deployment / Operations). Proposal B groups by _what you are doing right now_: a first-time user never thinks "I need to visit Config > Projects > Upstreams"; they think "I want to connect an RPC provider." The new tree mirrors the natural adoption arc:

1. **Get started** — zero to first proxied request (minutes)
2. **Architecture** — understand what you're running before you trust it
3. **Configure** — by capability not by config-struct (Resilience / Caching / Access Control / Providers)
4. **Operate** — deploy, observe, tune, debug
5. **Reference** — exhaustive lookup for agents and engineers who know what they need

### Full sidebar tree

Annotations: `[NEW]` page doesn't exist yet · `[KEPT]` same slug as today · `[MOVED]` slug changed (see redirects list)

```
── (no separator) ──────────────────────────────────────────────────
  Quick Start                          /                      [KEPT]
  Why eRPC?                            /why                   [KEPT]
  Free & Public RPCs                   /free                  [KEPT]
  FAQ                                  /faq                   [KEPT]

── GET STARTED ─────────────────────────────────────────────────────
  Installation & first request         /install               [NEW]
  Configuration formats (YAML / TS)    /config-format         [NEW]
  Complete config example              /config/example        [KEPT]

── ARCHITECTURE ────────────────────────────────────────────────────
  How eRPC works                       /architecture          [NEW]
  Projects & networks                  /architecture/projects [NEW]
  Upstreams & providers                /architecture/upstreams[NEW]
  EVM method handling                  /architecture/evm      [NEW]
  Error taxonomy                       /architecture/errors   [NEW]

── CONFIGURE ───────────────────────────────────────────────────────

  ▸ Server
    Server                             /config/server         [KEPT]
    URL structure & aliasing           /operation/url         [KEPT]
    CORS                               /config/projects/cors  [KEPT]
    Auth                               /config/auth           [KEPT]
    Rate limiters                      /config/rate-limiters  [KEPT]
    Matcher syntax                     /config/matcher        [KEPT]

  ▸ Providers & Networks
    Providers                          /config/projects/providers       [KEPT]
    Networks                           /config/projects/networks        [KEPT]
    Selection policy                   /config/projects/selection-policies [KEPT]
    Static responses                   /config/projects/static-responses[NEW]
    Shadow upstreams                   /config/projects/shadow-upstreams[NEW]

  ▸ Resilience
    Overview                           /config/failsafe       [KEPT]
    Timeout                            /config/failsafe/timeout    [KEPT]
    Retry                              /config/failsafe/retry      [KEPT]
    Hedge                              /config/failsafe/hedge      [KEPT]
    Consensus                          /config/failsafe/consensus  [KEPT]
    Integrity & empty data             /config/failsafe/integrity  [KEPT]
    Circuit breaker                    /config/failsafe/circuit-breaker [KEPT]

  ▸ Caching
    Cache policies                     /config/database/evm-json-rpc-cache [KEPT]
    Storage drivers                    /config/database/drivers    [KEPT]
    Shared state                       /config/database/shared-state [KEPT]

── OPERATE ─────────────────────────────────────────────────────────

  ▸ Deploy
    Docker                             /deployment/docker     [KEPT]
    Kubernetes                         /deployment/kubernetes [KEPT]
    Railway                            /deployment/railway    [KEPT]
    Hosted cloud                       /deployment/cloud      [KEPT]

  ▸ Observe
    Metrics (Prometheus)               /operation/monitoring  [KEPT]
    Tracing (OpenTelemetry)            /operation/tracing     [KEPT]
    Logging                            /operation/logging     [NEW]

  ▸ Manage
    Admin API                          /operation/admin       [KEPT]
    Healthcheck                        /operation/healthcheck [KEPT]
    Cordoning                          /operation/cordoning   [KEPT]
    CLI & env vars                     /operation/cli         [KEPT]

  ▸ Debug
    Directives                         /operation/directives  [KEPT]
    Batch & multiplexing               /operation/batch       [KEPT]
    Production checklist               /operation/production  [KEPT]

── REFERENCE ───────────────────────────────────────────────────────
  gRPC / BDS streaming                 /reference/grpc-bds    [NEW]
  Lanes & concurrency                  /reference/lanes       [NEW]
  HTTP client & proxy pools            /reference/http-client [NEW]
  Simulator                            /reference/simulator   [NEW]
  Presets (hidden from nav)            /presets/dvn-ready     [KEPT, hidden]
```

**Total pages: 51** (35 existing + 16 new)

### Redirects (slug changes)

No existing slug is renamed in this proposal. All existing pages keep their URLs. The new pages are additive. The only structural moves are the five "Architecture" pages which are **new** pages that synthesize content from KB files that currently have no doc page — no redirect needed.

The one item that should be cleaned up (noted in KB 42): `preview-retry` is in `_meta.js` as `display: "hidden"` but has no corresponding `.mdx` file. Remove it from `_meta.js` to prevent a potential 404.

| From | To | Reason |
|---|---|---|
| *(none — no renames)* | — | All existing slugs preserved intact |

---

## 2. Page List — KB Coverage Map

Every KB file (01–42) is mapped to at least one page. Status: `[E]` = page exists · `[N]` = new page needed.

### Top-level pages

| Page | Slug | Status | KB sources |
|---|---|---|---|
| Quick Start / home | `/` | [E] | KB04 (bootstrap/install), KB32 (URL), KB40 (Docker quickstart) |
| Why eRPC? | `/why` | [E] | KB01 (pipeline overview), KB39 (architecture) |
| Free & Public RPCs | `/free` | [E] | KB04, KB32 |
| FAQ | `/faq` | [E] | KB04, KB20, KB16, KB17 |

### Get Started section

| Page | Slug | Status | KB sources |
|---|---|---|---|
| Installation & first request | `/install` | [N] | KB04 (CLI bootstrap, config loading), KB05 (TS config format), KB40 (Docker) |
| Configuration formats (YAML / TS) | `/config-format` | [N] | KB04, KB05 |
| Complete config example | `/config/example` | [E] | KB04, KB05, KB07, KB08, KB09 — all config-level KB files synthesized |

### Architecture section

| Page | Slug | Status | KB sources |
|---|---|---|---|
| How eRPC works | `/architecture` | [N] | KB01 (request lifecycle), KB39 (EVM architecture overview) |
| Projects & networks | `/architecture/projects` | [N] | KB06 (projects/CORS), KB07 (networks) |
| Upstreams & providers | `/architecture/upstreams` | [N] | KB08 (upstream lifecycle), KB09 (providers/vendors), KB37 (HTTP client/proxy pools) |
| EVM method handling | `/architecture/evm` | [N] | KB23 (getLogs splitting), KB24 (EVM method handlers), KB25 (state poller/served tip), KB34 (static responses), KB36 (lanes) |
| Error taxonomy | `/architecture/errors` | [N] | KB38 (error taxonomy) |

### Configure — Server subsection

| Page | Slug | Status | KB sources |
|---|---|---|---|
| Server | `/config/server` | [E] | KB02 (HTTP server), KB04 (config loading) |
| URL structure & aliasing | `/operation/url` | [E] | KB32 (URL structure/routing) |
| CORS | `/config/projects/cors` | [E] | KB06 (projects/CORS) |
| Auth | `/config/auth` | [E] | KB20 (auth) |
| Rate limiters | `/config/rate-limiters` | [E] | KB16 (rate limiters) |
| Matcher syntax | `/config/matcher` | [E] | KB21 (matchers) |

### Configure — Providers & Networks subsection

| Page | Slug | Status | KB sources |
|---|---|---|---|
| Providers | `/config/projects/providers` | [E] | KB09 (providers/vendors) |
| Networks | `/config/projects/networks` | [E] | KB07 (networks), KB23 (getLogs splitting), KB34 (static responses — partial) |
| Selection policy | `/config/projects/selection-policies` | [E] | KB22 (selection policies/scoring), KB13 (circuit breaker — selection side), KB26 (health tracker scoring) |
| Static responses | `/config/projects/static-responses` | [N] | KB34 (static responses) |
| Shadow upstreams | `/config/projects/shadow-upstreams` | [N] | KB35 (shadow upstreams) |

### Configure — Resilience subsection

| Page | Slug | Status | KB sources |
|---|---|---|---|
| Failsafe overview | `/config/failsafe` | [E] | KB01 (pipeline), KB10, KB11, KB12, KB13, KB14, KB15 (all failsafe KB) |
| Timeout | `/config/failsafe/timeout` | [E] | KB12 (failsafe timeout) |
| Retry | `/config/failsafe/retry` | [E] | KB10 (failsafe retry) |
| Hedge | `/config/failsafe/hedge` | [E] | KB11 (failsafe hedge) |
| Consensus | `/config/failsafe/consensus` | [E] | KB14 (consensus) |
| Integrity & empty data | `/config/failsafe/integrity` | [E] | KB15 (failsafe integrity) |
| Circuit breaker | `/config/failsafe/circuit-breaker` | [E] | KB13 (circuit breaker), KB22 (selection policy — exclusion side) |

### Configure — Caching subsection

| Page | Slug | Status | KB sources |
|---|---|---|---|
| Cache policies | `/config/database/evm-json-rpc-cache` | [E] | KB17 (cache policies), KB25 (state poller — served-tip in cache reads) |
| Storage drivers | `/config/database/drivers` | [E] | KB18 (cache connectors/drivers) |
| Shared state | `/config/database/shared-state` | [E] | KB19 (shared state) |

### Operate — Deploy subsection

| Page | Slug | Status | KB sources |
|---|---|---|---|
| Docker | `/deployment/docker` | [E] | KB40 (deployment: Docker/k8s/monitoring) |
| Kubernetes | `/deployment/kubernetes` | [E] | KB40 |
| Railway | `/deployment/railway` | [E] | KB40 |
| Hosted cloud | `/deployment/cloud` | [E] | (commercial — no KB file) |

### Operate — Observe subsection

| Page | Slug | Status | KB sources |
|---|---|---|---|
| Metrics (Prometheus) | `/operation/monitoring` | [E] | KB27 (observability: metrics) |
| Tracing (OpenTelemetry) | `/operation/tracing` | [E] | KB28 (observability: tracing/logging) |
| Logging | `/operation/logging` | [N] | KB28 (logging section) |

### Operate — Manage subsection

| Page | Slug | Status | KB sources |
|---|---|---|---|
| Admin API | `/operation/admin` | [E] | KB29 (admin API) |
| Healthcheck | `/operation/healthcheck` | [E] | KB30 (healthcheck) |
| Cordoning | `/operation/cordoning` | [E] | KB29 (cordoning methods in admin) |
| CLI & env vars | `/operation/cli` | [E] | KB04 (CLI bootstrap) |

### Operate — Debug subsection

| Page | Slug | Status | KB sources |
|---|---|---|---|
| Directives | `/operation/directives` | [E] | KB31 (request directives) |
| Batch & multiplexing | `/operation/batch` | [E] | KB33 (batching/multiplexing) |
| Production checklist | `/operation/production` | [E] | KB40 (production tuning), KB27 (monitoring), KB28 (tracing) |

### Reference section

| Page | Slug | Status | KB sources |
|---|---|---|---|
| gRPC / BDS streaming | `/reference/grpc-bds` | [N] | KB03 (gRPC server/BDS) |
| Lanes & concurrency | `/reference/lanes` | [N] | KB36 (lanes/concurrency) |
| HTTP client & proxy pools | `/reference/http-client` | [N] | KB37 (HTTP client/proxy pools) |
| Simulator | `/reference/simulator` | [N] | KB41 (eRPC simulator) |

### KB coverage verification

All 42 KB files are mapped:

| KB | Mapped to |
|---|---|
| 01 | `/architecture`, `/config/failsafe`, `/` |
| 02 | `/config/server` |
| 03 | `/reference/grpc-bds` |
| 04 | `/install`, `/config-format`, `/operation/cli`, `/` |
| 05 | `/install`, `/config-format`, `/config/example` |
| 06 | `/architecture/projects`, `/config/projects/cors` |
| 07 | `/architecture/projects`, `/config/projects/networks`, `/config/example` |
| 08 | `/architecture/upstreams`, `/config/example` |
| 09 | `/architecture/upstreams`, `/config/projects/providers` |
| 10 | `/config/failsafe/retry` |
| 11 | `/config/failsafe/hedge` |
| 12 | `/config/failsafe/timeout` |
| 13 | `/config/failsafe/circuit-breaker`, `/config/projects/selection-policies` |
| 14 | `/config/failsafe/consensus` |
| 15 | `/config/failsafe/integrity` |
| 16 | `/config/rate-limiters` |
| 17 | `/config/database/evm-json-rpc-cache` |
| 18 | `/config/database/drivers` |
| 19 | `/config/database/shared-state` |
| 20 | `/config/auth` |
| 21 | `/config/matcher` |
| 22 | `/config/projects/selection-policies` |
| 23 | `/architecture/evm`, `/config/projects/networks` |
| 24 | `/architecture/evm` |
| 25 | `/architecture/evm`, `/config/database/evm-json-rpc-cache` |
| 26 | `/config/projects/selection-policies` |
| 27 | `/operation/monitoring` |
| 28 | `/operation/tracing`, `/operation/logging` |
| 29 | `/operation/admin`, `/operation/cordoning` |
| 30 | `/operation/healthcheck` |
| 31 | `/operation/directives` |
| 32 | `/operation/url`, `/` |
| 33 | `/operation/batch` |
| 34 | `/config/projects/static-responses`, `/architecture/evm` |
| 35 | `/config/projects/shadow-upstreams` |
| 36 | `/reference/lanes`, `/architecture/evm` |
| 37 | `/reference/http-client`, `/architecture/upstreams` |
| 38 | `/architecture/errors` |
| 39 | `/architecture`, `/why` |
| 40 | `/deployment/docker`, `/deployment/kubernetes`, `/operation/production`, `/` |
| 41 | `/reference/simulator` |
| 42 | (audit source — informs all pages, no direct page needed) |

---

## 3. Page Anatomy

Every page follows the same three-zone layout. The zones are presented in order of human reading priority.

### Zone 1 — L1 (default visible body)

**Purpose:** CTO-level. What this feature is and what outcome it produces. Enough context to decide whether to read further or trust the system.

**Constraints:**
- Prose only — no config tables, no field lists.
- Maximum ~150 words. A reader should feel done in under 30 seconds.
- One or two concrete examples of what the feature prevents/enables (a sentence each).
- Ends with a "When to use / when to skip" line if the feature is situational.

**MDX structure:**
```mdx
---
title: "Page Title"
description: "One-sentence SEO description"
---

import { LLMsTxtLink, AISection, ConfigTabs, ConfigCode } from "../../components"

<LLMsTxtLink />

# Page Title

{/* L1 prose — 100–150 words */}

Brief intro paragraph describing the capability in human terms.
What problem it solves. What the result looks like.

Optional: one sentence on limits or tradeoffs ("This only applies to EVM chains").
```

### Zone 2 — L2 (human deep-dive)

**Purpose:** Staff/Principal engineer view. The interesting nuances of _how_ the feature behaves — without drowning in field minutiae.

**Presentation convention:**
- Use `## Heading` (H2) sections for the main L2 topics. These appear in the Nextra right-hand TOC, which is the natural signal for "opt-in reading."
- For longer pages with many L2 topics, wrap individual topics in `<details><summary>` (the same visual language as AISection but human-authored and manually authored — not the `AISection` component itself). Recommended threshold: more than 4 L2 topics on a single page → use collapsible details for all but the 2 most universally relevant ones.
- L2 contains `ConfigTabs` (YAML + TS examples) and `ConfigCode` blocks with `path=` breadcrumbs.
- L2 does NOT repeat field tables — those live exclusively in L3.

**MDX structure:**
```mdx
## How it works

[narrative — 2–4 paragraphs]

<ConfigTabs
  path="projects[].networks[].failsafe[].retry"
  yaml={`...`}
  ts={`...`}
/>

## Behavior when sub-requests fail

<details>
<summary>getLogs splitting edge cases</summary>

[prose + example]

</details>

## Production recommendations

[checklist or bullet advice, still prose — no field tables]
```

### Zone 3 — L3 (agent section)

**Purpose:** Agents only. Every config field, every query parameter, every metric name, every enum value, every edge case, exact defaults (verified against `common/defaults.go`), and links to source code.

**Presentation convention:**
- Rendered inside a single `<AISection>` component at the bottom of every page.
- `defaultOpen={false}` always (humans shouldn't need to open this unless debugging).
- `title` attribute is the page name (e.g. `title="Retry policy — full agent reference"`).
- Internal structure uses `###` H3 and `####` H4 headings with markdown tables for field lists.
- Every default value is annotated with the source file and line: `(source: common/defaults.go:L148)`.
- Every enum value is listed exhaustively.
- Source code links use the canonical format: `[description](https://github.com/erpc/erpc/blob/main/path/to/file.go#L123)`.
- Metrics referenced by this feature are listed with their full label set.
- Edge cases that affect agent behavior (e.g. "splitOnError resets the block range counter") are itemized.

**MDX structure:**
```mdx
<AISection title="Retry policy — full agent reference">

### Config fields

| Field | Type | Default | Notes |
|---|---|---|---|
| `maxAttempts` | `*int` | `1` (source: [defaults.go:L148](https://github.com/erpc/erpc/blob/main/common/defaults.go#L148)) | Total attempts including the first. `1` = no retry. |
| `delay` | `duration` | `"500ms"` | Fixed delay between attempts. |
| `backoffFactor` | `*float64` | `2.0` | Exponential multiplier applied each attempt. |
...

### Enum values

**`emptyResultAccept`:** `false` (default) | `true`

...

### Source code entry points

- Retry executor: [erpc/failsafe_executor.go#L45](https://github.com/erpc/erpc/blob/main/erpc/failsafe_executor.go#L45)
- Config struct: [common/config.go#L312](https://github.com/erpc/erpc/blob/main/common/config.go#L312)
- Defaults: [common/defaults.go#L148](https://github.com/erpc/erpc/blob/main/common/defaults.go#L148)

### Related metrics

| Metric | Labels | Notes |
|---|---|---|
| `erpc_failsafe_retry_total` | `project, network, upstream, method` | Incremented each retry attempt. |

### Edge cases

- Network-scope retry rotates across upstreams (different upstream each attempt); upstream-scope retry hits the same upstream repeatedly.
- `emptyResultMaxAttempts` caps empty-result retries independently of `maxAttempts`.
- `blockUnavailableDelay` applies only when the upstream returns `erpc_block_unavailable`.

</AISection>
```

---

## 4. Component API Spec

### Existing components (reuse as-is)

All four existing components in `docs/components/` are reused unchanged. The `data-component` attributes that `build-llms.mjs` depends on are preserved.

#### `AISection` — no changes needed

Used exactly as documented in KB 42. One new convention: the `title` prop should follow the pattern `"<Page subject> — full agent reference"` to make section headings in the generated `.llms.txt` files self-describing when multiple pages are concatenated.

```tsx
// Props (unchanged):
title?: string          // default: "Copy for your AI assistant"
hint?: string           // default: long hint string
defaultOpen?: boolean   // default: false — NEVER set true in production pages
children: ReactNode

// data-component="ai-section" — build-llms.mjs expands this fully
// data-title={title}          — used as heading in .llms.txt
```

#### `ConfigTabs` — no changes needed

Used in all L2 sections that show YAML + TypeScript config examples.

```tsx
// Required props:
yaml: string            // YAML config snippet
ts: string              // TypeScript config snippet

// Optional props:
path?: string           // Config breadcrumb e.g. "projects[] > networks[] > failsafe[]"
focus?: string          // Line ranges for both tabs "1,3-5"
focusYaml?: string      // YAML-specific focus override
focusTs?: string        // TS-specific focus override
filenameYaml?: string   // default "erpc.yaml"
filenameTs?: string     // default "erpc.ts"
showLineNumbers?: boolean
```

#### `ConfigCode` — no changes needed

Used in L2 and L3 for single-format examples (bash, JSON response bodies, etc.) and for standalone YAML or TypeScript when both formats aren't needed.

```tsx
// Required:
language: string        // "yaml" | "typescript" | "bash" | "json" | ...

// Optional:
filename?: string
path?: string           // Config breadcrumb
focus?: string          // "1,3-5,8" (1-indexed)
showLineNumbers?: boolean
code?: string           // preferred when wiring from React
children?: ReactNode    // alternative: inline JSX children in MDX
// data-component="config-code" — rendered by build-llms.mjs
```

#### `LLMsTxtLink` — no changes needed

Placed at the top of every page (after imports, before the H1 or immediately after). Derives the `.llms.txt` URL from `router.asPath`.

```tsx
// No props.
// Renders: data-component="llms-txt-link"
// URL derivation: path.split("#")[0].split("?")[0].replace(/\/$/, "")
//   "/" → "/llms.txt"
//   "/config/server" → "/config/server.llms.txt"
```

### New components needed

#### `KBSource` — inline KB attribution badge `[NEW]`

A small inline badge linking back to the KB file that grounded a claim. Used exclusively in L3 (`AISection` content) to make the chain of evidence visible to agents.

```tsx
// Proposed props:
interface KBSourceProps {
  id: string;   // e.g. "KB08" — maps to docs/kb/08-upstreams-lifecycle.md
  line?: number; // Optional: specific line in the KB file
}

// Renders as: <code data-component="kb-source" data-id="KB08">[KB08]</code>
// In .llms.txt: rendered as plain text "(grounded in docs/kb/08-upstreams-lifecycle.md)"
// build-llms.mjs: strip the JSX, emit the grounding text in its place
```

This component is entirely optional — pages that don't use it still compile. It improves agent confidence by making the evidence chain explicit.

#### `SourceLink` — source code link with file+line `[NEW]`

A standardized link component for source code references in L3. Produces consistent formatting in both HTML and the `.llms.txt` surface.

```tsx
interface SourceLinkProps {
  file: string;   // repo-relative path, e.g. "common/defaults.go"
  line?: number;  // specific line
  children: ReactNode; // link text
}

// Renders as: <a href="https://github.com/erpc/erpc/blob/main/{file}#L{line}"
//                data-component="source-link">{children}</a>
// In .llms.txt: rendered as: [{children}](https://github.com/erpc/erpc/blob/main/{file}#L{line})
// build-llms.mjs: existing link rewrite already handles <a href="..."> passthrough
```

Both new components require a small addition to `build-llms.mjs`:

```js
// In the transforms section, after transformAISection():
function transformKBSource(src) {
  return transformTag(src, "KBSource", ({ attrs }) => {
    const id = attrs.id ?? "";
    const num = id.replace("KB", "").padStart(2, "0");
    return `(grounded in docs/kb/${num}-*.md)`;
  });
}

function transformSourceLink(src) {
  // Already handled by existing link rewriting — no new transform needed
  // provided the href is a full https:// URL (not an internal link).
  return src;
}
```

### `data-component` attributes summary (for build-llms.mjs)

| Component | `data-component` value | Effect in `.llms.txt` |
|---|---|---|
| `AISection` | `"ai-section"` | Expanded fully; `data-title` used as H3 heading |
| `ConfigCode` | `"config-code"` | Path breadcrumb + fenced code block |
| `ConfigTabs` | (no data-component; attrs parsed directly by scanner) | Two fenced blocks with `**YAML:**` / `**TypeScript:**` headers |
| `LLMsTxtLink` | `"llms-txt-link"` | Stripped (navigation UI element) |
| `KBSource` | `"kb-source"` | `"(grounded in docs/kb/NN-*.md)"` inline text |
| `SourceLink` | `"source-link"` | Standard href passthrough |

---

## 5. llms.txt Scheme

### Architecture

The `build-llms.mjs` pipeline already produces:
1. One `docs/public/<page-path>.llms.txt` per MDX page — fully expanded L3 content.
2. One `docs/public/llms.txt` root file — nav tree + flat URL list.

Proposal B extends this without breaking the existing pipeline.

### Root `llms.txt` structure

The root `llms.txt` guarantees transitive reachability of every page. It contains:

```
# eRPC Documentation
# https://docs.erpc.cloud/llms.txt

## About
eRPC is an EVM RPC proxy with multi-upstream failsafe, caching, consensus,
and selection policies. This file is the agent entry point for the full docs.

## Navigation tree
(rendered by renderNavTree() — hierarchical bullet list where every page
title links to its <page>.llms.txt, not to the HTML page)

### Quick start
- [Quick Start](https://docs.erpc.cloud/index.llms.txt)
- [Why eRPC?](https://docs.erpc.cloud/why.llms.txt)
- [Free & Public RPCs](https://docs.erpc.cloud/free.llms.txt)
- [FAQ](https://docs.erpc.cloud/faq.llms.txt)

### Get started
- [Installation & first request](https://docs.erpc.cloud/install.llms.txt)
- [Configuration formats](https://docs.erpc.cloud/config-format.llms.txt)
- [Complete config example](https://docs.erpc.cloud/config/example.llms.txt)

### Architecture
- [How eRPC works](https://docs.erpc.cloud/architecture.llms.txt)
... (all pages) ...

## All pages (flat list)
https://docs.erpc.cloud/index.llms.txt — Quick Start
https://docs.erpc.cloud/why.llms.txt — Why eRPC?
... (one line per page, 51 total) ...
```

All internal links in the root `llms.txt` point to `.llms.txt` URLs (not HTML), preserving the invariant that an agent starting from `GET /llms.txt` can follow links without ever exiting the machine-readable surface. This is already enforced by `rewriteInternalLinks()` in `build-llms.mjs` — no change needed.

### Per-page `<page>.llms.txt` structure

Each per-page file is built by `renderPage()` → `buildPageLlmsTxt()` and contains, in order:

1. **Frontmatter block** — `# <title>`, `URL: https://docs.erpc.cloud/<slug>`, `Description: <description>`.
2. **L1 content** — prose, fully rendered, no JSX wrappers remaining.
3. **L2 content** — all `ConfigTabs` expanded to `**YAML:**` + `**TypeScript:**` fenced blocks; all `ConfigCode` expanded to `**Config path:** ...` + fenced block; all `<details>` blocks expanded (the HTML detail/summary collapses are stripped, content preserved).
4. **L3 content** — all `AISection` blocks fully expanded and titled. This is the high-density section agents rely on.
5. **Related pages** — a short `## See also` section listing sibling/parent page `.llms.txt` links so agents can navigate laterally without going back to root.

The "Related pages" section is new and requires a small build-llms.mjs addition: when processing each page, derive its siblings from the nav tree and append:

```js
// After renderPage(), before writing the output file:
const siblings = getSiblings(navTree, pageRel); // returns [{title, llmsUrl}]
if (siblings.length > 0) {
  output += "\n\n## See also\n\n";
  for (const s of siblings) {
    output += `- [${s.title}](${s.llmsUrl})\n`;
  }
}
```

### L2 content expansion in `.llms.txt`

`<details><summary>` blocks: the `build-llms.mjs` scanner does not currently handle `<details>`/`<summary>`. Add one transform:

```js
function transformDetails(src) {
  return transformTag(src, "details", ({ inner }) => {
    // Strip the <summary> tag but keep the summary text and the body.
    const summaryMatch = inner.match(/<summary>([\s\S]*?)<\/summary>/);
    const heading = summaryMatch ? summaryMatch[1].trim() : "";
    const body = inner.replace(/<summary>[\s\S]*?<\/summary>/, "").trim();
    return heading
      ? `\n\n#### ${heading}\n\n${body}\n\n`
      : `\n\n${body}\n\n`;
  });
}
```

### L3 agent-section expansion

The existing `transformAISection()` already fully expands `AISection` content. The only additions:
- `transformKBSource()` (described in §4) strips the badge and emits `(grounded in docs/kb/...)` text.
- `SourceLink` components are already handled by link passthrough.

### Transitive reachability guarantee

An agent starting at `GET https://docs.erpc.cloud/llms.txt` can reach every page by:
1. Root `llms.txt` → flat list of 51 `.llms.txt` URLs (direct, no hops).
2. Root `llms.txt` → nav tree links → each page `.llms.txt` (also direct).
3. Each page `.llms.txt` → "See also" siblings → related pages (lateral navigation).

No page requires more than 1 hop from root. The agent never needs to parse HTML.

---

## 6. The 4–5 Minute Test

### Target: learn eRPC end-to-end in under 5 minutes

The human path is the sequence of L1 sections read top-to-bottom across the pages below. Each L1 is ~100–150 words. Reading speed: ~200–250 words/minute for technical prose.

### The 5-minute path

| Stop | Page | L1 word target | Running time |
|---|---|---|---|
| 1 | Quick Start (`/`) — What is eRPC, one command to run | 120 | ~0:30 |
| 2 | How eRPC works (`/architecture`) — The request pipeline in one paragraph | 130 | ~1:05 |
| 3 | Upstreams & providers (`/architecture/upstreams`) — One config entry per vendor | 100 | ~1:30 |
| 4 | Resilience overview (`/config/failsafe`) — Timeout/retry/hedge/consensus in 4 sentences | 120 | ~2:00 |
| 5 | Cache policies (`/config/database/evm-json-rpc-cache`) — Finality-aware caching | 100 | ~2:25 |
| 6 | Selection policy (`/config/projects/selection-policies`) — The circuit-breaker equivalent | 100 | ~2:50 |
| 7 | Metrics (`/operation/monitoring`) — What to watch in production | 100 | ~3:10 |
| 8 | Complete config example (`/config/example`) — Skim the annotated full config | 150 | ~4:00 |
| 9 | Production checklist (`/operation/production`) — What to set before go-live | 120 | ~4:30 |

**Total: ~1,040 words of L1 content. Estimated reading time: ~4:30.**

This path covers: what eRPC is, how it routes requests, how to connect providers, how failsafe works, how caching works, how upstream scoring works, what to monitor, a real config, and production gotchas. A reader who completes this path can make an informed deployment decision.

### Total site L1 word budget

With 51 pages × 100–150 words per L1 = **5,100–7,650 words** of L1 content total. At 250 wpm that is 20–30 minutes to read every L1 — which is the "complete survey" mode for a thorough engineer evaluating eRPC. The 5-minute path above covers the 9 most essential pages.

### Enforcement

The word budget should be checked at build time. Add a linting step to `build-llms.mjs` or a separate script:

```js
// For each page, after stripping AISection and L2 details blocks,
// count the remaining words. Warn if > 200 words (allows for code examples)
// and fail CI if > 400 words (catches runaway L1 sections).
```

---

## Appendix A — _meta.js changes required

### `docs/pages/_meta.js` (top-level)

Add the new top-level sections. The existing separator pattern is preserved.

```js
module.exports = {
  index: { title: "Quick start", theme: { toc: false, layout: "full", breadcrumb: false, pagination: false } },
  why: { title: "Why eRPC?" },
  free: { title: "Free & Public RPCs" },
  faq: { title: "FAQ" },

  "-- Get Started": { type: "separator", title: "Get Started" },
  install: { title: "Installation" },
  "config-format": { title: "Config formats" },

  "-- Architecture": { type: "separator", title: "Architecture" },
  architecture: { display: "children", title: "Architecture" },

  "-- Config": { type: "separator", title: "Configure" },
  config: { display: "children", title: "Config" },

  "-- Deployment": { type: "separator", title: "Deploy" },
  deployment: { title: "Deployment", display: "children" },

  "-- Operations": { type: "separator", title: "Operate" },
  operation: { title: "Operation", display: "children" },

  "-- Reference": { type: "separator", title: "Reference" },
  reference: { display: "children", title: "Reference" },

  presets: { display: "hidden" },
  // "preview-retry" entry REMOVED — no corresponding .mdx exists (KB 42, edge case #1)
};
```

### `docs/pages/architecture/_meta.js` (new directory)

```js
module.exports = {
  index: { title: "How eRPC works" },     // /architecture (index.mdx)
  projects: { title: "Projects & networks" },
  upstreams: { title: "Upstreams & providers" },
  evm: { title: "EVM method handling" },
  errors: { title: "Error taxonomy" },
};
```

### `docs/pages/config/projects/_meta.js` (add new pages)

```js
module.exports = {
  networks: { title: "Networks" },
  upstreams: { title: "Upstreams" },
  providers: { title: "Providers" },
  "selection-policies": { title: "Selection policy" },
  cors: { title: "CORS" },
  "static-responses": { title: "Static responses" },    // NEW
  "shadow-upstreams": { title: "Shadow upstreams" },    // NEW
};
```

### `docs/pages/operation/_meta.js` (add logging)

```js
module.exports = {
  url: { title: "URL & routing" },
  healthcheck: { title: "Healthcheck" },
  batch: { title: "Batch & multiplexing" },
  directives: { title: "Directives" },
  production: { title: "Production checklist" },
  monitoring: { title: "Metrics" },
  tracing: { title: "Tracing" },
  logging: { title: "Logging" },     // NEW
  admin: { title: "Admin API" },
  cordoning: { title: "Cordoning" },
  cli: { title: "CLI & env vars" },
};
```

### `docs/pages/reference/_meta.js` (new directory)

```js
module.exports = {
  "grpc-bds": { title: "gRPC / BDS streaming" },
  lanes: { title: "Lanes & concurrency" },
  "http-client": { title: "HTTP client & proxy pools" },
  simulator: { title: "Simulator" },
};
```

---

## Appendix B — Nav tree in Operate: grouping without sub-folders

The "Operate" section has four natural sub-groups (Deploy / Observe / Manage / Debug) but the current Nextra setup uses flat `_meta.js` files without sub-folder nesting. Rather than creating four additional directories (which would require `display: "children"` wrappers and add URL path segments), use Nextra `type: "separator"` entries _inside_ the `operation/_meta.js` to create visual grouping without URL changes:

```js
module.exports = {
  "-- deploy": { type: "separator", title: "Deploy" },
  // (deployment pages live in /deployment/ — not repeated here)

  "-- observe": { type: "separator", title: "Observe" },
  monitoring: { title: "Metrics" },
  tracing: { title: "Tracing" },
  logging: { title: "Logging" },

  "-- manage": { type: "separator", title: "Manage" },
  admin: { title: "Admin API" },
  healthcheck: { title: "Healthcheck" },
  cordoning: { title: "Cordoning" },
  cli: { title: "CLI & env vars" },

  "-- debug": { type: "separator", title: "Debug" },
  url: { title: "URL & routing" },
  directives: { title: "Directives" },
  batch: { title: "Batch & multiplexing" },
  production: { title: "Production checklist" },
};
```

The `deployment/` section remains its own top-level separator with its own `_meta.js`. The visual effect in the sidebar is four labeled groups under "Operate" without any URL segment change.

---

## Appendix C — Staleness notes for page authors

These are the known staleness issues from KB 42 that MUST be corrected when writing or rewriting pages (verified against `common/defaults.go`):

| Field | Docs claim | Actual default | Source |
|---|---|---|---|
| `server.maxTimeout` | `30s` | `150s` | `defaults.go:L694-L696` |
| `server.readTimeout` | `0` (no timeout) | `30s` | `defaults.go:L698-L700` |
| `server.writeTimeout` | `0` (no timeout) | `120s` | `defaults.go:L702-L704` |
| `server.includeErrorDetails` | `false` | `true` | `defaults.go:L717-L719` |
| `server.trustedIPHeaders` | `["X-Forwarded-For"]` | `[]` | `defaults.go:L730-L732` |
| `server.waitBeforeShutdown` | `0s` | `10s` | `defaults.go:L709-L711` |
| `server.grpcPortV4` | `4100` | same as `httpPortV4` (4000) | `defaults.go:L676-L678` |
| `server.grpcMaxRecvMsgSize` | `4 MiB` | `100 MiB` | `defaults.go:L688-L692` |
| `metrics.enableGzip` | `false` | `true` | `defaults.go:L706-L708` |
| upstream default failsafe | `2 retries, 15s timeout` | `1 attempt, 60s timeout` | `defaults.go:L148-L160` |
| `selection-policies` default latency threshold | inconsistent (10k vs 30k ms) | verify from KB 22 | KB 22 is ground truth |

The L3 `AISection` on each page must use the correct value and cite the `defaults.go` line.
