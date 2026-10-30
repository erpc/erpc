package common

import (
	"testing"
	"time"

	"github.com/grafana/sobek"
	"github.com/stretchr/testify/require"
)

func TestCovValidation_Failsafe(t *testing.T) {
	baseline := func() *FailsafeConfig {
		return &FailsafeConfig{
			MatchMethod:      "eth_*",
			MatchRequestKind: "user",
			MatchCommitment:  []string{"none", "processed"},
			Timeout:          &TimeoutPolicyConfig{Duration: &AdaptiveDuration{Base: Duration(time.Second)}},
			Retry:            &RetryPolicyConfig{MaxAttempts: 3, BackoffFactor: 1.5, BackoffMaxDelay: Duration(time.Second)},
			Hedge:            &HedgePolicyConfig{Delay: &AdaptiveDuration{Quantile: 0.9, Max: Duration(time.Second)}, MaxCount: 1},
			CircuitBreaker: &CircuitBreakerPolicyConfig{
				HalfOpenAfter: Duration(time.Minute), FailureThresholdCount: 5, FailureThresholdCapacity: 10,
				SuccessThresholdCount: 2, SuccessThresholdCapacity: 3,
			},
		}
	}
	cvRunCases(t, baseline, (*FailsafeConfig).Validate, []cvCase[FailsafeConfig]{
		{"matchMethod empty", func(f *FailsafeConfig) { f.MatchMethod = "" }, "failsafe.matchMethod cannot be empty, use '*' to match any method"},
		{"matchRequestKind unknown", func(f *FailsafeConfig) { f.MatchRequestKind = "admin" }, "failsafe.matchRequestKind 'admin' is invalid, must be one of: user | internal | *"},
		{"matchRequestKind internal", func(f *FailsafeConfig) { f.MatchRequestKind = "internal" }, ""},

		{"timeout without duration", func(f *FailsafeConfig) { f.Timeout.Duration = nil }, "upstream.*.failsafe.timeout.duration is required"},
		{"timeout quantile out of range", func(f *FailsafeConfig) { f.Timeout.Duration = &AdaptiveDuration{Quantile: 1.5, Base: 1} }, "upstream.*.failsafe.timeout.duration.quantile must be between 0 and 1"},
		{"timeout quantile without base or max", func(f *FailsafeConfig) { f.Timeout.Duration = &AdaptiveDuration{Quantile: 0.5} }, "upstream.*.failsafe.timeout.duration requires base or max when quantile is set"},
		{"timeout empty spec", func(f *FailsafeConfig) { f.Timeout.Duration = &AdaptiveDuration{} }, "upstream.*.failsafe.timeout.duration must specify at least one of base/quantile/min/max"},
		{"timeout min above max", func(f *FailsafeConfig) {
			f.Timeout.Duration = &AdaptiveDuration{Base: 1, Min: Duration(2 * time.Second), Max: Duration(time.Second)}
		}, "upstream.*.failsafe.timeout.duration.min must be <= upstream.*.failsafe.timeout.duration.max"},

		{"retry backoffFactor zero", func(f *FailsafeConfig) { f.Retry.BackoffFactor = 0 }, "upstream.*.failsafe.retry.backoffFactor must be greater than 0"},
		{"retry backoffMaxDelay zero", func(f *FailsafeConfig) { f.Retry.BackoffMaxDelay = 0 }, "upstream.*.failsafe.retry.backoffMaxDelay is required"},

		{"hedge without delay", func(f *FailsafeConfig) { f.Hedge.Delay = nil }, "failsafe.hedge.delay is required"},
		{"hedge zero delay", func(f *FailsafeConfig) { f.Hedge.Delay = &AdaptiveDuration{} }, "failsafe.hedge.delay is required"},
		{"hedge bad quantile", func(f *FailsafeConfig) { f.Hedge.Delay.Quantile = -0.1 }, "failsafe.hedge.delay.quantile must be between 0 and 1"},

		{"cb halfOpenAfter", func(f *FailsafeConfig) { f.CircuitBreaker.HalfOpenAfter = 0 }, "failsafe.circuitBreaker.halfOpenAfter is required"},
		{"cb failure capacity", func(f *FailsafeConfig) { f.CircuitBreaker.FailureThresholdCapacity = 0 }, "failsafe.circuitBreaker.failureThresholdCapacity must be greater than 0"},
		{"cb failure count", func(f *FailsafeConfig) { f.CircuitBreaker.FailureThresholdCount = 0 }, "failsafe.circuitBreaker.failureThresholdCount must be greater than 0"},
		{"cb failure count above capacity", func(f *FailsafeConfig) { f.CircuitBreaker.FailureThresholdCount = 11 }, "failsafe.circuitBreaker.failureThresholdCount must be less than or equal to failureThresholdCapacity"},
		{"cb success count", func(f *FailsafeConfig) { f.CircuitBreaker.SuccessThresholdCount = 0 }, "failsafe.circuitBreaker.successThresholdCount must be greater than 0"},
		{"cb success capacity", func(f *FailsafeConfig) { f.CircuitBreaker.SuccessThresholdCapacity = 0 }, "failsafe.circuitBreaker.successThresholdCapacity must be greater than 0"},
		{"cb success count above capacity", func(f *FailsafeConfig) { f.CircuitBreaker.SuccessThresholdCount = 4 }, "failsafe.circuitBreaker.successThresholdCount must be less than or equal to"},
	})
}

func TestCovValidation_Consensus(t *testing.T) {
	baseline := func() *ConsensusPolicyConfig {
		return &ConsensusPolicyConfig{
			MaxParticipants:    4,
			AgreementThreshold: 2,
			PunishMisbehavior: &PunishMisbehaviorConfig{
				DisputeThreshold: 3, DisputeWindow: Duration(time.Minute), SitOutPenalty: Duration(time.Minute),
			},
			MisbehaviorsDestination: &MisbehaviorsDestinationConfig{Type: MisbehaviorsDestinationTypeFile, Path: "/var/log/erpc"},
			RequiredParticipants:    []*ConsensusRequiredParticipant{{Tag: "archive", MinParticipants: 2, MinAgreement: 1}},
		}
	}
	cvRunCases(t, baseline, (*ConsensusPolicyConfig).Validate, []cvCase[ConsensusPolicyConfig]{
		{"maxParticipants", func(c *ConsensusPolicyConfig) { c.MaxParticipants = 0 }, "consensus.maxParticipants must be greater than 0"},
		{"agreementThreshold", func(c *ConsensusPolicyConfig) { c.AgreementThreshold = 0 }, "consensus.agreementThreshold must be greater than 0"},
		{"threshold above participants", func(c *ConsensusPolicyConfig) { c.AgreementThreshold = 5 }, "consensus.maxParticipants must be greater than or equal to agreementThreshold"},
		{"punish disputeThreshold", func(c *ConsensusPolicyConfig) { c.PunishMisbehavior.DisputeThreshold = 0 }, "consensus.punishMisbehavior.disputeThreshold must be greater than 0"},
		{"punish disputeWindow", func(c *ConsensusPolicyConfig) { c.PunishMisbehavior.DisputeWindow = 0 }, "consensus.punishMisbehavior.disputeWindow must be greater than 0"},
		{"punish sitOutPenalty", func(c *ConsensusPolicyConfig) { c.PunishMisbehavior.SitOutPenalty = 0 }, "consensus.punishMisbehavior.sitOutPenalty must be greater than 0"},
		{"destination invalid", func(c *ConsensusPolicyConfig) { c.MisbehaviorsDestination.Path = "relative/dir" }, "consensus.misbehaviorsDestination is invalid: consensus.misbehaviorsDestination.path must be an absolute path"},
		{"required participant nil", func(c *ConsensusPolicyConfig) { c.RequiredParticipants = append(c.RequiredParticipants, nil) }, "consensus.requiredParticipants[1] must not be null"},
		{"required participant tag", func(c *ConsensusPolicyConfig) { c.RequiredParticipants[0].Tag = " " }, "consensus.requiredParticipants[0].tag is required"},
		{"required participant min", func(c *ConsensusPolicyConfig) { c.RequiredParticipants[0].MinParticipants = 0 }, "consensus.requiredParticipants[0].minParticipants must be greater than 0"},
		{"required participant above max", func(c *ConsensusPolicyConfig) { c.RequiredParticipants[0].MinParticipants = 5 }, "consensus.requiredParticipants[0].minParticipants (5) cannot exceed maxParticipants (4)"},
		{"required participant negative agreement", func(c *ConsensusPolicyConfig) { c.RequiredParticipants[0].MinAgreement = -1 }, "consensus.requiredParticipants[0].minAgreement must not be negative"},
		{"required participant agreement above min", func(c *ConsensusPolicyConfig) { c.RequiredParticipants[0].MinAgreement = 3 }, "consensus.requiredParticipants[0].minAgreement (3) cannot exceed minParticipants (2)"},
	})
}

func TestCovValidation_MisbehaviorsDestination(t *testing.T) {
	baseline := func() *MisbehaviorsDestinationConfig {
		return &MisbehaviorsDestinationConfig{
			Type: "S3", Path: "S3://bucket/prefix",
			S3: &S3FlushConfig{
				MaxRecords: 100, MaxSize: 1 << 20, FlushInterval: Duration(time.Minute),
				Endpoint:    "https://s3.example",
				Credentials: &AwsAuthConfig{Mode: "file", CredentialsFile: "/creds", Profile: "default"},
			},
		}
	}
	cvRunCases(t, baseline, (*MisbehaviorsDestinationConfig).Validate, []cvCase[MisbehaviorsDestinationConfig]{
		{"unknown type", func(m *MisbehaviorsDestinationConfig) { m.Type = "kafka" }, "consensus.misbehaviorsDestination.type must be 'file' or 's3'"},
		{"file without path", func(m *MisbehaviorsDestinationConfig) { m.Type, m.Path = "", " " }, "consensus.misbehaviorsDestination.path is required for file destination"},
		{"file relative path", func(m *MisbehaviorsDestinationConfig) { m.Type, m.Path = MisbehaviorsDestinationTypeFile, "logs" }, "consensus.misbehaviorsDestination.path must be an absolute path for file destination"},
		{"empty type defaults to file", func(m *MisbehaviorsDestinationConfig) { m.Type, m.Path = "", "/tmp/x" }, ""},
		{"s3 without path", func(m *MisbehaviorsDestinationConfig) { m.Path = "" }, "consensus.misbehaviorsDestination.path is required for s3 destination"},
		{"s3 non-s3 path", func(m *MisbehaviorsDestinationConfig) { m.Path = "/local" }, "consensus.misbehaviorsDestination.path must start with s3:// for s3 destination"},
		{"s3 without s3 block", func(m *MisbehaviorsDestinationConfig) { m.S3 = nil }, "consensus.misbehaviorsDestination.s3 is required when type is 's3'"},
		{"s3 negative maxRecords", func(m *MisbehaviorsDestinationConfig) { m.S3.MaxRecords = -1 }, "consensus.misbehaviorsDestination.s3 is invalid: s3.maxRecords must be >= 0"},
		{"s3 negative maxSize", func(m *MisbehaviorsDestinationConfig) { m.S3.MaxSize = -1 }, "s3.maxSize must be >= 0"},
		{"s3 negative flushInterval", func(m *MisbehaviorsDestinationConfig) { m.S3.FlushInterval = -1 }, "s3.flushInterval must be >= 0"},
		{"s3 endpoint bad scheme", func(m *MisbehaviorsDestinationConfig) { m.S3.Endpoint = "ftp://x" }, `s3.endpoint must be a valid http(s) URL, got "ftp://x"`},
		{"s3 endpoint without host", func(m *MisbehaviorsDestinationConfig) { m.S3.Endpoint = "https://" }, `s3.endpoint must be a valid http(s) URL, got "https://"`},
		{"s3 file creds without file", func(m *MisbehaviorsDestinationConfig) { m.S3.Credentials.CredentialsFile = "" }, "s3.credentials.credentialsFile is required when mode is 'file'"},
		{"s3 file creds without profile", func(m *MisbehaviorsDestinationConfig) { m.S3.Credentials.Profile = "" }, "s3.credentials.profile is required when mode is 'file'"},
		{"s3 secret creds incomplete", func(m *MisbehaviorsDestinationConfig) {
			m.S3.Credentials = &AwsAuthConfig{Mode: "secret", AccessKeyID: "AK"}
		}, "s3.credentials.accessKeyID and secretAccessKey are required when mode is 'secret'"},
		{"s3 secret creds complete", func(m *MisbehaviorsDestinationConfig) {
			m.S3.Credentials = &AwsAuthConfig{Mode: "Secret", AccessKeyID: "AK", SecretAccessKey: "SK"}
		}, ""},
		{"s3 env creds", func(m *MisbehaviorsDestinationConfig) { m.S3.Credentials = &AwsAuthConfig{Mode: "env"} }, ""},
		{"s3 default chain creds", func(m *MisbehaviorsDestinationConfig) { m.S3.Credentials = &AwsAuthConfig{} }, ""},
		{"s3 unknown creds mode", func(m *MisbehaviorsDestinationConfig) { m.S3.Credentials = &AwsAuthConfig{Mode: "imds"} }, "s3.credentials.mode must be one of: env, file, secret"},
	})
}

func cvValidSelectionPolicy(t *testing.T) *SelectionPolicyConfig {
	t.Helper()
	src := "(upstreams) => upstreams"
	prog, err := sobek.Compile("", src, false)
	require.NoError(t, err)
	return &SelectionPolicyConfig{
		EvalInterval:    Duration(time.Minute),
		EvalTimeout:     Duration(time.Second),
		EvalFunc:        src,
		CompiledProgram: prog,
	}
}

func TestCovValidation_SelectionPolicy(t *testing.T) {
	baseline := func() *SelectionPolicyConfig { return cvValidSelectionPolicy(t) }
	cvRunCases(t, baseline, (*SelectionPolicyConfig).Validate, []cvCase[SelectionPolicyConfig]{
		{"evalInterval zero", func(s *SelectionPolicyConfig) { s.EvalInterval = 0 }, "selectionPolicy.evalInterval must be greater than 0"},
		{"evalTimeout zero", func(s *SelectionPolicyConfig) { s.EvalTimeout = 0 }, "selectionPolicy.evalTimeout must be greater than 0"},
		{"evalTimeout not below interval", func(s *SelectionPolicyConfig) { s.EvalTimeout = s.EvalInterval }, "selectionPolicy.evalTimeout (1m0s) must be less than evalInterval (1m0s)"},
		{"evalFunc missing", func(s *SelectionPolicyConfig) { s.EvalFunc = "" }, "selectionPolicy.evalFunc is required"},
		{"not compiled", func(s *SelectionPolicyConfig) { s.CompiledProgram = nil }, "selectionPolicy.evalFunc failed to compile (CompiledProgram is nil)"},
		{"ts sentinel needs no program", func(s *SelectionPolicyConfig) {
			s.EvalFunc, s.CompiledProgram = tsFunctionSentinelPrefix+"7", nil
		}, ""},
	})
}

func TestCovValidation_Network(t *testing.T) {
	cfg := cvConfigWithBudgets()
	baseline := func() *NetworkConfig {
		n := cvValidEvmNetwork(1)
		n.Failsafe = []*FailsafeConfig{{MatchMethod: "*"}}
		n.SelectionPolicy = cvValidSelectionPolicy(t)
		n.RateLimitBudget = "plain"
		n.Alias = "eth-main_1"
		n.CacheKeySuffix = "v1"
		n.StaticResponses = []*StaticResponseConfig{{Method: "web3_clientVersion", Response: &StaticResponseBodyConfig{Result: "erpc"}}}
		n.Integrity = &IntegrityConfig{HeaderMode: "off"}
		return n
	}
	validate := func(n *NetworkConfig) error { return n.Validate(cfg) }
	cvRunCases(t, baseline, validate, []cvCase[NetworkConfig]{
		{"architecture missing", func(n *NetworkConfig) { n.Architecture = "" }, "network.*.architecture is required"},
		{"evm block missing", func(n *NetworkConfig) { n.Evm = nil }, "network.*.evm is required for evm networks"},
		{"evm block invalid", func(n *NetworkConfig) { n.Evm.FallbackFinalityDepth = 0 }, "network.*.evm.fallbackFinalityDepth must be greater than 0"},
		{"failsafe invalid", func(n *NetworkConfig) { n.Failsafe[0].Retry = &RetryPolicyConfig{} }, "upstream.*.failsafe.retry.backoffFactor must be greater than 0"},
		{"consensus allowed at network level", func(n *NetworkConfig) {
			n.Failsafe[0].Consensus = &ConsensusPolicyConfig{MaxParticipants: 2, AgreementThreshold: 2}
		}, ""},
		{"selection policy invalid", func(n *NetworkConfig) { n.SelectionPolicy.EvalFunc = "" }, "selectionPolicy.evalFunc is required"},
		{"unknown budget", func(n *NetworkConfig) { n.RateLimitBudget = "nope" }, "network.*.rateLimitBudget 'nope' does not exist in config.rateLimiters"},
		{"bad alias", func(n *NetworkConfig) { n.Alias = "eth main" }, "network.*.alias 'eth main' must contain only alphanumeric characters, dash, or underscore"},
		{"bad cacheKeySuffix", func(n *NetworkConfig) { n.CacheKeySuffix = "a/b" }, "network.*.cacheKeySuffix 'a/b' must contain only alphanumeric characters, dash, or underscore"},
		{"nil static response", func(n *NetworkConfig) { n.StaticResponses = append(n.StaticResponses, nil) }, "network.*.staticResponses[1]: entry is nil"},
		{"static response without method", func(n *NetworkConfig) { n.StaticResponses[0].Method = "" }, "network.*.staticResponses[0]: method is required"},
		{"static response without response", func(n *NetworkConfig) { n.StaticResponses[0].Response = nil }, "network.*.staticResponses[0]: response is required"},
		{"static response with both", func(n *NetworkConfig) {
			n.StaticResponses[0].Response.Error = &StaticResponseErrorConfig{Code: -32000, Message: "x"}
		}, "response must set exactly one of result or error, got both"},
		{"static response with neither", func(n *NetworkConfig) { n.StaticResponses[0].Response.Result = nil }, "response must set exactly one of result or error, got neither"},
		{"static error without message", func(n *NetworkConfig) {
			n.StaticResponses[0].Response = &StaticResponseBodyConfig{Error: &StaticResponseErrorConfig{Code: -32000}}
		}, "network.*.staticResponses[0]: response.error.message is required"},
		{"static error with message", func(n *NetworkConfig) {
			n.StaticResponses[0].Response = &StaticResponseBodyConfig{Error: &StaticResponseErrorConfig{Code: -32601, Message: "disabled"}}
		}, ""},
		{"integrity invalid", func(n *NetworkConfig) { n.Integrity.HeaderMode = "verbose" }, "network.*: integrity.headerMode 'verbose' is invalid"},
		{"generic block on evm network", func(n *NetworkConfig) { n.Generic = &GenericNetworkConfig{Slug: "starknet"} }, "network.*.generic is only valid with architecture: jsonrpc"},
		{"jsonrpc without slug", func(n *NetworkConfig) {
			n.Architecture, n.Evm, n.Integrity = ArchitectureJsonRpc, nil, nil
		}, "network.*.generic.slug is required for jsonrpc networks"},
		{"jsonrpc bad slug", func(n *NetworkConfig) {
			n.Architecture, n.Evm, n.Integrity = ArchitectureJsonRpc, nil, nil
			n.Generic = &GenericNetworkConfig{Slug: "star:net"}
		}, "network.*.generic.slug 'star:net' is invalid"},
		{"jsonrpc bad replaySafeMethods pattern", func(n *NetworkConfig) {
			n.Architecture, n.Evm, n.Integrity = ArchitectureJsonRpc, nil, nil
			n.Generic = &GenericNetworkConfig{Slug: "starknet", ReplaySafeMethods: []string{"(starknet_get*"}}
		}, "network.*.generic.replaySafeMethods pattern '(starknet_get*' is invalid"},
		{"jsonrpc rejects evm block", func(n *NetworkConfig) {
			n.Architecture, n.Integrity = ArchitectureJsonRpc, nil
			n.Generic = &GenericNetworkConfig{Slug: "starknet"}
		}, "network.*.evm is not supported on jsonrpc networks (no block tracking or finality)"},
		{"jsonrpc rejects svm block", func(n *NetworkConfig) {
			n.Architecture, n.Evm, n.Integrity, n.Svm = ArchitectureJsonRpc, nil, nil, &SvmNetworkConfig{}
			n.Generic = &GenericNetworkConfig{Slug: "starknet"}
		}, "network.*.svm is not supported on jsonrpc networks (no block tracking or finality)"},
		{"jsonrpc rejects integrity block", func(n *NetworkConfig) {
			n.Architecture, n.Evm = ArchitectureJsonRpc, nil
			n.Generic = &GenericNetworkConfig{Slug: "starknet"}
		}, "network.*.integrity is not supported on jsonrpc networks (no block tracking or finality)"},
		{"jsonrpc rejects consensus", func(n *NetworkConfig) {
			n.Architecture, n.Evm, n.Integrity = ArchitectureJsonRpc, nil, nil
			n.Generic = &GenericNetworkConfig{Slug: "starknet"}
			n.Failsafe[0].Consensus = &ConsensusPolicyConfig{MaxParticipants: 2, AgreementThreshold: 2}
		}, "network.*.failsafe.consensus is not supported on jsonrpc networks (no block tracking or finality)"},
		{"jsonrpc valid", func(n *NetworkConfig) {
			n.Architecture, n.Evm, n.Integrity = ArchitectureJsonRpc, nil, nil
			n.Generic = &GenericNetworkConfig{Slug: "starknet", ReplaySafeMethods: []string{"starknet_get*"}}
		}, ""},
	})
}

func TestCovValidation_EvmNetwork(t *testing.T) {
	baseline := func() *EvmNetworkConfig {
		e := cvValidEvmNetwork(1).Evm
		e.LocalAnswers = map[string]bool{LocalAnswerMethods()[0]: false}
		e.SafeBlockSource = "tag:archive"
		return e
	}
	cvRunCases(t, baseline, (*EvmNetworkConfig).Validate, []cvCase[EvmNetworkConfig]{
		{"fallbackFinalityDepth", func(e *EvmNetworkConfig) { e.FallbackFinalityDepth = 0 }, "network.*.evm.fallbackFinalityDepth must be greater than 0"},
		{"fallbackStatePollerDebounce", func(e *EvmNetworkConfig) { e.FallbackStatePollerDebounce = 0 }, "network.*.evm.fallbackStatePollerDebounce is required"},
		{"getLogsMaxAllowedRange", func(e *EvmNetworkConfig) { e.GetLogsMaxAllowedRange = 0 }, "network.*.evm.getLogsMaxAllowedRange must be greater than 0"},
		{"unknown local answer", func(e *EvmNetworkConfig) { e.LocalAnswers["eth_getBalance"] = true }, "network.*.evm.localAnswers.eth_getBalance: eRPC has no local answer for this method"},
		{"bad safeBlockSource", func(e *EvmNetworkConfig) { e.SafeBlockSource = "(tag:archive" }, `network.*.evm.safeBlockSource has invalid selector "(tag:archive"`},
	})
}
