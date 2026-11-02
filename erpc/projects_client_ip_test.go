package erpc

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/thirdparty"
	"github.com/erpc/erpc/upstream"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// lockedBuffer is a bytes.Buffer safe for concurrent writes from the logger.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestProject_ForwardLogsClientIP(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	gock.New("http://rpc1.localhost").
		Post("").
		Filter(func(r *http.Request) bool {
			return strings.Contains(util.SafeReadBody(r), "eth_getBalance")
		}).
		Reply(200).
		JSON(map[string]interface{}{"jsonrpc": "2.0", "id": 1, "result": "0x1"})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// util's init disables logging in tests
	prev := zerolog.GlobalLevel()
	zerolog.SetGlobalLevel(zerolog.InfoLevel)
	defer zerolog.SetGlobalLevel(prev)

	out := &lockedBuffer{}
	lg := zerolog.New(out).Level(zerolog.InfoLevel)

	rlr, err := upstream.NewRateLimitersRegistry(ctx, &common.RateLimiterConfig{}, &lg)
	require.NoError(t, err)
	ssr, err := data.NewSharedStateRegistry(ctx, &lg, &common.SharedStateConfig{
		Connector: &common.ConnectorConfig{
			Driver: "memory",
			Memory: &common.MemoryConnectorConfig{MaxItems: 100_000, MaxTotalSize: "1GB"},
		},
	})
	require.NoError(t, err)
	prjReg, err := NewProjectsRegistry(
		ctx,
		&lg,
		[]*common.ProjectConfig{
			{
				Id: "prjA",
				Networks: []*common.NetworkConfig{
					{Architecture: common.ArchitectureEvm, Evm: &common.EvmNetworkConfig{ChainId: 123}},
				},
				Upstreams: []*common.UpstreamConfig{
					{
						Id:       "rpc1",
						Endpoint: "http://rpc1.localhost",
						Type:     common.UpstreamTypeEvm,
						Evm:      &common.EvmUpstreamConfig{ChainId: 123},
					},
				},
			},
		},
		ssr,
		nil, // evmJsonRpcCache
		nil, // svmJsonRpcCache
		rlr,
		thirdparty.NewVendorsRegistry(),
		nil, // ProxyPoolRegistry
		nil, // userScript
	)
	require.NoError(t, err)
	prjReg.Bootstrap(ctx)
	time.Sleep(100 * time.Millisecond)

	prj, err := prjReg.GetProject("prjA")
	require.NoError(t, err)

	nq := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x0000000000000000000000000000000000000001","latest"]}`))
	nq.SetClientIP("203.0.113.7")
	_, err = prj.Forward(ctx, "evm:123", nq)
	require.NoError(t, err)

	var forwarded string
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.Contains(line, "successfully forwarded request for network") {
			forwarded = line
		}
	}
	require.NotEmpty(t, forwarded, "no 'successfully forwarded' log line")
	require.Contains(t, forwarded, `"clientIP":"203.0.113.7"`)
}
