# erpc — Claude Code Guide

See [.cursor/rules/](.cursor/rules/) for all project rules and conventions.

## Design razor: weakest hypothesis, not shortest

Binding for all design and review work in this repo. From Bennett, AGI-23
([arXiv:2301.12987](https://arxiv.org/abs/2301.12987)); full eRPC-tailored
version in [.cursor/rules/erpc.md](.cursor/rules/erpc.md).

Among designs that EXACTLY handle every observed case, prefer the one
committing to the least beyond the data — "explanations should be no more
specific than necessary". Generality lives in a design's extension (the unseen
inputs it still handles correctly), not its form: short/tidy is provably
neither necessary nor sufficient, and a compact regex or clean enum can be
maximally overcommitted.

eRPC's domain is open-ended sets (chains, vendors, methods, error shapes,
client quirks), so:

- The unknown-case fallthrough is the primary path — design and test it first;
  enumerated cases (method tables, error matchers, chain special-cases) are
  optimisations on top and only acceptable when the unmatched path is safe,
  correct, and observable.
- No unforced commitments: no hard-coded method lists, vendor error-string
  matches, chain-ID special cases, or "all vendors we checked do X" thresholds
  unless today's observed data forces them.
- Weaken by DELETING structure (string + discovery over enum, kind + metadata
  over parallel structs, pass-through over interpretation, config over code
  constants) — never by adding speculative abstraction; unexercised machinery
  is itself a commitment.
- Weak ≠ vague: still decide every observed case exactly, and resolve open
  inputs into bounded low-cardinality interfaces (normalized error codes,
  finite behaviours).
- Wire/protocol facts are explicit validated commitments at the edge; measured
  behaviour of N chains/vendors is NOT an invariant — when reality violates a
  bound, delete the bound rather than stacking exceptions.
- Review test: what unseen-but-plausible input does this silently mishandle,
  and what in today's data forces that commitment? If nothing forces it,
  weaken the design.
