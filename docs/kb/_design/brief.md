# Docs Revamp — Design Brief (north star)

## Vision

Rebuild the eRPC public docs **agent-first, human-second**, grounded entirely in
current code reality (the `docs/kb/` knowledge base). Humans read docs for
high-level understanding with a short attention span; agents read docs to
actually configure eRPC and need maximal, exhaustive detail.

## Three content levels (every page)

1. **L1 — High-level (humans, CTO attention span).** What the feature is and what
   it achieves. Super short, easy to digest. A reader should be able to learn
   eRPC end-to-end across all L1 content in **under 4–5 minutes total**.
   Example grain: "eRPC has hedging" / "eth_getLogs auto-splitting exists and
   saves you from provider range limits."
2. **L2 — Mid-level (Principal/Staff engineers).** The interesting "how": nuances
   and design behavior — e.g. "how does getLogs splitting behave when 1
   sub-request of a split fails?" — but NOT config-field minutiae. Presented as
   collapsible or lower-header sections a curious human can opt into.
3. **L3 — Low-level (agents).** Full detail of every config field, query param,
   exact numbers, request/response bodies, exact metric names, edge cases.
   Readability matters less; **complete coverage of edge cases and nuances is
   mandatory**. Presented as a collapsible agent-targeted section humans don't
   normally open. HIGHLY linked to source code (`github.com/erpc/erpc/blob/main/...#L123`)
   so agents can follow links to ground truth. Where prose would duplicate
   code, prefer pointing agents at the code path + what to look for.

## Design requirements

- **Left nav redesigned** to show off eRPC capabilities + operational topics.
  Current tree is decent but grouping/hierarchy can improve.
- Each page: L1 is the default visible body; L2 in collapsible/lower-header
  human-friendly sections; L3 in a collapsible agent-only section.
- **llms.txt for every single path/page including root.** Root llms.txt must
  (transitively) link to ALL pages, so an agent starting from root can navigate
  to anything. All internal links inside .llms.txt files stay on the
  machine-readable surface. (Pipeline exists: docs/scripts/build-llms.mjs —
  extend, don't regress.)

## Grounding rules

- Every claim must come from `docs/kb/` (which itself cites file:line). No
  memory-based claims. KB is the single source of truth for page writers.
- Real-world best practices and recommendations should be woven in (production
  checklists, sizing, monitoring recommendations), still grounded in what the
  code actually does.

## Existing assets (extend or replace deliberately, not accidentally)

- Nextra 2.13.4 (pages router) + theme-docs, dark default.
- Components: `ConfigCode` (path breadcrumb + focus dimming), `ConfigTabs`
  (yaml/ts synced tabs), `AISection` (collapsible violet AI panel),
  `LLMsTxtLink` (per-page .llms.txt button). Styles in
  `docs/styles/components.css` (`.cv-*` namespace, `data-component` attrs that
  build-llms.mjs relies on).
- build-llms.mjs: per-page .llms.txt + root llms.txt with nav tree + flat list.
