package thirdparty

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
	"github.com/spf13/afero"
	"github.com/stretchr/testify/require"
)

func init() { util.ConfigureTestLogger() }

// Exercise the config loaders and the actual cache freshness decision, rather
// than just checking that the duration parser returns the expected value.
func TestDrpcVendor_ConfiguredRecheckInterval(t *testing.T) {
	for _, format := range []string{"yaml", "ts"} {
		for _, value := range []string{`"1h"`, `3600000`, `null`} {
			for _, method := range []string{"SupportsNetwork", "GenerateConfigs"} {
				t.Run(format+"/"+value+"/"+method, func(t *testing.T) {
					var calls atomic.Int32
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						calls.Add(1)
						w.Header().Set("Content-Type", "application/json")
						_, _ = w.Write([]byte(`[]`))
					}))
					defer server.Close()

					path := filepath.Join(t.TempDir(), "erpc."+format)
					src := fmt.Sprintf("projects:\n  - id: main\n    providers:\n      - id: catalog\n        vendor: drpc\n        settings:\n          apiKey: test-key\n          chainsUrl: %s\n          recheckInterval: %s\n", server.URL, value)
					if format == "ts" {
						src = fmt.Sprintf(`export default {projects: [{id: "main", providers: [{id: "catalog", vendor: "drpc", settings: {apiKey: "test-key", chainsUrl: %q, recheckInterval: %s}}]}]};`, server.URL, value)
					}
					fs := afero.NewOsFs()
					require.NoError(t, afero.WriteFile(fs, path, []byte(src), 0o600))
					cfg, err := common.LoadConfig(fs, path, &common.DefaultOptions{})
					require.NoError(t, err)
					settings := cfg.Projects[0].Providers[0].Settings

					vendor := CreateDrpcVendor().(*DrpcVendor)
					vendor.cache.snapshot.Store(&remoteCacheSnapshot[map[int64]string]{
						values:    map[string]map[int64]string{server.URL: {1: "ethereum"}},
						fetchedAt: map[string]time.Time{server.URL: time.Now().Add(-2 * time.Hour)},
					})
					logger := zerolog.Nop()
					if method == "SupportsNetwork" {
						supported, err := vendor.SupportsNetwork(context.Background(), &logger, settings, "evm:1")
						require.NoError(t, err)
						require.True(t, supported)
					} else {
						upstreams, err := vendor.GenerateConfigs(context.Background(), &logger, &common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 1}}, settings)
						require.NoError(t, err)
						require.Len(t, upstreams, 1)
						require.Contains(t, upstreams[0].Endpoint, "network=ethereum")
					}
					waitRefreshesIdle(t, vendor.cache)
					want := int32(1)
					if value == "null" {
						want = 0 // The unchanged 24h default keeps the 2h-old entry fresh.
					}
					require.Equal(t, want, calls.Load())
				})
			}
		}
	}
}

func TestVendors_InvalidRecheckInterval(t *testing.T) {
	for _, create := range []func() common.Vendor{
		CreateAlchemyVendor, CreateChainstackVendor, CreateConduitVendor, CreateDrpcVendor,
		CreateQuicknodeVendor, CreateRepositoryVendor, CreateSuperchainVendor, CreateTenderlyVendor,
	} {
		vendor := create()
		t.Run(vendor.Name(), func(t *testing.T) {
			if alchemy, ok := vendor.(*AlchemyVendor); ok {
				// Its lifecycle also refreshes pricing; keep this test offline.
				primeRemoteCache(t, alchemy.cuCache, alchemyCreditUnitsURL, map[string]int64{"*": 20})
			}
			logger := zerolog.Nop()
			settings := common.VendorSettings{"apiKey": "test-key", "recheckInterval": "invalid"}
			_, err := vendor.SupportsNetwork(context.Background(), &logger, settings, "evm:1")
			require.ErrorContains(t, err, "settings.recheckInterval")
			_, err = vendor.GenerateConfigs(context.Background(), &logger, &common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 1}}, settings)
			require.ErrorContains(t, err, "settings.recheckInterval")
		})
	}
}
