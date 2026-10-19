package thirdparty

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/internal/policy"
	"github.com/erpc/erpc/internal/policy/stdlib"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

type pacedUpstream struct {
	common.Upstream
	cfg    *common.UpstreamConfig
	vendor common.Vendor
}

func (u *pacedUpstream) Id() string                     { return u.cfg.Id }
func (u *pacedUpstream) VendorName() string             { return u.cfg.VendorName }
func (u *pacedUpstream) Config() *common.UpstreamConfig { return u.cfg }
func (u *pacedUpstream) Vendor() common.Vendor          { return u.vendor }
func (u *pacedUpstream) Logger() *zerolog.Logger        { l := zerolog.Nop(); return &l }
func (u *pacedUpstream) NetworkId() string              { return "evm:42161" }
func (u *pacedUpstream) NetworkLabel() string           { return "evm:42161" }
func (u *pacedUpstream) Tracker() common.HealthTracker  { return nil }
func (u *pacedUpstream) Cordon(method, reason string)   {}
func (u *pacedUpstream) Uncordon(method, reason string) {}

// A QuickNode upstream with a prepaid allowance leads while account usage is
// on pace, drops out once usage runs ahead of the pro-rata allowance, and
// keeps its place on cold start before the first usage reading lands.
func TestQuicknodeCreditPace_DrivesPolicyOrder(t *testing.T) {
	const allowance = int64(90_000_000_000)
	periodStart := time.Now().Add(-10 * 24 * time.Hour).Unix()
	usedByKey := map[string]int64{
		"under-pace": 1_000_000_000,
		"over-pace":  allowance, // whole month's allowance used in 10 days
	}
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		used, ok := usedByKey[r.Header.Get("x-api-key")]
		if r.URL.Path != "/v0/usage/rpc" || !ok {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = fmt.Fprintf(w, `{"data":{"start_time":%d,"end_time":%d,"credits_used":%d,"limit":2000000000},"error":null}`,
			periodStart, time.Now().Unix(), used)
	}))
	defer server.Close()
	prev := quicknodeUsageURL
	quicknodeUsageURL = server.URL + "/v0/usage/rpc"
	defer func() { quicknodeUsageURL = prev }()

	for _, tc := range []struct {
		apiKey string
		want   []string
	}{
		{apiKey: "under-pace", want: []string{"quicknode", "fallback"}},
		{apiKey: "over-pace", want: []string{"fallback"}},
	} {
		t.Run(tc.apiKey, func(t *testing.T) {
			qn := CreateQuicknodeVendor()
			ups := []common.Upstream{
				&pacedUpstream{vendor: qn, cfg: &common.UpstreamConfig{
					Id: "quicknode", VendorName: "quicknode",
					CreditAllowance: &common.CreditAllowanceConfig{Credits: allowance, ApiKey: tc.apiKey},
				}},
				&pacedUpstream{cfg: &common.UpstreamConfig{Id: "fallback", VendorName: "chainstack"}},
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			logger := zerolog.Nop()
			engine := policy.NewEngine(ctx, &logger, "p1", health.NewTracker(&logger, "p1", time.Minute), stdlib.Install, nil)
			defer engine.Stop()
			cfg := &common.SelectionPolicyConfig{
				EvalTimeout: common.Duration(time.Second),
				EvalFunc:    `(upstreams) => upstreams.excludeIf(creditPaceAbove(1.0), { probe: false })`,
			}
			require.NoError(t, cfg.SetDefaults())
			require.NoError(t, engine.RegisterNetwork("evm:42161", "", func() []common.Upstream { return ups }, cfg))

			order := func() []string {
				policy.TickForTest(engine, "evm:42161", "*")
				var got []string
				for _, u := range engine.GetOrdered("evm:42161", "*", "*") {
					got = append(got, u.Id())
				}
				return got
			}

			// Cold start: the first tick only triggers the fetch, so the
			// configured order holds.
			require.Equal(t, []string{"quicknode", "fallback"}, order())

			release <- struct{}{}
			provider := qn.(common.CreditUsageProvider)
			require.Eventually(t, func() bool {
				_, ok := provider.CreditUsage(&logger, &common.CreditAllowanceConfig{Credits: allowance, ApiKey: tc.apiKey})
				return ok
			}, 5*time.Second, 10*time.Millisecond)

			require.Equal(t, tc.want, order())
		})
	}
}
