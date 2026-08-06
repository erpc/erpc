// Package integrity validates that an EVM JSON-RPC response is internally
// consistent and consistent with the chain, and converts a provably-wrong
// response into an ErrEndpointContentValidation so the normal retry/consensus
// machinery routes around the offending upstream.
//
// Architecture (one decode, many small checks, one engine):
//
//   - Decoded  — the response result decoded ONCE into lightweight EVM views
//     (header, transactions, receipts, logs). Every check reads from it; no
//     check re-parses the JSON. (decode.go)
//
//   - Check    — a single, self-contained validation unit: an id, the methods
//     it applies to, a family, a failure class, and a Run func over the Decoded
//     view. Checks self-register into the registry. (check.go, checks_*.go)
//
//   - CheckSet — the resolved configuration: which check ids are enabled and
//     with what parameters. Produced by the caller (from a level preset,
//     directives, headers, or a chain profile); the engine stays oblivious to
//     where it came from. (config.go)
//
//   - Validate — the engine: selects the enabled checks applicable to the
//     method, runs them against the single Decoded view, and maps the first
//     violation to a content-validation error. (engine.go)
//
// This package owns the validation logic; the surrounding evm package owns only
// the hook wiring and the mapping from request directives to a CheckSet, so the
// two concerns stay separate and the catalog can grow without touching the hook.
package integrity
