// CAPITAL_SNAKE_CASE aliases for selection-policy `evalScope` and
// `stickyPrimary({scope})`. These are the canonical names a TypeScript
// config imports:
//
//   import { NETWORK, NETWORK_FINALITY } from '@erpc/config'
//
// The string values are the kebab-case forms the Go side validates
// against (see `common.EvalScope`). At policy-runtime load time, the
// stdlib also installs these names as ambient globals in the sobek
// runtime so they resolve identically inside an `evalFunc` body
// without an explicit import — see `internal/policy/stdlib/install.go`.

import {
  EvalScopeNetwork,
  EvalScopeNetworkMethod,
  EvalScopeNetworkFinality,
  EvalScopeNetworkMethodFinality,
} from "./generated";

/** One primary per network — max cohesion across methods + finalities. Default. */
export const NETWORK = EvalScopeNetwork;
/** One primary per `(network, method)`; finalities share. */
export const NETWORK_METHOD = EvalScopeNetworkMethod;
/** One primary per `(network, finality)`; methods share. */
export const NETWORK_FINALITY = EvalScopeNetworkFinality;
/** One primary per slot — independent per `(network, method, finality)`. */
export const NETWORK_METHOD_FINALITY = EvalScopeNetworkMethodFinality;

// Finality bit-flag constants — used INSIDE an `evalFunc` body with
// the chainable `when(mask, fn)` primitive. Compose with bitwise OR:
//
//   .when(REALTIME | UNFINALIZED | UNKNOWN, u => u.stickyPrimary({...}))
//
// These are also installed as ambient globals at policy-runtime load
// time, so an `evalFunc` body doesn't need to import them. The
// importable versions exist for symmetry — and so a TypeScript config
// that wants to compute a mask AT CONFIG-LOAD TIME (e.g. into a
// derived constant outside the eval body) can do so without relying
// on the runtime ambient.
/** Bit 0 — request is reading live tip data (e.g. eth_blockNumber). */
export const REALTIME = 1 << 0;
/** Bit 1 — request is reading a recent-but-not-yet-finalized block. */
export const UNFINALIZED = 1 << 1;
/** Bit 2 — request is reading a finalized (no-reorg) block. */
export const FINALIZED = 1 << 2;
/** Bit 3 — finality couldn't be determined for this request. */
export const UNKNOWN = 1 << 3;
