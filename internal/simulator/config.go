package simulator

import (
	"bytes"
	"fmt"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/internal/policy"
	yaml "gopkg.in/yaml.v3"
)

// SeedYAMLExpanded is what the orchestrator actually boots from on
// first start: the templated SeedYAML with `{SELECTION_POLICY_FUNC}`
// expanded once at process init to the canonical default policy. This
// keeps the backend's runtime path free of placeholder logic — by the
// time bootFromYAML is called, the YAML is already a fully-formed
// eRPC config.
var SeedYAMLExpanded string

func init() {
	SeedYAMLExpanded = expandPolicyPlaceholder(SeedYAML, policy.DefaultPolicySource())
}

// SeedYAML is the simulator's default eRPC config. It mirrors a realistic
// eth-mainnet deployment: one project, one network, eight upstreams across
// six "vendors" plus a fallback. The YAML editor in the UI starts with
// this exact text, and every behavioural lever — multiplexing, failsafe,
// selection policy, retry/hedge/circuit-breaker, allow/ignore methods —
// is just a YAML field the operator edits live.
//
// Endpoint URLs are placeholders. The simulator rewrites every upstream's
// endpoint to its synthetic loopback (`http://<hub>/sim/<id>`) at boot,
// so the eRPC HTTP transport is exercised end-to-end while the operator's
// synthetic knobs (latency, errors, throttle, block-lag) drive the
// upstream's behaviour.
const SeedYAML = `# eRPC Traffic Simulator — base configuration.
# Edit me. Every field is real eRPC config: selectionPolicy.evalFunc runs
# in sobek server-side, failsafe wraps real attempts, multiplexing flows
# through the real handler. Upstream endpoints are auto-rewritten to the
# simulator's synthetic loopback at apply time, so the topology and behaviour
# you see here is what the operator would deploy.
logLevel: warn

projects:
  - id: sim
    networks:
      - architecture: evm
        evm:
          chainId: 1
        # Multiplexing: when true, eRPC dedupes concurrent identical
        # requests so one upstream call serves many clients. Toggle false
        # to see every generated request hit an upstream individually.
        multiplexing: true
        # Network-level failsafe is where retry / timeout / hedge live.
        # Circuit-breaker is per-upstream (see the failsafe block on
        # each upstream below) since it tracks per-upstream health.
        failsafe:
          # IMPORTANT: failsafe rules match TOP-DOWN — the first rule
          # whose matchMethod accepts the request wins, so specific
          # method patterns MUST come BEFORE the wildcard catch-all.
          #
          # ─────────────────────────────────────────────────────────
          # Want to test consensus? Uncomment the block below to make
          # eth_getLogs require 2-of-3 upstreams to agree on the log
          # set. Mismatches trigger disputeBehavior=returnError so the
          # operator can see disputes surface in the lifecycle drawer.
          # Keep specific-method rules BEFORE the wildcard below.
          # ─────────────────────────────────────────────────────────
          # - matchMethod: "eth_getLogs"
          #   consensus:
          #     maxParticipants: 3
          #     agreementThreshold: 2
          #     disputeBehavior: returnError
          #     lowParticipantsBehavior: acceptMostCommonValidResult
          #   retry:
          #     maxAttempts: 2
          #     delay: 0ms
          #   timeout:
          #     duration: 30s
          # Network-level catch-all: retry, timeout, hedge.
          # CB is per-upstream (see failsafe blocks below).
          - matchMethod: "*"
            retry:
              maxAttempts: 3
              delay: 0ms
              backoffFactor: 0.3
              jitter: 200ms
            timeout:
              duration: 30s
            hedge:
              delay: 350ms
              maxCount: 2
              quantile: 0.85
        # selectionPolicy.evalFunc is the JS that decides which upstreams
        # (and in what order) handle each request. We hold its body in a
        # SEPARATE editor (the "Selection Policy" tab), and the frontend
        # substitutes the {SELECTION_POLICY_FUNC} placeholder below with
        # the policy editor's current source before sending this YAML
        # to the backend. The backend also recognizes the placeholder as
        # a safety net (e.g. when an AI tool call sends raw template YAML).
        selectionPolicy:
          evalInterval: 1s
          evalFunc: "{SELECTION_POLICY_FUNC}"
    upstreams:
      - id: alc-eth-1
        endpoint: alchemy://placeholder
        vendorName: alchemy
        tags: [eth, us-east]
        evm: { chainId: 1 }
        failsafe:
          - matchMethod: "*"
            circuitBreaker:
              failureThresholdCount: 7
              failureThresholdCapacity: 10
              halfOpenAfter: 30s
              successThresholdCount: 4
              successThresholdCapacity: 5
      - id: alc-eth-2
        endpoint: alchemy://placeholder
        vendorName: alchemy
        tags: [eth, us-west]
        evm: { chainId: 1 }
        failsafe: &cb
          - matchMethod: "*"
            circuitBreaker:
              failureThresholdCount: 7
              failureThresholdCapacity: 10
              halfOpenAfter: 30s
              successThresholdCount: 4
              successThresholdCapacity: 5
      - id: drpc-eth-1
        endpoint: drpc://placeholder
        vendorName: drpc
        tags: [eth]
        evm: { chainId: 1 }
        failsafe: *cb
      - id: quik-eth-1
        endpoint: https://placeholder/quiknode/eth
        vendorName: quicknode
        tags: [eth, premium]
        evm: { chainId: 1 }
        failsafe: *cb
      - id: chainstack-eth-1
        endpoint: chainstack://placeholder
        vendorName: chainstack
        tags: [eth]
        evm: { chainId: 1 }
        failsafe: *cb
      - id: conduit-eth-1
        endpoint: conduit://placeholder
        vendorName: conduit
        tags: [eth, dedicated]
        evm: { chainId: 1 }
        failsafe: *cb
      - id: self-eth-1
        endpoint: http://internal.placeholder/eth
        vendorName: self-hosted
        tags: [eth, lhr]
        evm: { chainId: 1 }
        failsafe: *cb
      - id: public-eth-1
        endpoint: http://placeholder/public/eth
        vendorName: public
        tags: [eth, tier:fallback]
        evm: { chainId: 1 }
        failsafe: *cb
`

// DecodeConfigYAML parses + legacy-migrates a YAML document into a
// `*common.Config` WITHOUT applying SetDefaults or Validate. The
// orchestrator runs those AFTER rewriting upstream endpoints to the
// loopback hub — vendor scheme URLs (`alchemy://`, `drpc://`, ...) fail
// validation when paired with an explicit `evm.chainId`, so we must
// neutralize them before validation.
func DecodeConfigYAML(data []byte) (*common.Config, error) {
	var cfg common.Config
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("yaml decode: %w", err)
	}
	if common.LegacyTranslateFn != nil {
		warnings, err := common.LegacyTranslateFn(&cfg)
		if err != nil {
			return nil, fmt.Errorf("legacy migration: %w", err)
		}
		if common.LegacyTranslateLogger != nil {
			for _, w := range warnings {
				common.LegacyTranslateLogger(w)
			}
		}
	}
	return &cfg, nil
}

// FinalizeConfig applies SetDefaults + Validate, returning errors with
// pipeline-stage context. Call this AFTER `RewriteEndpoints` so the
// validator sees the simulator's loopback URLs instead of the operator's
// placeholder vendor:// schemes.
func FinalizeConfig(cfg *common.Config) error {
	if err := cfg.SetDefaults(&common.DefaultOptions{}); err != nil {
		return fmt.Errorf("set defaults: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("validate: %w", err)
	}
	return nil
}

// ParseConfigYAML is the legacy single-call shape kept for convenience.
// It runs DecodeConfigYAML → FinalizeConfig without an endpoint rewrite,
// so it'll FAIL on YAML using vendor:// schemes. The orchestrator does
// not use this path; callers running ad-hoc validation should.
func ParseConfigYAML(data []byte) (*common.Config, error) {
	cfg, err := DecodeConfigYAML(data)
	if err != nil {
		return nil, err
	}
	if err := FinalizeConfig(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// RewriteEndpoints overwrites every upstream's `endpoint` with the
// simulator's loopback URL keyed by upstream ID. Called after parsing so
// the eRPC instance the simulator boots routes every request through the
// in-process UpstreamHub — the synthetic knobs at runtime are what
// actually shape the response (latency, errors, throttling, block lag).
//
// Returns the list of (id, vendor, tags) tuples in document order so the
// orchestrator can sync its UpstreamKnobs map.
func RewriteEndpoints(cfg *common.Config, hubAddr string) []UpstreamIdentity {
	var idents []UpstreamIdentity
	for _, p := range cfg.Projects {
		for _, u := range p.Upstreams {
			if u.Id == "" {
				continue
			}
			u.Endpoint = EndpointFor(hubAddr, u.Id)
			// Force vendor to "generic" so eRPC doesn't try to interpret
			// the (rewritten) URL as a specific provider scheme. The
			// operator's intent is captured in `VendorName` which the
			// selection-policy stdlib can still read via `u.vendor`.
			if u.Type == "" {
				u.Type = common.UpstreamTypeEvm
			}
			idents = append(idents, UpstreamIdentity{
				ID:     u.Id,
				Vendor: u.VendorName,
				Tags:   append([]string(nil), u.Tags...),
			})
		}
	}
	return idents
}

// UpstreamIdentity is the lookup key the orchestrator uses to materialize
// (or refresh) a synthetic-knob entry for each upstream defined in YAML.
type UpstreamIdentity struct {
	ID     string
	Vendor string
	Tags   []string
}

// EndpointFor returns the loopback URL an upstream knob set advertises
// to the eRPC config. Path-segmented so a single hub serves N upstreams.
func EndpointFor(hubAddr, id string) string {
	return fmt.Sprintf("http://%s/sim/%s", hubAddr, id)
}
