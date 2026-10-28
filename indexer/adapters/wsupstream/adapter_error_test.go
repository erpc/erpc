package wsupstream

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/indexer"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAdapterEnsureFilterKeepsStructuredError: an upstream rejection of a
// filter subscribe, as the indexer hands it to the subscription manager
// (errors.Join over ingresses), must grade and label by its error code and
// reach the client with the upstream's JSON-RPC code, as on the HTTP path.
func TestAdapterEnsureFilterKeepsStructuredError(t *testing.T) {
	server := newNotifyServer(t)
	a, sink := newTestAdapter(t, server.wsURL(t))
	nop := zerolog.Nop()
	a.logger = &nop
	t.Cleanup(func() {
		a.subsMu.Lock()
		if a.resubCancel != nil {
			a.resubCancel()
		}
		a.subsMu.Unlock()
	})
	<-server.newConn
	require.Eventually(t, a.wsClient.IsConnected, 3*time.Second, 10*time.Millisecond)
	a.sink = sink

	upstreamErr := common.NewErrEndpointClientSideException(
		common.NewErrJsonRpcExceptionInternal(-32602, common.JsonRpcErrorInvalidArgument, "invalid filter", nil, nil),
	)
	a.forward = func(context.Context, *common.NormalizedRequest, bool) (*common.NormalizedResponse, error) {
		return nil, upstreamErr
	}

	params := logsParams("0xabc")
	ensureErr := a.EnsureFilter(context.Background(), indexer.SubTypeLogs, indexer.BuildParamsKey(params), params)
	require.Error(t, ensureErr)
	err := errors.Join(ensureErr) // indexer.subscribe's shape

	assert.Equal(t, common.SeverityInfo, common.ClassifySeverity(err))
	assert.Equal(t, common.ErrorFingerprint(upstreamErr), common.ErrorFingerprint(err))

	jre := &common.ErrJsonRpcExceptionInternal{}
	require.True(t, errors.As(common.TranslateToJsonRpcException(err), &jre))
	assert.EqualValues(t, common.JsonRpcErrorInvalidArgument, jre.NormalizedCode())
}
