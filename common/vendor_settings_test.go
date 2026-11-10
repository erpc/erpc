package common

import (
	"fmt"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/erpc/erpc/util"
	"github.com/spf13/afero"
	"github.com/stretchr/testify/require"
)

func init() { util.ConfigureTestLogger() }

func TestVendorSettings_RecheckInterval(t *testing.T) {
	const defaultInterval = 24 * time.Hour
	for _, tt := range []struct {
		name  string
		value interface{}
		want  time.Duration
	}{
		{"null", nil, defaultInterval},
		{"duration string", "1h", time.Hour},
		{"compound string", "1h30m", 90 * time.Minute},
		{"fractional string", "0.5ms", 500 * time.Microsecond},
		{"int milliseconds", 3600000, time.Hour},
		{"int64 milliseconds", int64(3600000), time.Hour},
		{"uint64 milliseconds", uint64(3600000), time.Hour},
		{"float milliseconds", float64(3600000), time.Hour},
		{"fractional milliseconds", 0.5, 500 * time.Microsecond},
		{"Go duration", time.Hour, time.Hour},
		{"Go nanoseconds", time.Nanosecond, time.Nanosecond},
		{"common duration", Duration(time.Hour), time.Hour},
		{"zero", 0, 0},
		{"zero string", "0s", 0},
		{"negative", -1, -time.Millisecond},
		{"negative string", "-1ms", -time.Millisecond},
		{"negative Go duration", -time.Millisecond, -time.Millisecond},
		{"largest integer milliseconds", int64(math.MaxInt64 / int64(time.Millisecond)), time.Duration(math.MaxInt64/int64(time.Millisecond)) * time.Millisecond},
		{"smallest integer milliseconds", int64(math.MinInt64 / int64(time.Millisecond)), time.Duration(math.MinInt64/int64(time.Millisecond)) * time.Millisecond},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := (VendorSettings{"recheckInterval": tt.value}).RecheckInterval(defaultInterval)
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
	for _, settings := range []VendorSettings{nil, {}, {"anotherSetting": "ignored"}} {
		got, err := settings.RecheckInterval(defaultInterval)
		require.NoError(t, err)
		require.Equal(t, defaultInterval, got)
	}
	for _, tt := range []struct {
		name  string
		value interface{}
	}{
		{"empty string", ""},
		{"missing unit", "3600000"},
		{"invalid unit", "1hour"},
		{"bool", false},
		{"list", []interface{}{1}},
		{"object", map[string]interface{}{"duration": "1h"}},
		{"positive integer overflow", int64(math.MaxInt64/int64(time.Millisecond) + 1)},
		{"negative integer overflow", int64(math.MinInt64/int64(time.Millisecond) - 1)},
		{"unsigned overflow", uint64(math.MaxUint64)},
		{"float overflow", float64(math.MaxInt64) / float64(time.Millisecond)},
		{"negative float overflow", -1e20},
		{"NaN", math.NaN()},
		{"positive infinity", math.Inf(1)},
		{"negative infinity", math.Inf(-1)},
		{"string overflow", "999999999999999h"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := (VendorSettings{"recheckInterval": tt.value}).RecheckInterval(defaultInterval)
			require.ErrorContains(t, err, "settings.recheckInterval")
		})
	}
}

func TestLoadConfig_ProviderRecheckInterval(t *testing.T) {
	for _, format := range []string{"yaml", "ts"} {
		for _, tt := range []struct {
			value string
			want  time.Duration
			bad   bool
		}{
			{`"1h"`, time.Hour, false},
			{`3600000`, time.Hour, false},
			{`0.5`, 500 * time.Microsecond, false},
			{`null`, 24 * time.Hour, false},
			{`"invalid"`, 0, true},
			{`false`, 0, true},
			{`[]`, 0, true},
			{`9223372036854775807`, 0, true},
		} {
			t.Run(format+"/"+tt.value, func(t *testing.T) {
				fs := afero.NewOsFs()
				path := filepath.Join(t.TempDir(), "erpc."+format)
				src := fmt.Sprintf("projects:\n  - id: main\n    providers:\n      - id: catalog\n        vendor: drpc\n        settings:\n          recheckInterval: %s\n", tt.value)
				if format == "ts" {
					src = fmt.Sprintf(`export default {projects: [{id: "main", providers: [{id: "catalog", vendor: "drpc", settings: {recheckInterval: %s}}]}]};`, tt.value)
				}
				require.NoError(t, afero.WriteFile(fs, path, []byte(src), 0o600))
				cfg, err := LoadConfig(fs, path, &DefaultOptions{})
				if tt.bad {
					require.ErrorContains(t, err, "provider 'catalog': settings.recheckInterval")
					return
				}
				require.NoError(t, err)
				got, err := cfg.Projects[0].Providers[0].Settings.RecheckInterval(24 * time.Hour)
				require.NoError(t, err)
				require.Equal(t, tt.want, got)
			})
		}
	}
}
