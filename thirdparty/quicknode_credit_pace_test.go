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

// A QuickNode upstream on the allowance account is promoted ahead of a
// per-call vendor while account usage is behind pace, falls back to its
// configured last place once ahead, and is dropped near the allowance. An
// upstream on another QuickNode account (its credits bill elsewhere) never
// gets credit fields, so it keeps its configured place. Cold start, before
// the first usage and endpoint readings, keeps the configured order.
func TestQuicknodeCreditPace_DrivesPolicyOrder(t *testing.T) {
	const allowance = int64(90_000_000_000)
	periodStart := time.Now().Add(-10 * 24 * time.Hour).Unix()
	usedByKey := map[string]int64{
		"behind-pace":  1_000_000_000,
		"ahead-pace":   50_000_000_000,
		"at-allowance": allowance - 1_000_000_000,
	}
	release := map[string]chan struct{}{}
	for k := range usedByKey {
		release[k] = make(chan struct{})
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		used, ok := usedByKey[r.Header.Get("x-api-key")]
		if !ok {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/v0/endpoints":
			_, _ = fmt.Fprint(w, `{"data":[{"id":"1","http_url":"https://on-account-name.arbitrum-mainnet.quiknode.pro/tok/","chain":"arb","network":"arbitrum-mainnet"}]}`)
		case "/v0/usage/rpc":
			<-release[r.Header.Get("x-api-key")]
			_, _ = fmt.Fprintf(w, `{"data":{"start_time":%d,"end_time":%d,"credits_used":%d,"limit":2000000000},"error":null}`,
				periodStart, time.Now().Unix(), used)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	prevUsage, prevEndpoints := quicknodeUsageURL, quicknodeEndpointsURL
	quicknodeUsageURL = server.URL + "/v0/usage/rpc"
	quicknodeEndpointsURL = server.URL + "/v0/endpoints"
	defer func() { quicknodeUsageURL, quicknodeEndpointsURL = prevUsage, prevEndpoints }()

	for _, tc := range []struct {
		apiKey string
		want   []string
	}{
		{apiKey: "behind-pace", want: []string{"quicknode", "fallback", "other-account"}},
		{apiKey: "ahead-pace", want: []string{"fallback", "quicknode", "other-account"}},
		{apiKey: "at-allowance", want: []string{"fallback", "other-account"}},
	} {
		t.Run(tc.apiKey, func(t *testing.T) {
			qn := CreateQuicknodeVendor()
			ca := &common.CreditAllowanceConfig{Credits: allowance, ApiKey: tc.apiKey}
			onAccount := &common.UpstreamConfig{
				Id: "quicknode", VendorName: "quicknode", CreditAllowance: ca,
				Endpoint: "https://on-account-name.arbitrum-mainnet.quiknode.pro/tok/",
			}
			ups := []common.Upstream{
				&pacedUpstream{cfg: &common.UpstreamConfig{Id: "fallback", VendorName: "chainstack"}},
				&pacedUpstream{vendor: qn, cfg: onAccount},
				&pacedUpstream{vendor: qn, cfg: &common.UpstreamConfig{
					Id: "other-account", VendorName: "quicknode", CreditAllowance: ca,
					Endpoint: "https://other-account-name.arbitrum-mainnet.quiknode.pro/tok/",
				}},
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			logger := zerolog.Nop()
			engine := policy.NewEngine(ctx, &logger, "p1", health.NewTracker(&logger, "p1", time.Minute), stdlib.Install, nil)
			defer engine.Stop()
			cfg := &common.SelectionPolicyConfig{
				EvalTimeout: common.Duration(time.Second),
				EvalFunc: `(upstreams) => {
					const live = upstreams.excludeIf(creditRemainingBelow(5e9), { probe: false });
					return live.filter(creditPaceBelow(1.0)).union(live);
				}`,
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

			// Cold start: the first ticks only trigger the fetches, so the
			// configured order holds.
			require.Equal(t, []string{"fallback", "quicknode", "other-account"}, order())
			provider := qn.(common.CreditUsageProvider)
			require.Eventually(t, func() bool {
				provider.CreditUsage(&logger, onAccount)
				return qn.(*QuicknodeVendor).accountEndpoints.Has(tc.apiKey)
			}, 5*time.Second, 10*time.Millisecond)
			require.Equal(t, []string{"fallback", "quicknode", "other-account"}, order())

			close(release[tc.apiKey])
			require.Eventually(t, func() bool {
				_, ok := provider.CreditUsage(&logger, onAccount)
				return ok
			}, 5*time.Second, 10*time.Millisecond)

			require.Equal(t, tc.want, order())
		})
	}
}
