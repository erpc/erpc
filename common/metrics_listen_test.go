package common

import (
	"testing"

	"github.com/erpc/erpc/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMetricsListenAddr(t *testing.T) {
	tests := []struct {
		name    string
		cfg     *MetricsConfig
		want    string
		wantErr string
	}{
		{
			name: "legacy config without listen keys keeps binding hostV4",
			cfg: &MetricsConfig{
				Port:   util.IntPtr(4001),
				HostV4: util.StringPtr("0.0.0.0"),
			},
			want: "0.0.0.0:4001",
		},
		{
			name: "listenV4 honoured",
			cfg: &MetricsConfig{
				Port:     util.IntPtr(14001),
				ListenV4: util.BoolPtr(true),
				HostV4:   util.StringPtr("127.0.0.1"),
				HostV6:   util.StringPtr("[::1]"),
				ListenV6: util.BoolPtr(false),
			},
			want: "127.0.0.1:14001",
		},
		{
			name: "listenV4 false falls through to listenV6",
			cfg: &MetricsConfig{
				Port:     util.IntPtr(14001),
				ListenV4: util.BoolPtr(false),
				HostV4:   util.StringPtr("127.0.0.1"),
				HostV6:   util.StringPtr("[::1]"),
				ListenV6: util.BoolPtr(true),
			},
			want: "[::1]:14001",
		},
		{
			name: "hostV6 only, no listen flags",
			cfg: &MetricsConfig{
				Port:   util.IntPtr(14001),
				HostV6: util.StringPtr("[::1]"),
			},
			want: "[::1]:14001",
		},
		{
			name: "missing port",
			cfg: &MetricsConfig{
				HostV4: util.StringPtr("0.0.0.0"),
			},
			wantErr: "metrics.port is required",
		},
		{
			name: "every listener switched off",
			cfg: &MetricsConfig{
				Port:     util.IntPtr(4001),
				ListenV4: util.BoolPtr(false),
				HostV4:   util.StringPtr("0.0.0.0"),
				ListenV6: util.BoolPtr(false),
			},
			wantErr: "at least one of metrics.listenV4 or metrics.listenV6",
		},
		{
			name: "listenV4 without hostV4",
			cfg: &MetricsConfig{
				Port:     util.IntPtr(4001),
				ListenV4: util.BoolPtr(true),
			},
			wantErr: "metrics.hostV4 is not set",
		},
		{
			name: "listenV6 without hostV6",
			cfg: &MetricsConfig{
				Port:     util.IntPtr(4001),
				ListenV4: util.BoolPtr(false),
				ListenV6: util.BoolPtr(true),
			},
			wantErr: "metrics.hostV6 is not set",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.cfg.ListenAddr()
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestMetricsValidateRejectsUnboundConfig(t *testing.T) {
	cfg := &MetricsConfig{
		Enabled:  util.BoolPtr(true),
		Port:     util.IntPtr(4001),
		ListenV4: util.BoolPtr(false),
		ListenV6: util.BoolPtr(false),
		HostV4:   util.StringPtr("127.0.0.1"),
	}
	require.Error(t, cfg.Validate())
	assert.Contains(t, cfg.Validate().Error(), "metrics.listenV4")
}
