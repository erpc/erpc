package data

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestExtractRateLimiterAddrAndDB(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		uri     string
		addr    string
		cfgDB   int
		wantAddr string
		wantDB  int
		wantErr bool
	}{
		{
			name:     "rediss URI from SetDefaults IAM path, DB 0",
			uri:      "rediss://my-cluster.abc123.cache.amazonaws.com:6379/0",
			wantAddr: "my-cluster.abc123.cache.amazonaws.com:6379",
			wantDB:   0,
		},
		{
			name:     "URI with non-zero DB in path",
			uri:      "rediss://host:6379/5",
			wantAddr: "host:6379",
			wantDB:   5,
		},
		{
			name:     "URI DB path takes precedence over cfgDB",
			uri:      "rediss://host:6379/3",
			cfgDB:    1,
			wantAddr: "host:6379",
			wantDB:   3,
		},
		{
			name:     "redis URI with credentials stripped",
			uri:      "redis://user:pass@localhost:6379/0",
			wantAddr: "localhost:6379",
			wantDB:   0,
		},
		{
			name:     "bare addr uses cfgDB",
			addr:     "localhost:6379",
			cfgDB:    2,
			wantAddr: "localhost:6379",
			wantDB:   2,
		},
		{
			name:    "both empty returns error",
			wantErr: true,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gotAddr, gotDB, err := extractRateLimiterAddrAndDB(tc.uri, tc.addr, tc.cfgDB)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.wantAddr, gotAddr)
			require.Equal(t, tc.wantDB, gotDB)
		})
	}
}
