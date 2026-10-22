package simulator

import (
	"fmt"
	"os"

	"github.com/erpc/erpc/internal/policy"
)

// SeedFromFile loads a seed config from disk (operator-provided YAML,
// e.g. mirroring a production topology) and expands the
// {SELECTION_POLICY_FUNC} placeholder the same way the built-in
// presets do. Endpoints still get rewritten to the synthetic loopback
// hub at boot — the file only shapes the topology (ids, vendors, tags,
// networks) and the failsafe/policy config the simulator starts with.
func SeedFromFile(path string) (string, error) {
	b, err := os.ReadFile(path) // #nosec G304 — operator-supplied path by design
	if err != nil {
		return "", fmt.Errorf("simulator: read seed file: %w", err)
	}
	return expandPolicyPlaceholder(string(b), policy.DefaultPolicySource()), nil
}

// SeedYAMLSvmExpanded mirrors SeedYAMLExpanded for the Solana preset:
// the `{SELECTION_POLICY_FUNC}` placeholder expanded once at init.
var SeedYAMLSvmExpanded string

func init() {
	SeedYAMLSvmExpanded = expandPolicyPlaceholder(SeedYAMLSvm, policy.DefaultPolicySource())
}

// SeedYAMLSvm is the simulator's Solana preset (`-preset svm`). Same
// shape as the EVM seed but with an `svm:mainnet-beta` network and
// SVM-typed upstreams. The `arch:svm` tag switches each synthetic
// upstream to 400ms slot advancement (see defaultKnobsFor), so the
// BlockLag knob reads as "slots behind" and the default selection
// policy's blockNumberLagAbove(...) predicate operates on slot lag.
//
// The genesis gate is exercised for real: the hub answers
// getGenesisHash with the actual mainnet-beta genesis, satisfying the
// fail-closed check for `cluster: mainnet-beta`.
const SeedYAMLSvm = `# eRPC Traffic Simulator — Solana (SVM) preset.
# Edit me. Every field is real eRPC config: the network below is a real
# svm:mainnet-beta network, the upstreams are real SVM upstreams whose
# endpoints get rewritten to the simulator's synthetic loopback at apply
# time. Knobs (latency, errors, throttle, slot-lag) shape behaviour live.
logLevel: warn

projects:
  - id: sim
    # Tight tracker window so health-driven ranking changes show up in
    # seconds — see the EVM seed for the production-tuning caveats.
    scoreMetricsWindowSize: 10s
    networks:
      - architecture: svm
        svm:
          cluster: mainnet-beta
          # Default commitment injected when a request omits one; cache
          # finality follows it (confirmed → unfinalized, re-org aware).
          commitment: confirmed
          # Poller cadence: keeps idle/excluded upstreams' slot metrics
          # fresh (getSlot processed+finalized, getHealth,
          # getMaxShredInsertSlot) so exclusion and re-admission both
          # react within a couple of seconds in the simulator.
          statePollerDebounce: 2s
        multiplexing: true
        failsafe:
          # sendTransaction is non-retryable toward the network by the
          # SVM error taxonomy (double-broadcast guard) — no special
          # failsafe rule needed; watch it in the lifecycle drawer.
          - matchMethod: "*"
            retry:
              maxAttempts: 3
              delay: 0ms
              backoffFactor: 0.3
              jitter: 200ms
            timeout:
              duration: 15s
            hedge:
              delay: 300ms
              maxCount: 1
              quantile: 0.85
        selectionPolicy:
          # Simulator-only: tight cadence so eval decisions land within
          # a second of toggling a scenario (production default: 15s).
          evalInterval: 1s
          evalFunc: "{SELECTION_POLICY_FUNC}"
    upstreams:
      # Identity-only fields; behaviour comes from the synthetic knobs.
      # The arch:svm tag switches the hub to 400ms slot advancement.
      - id: helius-sol-1
        endpoint: https://placeholder/helius/sol
        type: svm
        vendorName: helius
        tags: [sol, premium, arch:svm]
        svm: { cluster: mainnet-beta }
      - id: triton-sol-1
        endpoint: https://placeholder/triton/sol
        type: svm
        vendorName: triton
        tags: [sol, dedicated, arch:svm]
        svm: { cluster: mainnet-beta }
      - id: quik-sol-1
        endpoint: https://placeholder/quiknode/sol
        type: svm
        vendorName: quicknode
        tags: [sol, premium, arch:svm]
        svm: { cluster: mainnet-beta }
      - id: self-sol-1
        endpoint: http://internal.placeholder/sol
        type: svm
        vendorName: self-hosted
        tags: [sol, lhr, arch:svm]
        svm: { cluster: mainnet-beta }
      - id: publicnode-sol-1
        endpoint: http://placeholder/public/sol
        type: svm
        vendorName: public
        tags: [sol, tier:fallback, arch:svm]
        svm: { cluster: mainnet-beta }
      - id: labs-sol-1
        endpoint: http://placeholder/labs/sol
        type: svm
        vendorName: public
        tags: [sol, tier:fallback, arch:svm]
        svm: { cluster: mainnet-beta }
`
