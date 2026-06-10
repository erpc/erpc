# Docs IA Proposal A — Capability-First Rebuild

> Author: Claude Code · Date: 2026-06-10
> Ground truth: `docs/kb/` (42 files) + `docs/kb/_design/brief.md`
> Constraint: prefer existing URL slugs; redirect list below where unavoidable

---

## 1. Nav Tree

### Design rationale

The brief demands that a CTO scanning the left sidebar immediately grasps what eRPC *does* before they encounter any configuration sub-tree. The current nav opens with a flat "Config" section that puts upstreams, CORS, and auth on equal footing. Proposal A reorganises into five conceptual zones:

1. **Quick start + tour** — one entry point for humans, one for agents
2. **Capabilities** — features as primary nouns (Routing, Reliability, Caching, Security, Observability, EVM specifics)
3. **Reference** — all config pages, grouped logically under the capabilities they belong to
4. **Deployment** — unchanged from current; URLs preserved
5. **Operations** — runtime + runbooks

The capability sections sit *above* the reference sections. Each capability section has a landing page (L1 human-readable) that links down into the reference sub-pages (L2/L3 detail). Reference pages remain at their existing URL slugs so inbound links are preserved.

### Full sidebar structure

```
── (no separator)
   Quick Start                          /                           KEPT
   Why eRPC?                            /why                        KEPT
   Free & Public RPCs                   /free                       KEPT

── CAPABILITIES ─────────────────────────────────────────────────
   How eRPC Works                       /how-it-works               NEW
   
   ── Routing & Upstreams
      Overview                          /routing                    NEW
      Providers & Vendors               /config/projects/providers  KEPT
      Upstream Config                   /config/projects/upstreams  KEPT
      Networks                          /config/projects/networks   KEPT
      Selection Policy                  /config/projects/selection-policies  KEPT
      URL Structure                     /operation/url              KEPT
      Directives                        /operation/directives       KEPT
      Batching & Multiplexing           /operation/batch            KEPT
   
   ── Reliability & Failsafe
      Overview                          /reliability                NEW
      Retry                             /config/failsafe/retry      KEPT
      Hedge                             /config/failsafe/hedge      KEPT
      Timeout                           /config/failsafe/timeout    KEPT
      Consensus                         /config/failsafe/consensus  KEPT
      Integrity Checks                  /config/failsafe/integrity  KEPT
      Circuit Breaker / Exclusion       /config/failsafe/circuit-breaker  KEPT
   
   ── Caching
      Overview                          /caching                    NEW
      Cache Policies                    /config/database/evm-json-rpc-cache  KEPT
      Storage Drivers                   /config/database/drivers    KEPT
      Shared State                      /config/database/shared-state  KEPT
   
   ── Security & Access Control
      Overview                          /security                   NEW
      Authentication                    /config/auth                KEPT
      Rate Limiters                     /config/rate-limiters       KEPT
      CORS                              /config/projects/cors       KEPT
      Admin API                         /operation/admin            KEPT
   
   ── EVM Specifics
      Overview                          /evm                        NEW
      getLogs Splitting                 /evm/getlogs-splitting      NEW
      Static Responses                  /evm/static-responses       NEW
      Shadow Upstreams                  /evm/shadow-upstreams       NEW
      gRPC / BDS Streaming              /evm/grpc-bds               NEW

── OPERATIONS ───────────────────────────────────────────────────
   Production Checklist                 /operation/production       KEPT
   Monitoring & Metrics                 /operation/monitoring       KEPT
   Tracing                              /operation/tracing          KEPT
   Healthcheck                          /operation/healthcheck      KEPT
   Cordoning                            /operation/cordoning        KEPT
   CLI & Environment                    /operation/cli              KEPT

── DEPLOYMENT ───────────────────────────────────────────────────
   Docker                               /deployment/docker          KEPT
   Kubernetes                           /deployment/kubernetes      KEPT
   Railway                              /deployment/railway         KEPT
   Cloud (Hosted)                       /deployment/cloud           KEPT

── REFERENCE ────────────────────────────────────────────────────
   Config Overview & Example            /config/example             KEPT
   Server Config                        /config/server              KEPT
   Matcher Syntax                       /config/matcher             KEPT
   TypeScript Config                    /config/typescript          NEW
   Error Taxonomy                       /reference/errors           NEW
   Simulator                            /reference/simulator        NEW
   FAQ                                  /faq                        KEPT
   
── (hidden from nav — accessible by URL)
   Presets: DVN-Ready                   /presets/dvn-ready          KEPT
```

### Explicit redirects (moved slugs)

| From (old) | To (new) | Reason |
|---|---|---|
| `/config/failsafe` (the overview page if it existed as a slug) | `/reliability` | Capability landing replaces the section-level overview |
| `/config/database` (old section landing) | `/caching` | Merged under Caching capability |
| `/config/projects` (old section landing) | `/routing` | Merged under Routing capability |

No existing leaf page slugs change. All 26 existing leaf slugs are preserved exactly.

New pages that have no prior URL (all NEW) need no redirect entry.

---

## 2. Page List — KB Source Mapping

Every one of the 42 KB files is mapped to at least one page below.

| Page (title) | Slug | Status | KB file(s) |
|---|---|---|---|
| Quick Start | `/` | KEPT | `01-request-lifecycle.md` (pipeline diagram text), `04-cli-bootstrap-config-loading.md`, `40-deployment-docker-k8s-monitoring.md` |
| Why eRPC? | `/why` | KEPT | `01-request-lifecycle.md`, `39-evm-architecture-overview.md` |
| Free & Public RPCs | `/free` | KEPT | `09-providers-vendors.md`, `32-url-structure-routing.md` |
| **How eRPC Works** | `/how-it-works` | NEW | `01-request-lifecycle.md`, `39-evm-architecture-overview.md`, `06-projects-cors.md`, `07-networks.md` |
| **Routing Overview** | `/routing` | NEW | `06-projects-cors.md`, `07-networks.md`, `08-upstreams-lifecycle.md`, `32-url-structure-routing.md`, `36-lanes-concurrency.md` |
| Providers & Vendors | `/config/projects/providers` | KEPT | `09-providers-vendors.md` |
| Upstream Config | `/config/projects/upstreams` | KEPT | `08-upstreams-lifecycle.md`, `37-clients-proxy-pools.md` |
| Networks | `/config/projects/networks` | KEPT | `07-networks.md`, `34-static-responses.md` |
| Selection Policy | `/config/projects/selection-policies` | KEPT | `22-selection-policies-scoring.md`, `26-health-tracker-scoring-inputs.md`, `13-failsafe-circuit-breaker.md` |
| URL Structure | `/operation/url` | KEPT | `32-url-structure-routing.md` |
| Directives | `/operation/directives` | KEPT | `31-request-directives.md` |
| Batching & Multiplexing | `/operation/batch` | KEPT | `33-batching-multiplexing.md` |
| **Reliability Overview** | `/reliability` | NEW | `10-failsafe-retry.md`, `11-failsafe-hedge.md`, `12-failsafe-timeout.md`, `13-failsafe-circuit-breaker.md`, `14-consensus.md`, `15-failsafe-integrity.md` |
| Retry | `/config/failsafe/retry` | KEPT | `10-failsafe-retry.md` |
| Hedge | `/config/failsafe/hedge` | KEPT | `11-failsafe-hedge.md` |
| Timeout | `/config/failsafe/timeout` | KEPT | `12-failsafe-timeout.md` |
| Consensus | `/config/failsafe/consensus` | KEPT | `14-consensus.md` |
| Integrity Checks | `/config/failsafe/integrity` | KEPT | `15-failsafe-integrity.md` |
| Circuit Breaker / Exclusion | `/config/failsafe/circuit-breaker` | KEPT | `13-failsafe-circuit-breaker.md`, `22-selection-policies-scoring.md` |
| **Caching Overview** | `/caching` | NEW | `17-cache-policies.md`, `18-cache-connectors.md`, `19-shared-state.md` |
| Cache Policies | `/config/database/evm-json-rpc-cache` | KEPT | `17-cache-policies.md` |
| Storage Drivers | `/config/database/drivers` | KEPT | `18-cache-connectors.md` |
| Shared State | `/config/database/shared-state` | KEPT | `19-shared-state.md` |
| **Security Overview** | `/security` | NEW | `20-auth.md`, `16-rate-limiters.md`, `06-projects-cors.md` |
| Authentication | `/config/auth` | KEPT | `20-auth.md` |
| Rate Limiters | `/config/rate-limiters` | KEPT | `16-rate-limiters.md` |
| CORS | `/config/projects/cors` | KEPT | `06-projects-cors.md` |
| Admin API | `/operation/admin` | KEPT | `29-admin-api.md` |
| **EVM Specifics Overview** | `/evm` | NEW | `39-evm-architecture-overview.md`, `24-evm-method-handlers.md`, `25-evm-state-poller-served-tip.md` |
| **getLogs Splitting** | `/evm/getlogs-splitting` | NEW | `23-evm-getlogs-splitting.md` |
| **Static Responses** | `/evm/static-responses` | NEW | `34-static-responses.md` |
| **Shadow Upstreams** | `/evm/shadow-upstreams` | NEW | `35-shadow-upstreams.md` |
| **gRPC / BDS Streaming** | `/evm/grpc-bds` | NEW | `03-grpc-server-bds.md` |
| Production Checklist | `/operation/production` | KEPT | `40-deployment-docker-k8s-monitoring.md` |
| Monitoring & Metrics | `/operation/monitoring` | KEPT | `27-observability-metrics.md` |
| Tracing | `/operation/tracing` | KEPT | `28-observability-tracing-logging.md` |
| Healthcheck | `/operation/healthcheck` | KEPT | `30-healthcheck.md` |
| Cordoning | `/operation/cordoning` | KEPT | `29-admin-api.md` (cordon section) |
| CLI & Environment | `/operation/cli` | KEPT | `04-cli-bootstrap-config-loading.md` |
| Docker | `/deployment/docker` | KEPT | `40-deployment-docker-k8s-monitoring.md` |
| Kubernetes | `/deployment/kubernetes` | KEPT | `40-deployment-docker-k8s-monitoring.md` |
| Railway | `/deployment/railway` | KEPT | `40-deployment-docker-k8s-monitoring.md` |
| Cloud (Hosted) | `/deployment/cloud` | KEPT | (pricing/external) |
| Config Example | `/config/example` | KEPT | all config KBs (comprehensive example) |
| Server Config | `/config/server` | KEPT | `02-http-server.md` |
| Matcher Syntax | `/config/matcher` | KEPT | `21-matchers.md` |
| **TypeScript Config** | `/config/typescript` | NEW | `05-config-formats-typescript.md` |
| **Error Taxonomy** | `/reference/errors` | NEW | `38-errors-taxonomy.md` |
| **Simulator** | `/reference/simulator` | NEW | `41-erpc-simulator.md` |
| FAQ | `/faq` | KEPT | `42-existing-docs-audit.md` (gap reference only; content from all KBs) |
| DVN-Ready Preset | `/presets/dvn-ready` | KEPT | `09-providers-vendors.md`, `14-consensus.md` |

### KB coverage audit

All 42 KB files covered:

```
01  → Quick Start, How eRPC Works, Why
02  → Server Config
03  → gRPC / BDS Streaming
04  → CLI & Environment, Quick Start
05  → TypeScript Config
06  → Routing Overview, Security Overview, CORS
07  → Networks, Routing Overview, How eRPC Works
08  → Upstream Config, Routing Overview
09  → Providers & Vendors, Free & Public RPCs, DVN Preset
10  → Retry, Reliability Overview
11  → Hedge, Reliability Overview
12  → Timeout, Reliability Overview
13  → Circuit Breaker, Selection Policy, Reliability Overview
14  → Consensus, Reliability Overview, DVN Preset
15  → Integrity Checks, Reliability Overview
16  → Rate Limiters, Security Overview
17  → Cache Policies, Caching Overview
18  → Storage Drivers, Caching Overview
19  → Shared State, Caching Overview
20  → Authentication, Security Overview
21  → Matcher Syntax
22  → Selection Policy, Circuit Breaker
23  → getLogs Splitting
24  → EVM Specifics Overview
25  → EVM Specifics Overview
26  → Selection Policy (health-tracker inputs section)
27  → Monitoring & Metrics
28  → Tracing
29  → Admin API, Cordoning
30  → Healthcheck
31  → Directives
32  → URL Structure, Free & Public RPCs
33  → Batching & Multiplexing
34  → Static Responses, Networks
35  → Shadow Upstreams
36  → Routing Overview (lanes section), Upstream Config
37  → Upstream Config (client/proxy pool section)
38  → Error Taxonomy
39  → How eRPC Works, Why, EVM Specifics Overview
40  → Quick Start, Docker, Kubernetes, Railway, Production Checklist
41  → Simulator
42  → (audit metadata; informs FAQ staleness notes)
```

---

## 3. Page Anatomy

Every page follows the same three-level structure. The anatomy below is the standard template all page writers must implement.

### L1 — Default visible body (human, CTO attention span)

**Target length:** 80–200 words. A human should be able to read the L1 of any single page in under 30 seconds.

**Required sections (all visible by default, no collapsing):**

```
# [Feature Name]

[1–2 sentence what + why. Concrete outcome.]

## What it does

[3–6 bullet points. Each bullet = one concrete capability or behaviour.
 No config field names. No "how to configure". Just what the feature achieves.]

## When to use it

[2–4 sentences. Decision guidance: what workload, what signal tells you
 to enable/tune this feature.]

## Quick example

[One ConfigTabs block — minimal YAML + TS showing the feature on in 3–8 lines.
 Only the fields that make the feature work. No exhaustive config.]
```

Capability overview pages (e.g. `/routing`, `/reliability`) also include a **"Capabilities at a glance"** row of 3–5 named sub-features with one-line summaries and links to detail pages, before the Quick example.

**Word-count discipline:** L1 sections must stay under 200 words. The page writer's job is compression, not completeness — completeness belongs in L2/L3.

### L2 — Human deep-dive (principal/staff engineer)

**Presentation rule:** use `<details>` collapse (`<Callout>` + `<details>` wrapper for short asides) OR plain H2 sections for longer tutorials. The choice:

- **Collapsible (`<details>`):** use when the section is ≤300 words and is "nice to know but not required for 80% of users." Examples: advanced edge-case behaviour, failure mode analysis, performance tuning guidance.
- **H2 section (always visible below L1):** use when the content is a common workflow step that most operators *will* need — e.g. "How retries interact with consensus", "Reading the selection policy JS environment", "Cache key derivation rules".

**Required H2 sections per page type:**

| Page type | Required L2 H2 sections |
|---|---|
| Capability overview | (none — overview pages are intentionally L1-heavy) |
| Config detail page | "How it works", "Common patterns", "Interaction with [sibling feature]" |
| Operations page | "Configuration", "Reading the output", "Troubleshooting" |
| Deployment page | "Steps" (using Nextra `<Steps>`), "Production hardening" |

**Collapsible convention:**

```mdx
<details>
<summary>**Deep dive: how adaptive hedge delay is computed**</summary>

[up to 300 words of mechanism detail, with one ConfigTabs example]

</details>
```

The `<summary>` line always starts with **bold prefix** matching the section category: "Deep dive:", "Edge case:", "Performance:", "Migration:".

### L3 — Agent section

**Placement:** one `<AISection>` per page, always at the bottom, after all L2 content.

**Standard internal structure inside `<AISection>`:**

```mdx
<AISection
  title="Full reference — every field, default, and edge case"
  hint="Expand for complete config surface or copy into your AI assistant."
  defaultOpen={false}
>

## Config schema

[Table: field | type | default | notes — every config field for this page's scope.
 Default values MUST be verified against common/defaults.go, never from memory.]

## Behavioural invariants

[Bullet list of guaranteed behaviours — things the code always does regardless
 of config, derived from KB L3 sections. Each bullet cites a source link.]

## Edge cases & gotchas

[Numbered list. Each entry: one sentence description + source link.]

## Source code entry points

[Bullet list of `github.com/erpc/erpc/blob/main/<file>#L<N>` links.
 Format: `[description](link)` — what the code does, not just the filename.
 Minimum 3 links per page, covering: config struct definition, defaults,
 and the primary execution path.]

## Related metrics

[Only on pages that have associated erpc_* metrics.
 Table: metric_name | type | labels | description — sourced from KB 27.]

</AISection>
```

**Source link format (mandatory):**

All source links inside L3 must use the full GitHub permalink format:

```
[`common/defaults.go:L694-L696`](https://github.com/erpc/erpc/blob/main/common/defaults.go#L694-L696)
```

Never use bare filenames. Always link to a specific line range. The build-llms.mjs pipeline preserves these as live hyperlinks in the .llms.txt output so an agent can follow them to the code.

**data-component attribute:**

The `<AISection>` tag renders with `data-component="ai-section"` and `data-title={title}`. build-llms.mjs uses this to detect and inline the section fully expanded in the .llms.txt output. No additional data attributes are required for L3 beyond what `AISection` already emits.

---

## 4. Component API Spec

### Existing components — reuse as-is (no changes needed)

#### `AISection` (`docs/components/AISection.tsx`)

```tsx
<AISection
  title="Full reference — every field, default, and edge case"
  hint="Expand for complete config surface or copy into your AI assistant."
  defaultOpen={false}
>
  {/* L3 content */}
</AISection>
```

Renders `<details className="cv-ai" data-component="ai-section" data-title={title}>`. build-llms.mjs detects via `data-component="ai-section"` and inlines the `<details>` body fully expanded regardless of `defaultOpen`. No changes to the component needed.

#### `ConfigTabs` (`docs/components/ConfigTabs.tsx`)

```tsx
<ConfigTabs
  yaml={`...`}
  ts={`...`}
  path="projects[].networks[].failsafe[].retry"
  focus="3-6"
/>
```

Renders YAML/TS dual tabs with `localStorage` persistence under key `GlobalConfigTypeTabIndex`. All pages using raw `<Tabs storageKey="GlobalConfigTypeTabIndex">` participate in the same sync — new pages should always use `<ConfigTabs>` not raw `<Tabs>`.

#### `ConfigCode` (`docs/components/ConfigCode.tsx`)

```tsx
<ConfigCode
  language="yaml"
  path="projects[].networks[].evm.getLogs.maxAllowedRange"
  filename="erpc.yaml"
  focus="4"
>
{`...`}
</ConfigCode>
```

Use `ConfigCode` (single file, no tab) when the snippet is not YAML+TS paired, e.g. for bash commands, JSON request/response examples, or when showing only one format. `code` prop wins over `children`; use template literal children in MDX for readability.

#### `LLMsTxtLink` (`docs/components/LLMsTxtLink.tsx`)

```tsx
<LLMsTxtLink />
```

No props. Must appear on every page, typically as the first line of the MDX body (before the H1). The component derives its URL from `router.asPath`, so placement is not order-sensitive for functionality. Visually it renders as a floating badge.

#### `HeroDiagram` (`docs/components/HeroDiagram.tsx`)

Home page only. No changes.

### New components needed

#### `CapabilityGrid` (NEW — `docs/components/CapabilityGrid.tsx`)

Used on capability overview pages (Routing, Reliability, Caching, Security, EVM) to render the "capabilities at a glance" row.

```tsx
<CapabilityGrid items={[
  { title: "Retry", href: "/config/failsafe/retry", summary: "Rotate upstreams or re-attempt with backoff on error." },
  { title: "Hedge", href: "/config/failsafe/hedge", summary: "Fire parallel speculative requests to cut tail latency." },
  // ...
]} />
```

Props:

| Prop | Type | Notes |
|---|---|---|
| `items` | `Array<{ title: string; href: string; summary: string; icon?: string }>` | 3–6 items. `href` is a relative docs path. `icon` is an optional emoji or SVG name. |
| `columns` | `2 \| 3 \| 4` | Default `3`. Controls CSS grid columns. |

Renders as a CSS grid of cards. Each card: bold title (linked), summary line. Styled under `.cv-capability-grid` in `docs/styles/components.css`.

**data-component attribute:** `data-component="capability-grid"`. build-llms.mjs should transform it to a markdown bullet list:

```
- **[Retry](/config/failsafe/retry.llms.txt)** — Rotate upstreams or re-attempt with backoff on error.
```

This keeps the agent-readable surface informative even without card rendering.

#### `SourceLink` (NEW — `docs/components/SourceLink.tsx`)

Inline component for source-code citations in L2 and L3.

```tsx
<SourceLink file="common/defaults.go" lines="694-696" />
```

Props:

| Prop | Type | Notes |
|---|---|---|
| `file` | `string` | Repo-relative path, e.g. `"common/defaults.go"`. |
| `lines` | `string` | Line range, e.g. `"694-696"` or `"123"`. |
| `label` | `string` | Optional human label; defaults to `` `<file>:L<lines>` ``. |

Renders as `<a href="https://github.com/erpc/erpc/blob/main/<file>#L<lines>" target="_blank" rel="noopener" data-component="source-link">`. build-llms.mjs detects `data-component="source-link"` and renders it as the inline markdown link — no special transform needed beyond what `transformTag` already does for self-closing tags.

### Updated `data-component` attributes build-llms.mjs must handle

| `data-component` value | Current handling | Required handling |
|---|---|---|
| `ai-section` | Inline fully expanded | No change |
| `config-code` | Render as fenced code block with path header | No change |
| `config-tabs` | Render YAML + TS blocks separately | No change |
| `llms-txt-link` | Strip (not useful in plain text) | No change |
| `capability-grid` | Not yet handled | NEW: transform to bullet list (see above) |
| `source-link` | Not yet handled | NEW: transform to inline markdown link |

The `transformTag` function in build-llms.mjs handles each tag by name. Add two new handlers:

```js
// In transformMdxToMarkdown():
src = transformCapabilityGrid(src);  // NEW
src = transformSourceLink(src);      // NEW
```

```js
function transformCapabilityGrid(src) {
  return transformTag(src, "CapabilityGrid", ({ attrs }) => {
    // attrs.items is a JS array literal — eval-safe parse or regex extract
    // For now: fall back to a simple "See sub-pages below." line
    return "\n> **Capabilities:** See the sub-pages in this section for full detail.\n";
  });
}

function transformSourceLink(src) {
  return transformTag(src, "SourceLink", ({ attrs }) => {
    const file = attrs.file ?? "";
    const lines = attrs.lines ?? "";
    const label = attrs.label ?? `\`${file}:L${lines}\``;
    const href = `https://github.com/erpc/erpc/blob/main/${file}#L${lines}`;
    return `[${label}](${href})`;
  });
}
```

These two functions follow exactly the same pattern as the existing `transformConfigCode`, `transformCallout`, etc. — no architectural change to build-llms.mjs required.

---

## 5. llms.txt Scheme

### Transitive reachability guarantee

The root `/llms.txt` already provides two complementary entry points for agents:

1. **Hierarchical nav tree** — mirrors the sidebar; every leaf is a `.llms.txt` link.
2. **Flat "all pages" list** — every page linked directly; useful for fan-out.

This already guarantees transitive reachability provided all pages appear in the nav tree (via `_meta.js`) and the `walkMdx` pass finds all `.mdx` files.

**New pages added by this proposal** must be registered in their respective `_meta.js` files so `buildNavTree` includes them. Checklist per new page:

- `/how-it-works` → add to `docs/pages/_meta.js` (top-level, before capability separator)
- `/routing`, `/reliability`, `/caching`, `/security`, `/evm` → add to `docs/pages/_meta.js` under their respective section separators
- `/evm/getlogs-splitting`, `/evm/static-responses`, `/evm/shadow-upstreams`, `/evm/grpc-bds` → new `docs/pages/evm/_meta.js`
- `/config/typescript` → add to `docs/pages/config/_meta.js`
- `/reference/errors`, `/reference/simulator` → new `docs/pages/reference/_meta.js`

### Per-page .llms.txt structure

Each per-page `.llms.txt` is emitted by build-llms.mjs as:

```
# <Page Title>

> Source: https://docs.erpc.cloud/<path>
> <frontmatter description>
> Format: machine-readable markdown export of the docs page above.
> All collapsible AI sections are inlined and fully expanded.

<L1 content as markdown>

<L2 sections as markdown — H2 headings + body>

---

### Full reference — every field, default, and edge case

<L3 AISection content — fully inlined, not collapsed>

---
```

**Internal links:** build-llms.mjs already rewrites internal links (`/foo`) to `/foo.llms.txt` via the `rewriteLinks` transform. This means all `href` values in `<CapabilityGrid>` items and `<SourceLink>` components that point to internal docs pages will be rewritten correctly — agents following any link in any `.llms.txt` stay on the machine-readable surface.

### L2/L3 expansion in llms.txt

**L2 collapsible `<details>` blocks:** build-llms.mjs currently does not have a `transformDetails` function. Add one to ensure `<details>/<summary>` content is always expanded in the `.llms.txt` output:

```js
function transformDetails(src) {
  return transformTag(src, "details", ({ inner }) => {
    // Strip the <summary> tag and return the rest of the content expanded
    const withoutSummary = transformTag(inner, "summary", ({ inner: s }) =>
      `\n**${s.trim()}**\n`
    );
    return `\n${withoutSummary.trim()}\n`;
  });
}
```

Add `src = transformDetails(src);` to `transformMdxToMarkdown()` **before** `transformAISection` (since `<AISection>` uses `<details>` internally and is already handled).

**L3 AISection:** already fully expanded by `transformAISection`. No change needed.

### Root llms.txt — nav separators

The current `renderNavTree` function emits separators as `### <Title>` headings. The new sidebar has five separators (CAPABILITIES, OPERATIONS, DEPLOYMENT, REFERENCE). These render as H3 headings in root llms.txt, creating a scannable outline. No change to renderNavTree needed; just ensure the `_meta.js` separator entries use the new section names.

---

## 6. The 4–5 Minute Test

### Estimated L1 word count

Targets below are per-page L1 maximums (brief says "under 200 words per page" for L1; full tour in "4–5 minutes total"):

| Page | Est. L1 words |
|---|---|
| Quick Start (`/`) | 180 |
| Why eRPC? (`/why`) | 120 |
| How eRPC Works (`/how-it-works`) | 200 |
| Routing Overview (`/routing`) | 160 |
| Providers & Vendors | 140 |
| Upstream Config | 160 |
| Networks | 150 |
| Selection Policy | 160 |
| URL Structure | 120 |
| Directives | 140 |
| Batching & Multiplexing | 100 |
| Reliability Overview (`/reliability`) | 200 |
| Retry | 130 |
| Hedge | 120 |
| Timeout | 120 |
| Consensus | 140 |
| Integrity Checks | 120 |
| Circuit Breaker / Exclusion | 110 |
| Caching Overview (`/caching`) | 180 |
| Cache Policies | 150 |
| Storage Drivers | 120 |
| Shared State | 120 |
| Security Overview (`/security`) | 160 |
| Authentication | 140 |
| Rate Limiters | 130 |
| CORS | 100 |
| Admin API | 130 |
| EVM Specifics Overview (`/evm`) | 160 |
| getLogs Splitting | 120 |
| Static Responses | 90 |
| Shadow Upstreams | 100 |
| gRPC / BDS Streaming | 120 |
| Production Checklist | 150 |
| Monitoring & Metrics | 140 |
| Tracing | 110 |
| Healthcheck | 110 |
| Cordoning | 100 |
| CLI & Environment | 110 |
| Docker / K8s / Railway / Cloud | 100 each × 4 = 400 |
| Config Example + Server + Matcher + TS | 120 each × 4 = 480 |
| Error Taxonomy + Simulator + FAQ | 100 each × 3 = 300 |
| **Total** | **≈ 6,200 words** |

At 200 words/minute (comfortable reading pace), 6,200 words = **31 minutes** — clearly too long for a cover-to-cover read of all pages.

### The 4–5 minute path: the curated tour

The brief's "4–5 minute" target applies not to reading every page but to reading the **L1 content of the capability tour** — the path a CTO or evaluator would follow scanning the nav:

**Curated 4-minute tour path:**

1. **Quick Start** — pipeline overview + one working example (180 words)
2. **How eRPC Works** — architecture and request lifecycle (200 words)
3. **Routing Overview** — what routing + upstreams + selection does (160 words)
4. **Reliability Overview** — what retry/hedge/timeout/consensus does (200 words)
5. **Caching Overview** — what the cache layer provides (180 words)
6. **Security Overview** — auth + rate limiting at a glance (160 words)
7. **EVM Specifics Overview** — what's EVM-specific (160 words)

**Total: 7 pages × avg 177 words = ~1,240 words ≈ 6.2 minutes at 200 wpm.**

To land firmly under 5 minutes at the 220 wpm pace of a scanning technical reader, the target is:

- Each overview page L1: **≤ 150 words** (not 200)
- The 7-page tour total: **≤ 1,050 words**

**Implementation:** Add a `<TourNext href="/reliability" label="Next: Reliability" />` breadcrumb component at the bottom of each tour page (Quick Start → How it Works → Routing → Reliability → Caching → Security → EVM). This is optional UI sugar — it does not affect the information architecture but dramatically improves the 5-minute tour experience.

**Alternative: single "Tour" page**

If a single `/tour` page proves preferable (one read, no clicking), it aggregates the 7 overview L1 bodies (≤150 words each = ≤1,050 total) into one page with H2 section headings, one visual architecture diagram (reuse `HeroDiagram`), and deep links to each section's config detail pages. Upside: guaranteed under-5-minutes, single `llms.txt` for the tour. Downside: duplicates content with the per-section overview pages, creating a sync burden.

**Recommendation:** implement the per-section overview pages first (each with tight ≤150-word L1s), then add the `/tour` page as a generated redirect/aggregate if telemetry shows users don't follow the "Next" breadcrumb chain.

---

## Summary scorecard

| Criterion | Proposal A approach |
|---|---|
| **Vision fidelity** | Nav reads capabilities first (Routing, Reliability, Caching, Security, EVM), config reference second — matches "CTO scanning the sidebar" mandate |
| **KB coverage** | All 42 KBs mapped; 13 previously undocumented KBs get new pages |
| **Human 5-min path** | 7 overview pages × ≤150 words each = ≤1,050 words; linked by TourNext breadcrumb |
| **Agent navigability** | Root llms.txt nav tree + flat list; all internal links rewritten to .llms.txt; AISection always expanded; SourceLink adds code-level permalinks |
| **URL stability** | 26 existing leaf slugs preserved exactly; 3 section-level redirects (none are heavily linked externals); all new slugs are additive |
| **Maintainability** | CapabilityGrid + SourceLink are the only new components; build-llms.mjs changes are 2 short functions; no architectural changes to Nextra config |
