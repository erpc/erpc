package logfilter

import (
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func filt(t *testing.T, paramJSON string) *LogsFilter {
	t.Helper()
	var params []any
	require.NoError(t, common.SonicCfg.Unmarshal([]byte(`[`+paramJSON+`]`), &params))
	return Parse(params)
}

// The canonical examples from the Ethereum JSON-RPC spec: a log with topics
// [A, B] is matched by [] / [A] / [null, B] / [A, B] / [[A, C], [B, D]].
func TestLogsFilter_GethSemantics(t *testing.T) {
	addr, topics := "0xaddr", []string{"0xa", "0xb"}

	cases := []struct {
		name   string
		filter string
		match  bool
	}{
		{"empty topics matches", `{"topics":[]}`, true},
		{"absent topics matches", `{}`, true},
		{"[A] matches", `{"topics":["0xa"]}`, true},
		{"[null, B] matches", `{"topics":[null,"0xb"]}`, true},
		{"[A, B] matches", `{"topics":["0xa","0xb"]}`, true},
		{"[[A,C],[B,D]] OR matches", `{"topics":[["0xa","0xc"],["0xb","0xd"]]}`, true},
		{"[C] no match", `{"topics":["0xc"]}`, false},
		{"[A, D] no match", `{"topics":["0xa","0xd"]}`, false},
		{"filter longer than log topics never matches (geth)", `{"topics":["0xa","0xb",null]}`, false},
		{"case-insensitive topic compare", `{"topics":["0xA"]}`, true},
		{"address single match", `{"address":"0xADDR"}`, true},
		{"address OR-list match", `{"address":["0xother","0xaddr"]}`, true},
		{"address mismatch", `{"address":"0xother"}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := filt(t, tc.filter)
			require.NotNil(t, f)
			assert.Equal(t, tc.match, f.Matches(addr, topics))
		})
	}
}

func TestLogsFilter_ParseEdges(t *testing.T) {
	t.Run("concrete range", func(t *testing.T) {
		f := filt(t, `{"fromBlock":"0x10","toBlock":"0x12"}`)
		require.NotNil(t, f)
		from, to, ok := f.ConcreteRange()
		require.True(t, ok)
		assert.Equal(t, int64(0x10), from)
		assert.Equal(t, int64(0x12), to)
	})
	t.Run("tag range is non-concrete", func(t *testing.T) {
		f := filt(t, `{"fromBlock":"0x10","toBlock":"latest"}`)
		require.NotNil(t, f)
		_, _, ok := f.ConcreteRange()
		assert.False(t, ok)
	})
	t.Run("blockHash variant", func(t *testing.T) {
		f := filt(t, `{"blockHash":"0xBB"}`)
		require.NotNil(t, f)
		assert.Equal(t, "0xbb", f.BlockHash)
	})
	t.Run("non-string address entry → unparseable (nil)", func(t *testing.T) {
		assert.Nil(t, filt(t, `{"address":[1]}`))
	})
	t.Run("more than 4 topic positions → unparseable", func(t *testing.T) {
		assert.Nil(t, filt(t, `{"topics":[null,null,null,null,null]}`))
	})
	t.Run("no params → nil", func(t *testing.T) {
		assert.Nil(t, Parse(nil))
	})
}
