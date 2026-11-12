package thirdparty

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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

// creditPaceVendor is one prepaid vendor's account APIs, mocked: the server
// answers the account's endpoint/app list at once and holds the usage
// reading until release closes.
type creditPaceVendor struct {
	name                  string
	create                func() common.Vendor
	point                 func(serverURL string) (restore func())
	handler               func(used int64, release <-chan struct{}) http.HandlerFunc
	accountKey            func(r *http.Request) string
	accountListed         func(v common.Vendor, key string) bool
	onAccount, offAccount string
}

// A prepaid upstream on the allowance account is promoted ahead of a
// per-call vendor while account usage is behind pace, falls back to its
// configured last place once ahead, and is dropped near the allowance. An
// upstream on another account (its credits bill elsewhere) never gets credit
// fields, so it keeps its configured place. Cold start, before the first
// usage and account-list readings, keeps the configured order.
func TestCreditPace_DrivesPolicyOrder(t *testing.T) {
	const allowance = int64(90_000_000_000)
	periodStart := time.Now().Add(-10 * 24 * time.Hour).UTC().Truncate(time.Second)
	vendors := []creditPaceVendor{
		{
			name:   "quicknode",
			create: CreateQuicknodeVendor,
			point: func(base string) func() {
				prevUsage, prevEndpoints := quicknodeUsageURL, quicknodeEndpointsURL
				quicknodeUsageURL, quicknodeEndpointsURL = base+"/v0/usage/rpc", base+"/v0/endpoints"
				return func() { quicknodeUsageURL, quicknodeEndpointsURL = prevUsage, prevEndpoints }
			},
			handler: func(used int64, release <-chan struct{}) http.HandlerFunc {
				return func(w http.ResponseWriter, r *http.Request) {
					switch r.URL.Path {
					case "/v0/endpoints":
						_, _ = fmt.Fprint(w, `{"data":[{"id":"1","http_url":"https://on-account-name.arbitrum-mainnet.quiknode.pro/tok/","chain":"arb","network":"arbitrum-mainnet"}]}`)
					case "/v0/usage/rpc":
						<-release
						_, _ = fmt.Fprintf(w, `{"data":{"start_time":%d,"end_time":%d,"credits_used":%d,"limit":2000000000},"error":null}`,
							periodStart.Unix(), time.Now().Unix(), used)
					default:
						w.WriteHeader(http.StatusNotFound)
					}
				}
			},
			accountKey: func(r *http.Request) string { return r.Header.Get("x-api-key") },
			accountListed: func(v common.Vendor, key string) bool {
				return v.(*QuicknodeVendor).accountEndpoints.Has(key)
			},
			onAccount:  "https://on-account-name.arbitrum-mainnet.quiknode.pro/tok/",
			offAccount: "https://other-account-name.arbitrum-mainnet.quiknode.pro/tok/",
		},
		{
			name:   "alchemy",
			create: CreateAlchemyVendor,
			point: func(base string) func() {
				prevUsage, prevApps := alchemyUsageURL, alchemyAppsURL
				alchemyUsageURL, alchemyAppsURL = base+"/v1/usage/summary", base+"/v1/apps"
				return func() { alchemyUsageURL, alchemyAppsURL = prevUsage, prevApps }
			},
			handler: func(used int64, release <-chan struct{}) http.HandlerFunc {
				return func(w http.ResponseWriter, r *http.Request) {
					switch r.URL.Path {
					case "/v1/apps":
						// The on-account app sits on the second page.
						if r.URL.Query().Get("cursor") == "" {
							_, _ = fmt.Fprint(w, `{"data":{"apps":[{"id":"a1","name":"shadow","apiKey":"team-key-1","webhookApiKey":"w","chainNetworks":[],"createdAt":"2026-01-01T00:00:00Z"}],"totalCount":2,"cursor":"p2"}}`)
							return
						}
						_, _ = fmt.Fprint(w, `{"data":{"apps":[{"id":"a2","name":"erpc","apiKey":"team-key-2","webhookApiKey":"w","chainNetworks":[],"createdAt":"2026-01-01T00:00:00Z"}],"totalCount":2}}`)
					case "/v1/usage/summary":
						<-release
						_, _ = fmt.Fprintf(w, `{"data":{"billingPeriod":{"startTime":%q,"endTime":%q},"totals":{"monthToDate":{"amount":"%d","unit":"ALCHEMY_COMPUTE_UNIT","usd":"1.00"},"last7Days":{"amount":"0","unit":"ALCHEMY_COMPUTE_UNIT"},"last30Days":{"amount":"0","unit":"ALCHEMY_COMPUTE_UNIT"}},"freshness":{"dataThrough":%q,"containsPartialToday":true,"updateCadence":"minute"}}}`,
							periodStart.Format(time.RFC3339), periodStart.AddDate(0, 1, 0).Format(time.RFC3339), used, time.Now().UTC().Format(time.RFC3339))
					default:
						w.WriteHeader(http.StatusNotFound)
					}
				}
			},
			accountKey: func(r *http.Request) string { return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ") },
			accountListed: func(v common.Vendor, key string) bool {
				return v.(*AlchemyVendor).teamAppKeys.Has(key)
			},
			onAccount:  "https://arb-mainnet.g.alchemy.com/v2/team-key-2",
			offAccount: "https://arb-mainnet.g.alchemy.com/v2/other-team-key",
		},
	}

	for _, vendor := range vendors {
		for _, tc := range []struct {
			apiKey string
			used   int64
			want   []string
		}{
			{apiKey: "behind-pace", used: 1_000_000_000, want: []string{"prepaid", "fallback", "other-account"}},
			{apiKey: "ahead-pace", used: 50_000_000_000, want: []string{"fallback", "prepaid", "other-account"}},
			{apiKey: "at-allowance", used: allowance - 1_000_000_000, want: []string{"fallback", "other-account"}},
		} {
			t.Run(vendor.name+"/"+tc.apiKey, func(t *testing.T) {
				release := make(chan struct{})
				serve := vendor.handler(tc.used, release)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if vendor.accountKey(r) != tc.apiKey {
						w.WriteHeader(http.StatusUnauthorized)
						return
					}
					serve(w, r)
				}))
				defer server.Close()
				defer vendor.point(server.URL)()

				v := vendor.create()
				ca := &common.CreditAllowanceConfig{Credits: allowance, ApiKey: tc.apiKey}
				onAccount := &common.UpstreamConfig{
					Id: "prepaid", VendorName: vendor.name, CreditAllowance: ca, Endpoint: vendor.onAccount,
				}
				ups := []common.Upstream{
					&pacedUpstream{cfg: &common.UpstreamConfig{Id: "fallback", VendorName: "chainstack"}},
					&pacedUpstream{vendor: v, cfg: onAccount},
					&pacedUpstream{vendor: v, cfg: &common.UpstreamConfig{
						Id: "other-account", VendorName: vendor.name, CreditAllowance: ca, Endpoint: vendor.offAccount,
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
				require.Equal(t, []string{"fallback", "prepaid", "other-account"}, order())
				provider := v.(common.CreditUsageProvider)
				require.Eventually(t, func() bool {
					provider.CreditUsage(&logger, onAccount)
					return vendor.accountListed(v, tc.apiKey)
				}, 5*time.Second, 10*time.Millisecond)
				require.Equal(t, []string{"fallback", "prepaid", "other-account"}, order())

				close(release)
				require.Eventually(t, func() bool {
					_, ok := provider.CreditUsage(&logger, onAccount)
					return ok
				}, 5*time.Second, 10*time.Millisecond)

				require.Equal(t, tc.want, order())
			})
		}
	}
}
