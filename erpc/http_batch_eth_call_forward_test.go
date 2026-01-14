package erpc

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/require"
)

func TestForwardEthCallBatchCandidates(t *testing.T) {
	server := &HttpServer{serverCfg: &common.ServerConfig{IncludeErrorDetails: &common.TRUE}}
	startedAt := time.Now()

	makeCandidate := func(index int) ethCallBatchCandidate {
		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{"to":"0x0000000000000000000000000000000000000001","data":"0x"}]}`))
		ctx := common.StartRequestSpan(context.Background(), req)
		return ethCallBatchCandidate{
			index:  index,
			ctx:    ctx,
			req:    req,
			logger: log.Logger,
		}
	}

	responses := make([]interface{}, 1)
	server.forwardEthCallBatchCandidates(&startedAt, nil, nil, []ethCallBatchCandidate{makeCandidate(0)}, responses)
	require.NotNil(t, responses[0])

	origForward := forwardBatchProject
	t.Cleanup(func() {
		forwardBatchProject = origForward
	})

	t.Run("forward error", func(t *testing.T) {
		responses := make([]interface{}, 1)
		resp := common.NewNormalizedResponse()
		forwardBatchProject = func(ctx context.Context, project *PreparedProject, network *Network, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			return resp, errors.New("boom")
		}

		server.forwardEthCallBatchCandidates(&startedAt, &PreparedProject{}, &Network{}, []ethCallBatchCandidate{makeCandidate(0)}, responses)
		require.NotNil(t, responses[0])
	})

	t.Run("forward success", func(t *testing.T) {
		responses := make([]interface{}, 1)
		resp := common.NewNormalizedResponse()
		forwardBatchProject = func(ctx context.Context, project *PreparedProject, network *Network, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			return resp, nil
		}

		server.forwardEthCallBatchCandidates(&startedAt, &PreparedProject{}, &Network{}, []ethCallBatchCandidate{makeCandidate(0)}, responses)
		require.Equal(t, resp, responses[0])
	})
}
