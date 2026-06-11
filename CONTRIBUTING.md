# Contributing to eRPC

Thank you for considering contributing to eRPC! By contributing, you help improve the project for everyone.

## How Contributions May Be Used

Please be aware that by contributing to this project, you acknowledge that your contributions (including code, documentation, and other materials) may be incorporated into both the open-source and enterprise versions of the software. This allows us to provide enhanced features and support to enterprise users while maintaining the open-source project.

## How to Contribute

1. Fork the repository.
2. Create a new branch (`git checkout -b feature/YourFeature`).
3. Make your changes.
4. Commit your changes (`git commit -m 'Add some feature'`).
5. Push to the branch (`git push origin feature/YourFeature`).
6. Open a Pull Request.

## Documentation

The docs (`docs/pages/`, published at <https://docs.erpc.cloud>) are written
**agent-first, human-second**. Every page has exactly two layers:

1. **Visible body (humans, short attention span).** A 40–90 word promise-level
   pitch — what pain disappears, what the user gets — plus at most one
   "Quick taste" config snippet. The snippet must show the **full parent chain**
   from the config root (`projects:` …), put explanatory comments **above** the
   line they describe, and highlight only the teaching lines via
   `focusYaml`/`focusTs` line ranges. No field tables, no defaults, no algorithm
   narration in the visible body.
2. **Agent panel (`<AISection>`, collapsible).** The exhaustive reference an AI
   agent needs to actually configure the feature, structured as: How it works,
   Config schema (every field with exact defaults cited to source), Worked
   examples (realistic scenarios with WHY comments), Request/response behavior,
   Best practices, Edge cases & gotchas, Observability (exact metric
   names/labels), Source code entry points (GitHub permalinks incl. one test),
   Related pages.

Between the two layers sits an `## Agent reference` heading with copy-paste
`<PromptExample>` blocks — goal-phrased prompts that point an agent at the
page's machine-readable companion (`https://docs.erpc.cloud/<path>.llms.txt`).
Prompts must stay config-format agnostic ("my eRPC config", never a specific
filename).

Conventions that keep the system working:

- **Ground every claim in code.** Defaults come from `common/defaults.go` /
  `SetDefaults` methods, behaviors from the executing code and its tests —
  cite `file.go:L123` as GitHub permalinks. Never write a default from memory.
- **Exemplar page:** `docs/pages/config/failsafe/hedge.mdx` — match its shape
  when adding a page.
- **Examples must never identify any operator's infrastructure.** Chain names
  and public vendor names are fine; company names, internal service names,
  cluster keys, internal hostnames/k8s DNS, and platform fingerprints are not —
  use neutral placeholders (`prod-us-east-1`, `eth-reader`, `${REGION}`).
- **llms.txt is generated** — `docs/scripts/build-llms.mjs` runs on every
  build, emitting a `.llms.txt` companion per page (agent panels fully
  expanded, navigation links up/down/sideways), the root `llms.txt`, and
  `llms-full.txt`. Don't edit generated files; new pages just need a
  `_meta.js` entry.
- **Agents are served markdown automatically** — `docs/middleware.ts`
  302-redirects requests whose `Accept` header lists `text/markdown` (Claude
  Code sends this on every fetch) or whose `User-Agent` matches a known AI
  fetcher to the page's `.llms.txt` twin. If you add a non-page route, make
  sure the middleware's `matcher` excludes it; new AI fetchers belong in the
  `AI_FETCHER_UA` regex.
- **Build:** `cd docs && pnpm install && pnpm build` (standalone pnpm
  workspace; `next dev` for live preview).

When you change runtime behavior — a new config field, a changed default, a
new edge case or metric — the same PR must update the relevant page's agent
panel (schema table, edge cases, observability) and, only if the *promise*
changed, the visible body.

## Contributor License Agreement (CLA)

By submitting a pull request, you agree to the [CLA](./CLA.md).

## Code of Conduct

Please adhere to our [Code of Conduct](./CODE_OF_CONDUCT.md) in all your interactions with the project.
