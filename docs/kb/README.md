# eRPC Docs Knowledge Base (KB)

This directory is the **intermediate ground-truth layer** between the eRPC source code
and the public docs (`docs/pages/`). Every public docs page is generated/written FROM
these KB files — never from memory.

- One KB file per subsystem/capability: `NN-slug.md`
- `_census/` holds machine-oriented checklists extracted directly from code
  (every config field, metric, vendor, error code, directive, CLI flag, route).
  The census is the completeness contract: every census item MUST be covered by
  some KB file.

Each KB file has three levels mirroring the public docs design:

- **L1 — Capability (CTO view):** 2–4 sentences, what it is and what outcome it buys.
- **L2 — Mechanics (staff-engineer view):** how it works, flows, interactions, failure modes.
- **L3 — Exhaustive reference (agent view):** every config field with exact defaults,
  behaviors, algorithms, metrics, edge cases — all grounded in `file.go:L123` citations.

Regeneration rule: when source behavior changes, update the KB file first, then the
public page derived from it.
