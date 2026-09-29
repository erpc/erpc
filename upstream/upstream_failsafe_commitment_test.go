package upstream

import (
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func init() {
	util.ConfigureTestLogger()
}

func newUpstreamWithFailsafe(t *testing.T, cfgs []*common.FailsafeConfig) *Upstream {
	t.Helper()
	lg := log.Logger
	var execs []*upstreamExecutor
	for _, cfg := range cfgs {
		ex, err := NewUpstreamExecutor(cfg, &lg)
		require.NoError(t, err)
		execs = append(execs, ex)
	}
	noop, err := NewUpstreamExecutor(nil, &lg)
	require.NoError(t, err)
	execs = append(execs, noop)
	return &Upstream{
		config: &common.UpstreamConfig{
			Id:       "rpc1",
			Type:     common.UpstreamTypeEvm,
			Endpoint: "http://rpc1.localhost",
		},
		logger:            &lg,
		failsafeExecutors: execs,
	}
}

func TestGetFailsafeExecutor_MatchCommitment(t *testing.T) {
	newReq := func(commitment common.CommitmentLevel) *common.NormalizedRequest {
		req := common.NewNormalizedRequest([]byte(
			`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{"to":"0x123"},"0x100"]}`,
		))
		req.SetRequestedCommitment(commitment)
		return req
	}

	t.Run("omitted matcher is wildcard", func(t *testing.T) {
		u := newUpstreamWithFailsafe(t, []*common.FailsafeConfig{
			{MatchMethod: "*", Retry: &common.RetryPolicyConfig{MaxAttempts: 2}},
		})
		for _, c := range []common.CommitmentLevel{
			common.CommitmentNone, common.CommitmentProcessed,
			common.CommitmentConfirmed, common.CommitmentFinalized, common.CommitmentUnknown,
		} {
			ex := u.getFailsafeExecutor(newReq(c))
			require.NotNil(t, ex, "commitment %s", c)
			assert.Equal(t, "*", ex.MatchMethod())
			assert.Equal(t, 2, ex.cfg.Retry.MaxAttempts)
		}
	})

	t.Run("exact match each level", func(t *testing.T) {
		u := newUpstreamWithFailsafe(t, []*common.FailsafeConfig{
			{MatchMethod: "*", MatchCommitment: []common.CommitmentLevel{common.CommitmentProcessed}, Retry: &common.RetryPolicyConfig{MaxAttempts: 1}},
			{MatchMethod: "*", MatchCommitment: []common.CommitmentLevel{common.CommitmentConfirmed}, Retry: &common.RetryPolicyConfig{MaxAttempts: 2}},
			{MatchMethod: "*", MatchCommitment: []common.CommitmentLevel{common.CommitmentFinalized}, Retry: &common.RetryPolicyConfig{MaxAttempts: 3}},
			{MatchMethod: "*", MatchCommitment: []common.CommitmentLevel{common.CommitmentNone}, Retry: &common.RetryPolicyConfig{MaxAttempts: 4}},
			{MatchMethod: "*", Retry: &common.RetryPolicyConfig{MaxAttempts: 9}},
		})
		cases := []struct {
			c    common.CommitmentLevel
			want int
		}{
			{common.CommitmentProcessed, 1},
			{common.CommitmentConfirmed, 2},
			{common.CommitmentFinalized, 3},
			{common.CommitmentNone, 4},
		}
		for _, tc := range cases {
			ex := u.getFailsafeExecutor(newReq(tc.c))
			require.NotNil(t, ex)
			assert.Equal(t, tc.want, ex.cfg.Retry.MaxAttempts, "commitment %s", tc.c)
		}
	})

	t.Run("confirmed does not match processed", func(t *testing.T) {
		u := newUpstreamWithFailsafe(t, []*common.FailsafeConfig{
			{MatchMethod: "*", MatchCommitment: []common.CommitmentLevel{common.CommitmentProcessed}, Retry: &common.RetryPolicyConfig{MaxAttempts: 1}},
			{MatchMethod: "*", Retry: &common.RetryPolicyConfig{MaxAttempts: 9}},
		})
		ex := u.getFailsafeExecutor(newReq(common.CommitmentConfirmed))
		require.NotNil(t, ex)
		assert.Equal(t, 9, ex.cfg.Retry.MaxAttempts)
	})

	t.Run("unknown does not match none", func(t *testing.T) {
		u := newUpstreamWithFailsafe(t, []*common.FailsafeConfig{
			{MatchMethod: "*", MatchCommitment: []common.CommitmentLevel{common.CommitmentNone}, Retry: &common.RetryPolicyConfig{MaxAttempts: 1}},
			{MatchMethod: "*", Retry: &common.RetryPolicyConfig{MaxAttempts: 9}},
		})
		ex := u.getFailsafeExecutor(newReq(common.CommitmentUnknown))
		require.NotNil(t, ex)
		assert.Equal(t, 9, ex.cfg.Retry.MaxAttempts)
	})

	t.Run("AND with matchMethod", func(t *testing.T) {
		u := newUpstreamWithFailsafe(t, []*common.FailsafeConfig{
			{MatchMethod: "eth_getBalance", MatchCommitment: []common.CommitmentLevel{common.CommitmentFinalized}, Retry: &common.RetryPolicyConfig{MaxAttempts: 1}},
			{MatchMethod: "eth_call", MatchCommitment: []common.CommitmentLevel{common.CommitmentFinalized}, Retry: &common.RetryPolicyConfig{MaxAttempts: 2}},
		})
		ex := u.getFailsafeExecutor(newReq(common.CommitmentFinalized))
		require.NotNil(t, ex)
		assert.Equal(t, "eth_call", ex.MatchMethod())
		assert.Equal(t, 2, ex.cfg.Retry.MaxAttempts)
	})

	t.Run("specific commitment precedes catch-all", func(t *testing.T) {
		u := newUpstreamWithFailsafe(t, []*common.FailsafeConfig{
			{MatchMethod: "*", MatchCommitment: []common.CommitmentLevel{common.CommitmentFinalized}, Retry: &common.RetryPolicyConfig{MaxAttempts: 1}},
			{MatchMethod: "*", Retry: &common.RetryPolicyConfig{MaxAttempts: 9}},
		})
		ex := u.getFailsafeExecutor(newReq(common.CommitmentFinalized))
		require.NotNil(t, ex)
		assert.Equal(t, 1, ex.cfg.Retry.MaxAttempts)

		ex = u.getFailsafeExecutor(newReq(common.CommitmentNone))
		require.NotNil(t, ex)
		assert.Equal(t, 9, ex.cfg.Retry.MaxAttempts)
	})
}
