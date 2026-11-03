package clients

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func init() {
	util.ConfigureTestLogger()
}

type phonyUpstream struct {
	common.Upstream
}

var _ common.Upstream = (*phonyUpstream)(nil)

func (u *phonyUpstream) Id() string {
	return "phony-upstream"
}

type noopErrorExtractor struct{}

func (e *noopErrorExtractor) Extract(resp *http.Response, nr *common.NormalizedResponse, jr *common.JsonRpcResponse, upstream common.Upstream) error {
	return nil
}

// TestHTTPConnectionReuseUnderLoad sends concurrent requests through the
// production transport to a slow upstream and checks that each concurrent
// request stream keeps reusing one connection.
func TestHTTPConnectionReuseUnderLoad(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const (
			workers           = 20
			requestsPerWorker = 10
			latency           = 400 * time.Millisecond
		)

		var conns atomic.Int64
		server := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(latency)
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"jsonrpc":"2.0","result":"0x1","id":1}`)
		}))
		server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
			if state == http.StateNew {
				conns.Add(1)
			}
		}

		transport := newDefaultTransport()
		transport.DialContext = server.Client().Transport.(*http.Transport).DialContext
		defer transport.CloseIdleConnections()

		parsedURL, err := url.Parse(server.URL)
		require.NoError(t, err)
		logger := zerolog.Nop()
		client := &GenericHttpJsonRpcClient{
			Url:            parsedURL,
			appCtx:         t.Context(),
			logger:         &logger,
			projectId:      "test-project",
			httpClient:     &http.Client{Timeout: 60 * time.Second, Transport: transport},
			upstream:       &phonyUpstream{},
			gzipPool:       util.NewGzipReaderPool(),
			gzipWriterPool: util.NewGzipWriterPool(),
			errorExtractor: &noopErrorExtractor{},
		}

		var wg sync.WaitGroup
		for w := range workers {
			wg.Go(func() {
				for j := range requestsPerWorker {
					req := common.NewNormalizedRequest(fmt.Appendf(nil, `{"jsonrpc":"2.0","method":"eth_blockNumber","params":[],"id":%d}`, w*requestsPerWorker+j))
					resp, err := client.SendRequest(t.Context(), req)
					if !assert.NoError(t, err) {
						return
					}
					_, _ = resp.JsonRpcResponse()
					resp.Release()
					time.Sleep(10 * time.Millisecond)
				}
			})
		}
		wg.Wait()

		assert.LessOrEqual(t, conns.Load(), int64(workers), "connections must be reused across requests")
	})
}
