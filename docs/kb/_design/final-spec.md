# Docs Revamp — FINAL SPEC (binding contract for page writers)

> Synthesized from proposals A (capability-first nav), B (llms.txt enhancements,
> agent-reference conventions), C (L1 word discipline, zero-link-rot via redirects).
> Ground truth for all content: `docs/kb/*.md`. Never write a claim that isn't in a KB file.

---

## 1. Final nav tree & page list

Legend: KEPT (same URL), MOVED (new URL, 301 redirect added), NEW (no prior page).
Every page lists its KB sources — writers may ONLY use those KB files (plus `_census/` tables when told).

### Top level
| Title | URL | Status | KB sources |
|---|---|---|---|
| Quick start | `/` | KEPT | 04, 40, 32 |
| Why eRPC? | `/why` | KEPT | 01, 39 |
| How eRPC works | `/architecture` | NEW | 01, 39 |
| Free & Public RPCs | `/free` | KEPT | 09, 32 |
| FAQ | `/faq` | KEPT | cross-KB (only verified claims) |

### — Capabilities — Routing & upstreams
| Title | URL | Status | KB sources |
|---|---|---|---|
| Routing overview | `/routing` | NEW | L1s of 06,07,08,09,22 |
| Upstreams | `/routing/upstreams` | MOVED ← `/config/projects/upstreams` | 08, 37 |
| Providers & vendors | `/routing/providers` | MOVED ← `/config/projects/providers` | 09 |
| Networks | `/routing/networks` | MOVED ← `/config/projects/networks` | 07 (ref 34) |
| Selection & scoring | `/routing/selection-policies` | MOVED ← `/config/projects/selection-policies` | 22, 26 (ref 13) |
| Shadow upstreams | `/routing/shadow-upstreams` | NEW | 35 |
| Lanes & concurrency | `/routing/lanes` | NEW | 36 |

### — Capabilities — Resilience
| Title | URL | Status | KB sources |
|---|---|---|---|
| Resilience overview | `/resilience` | NEW | L1s of 10,11,12,13,14,15 |
| Timeout | `/resilience/timeout` | MOVED ← `/config/failsafe/timeout` | 12 |
| Retry | `/resilience/retry` | MOVED ← `/config/failsafe/retry` | 10 |
| Hedge | `/resilience/hedge` | MOVED ← `/config/failsafe/hedge` | 11 |
| Circuit breaker | `/resilience/circuit-breaker` | MOVED ← `/config/failsafe/circuit-breaker` | 13 (ref 22) |
| Consensus | `/resilience/consensus` | MOVED ← `/config/failsafe/consensus` | 14 |
| Integrity | `/resilience/integrity` | MOVED ← `/config/failsafe/integrity` | 15 |

### — Capabilities — Caching & data
| Title | URL | Status | KB sources |
|---|---|---|---|
| Caching overview | `/caching` | NEW | L1s of 17,18,19 |
| Cache policies | `/caching/policies` | MOVED ← `/config/database/evm-json-rpc-cache` | 17 |
| Storage drivers | `/caching/drivers` | MOVED ← `/config/database/drivers` | 18 |
| Shared state | `/caching/shared-state` | MOVED ← `/config/database/shared-state` | 19 |

### — Capabilities — Security & access
| Title | URL | Status | KB sources |
|---|---|---|---|
| Security overview | `/security` | NEW | L1s of 20,16,06; 02 (TLS) |
| Authentication | `/security/auth` | MOVED ← `/config/auth` | 20 |
| Rate limiters | `/security/rate-limiters` | MOVED ← `/config/rate-limiters` | 16 |
| CORS | `/security/cors` | MOVED ← `/config/projects/cors` | 06 |

### — Capabilities — EVM intelligence
| Title | URL | Status | KB sources |
|---|---|---|---|
| EVM overview | `/evm` | NEW | 39; L1s of 23,24,25,34 |
| getLogs auto-splitting | `/evm/getlogs-splitting` | NEW | 23 |
| Method handlers | `/evm/method-handlers` | NEW | 24 |
| Block tracking & served tip | `/evm/block-tracking` | NEW | 25 |
| Static responses | `/evm/static-responses` | NEW | 34 |

### — Configuration —
| Title | URL | Status | KB sources |
|---|---|---|---|
| Full config example | `/config/example` | KEPT | 04, 05 + per-section snippets from all config KBs |
| Server | `/config/server` | KEPT | 02 |
| Config formats (YAML/TS) | `/config/formats` | NEW | 04, 05 |
| Matcher syntax | `/config/matcher` | KEPT | 21 |

### — Operations —
| Title | URL | Status | KB sources |
|---|---|---|---|
| URL structure | `/operation/url` | KEPT | 32 |
| Directives | `/operation/directives` | KEPT | 31 |
| Batching & multiplexing | `/operation/batch` | KEPT | 33 |
| Healthcheck | `/operation/healthcheck` | KEPT | 30 |
| Monitoring & metrics | `/operation/monitoring` | KEPT | 27 |
| Tracing & logging | `/operation/tracing` | KEPT | 28 |
| Admin API | `/operation/admin` | KEPT | 29 |
| Cordoning | `/operation/cordoning` | KEPT | 29, 22 (exclusion), 08 (cordon reasons) |
| CLI & env vars | `/operation/cli` | KEPT | 04 |
| Production checklist | `/operation/production` | KEPT | 40, 02, 27 (recommendations grounded in KB only) |

### — Deployment —
| Title | URL | Status | KB sources |
|---|---|---|---|
| Docker | `/deployment/docker` | KEPT | 40 |
| Kubernetes | `/deployment/kubernetes` | KEPT | 40 |
| Railway | `/deployment/railway` | KEPT | 40 |
| Cloud (hosted) | `/deployment/cloud` | KEPT | keep existing content, refresh tone |

### — Reference —
| Title | URL | Status | KB sources |
|---|---|---|---|
| gRPC & BDS streaming | `/reference/grpc-bds` | NEW | 03 |
| HTTP client & proxy pools | `/reference/http-client` | NEW | 37 |
| Error taxonomy | `/reference/errors` | NEW | 38, `_census/errors.md` |
| Metrics reference | `/reference/metrics` | NEW | 27, `_census/metrics.md` (ALL 122 metrics) |
| Simulator | `/reference/simulator` | NEW | 41 |

### Hidden (URL kept, not in sidebar)
- `/presets/dvn-ready` KEPT hidden — KB 09, 14.

KB coverage check: 01–41 all mapped above; 42 is the audit (meta, no page). Total: 51 visible pages + 1 hidden.

## 2. Redirect map (next.config.mjs `redirects()`, all `permanent: true`)

```
/config/projects/upstreams          → /routing/upstreams
/config/projects/providers          → /routing/providers
/config/projects/networks           → /routing/networks
/config/projects/selection-policies → /routing/selection-policies
/config/projects/cors               → /security/cors
/config/projects                    → /routing
/config/failsafe/timeout            → /resilience/timeout
/config/failsafe/retry              → /resilience/retry
/config/failsafe/hedge              → /resilience/hedge
/config/failsafe/circuit-breaker    → /resilience/circuit-breaker
/config/failsafe/consensus          → /resilience/consensus
/config/failsafe/integrity          → /resilience/integrity
/config/failsafe                    → /resilience
/config/database/evm-json-rpc-cache → /caching/policies
/config/database/drivers            → /caching/drivers
/config/database/shared-state       → /caching/shared-state
/config/database                    → /caching
/config/auth                        → /security/auth
/config/rate-limiters               → /security/rate-limiters
```
Plus the same set with `.llms.txt` suffixes (explicit entries; Next redirects run before public/ static serving).

## 3. Page anatomy (every page, exactly this order)

```mdx
---
title: "<Title>"
description: "<one-sentence>"
---
import { LLMsTxtLink, AISection, ConfigTabs, ConfigCode, SourceLink } from "<rel>/components";

<LLMsTxtLink />

# <Title>

{/* ============ L1 — humans, CTO attention span ============ */}
<60–120 words prose. What it is + what outcome it buys + when to care.
 NO config fields, NO tables. One concrete prevented-failure or enabled-win.>

<ConfigTabs … />   {/* minimal 3–8 line quick example; only fields that turn the feature on */}

{/* ============ L2 — staff-engineer deep dive ============ */}
## How it works
<2–4 paragraphs of mechanics from KB L2. ConfigTabs/ConfigCode allowed.>

## <topic H2s as needed>
<details><summary>**Deep dive: <topic>**</summary> …≤300 words… </details>
  (bold prefixes: "Deep dive:", "Edge case:", "Performance:", "Migration:")
  Rule: >4 L2 topics ⇒ collapse all but the 2 most universally needed.
  L2 NEVER contains field tables — those live only in L3.

{/* ============ L3 — agents ============ */}
<AISection title="<Subject> — full agent reference">

### Config schema
<table: every field | type | exact default (cited) | behavior/footguns — from KB L3>

### Behavioral invariants
<bullets: what the code always does; each with a SourceLink/permalink>

### Edge cases & gotchas
<numbered list from KB L3; each grounded>

### Source code entry points
<≥3 GitHub permalinks: config struct, defaults, primary execution path —
 format: [`common/defaults.go:L694-L696`](https://github.com/erpc/erpc/blob/main/common/defaults.go#L694-L696)>

### Related metrics
<table: metric | type | labels | when it fires — only metrics relevant to this page>

</AISection>
```

Overview pages (`/routing`, `/resilience`, `/caching`, `/security`, `/evm`, `/architecture`):
L1 ≤150 words + `<CapabilityGrid>` of child pages + NO L2 + slim AISection that mostly
links children's `.llms.txt` files (agents should descend, not duplicate).

**The 5-minute path:** `/` → `/architecture` → 5 capability overviews ≈ 8 stops × ~130
words ≈ ~1,050 words ≈ 4½ min @230wpm. CI lint: overview L1 ≤150 words, leaf L1 ≤120 words.

## 4. Components

Reuse unchanged: `AISection`, `ConfigTabs`, `ConfigCode`, `LLMsTxtLink` (every page, first line after imports).
New:
- `CapabilityGrid` — `{ items: {title, href, summary, icon?}[], columns?: 2|3|4 }`,
  `data-component="capability-grid"`; llms transform → bullet list with `.llms.txt` links.
- `SourceLink` — `{ file, lines?, label? }` → GitHub permalink anchor,
  `data-component="source-link"`; llms transform → plain markdown link.

## 5. build-llms.mjs additions
1. `transformDetails` — expand `<details>/<summary>` (summary → bold line) BEFORE AISection transform.
2. `transformCapabilityGrid` — items → markdown bullets (parse the array literal; fall back to generic line).
3. `transformSourceLink` — emit `[label](permalink)`.
4. "See also" section appended per page: siblings from nav tree as `.llms.txt` links (B's lateral navigation).
5. NEW `llms-full.txt` at root: concatenation of every page's llms.txt (single-fetch agents).
6. Root `llms.txt`: unchanged mechanism (nav tree + flat list) — every page 1 hop from root.

## 6. Writer rules (enforced at review)
1. Every factual claim traces to the page's KB file(s). KB conflicts with old page ⇒ KB wins.
2. Exact defaults with citation; never "by default it usually…".
3. eRPC vocabulary fixed by KB titles (upstream, network, project, directive, lane, served tip…).
4. Don't link to `docs/kb/` from public pages (KB ships in repo, not on site); convert KB citations to GitHub permalinks.
5. Tone: L1 plain & confident; L2 precise prose; L3 terse & complete (tables > prose).
6. Old page content may be consulted for examples ONLY if re-verified against KB.
