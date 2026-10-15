package consensus

import (
	"context"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/failsafe"
	policyengine "github.com/erpc/erpc/internal/consensus/policy"
	"github.com/rs/zerolog"
)

// evalForWiring exercises every branch resolvePolicy must handle: a named
// pick gated on a verified role, an explicit null (use default), an unknown
// name (fail closed), and a throw (fail closed).
const evalForWiring = `(ctx) => {
	if (ctx.request.method === "boom") throw new Error("kaboom")
	if (ctx.request.method === "nul") return null
	if (ctx.request.method === "bogus") return "not-a-configured-policy"
	if (ctx.user?.auth === "admin") return "historical"
	return "standard"
}`

func TestCloseAll_StopsWhenDeadlineExpires(t *testing.T) {
	inflightAnalyzers.Add(1)
	t.Cleanup(func() { inflightAnalyzers.Done() })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	CloseAll(ctx)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("CloseAll blocked for %s after the deadline", elapsed)
	}
}

func newWiringConsensus(t *testing.T) *Consensus {
	t.Helper()
	lg := zerolog.Nop()
	cfg := &common.ConsensusPolicyConfig{
		MaxParticipants: 3,
		CustomPolicy:    &common.ConsensusCustomPolicyConfig{EvalFunction: evalForWiring},
		Policies: map[string]*common.ConsensusPolicyConfig{
			"historical": {MaxParticipants: 5},
			"standard":   {MaxParticipants: 3},
		},
	}
	c, err := NewConsensus(cfg, &lg)
	if err != nil {
		t.Fatalf("NewConsensus: %v", err)
	}
	if c.selector == nil {
		t.Fatal("expected selector to be compiled")
	}
	return c
}

func wiringRequest(t *testing.T, method string, auth string) *common.NormalizedRequest {
	t.Helper()
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"` + method + `"}`))
	if auth != "" {
		req.SetUser(&common.User{Auth: auth})
	}
	return req
}

func TestResolvePolicy_NamedPickOnAuth(t *testing.T) {
	c := newWiringConsensus(t)
	req := wiringRequest(t, "eth_getBalance", "admin")

	got := c.resolvePolicy(req)
	if got != c.named["historical"] {
		t.Fatalf("expected historical policy, got %p (default=%p)", got, c.policy)
	}
	if h := req.ExecState().ConsensusPolicy(); h != "historical" {
		t.Fatalf("header policy = %q, want historical", h)
	}
}

func TestResolvePolicy_DefaultPickWithoutRole(t *testing.T) {
	c := newWiringConsensus(t)
	req := wiringRequest(t, "eth_getBalance", "")

	got := c.resolvePolicy(req)
	if got != c.named["standard"] {
		t.Fatalf("expected standard policy, got %p", got)
	}
	if h := req.ExecState().ConsensusPolicy(); h != "standard" {
		t.Fatalf("header policy = %q, want standard", h)
	}
}

func TestResolvePolicy_NullFailsToDefault(t *testing.T) {
	c := newWiringConsensus(t)
	req := wiringRequest(t, "nul", "")

	got := c.resolvePolicy(req)
	if got != c.policy {
		t.Fatalf("null return should resolve to inline default policy")
	}
	if h := req.ExecState().ConsensusPolicy(); h != defaultPolicyName {
		t.Fatalf("header policy = %q, want %q", h, defaultPolicyName)
	}
}

func TestResolvePolicy_UnknownNameFailsClosed(t *testing.T) {
	c := newWiringConsensus(t)
	req := wiringRequest(t, "bogus", "")

	got := c.resolvePolicy(req)
	if got != c.policy {
		t.Fatalf("unknown name should fail closed to inline default policy")
	}
	if h := req.ExecState().ConsensusPolicy(); h != defaultPolicyName {
		t.Fatalf("header policy = %q, want %q", h, defaultPolicyName)
	}
}

func TestResolvePolicy_ThrowFailsClosed(t *testing.T) {
	c := newWiringConsensus(t)
	req := wiringRequest(t, "boom", "")

	got := c.resolvePolicy(req)
	if got != c.policy {
		t.Fatalf("a thrown eval should fail closed to inline default policy")
	}
	if h := req.ExecState().ConsensusPolicy(); h != defaultPolicyName {
		t.Fatalf("header policy = %q, want %q", h, defaultPolicyName)
	}
}

// A runaway eval that overruns evalTimeout must be interrupted and fail closed
// to the inline default, never hang the request path.
func TestResolvePolicy_TimeoutFailsClosed(t *testing.T) {
	lg := zerolog.Nop()
	cfg := &common.ConsensusPolicyConfig{
		MaxParticipants: 3,
		CustomPolicy: &common.ConsensusCustomPolicyConfig{
			EvalFunction: `(ctx) => { while (true) {} }`,
			EvalTimeout:  common.Duration(time.Millisecond),
		},
		Policies: map[string]*common.ConsensusPolicyConfig{"standard": {MaxParticipants: 3}},
	}
	c, err := NewConsensus(cfg, &lg)
	if err != nil {
		t.Fatalf("NewConsensus: %v", err)
	}
	req := wiringRequest(t, "eth_getBalance", "")

	done := make(chan *consensusPolicy, 1)
	go func() { done <- c.resolvePolicy(req) }()
	select {
	case got := <-done:
		if got != c.policy {
			t.Fatalf("timeout should fail closed to inline default policy")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("resolvePolicy hung on a runaway eval; timeout not enforced")
	}
	if h := req.ExecState().ConsensusPolicy(); h != defaultPolicyName {
		t.Fatalf("header policy = %q, want %q", h, defaultPolicyName)
	}
}

// A config with no customPolicy must leave the selector nil so Run stays on
// the zero-overhead inline-default path.
func TestNewConsensus_NoSelectorWhenCustomPolicyUnset(t *testing.T) {
	lg := zerolog.Nop()
	c, err := NewConsensus(&common.ConsensusPolicyConfig{MaxParticipants: 3}, &lg)
	if err != nil {
		t.Fatalf("NewConsensus: %v", err)
	}
	if c.selector != nil {
		t.Fatal("selector should be nil when customPolicy is unset")
	}
}

// Omitted evalTimeout (0) must resolve to ConsensusDefaultEvalTimeout inside
// NewConsensus so callers that skip SetDefaults still get a wall-clock cap.
func TestNewConsensus_ZeroEvalTimeoutUsesDefault(t *testing.T) {
	lg := zerolog.Nop()
	cfg := &common.ConsensusPolicyConfig{
		MaxParticipants: 3,
		CustomPolicy: &common.ConsensusCustomPolicyConfig{
			EvalFunction: `(ctx) => { while (true) {} }`,
			// EvalTimeout deliberately omitted (0).
		},
	}
	c, err := NewConsensus(cfg, &lg)
	if err != nil {
		t.Fatalf("NewConsensus: %v", err)
	}
	if c.evalTimeout != common.ConsensusDefaultEvalTimeout {
		t.Fatalf("evalTimeout = %v, want %v", c.evalTimeout, common.ConsensusDefaultEvalTimeout)
	}
	req := wiringRequest(t, "eth_getBalance", "")
	done := make(chan *consensusPolicy, 1)
	go func() { done <- c.resolvePolicy(req) }()
	select {
	case got := <-done:
		if got != c.policy {
			t.Fatalf("zero-timeout default should still fail closed to inline default")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("resolvePolicy hung; zero EvalTimeout was not resolved to the default cap")
	}
}

// Named policies must share the inline PunishMisbehavior pointer so the
// shared limiter cannot first-touch-win on a divergent copy.
func TestNewConsensus_SharesPunishMisbehaviorPointer(t *testing.T) {
	lg := zerolog.Nop()
	punish := &common.PunishMisbehaviorConfig{
		DisputeThreshold: 3,
		DisputeWindow:    common.Duration(time.Minute),
		SitOutPenalty:    common.Duration(time.Minute),
	}
	namedPunish := punish.Copy()
	cfg := &common.ConsensusPolicyConfig{
		MaxParticipants:   3,
		PunishMisbehavior: punish,
		Policies: map[string]*common.ConsensusPolicyConfig{
			"fallback": {
				MaxParticipants:   3,
				PunishMisbehavior: namedPunish,
			},
		},
	}
	c, err := NewConsensus(cfg, &lg)
	if err != nil {
		t.Fatalf("NewConsensus: %v", err)
	}
	if c.policy.punishMisbehavior != punish {
		t.Fatal("inline policy should keep the shared PunishMisbehavior pointer")
	}
	named := c.named["fallback"]
	if named == nil {
		t.Fatal("named policy missing")
	}
	if named.punishMisbehavior != punish {
		t.Fatal("named policy must share the inline PunishMisbehavior pointer, not its own copy")
	}
}

func TestConsensus_CloseUnregistersAndIdempotent(t *testing.T) {
	lg := zerolog.Nop()
	c, err := NewConsensus(&common.ConsensusPolicyConfig{MaxParticipants: 3}, &lg)
	if err != nil {
		t.Fatalf("NewConsensus: %v", err)
	}
	if _, ok := liveConsensuses.Load(c); !ok {
		t.Fatal("NewConsensus must register for CloseAll")
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, ok := liveConsensuses.Load(c); ok {
		t.Fatal("Close must unregister")
	}
	if err := c.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// blockNumberPtr must accept the untyped shapes EvmBlockNumber can carry and
// never error — an unparseable value is the "unknown block" case (nil).
func TestBlockNumberPtr(t *testing.T) {
	cases := []struct {
		in   interface{}
		want *int64
	}{
		{nil, nil},
		{int64(42), i64ptr(42)},
		{int(7), i64ptr(7)},
		{float64(100), i64ptr(100)},
		{"0x10", i64ptr(16)},
		{"256", i64ptr(256)},
		{"", nil},
		{"notanumber", nil},
	}
	for _, tc := range cases {
		got := blockNumberPtr(tc.in)
		switch {
		case tc.want == nil && got != nil:
			t.Errorf("blockNumberPtr(%v) = %d, want nil", tc.in, *got)
		case tc.want != nil && got == nil:
			t.Errorf("blockNumberPtr(%v) = nil, want %d", tc.in, *tc.want)
		case tc.want != nil && got != nil && *got != *tc.want:
			t.Errorf("blockNumberPtr(%v) = %d, want %d", tc.in, *got, *tc.want)
		}
	}
}

func i64ptr(n int64) *int64 { return &n }

// classTracker returns fixed cordon classes for every CordonClasses query.
// FakeHealthTracker.CordonClasses is a no-op; this stub is what the bridge
// needs to prove punished nodes land on PunishedOrOperator.
type classTracker struct {
	common.FakeHealthTracker
	classes []common.CordonClass
}

func (t *classTracker) CordonClasses(upstream common.Upstream, method string) []common.CordonClass {
	return t.classes
}

func TestBuildEvalContext_PunishedOrOperatorFromNetworkList(t *testing.T) {
	c := newWiringConsensus(t)

	punished := common.NewFakeUpstream("internal-blue",
		common.WithTags("type:internal"),
		common.WithHealthTracker(&classTracker{classes: []common.CordonClass{common.CordonClassPunishment}}),
	)
	healthy := common.NewFakeUpstream("alchemy", common.WithTags("type:external"))

	req := wiringRequest(t, "eth_getBalance", "")
	// Selection already dropped the punished node from candidates.
	req.SetUpstreams([]common.Upstream{healthy})
	req.SetNetworkUpstreams([]common.Upstream{punished, healthy})

	ectx := c.buildEvalContext(req)
	// Network list is preferred for health visibility, so both nodes appear
	// in Upstreams; the punished node is also on PunishedOrOperator for the pin.
	if len(ectx.Upstreams) != 2 {
		t.Fatalf("Upstreams = %+v, want both network nodes", ectx.Upstreams)
	}
	if len(ectx.PunishedOrOperator) != 1 || ectx.PunishedOrOperator[0].ID != "internal-blue" {
		t.Fatalf("PunishedOrOperator = %+v, want internal-blue", ectx.PunishedOrOperator)
	}
	foundPunishment := false
	for _, cl := range ectx.PunishedOrOperator[0].Health.CordonClasses {
		if cl == string(common.CordonClassPunishment) {
			foundPunishment = true
		}
	}
	if !foundPunishment {
		t.Fatalf("PunishedOrOperator cordonClasses = %v, want punishment", ectx.PunishedOrOperator[0].Health.CordonClasses)
	}
}

func TestBuildEvalContext_AvailabilityOnlyNotPinned(t *testing.T) {
	c := newWiringConsensus(t)

	avail := common.NewFakeUpstream("internal-blue",
		common.WithTags("type:internal"),
		common.WithHealthTracker(&classTracker{classes: []common.CordonClass{common.CordonClassAvailability}}),
	)
	healthy := common.NewFakeUpstream("alchemy", common.WithTags("type:external"))

	req := wiringRequest(t, "eth_getBalance", "")
	req.SetUpstreams([]common.Upstream{healthy})
	req.SetNetworkUpstreams([]common.Upstream{avail, healthy})

	ectx := c.buildEvalContext(req)
	if len(ectx.PunishedOrOperator) != 0 {
		t.Fatalf("availability-only must not enter PunishedOrOperator, got %+v", ectx.PunishedOrOperator)
	}
	// Availability-cordoned internals must still appear in Upstreams so
	// allUnavailable() can see the outage after removeCordoned dropped them
	// from dispatch candidates.
	foundAvail := false
	for _, u := range ectx.Upstreams {
		if u.ID == "internal-blue" {
			foundAvail = true
			break
		}
	}
	if !foundAvail {
		t.Fatalf("availability-cordoned internal missing from eval Upstreams: %+v", ectx.Upstreams)
	}
}

// An explicitly empty NetworkUpstreams snapshot (nothing method-eligible after
// filter) must not fall back to post-selection candidates — that would
// reintroduce method-ineligible nodes as healthy selector inputs.
func TestBuildEvalContext_EmptyNetworkSnapshotDoesNotFallback(t *testing.T) {
	c := newWiringConsensus(t)

	ineligible := common.NewFakeUpstream("alchemy", common.WithTags("type:external"))
	req := wiringRequest(t, "eth_getBalance", "")
	req.SetUpstreams([]common.Upstream{ineligible})
	req.SetNetworkUpstreams([]common.Upstream{})

	if !req.HasNetworkUpstreams() {
		t.Fatal("HasNetworkUpstreams must be true after SetNetworkUpstreams([] )")
	}
	if net := req.NetworkUpstreams(); net == nil {
		t.Fatal("NetworkUpstreams must return empty non-nil when set to empty")
	}

	ectx := c.buildEvalContext(req)
	if len(ectx.Upstreams) != 0 {
		t.Fatalf("empty registry snapshot must not fall back to Upstreams(), got %+v", ectx.Upstreams)
	}
}

func TestResolvePolicy_PunishedAbsentFromCandidatesPinsStandard(t *testing.T) {
	lg := zerolog.Nop()
	cfg := &common.ConsensusPolicyConfig{
		MaxParticipants: 3,
		CustomPolicy: &common.ConsensusCustomPolicyConfig{
			EvalFunction: `(ctx) => {
				if (ctx.upstreams.anyPunished()) return "standard"
				if (ctx.upstreams.withTag("type:internal").canServeBlock(ctx.request.blockNumber).length === 0)
					return "historical"
				return "standard"
			}`,
		},
		Policies: map[string]*common.ConsensusPolicyConfig{
			"historical": {MaxParticipants: 5},
			"standard":   {MaxParticipants: 3},
		},
	}
	c, err := NewConsensus(cfg, &lg)
	if err != nil {
		t.Fatalf("NewConsensus: %v", err)
	}

	punished := common.NewFakeUpstream("internal-blue",
		common.WithTags("type:internal"),
		common.WithHealthTracker(&classTracker{classes: []common.CordonClass{common.CordonClassPunishment}}),
	)
	external := common.NewFakeUpstream("alchemy", common.WithTags("type:external"))

	req := wiringRequest(t, "eth_getBalance", "")
	req.SetEvmBlockNumber(int64(100))
	req.SetUpstreams([]common.Upstream{external})
	req.SetNetworkUpstreams([]common.Upstream{punished, external})

	got := c.resolvePolicy(req)
	if got != c.named["standard"] {
		t.Fatalf("expected standard (punished pin), got %p (historical=%p default=%p)", got, c.named["historical"], c.policy)
	}
	if h := req.ExecState().ConsensusPolicy(); h != "standard" {
		t.Fatalf("header policy = %q, want standard", h)
	}
}

// openBreakerUpstream is an upstream whose catch-all circuit breaker is open
// and which holds no cordon. allUnavailable() must see it as unhealthy.
type openBreakerUpstream struct {
	common.Upstream
}

func (openBreakerUpstream) CircuitBreakerState() failsafe.State { return failsafe.StateOpen }

func TestBuildEvalContext_OpenBreakerIsUnhealthy(t *testing.T) {
	c := newWiringConsensus(t)
	down := openBreakerUpstream{Upstream: common.NewFakeUpstream("internal-blue", common.WithTags("type:internal"))}
	req := wiringRequest(t, "eth_getBalance", "")
	req.SetNetworkUpstreams([]common.Upstream{down})

	ectx := c.buildEvalContext(req)
	if len(ectx.Upstreams) != 1 {
		t.Fatalf("Upstreams = %+v, want the open-breaker node", ectx.Upstreams)
	}
	if ectx.Upstreams[0].Health.State != policyengine.StateUnhealthy {
		t.Fatalf("state = %q, want %q", ectx.Upstreams[0].Health.State, policyengine.StateUnhealthy)
	}
	if len(ectx.Upstreams[0].Health.CordonClasses) != 0 {
		t.Fatalf("cordonClasses = %v, want none", ectx.Upstreams[0].Health.CordonClasses)
	}
}

func TestResolvePolicy_ReturnDefaultString(t *testing.T) {
	lg := zerolog.Nop()
	cfg := &common.ConsensusPolicyConfig{
		MaxParticipants: 3,
		CustomPolicy: &common.ConsensusCustomPolicyConfig{
			EvalFunction: `(ctx) => "default"`,
		},
		Policies: map[string]*common.ConsensusPolicyConfig{
			"historical": {MaxParticipants: 5},
		},
	}
	c, err := NewConsensus(cfg, &lg)
	if err != nil {
		t.Fatalf("NewConsensus: %v", err)
	}
	req := wiringRequest(t, "eth_call", "")
	got := c.resolvePolicy(req)
	if got != c.policy {
		t.Fatalf("return \"default\" should select inline policy")
	}
	if h := req.ExecState().ConsensusPolicy(); h != defaultPolicyName {
		t.Fatalf("header = %q, want %q (not unknown_name)", h, defaultPolicyName)
	}
}
