# Content rebalance contract (v2) — binding for every page

User feedback driving this pass (verbatim intent): the visible content must SELL —
promise/marketing level, exciting, minimal tech; the agent panel is where ALL
implementation lives, and it must get BIGGER: more details, more examples, more
references, best practices. "Human-level content is to sell the feature and make the
user excited; agent-level is the place for actual implementation."

## Visible body (what renders expanded) — SHRINK HARD

1. **Length:** 40–90 words of prose total before the agent panel. One H1, no H2 sections
   in the visible body except `## Quick taste` (optional).
2. **Tone:** promise level. What pain disappears, what the user gets. Plain words.
   A CTO with 20 seconds should *want* this feature.
3. **Tech vocabulary:** at most 2-3 technical terms, only if they carry the promise
   (e.g. "p99", "finalized block"). NO config field names, NO defaults, NO algorithm
   narration, NO metric names in the visible body.
4. **Snippet:** at most ONE ConfigTabs, 3–6 lines, the *simplest possible* "it's on"
   illustration. Precede it with a line like "illustrative, not a tuned production
   config" (any phrasing). Pages that don't configure anything (reference pages) skip it.
5. **NO `<details>` deep-dives in the visible body.** Former L2 content moves INTO the
   agent panel (see below). The visible body may keep at most a 3–5 bullet "What you
   get" list if it sells (each bullet ≤ 10 words, zero jargon).
6. Use-case pages under /use-cases/ already follow this shape — match their feel.

## Agent panel (`<AISection>`) — EXPAND SUBSTANTIALLY

Keep title pattern `"<Subject> — full agent reference"`. New REQUIRED internal structure
(H3s, in this order; omit a section only when truly inapplicable):

1. `### How it works` — the former L2 mechanics prose, kept and tightened. Algorithms,
   ordering, precedence, scopes, interactions. This MOVES here from the visible body.
2. `### Config schema` — every field: full dotted path, type, exact default (cited),
   valid values, behavior, footguns. Keep existing tables; verify against docs/kb/.
3. `### Worked examples` — NEW, 2–4 examples: realistic YAML+TS snippets for distinct
   scenarios (e.g. "tight latency SLO", "cheap archival workload"), each with 1–2
   sentences on when and why. Pull scenarios from KB edge cases/test behaviors.
4. `### Request/response behavior` — where applicable: directive headers honored,
   response headers set, error codes produced, JSON-RPC bodies before/after.
5. `### Best practices` — NEW, 3–7 bullets of recommendations grounded in KB facts
   (defaults that need overriding in prod, footguns to pre-empt, sizing guidance).
6. `### Edge cases & gotchas` — keep, extend with anything from KB not yet covered.
7. `### Observability` — relevant metric names + labels + when they fire; key spans.
8. `### Source code entry points` — keep; ensure ≥4 GitHub permalinks incl. config
   struct, defaults, primary execution path, and one test encoding a key behavior.
9. `### Related pages` — sibling/interacting features as internal links.

Grounding: every NEW claim must come from the page's docs/kb/NN-*.md file(s). When the
KB lacks something you want to add, omit it — do not improvise.

## Mechanical rules

- MDX safety: `<details>`/`<summary>` (if any remain inside AISection for grouping) on
  separate lines; no unescaped `${` inside `={\`...\`}` template props; escape raw `<`/`>`
  in prose.
- Internal links must use the restored URL scheme: /config/projects/*, /config/failsafe/*,
  /config/database/*, /config/auth, /config/rate-limiters, /config/matcher,
  /use-cases/*, /reference/* (incl. /reference/evm/*), /operation/*, /deployment/*.
- Components import path: '../'.repeat(directory depth)+'components'.
- Frontmatter description: rewrite to promise level too (it shows in search/OG).
- Exemplar of the target shape: docs/pages/config/failsafe/hedge.mdx (after its rebalance
  commit) and all docs/pages/use-cases/*.mdx for the visible-body feel.

## Sanitization rules (production-mined content)

Worked examples may be inspired by real production configs, but published docs must
NEVER contain anything identifying the operator's infrastructure. Allowed: chain
names, public vendor/provider names, generic thresholds. FORBIDDEN: company names
(e.g. operator org), internal service names, cluster keys, internal hostnames or
k8s service DNS, deployment platform fingerprints (platform-specific env vars or
header names as the *configured value* — generic "e.g." mentions are fine),
customer names, S3 buckets, region-pinned identifiers. Replace with neutral
placeholders (prod-us-east-1, eth-reader, redis.internal, ${REGION}).
