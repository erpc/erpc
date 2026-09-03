package svm

import (
	"context"
	"errors"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/telemetry"
	"github.com/prometheus/client_golang/prometheus"
	promUtil "github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// The network/method the miss-reason fixtures below drive. finalizedNetwork
// (json_rpc_cache_test.go) has no alias, so NetworkLabel() falls back to its id.
const (
	missReasonNetworkLabel = "svm:test"
	missReasonMethod       = "getBlock"
)

// missReasonValues enumerates every value that can land on the `reason` label of
// erpc_cache_get_success_miss_total. SvmJsonRpcCache.Get never emits
// ttl_rejected — that is EVM's block-timestamp age guard, which has no
// slot-based equivalent here — but it is read anyway so a classifier that
// starts emitting it cannot slip past the "every sibling stays flat" check.
var missReasonValues = []string{"connector_error", "connector_miss", "ttl_rejected", "empty_result"}

// newMissReasonCache wires each connector as its own catch-all policy, in the
// given order. findGetPolicies dedupes by connector, so the connectors must be
// distinct instances for all of them to be consulted.
func newMissReasonCache(t *testing.T, projectId string, conns ...*data.MockConnector) (*SvmJsonRpcCache, []*data.CachePolicy) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	base, err := NewSvmJsonRpcCache(ctx, &log.Logger, &common.CacheConfig{})
	require.NoError(t, err)
	c := base.WithProjectId(projectId)

	policies := make([]*data.CachePolicy, 0, len(conns))
	for _, conn := range conns {
		p, err := data.NewCachePolicy(&common.CachePolicyConfig{
			// "*" because the fixture request carries finalizedNetwork, whose id
			// is "svm:test" rather than a production "svm:mainnet-beta".
			Network:   "*",
			Method:    "*",
			Finality:  common.DataFinalityStateFinalized,
			Connector: conn.Id(),
		}, conn)
		require.NoError(t, err)
		policies = append(policies, p)
	}
	c.SetPolicies(policies)
	return c, policies
}

// missCountsByReason reads erpc_cache_get_success_miss_total for every reason
// value against the label set Get attributes the fall-through miss to (the
// FIRST matched policy).
//
// This is also the cardinality guard for the panic this file exists for. The
// counter declares seven labels; SvmJsonRpcCache.Get passed six until the
// `reason` argument was added, and because WithLabelValues is variadic that
// mismatch compiled cleanly and only blew up — "inconsistent label
// cardinality: expected 7 label values but got 6" — on the first real SVM cache
// miss in production. Reading the metric with a full seven-value tuple means a
// future label added to the counter without updating Get's call site reddens
// this test instead of a production request.
func missCountsByReason(t *testing.T, projectId string, p *data.CachePolicy) map[string]float64 {
	t.Helper()
	counts := make(map[string]float64, len(missReasonValues))
	for _, reason := range missReasonValues {
		counts[reason] = promUtil.ToFloat64(telemetry.MetricCacheGetSuccessMissTotal.WithLabelValues(
			projectId,
			missReasonNetworkLabel,
			missReasonMethod,
			p.GetConnector().Id(),
			p.String(),
			ttlString(p.GetTTL()),
			reason,
		))
	}
	return counts
}

// cacheErrorTotalForProject sums every erpc_cache_get_error_total series tagged
// with projectId.
//
// Summing the whole metric rather than naming one error label is deliberate:
// the assertion these tests need is "this cold read recorded NO cache error at
// all", whatever error string the connector would have produced. Naming a label
// would let a differently-summarized error slip through, and for the real
// memory connector it would mean reconstructing the driver's internal partition
// and range keys. Every case uses a project id of its own, so this sum is
// unaffected by tests running beside it.
func cacheErrorTotalForProject(t *testing.T, projectId string) float64 {
	t.Helper()
	ch := make(chan prometheus.Metric, 256)
	go func() {
		telemetry.MetricCacheGetErrorTotal.Collect(ch)
		close(ch)
	}()
	var total float64
	for m := range ch {
		var pb dto.Metric
		require.NoError(t, m.Write(&pb))
		for _, l := range pb.GetLabel() {
			if l.GetName() == "project" && l.GetValue() == projectId {
				total += pb.GetCounter().GetValue()
				break
			}
		}
	}
	return total
}

func newMissReasonRequest(t *testing.T) *common.NormalizedRequest {
	t.Helper()
	req := common.NewNormalizedRequest(
		[]byte(`{"jsonrpc":"2.0","id":1,"method":"` + missReasonMethod + `","params":[100,{"commitment":"finalized"}]}`),
	)
	// Without a network req.Finality() resolves to Unknown and the Finalized
	// policies would not match, so Get would exit via the skipped-policy path.
	req.SetNetwork(finalizedNetwork{})
	return req
}

// TestSvmCache_Get_MissReasonLabel pins the VALUE of the `reason` label on
// erpc_cache_get_success_miss_total for the SVM cache, the error counter that
// must or must not move alongside it, and — as a side effect of reading both
// metrics with their full label tuples — their cardinality.
//
// The reason label is the only thing separating a cold cache from a broken one:
// without it a connector that is erroring or timing out is indistinguishable
// from one that genuinely holds no entry, so a cache-backend outage reads as a
// hit-rate regression instead of the latency problem it actually is.
//
// Each case drives Get down one classification path and asserts the WHOLE
// reason vector — the expected value rises by exactly one and every sibling
// stays flat. Collapsing the classifier to a constant therefore fails here even
// though it would still satisfy a bare "the miss counter went up" assertion.
func TestSvmCache_Get_MissReasonLabel(t *testing.T) {
	t.Parallel()

	// A fault with no semantic-miss code: a connector that actually failed.
	genuineFault := errors.New("connection refused")

	cases := []struct {
		name string
		// conns is one connector per policy, in order. nil means the connector
		// answers (nil, nil); otherwise it returns that error.
		conns      []error
		directives *common.RequestDirectives
		wantReason string
		// wantErrorTotal is the expected delta on erpc_cache_get_error_total.
		// A semantic miss must leave it flat — inventing a cache error rate out
		// of ordinary cold reads is half of the bug this file guards.
		wantErrorTotal float64
	}{
		{
			// doGet's (nil, nil) path: a connector that reports an empty value
			// with no error. No shipped driver does this today, but the branch
			// exists and must classify as a cold read.
			name:       "ConnectorHoldsNoEntry",
			conns:      []error{nil},
			wantReason: "connector_miss",
		},
		{
			// THE REGRESSION. Every data driver signals a cold key with
			// ErrRecordNotFound (data/memory.go, redis.go, dynamodb.go,
			// postgresql.go, grpc.go), so this — not (nil, nil) — is what an
			// ordinary SVM cache miss looks like in production. Before the
			// semantic-miss guard it was reported as connector_error and also
			// bumped the error counter, so a perfectly healthy cold cache
			// looked like a failing backend.
			name:       "RecordNotFoundIsAColdRead",
			conns:      []error{common.NewErrRecordNotFound("pk", "rk", "memory")},
			wantReason: "connector_miss",
		},
		{
			// An entry that existed and aged out is still "we have nothing to
			// serve", not a backend fault.
			name:       "ExpiredRecordIsAColdRead",
			conns:      []error{common.NewErrRecordExpired("pk", "rk", "memory", 0, 0)},
			wantReason: "connector_miss",
		},
		{
			// The gRPC/prism path: "range outside available" / cold storage
			// range. prism-solana is the connector SVM actually runs against,
			// so this is the cold read operators will see most.
			name:       "MissingDataIsAColdRead",
			conns:      []error{common.NewErrEndpointMissingData(errors.New("range outside available"), nil)},
			wantReason: "connector_miss",
		},
		{
			// The other half of the guard: it must not be widened into
			// swallowing real faults. A plain error is still an error, on both
			// the reason label and the error counter.
			name:           "OnlyConnectorFails",
			conns:          []error{genuineFault},
			wantReason:     "connector_error",
			wantErrorTotal: 1,
		},
		{
			// PRECEDENCE, borrowed from architecture/evm/json_rpc_cache.go: a
			// confirmed "I don't have it" outranks a peer that failed to answer.
			// A pool where one connector is down but another simply holds no
			// entry is a cold read, not an outage — attributing it to the error
			// lets one flaky connector rewrite the reason for every cold read
			// fanned out beside it, which is exactly the signal inversion the
			// label was added to prevent. Do not "tidy" the switch by ordering
			// sawError first.
			name:           "ConfirmedMissOutranksConnectorFailure",
			conns:          []error{genuineFault, nil},
			wantReason:     "connector_miss",
			wantErrorTotal: 1,
		},
		{
			// Same precedence with the outcomes swapped, so a classifier that
			// merely reports whichever policy was consulted last cannot pass.
			name:           "PrecedenceHoldsWhenTheFailureComesSecond",
			conns:          []error{nil, errors.New("i/o timeout")},
			wantReason:     "connector_miss",
			wantErrorTotal: 1,
		},
		{
			// Precedence must not silence the fault. The reason label says cold
			// read (a peer confirmed it holds nothing), but the down connector
			// still has to show up on the error counter — otherwise the guard
			// that fixed the false error rate would hide a genuine outage.
			name:           "SemanticMissDoesNotHideAPeerFault",
			conns:          []error{common.NewErrRecordNotFound("pk", "rk", "memory"), genuineFault},
			wantReason:     "connector_miss",
			wantErrorTotal: 1,
		},
		{
			// The directive skips every policy, so nothing is consulted and
			// there is no connector outcome to classify. Reporting this as a
			// connector miss would credit the cache with a cold read it never
			// performed.
			name:       "NoConnectorConsulted",
			conns:      []error{nil},
			directives: &common.RequestDirectives{SkipCacheRead: "true"},
			wantReason: "empty_result",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// A distinct project id per case gives every case its own label
			// tuple, so no counter written by a sibling case (or by any other
			// test sharing these process-wide collectors) can satisfy the
			// assertions below.
			projectId := "svm-miss-reason-" + tc.name

			conns := make([]*data.MockConnector, 0, len(tc.conns))
			for i, connErr := range tc.conns {
				conn := data.NewMockConnector(string(rune('a'+i)) + "-" + tc.name)
				conn.On("Get", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
					Return(nil, connErr)
				conns = append(conns, conn)
			}
			c, policies := newMissReasonCache(t, projectId, conns...)

			req := newMissReasonRequest(t)
			if tc.directives != nil {
				req.SetDirectives(tc.directives)
			}

			beforeMiss := missCountsByReason(t, projectId, policies[0])
			beforeErr := cacheErrorTotalForProject(t, projectId)
			resp, err := c.Get(context.Background(), req)
			afterMiss := missCountsByReason(t, projectId, policies[0])
			afterErr := cacheErrorTotalForProject(t, projectId)

			require.NoError(t, err)
			require.Nil(t, resp, "every case here is a miss that falls through to the upstream layer")

			for _, reason := range missReasonValues {
				want := 0.0
				if reason == tc.wantReason {
					want = 1.0
				}
				require.Equalf(t, want, afterMiss[reason]-beforeMiss[reason],
					"erpc_cache_get_success_miss_total{reason=%q} delta", reason)
			}
			require.Equal(t, tc.wantErrorTotal, afterErr-beforeErr,
				"erpc_cache_get_error_total delta")
		})
	}
}

// TestSvmCache_Get_ColdKeyOnRealConnectorIsAMiss is the same regression as the
// RecordNotFoundIsAColdRead row, driven through a REAL data connector instead of
// a mock imitating one.
//
// It matters separately because the whole defect was a mismatch between what the
// classifier expected a miss to look like — doGet returning (nil, nil) — and
// what every shipped driver actually returns for a key it does not hold, which
// is ErrRecordNotFound. A test that only ever asks a mock for (nil, nil) agrees
// with the broken assumption and stays green through the bug. This one does not:
// against the tree before the semantic-miss guard it measured
// reason=connector_error at 1 and connector_miss at 0.
func TestSvmCache_Get_ColdKeyOnRealConnectorIsAMiss(t *testing.T) {
	t.Parallel()
	const projectId = "svm-miss-reason-real-memory-connector"

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	base, err := NewSvmJsonRpcCache(ctx, &log.Logger, &common.CacheConfig{
		Connectors: []*common.ConnectorConfig{{
			Id:     "mem",
			Driver: common.DriverMemory,
			Memory: &common.MemoryConnectorConfig{MaxItems: 100, MaxTotalSize: "1MB"},
		}},
		Policies: []*common.CachePolicyConfig{{
			Connector: "mem",
			Network:   "*",
			Method:    "*",
			Finality:  common.DataFinalityStateFinalized,
		}},
	})
	require.NoError(t, err)
	c := base.WithProjectId(projectId)
	require.Len(t, c.policies, 1)

	beforeMiss := missCountsByReason(t, projectId, c.policies[0])
	beforeErr := cacheErrorTotalForProject(t, projectId)
	// Nothing was ever written to this cache, so this key is cold.
	resp, err := c.Get(context.Background(), newMissReasonRequest(t))
	afterMiss := missCountsByReason(t, projectId, c.policies[0])
	afterErr := cacheErrorTotalForProject(t, projectId)

	require.NoError(t, err)
	require.Nil(t, resp)

	for _, reason := range missReasonValues {
		want := 0.0
		if reason == "connector_miss" {
			want = 1.0
		}
		require.Equalf(t, want, afterMiss[reason]-beforeMiss[reason],
			"erpc_cache_get_success_miss_total{reason=%q} delta", reason)
	}
	require.Zero(t, afterErr-beforeErr,
		"a cold key is not a cache failure: erpc_cache_get_error_total must stay flat")
}
