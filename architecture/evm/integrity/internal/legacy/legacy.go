// Package legacy is a frozen, verbatim copy of package integrity as of
// bc12f2c0 (gfx-v0.0.2), before the shared block decode (only the package
// clause differs). It exists only as a test oracle: the parity tests and
// fuzzers in package integrity run every response through both
// implementations and require identical per-check outcomes and rejecting
// check ID. Production code must not import it, and it must not be "fixed":
// its whole value is that it keeps behaving exactly like the old decoders.
package legacy
