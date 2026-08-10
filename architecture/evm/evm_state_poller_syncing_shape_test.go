package evm

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/health"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// `eth_syncing` has no single wire shape: the spec says false-or-object, Arbitrum
// answers with `msgCount`, some clients answer `{Ok: bool}`, and the next client
// will invent another one. These tests pin that an UNRECOGNIZED shape degrades to
// "syncing state unknown" instead of failing the poll — a shape the poller cannot
// interpret used to surface as a parse error that joined Poll()'s return value,
// so Bootstrap and every later poll reported failure until the 10-consecutive-
// failure silencer permanently disabled the check. Genuine transport / JSON-RPC
// failures must still propagate, and the recognized shapes must behave exactly as
// before.

// syncingShapeUpstream answers eth_syncing with an operator-supplied raw result
// (or a transport error) and serves the other state-poller calls with valid
// data, so Poll() fails only when the syncing check itself fails.
type syncingShapeUpstream struct {
	cfg    *common.UpstreamConfig
	logger zerolog.Logger

	mu            sync.Mutex
	syncingResult string
	syncingErr    error
	// jsonRpcError, when set, is returned as the JSON-RPC `error` object of an
	// otherwise successful eth_syncing response.
	jsonRpcError string

	syncingCalls atomic.Int64
}

func newSyncingShapeUpstream(result string) *syncingShapeUpstream {
	return &syncingShapeUpstream{
		cfg: &common.UpstreamConfig{
			Id:   "test-ups",
			Type: common.UpstreamTypeEvm,
			Evm:  &common.EvmUpstreamConfig{ChainId: 123},
		},
		logger:        zerolog.Nop(),
		syncingResult: result,
	}
}

func (u *syncingShapeUpstream) setSyncingResult(result string, err error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.syncingResult = result
	u.syncingErr = err
}

func (u *syncingShapeUpstream) Id() string                     { return u.cfg.Id }
func (u *syncingShapeUpstream) VendorName() string             { return "test" }
func (u *syncingShapeUpstream) NetworkId() string              { return "evm:123" }
func (u *syncingShapeUpstream) NetworkLabel() string           { return "evm:123" }
func (u *syncingShapeUpstream) Config() *common.UpstreamConfig { return u.cfg }
func (u *syncingShapeUpstream) Logger() *zerolog.Logger        { return &u.logger }
func (u *syncingShapeUpstream) Vendor() common.Vendor          { return nil }
func (u *syncingShapeUpstream) Tracker() common.HealthTracker  { return nil }
func (u *syncingShapeUpstream) Cordon(_, _ string)             {}
func (u *syncingShapeUpstream) Uncordon(_, _ string)           {}
func (u *syncingShapeUpstream) IgnoreMethod(_ string)          {}
func (u *syncingShapeUpstream) ShouldHandleMethod(_ string) (bool, error) {
	return true, nil
}

func (u *syncingShapeUpstream) Forward(_ context.Context, nq *common.NormalizedRequest, _, _ bool) (*common.NormalizedResponse, error) {
	method, err := nq.Method()
	if err != nil {
		return nil, err
	}
	switch method {
	case "eth_syncing":
		u.syncingCalls.Add(1)
		u.mu.Lock()
		result, syncErr, rpcErr := u.syncingResult, u.syncingErr, u.jsonRpcError
		u.mu.Unlock()
		if syncErr != nil {
			return nil, syncErr
		}
		if rpcErr != "" {
			jrr := common.MustNewJsonRpcResponseFromBytes([]byte("1"), nil, []byte(rpcErr))
			return common.NewNormalizedResponse().WithRequest(nq).WithJsonRpcResponse(jrr), nil
		}
		return jsonRpcResult(nq, result), nil
	case "eth_getBlockByNumber":
		return jsonRpcResult(nq, `{"number":"0x1000","timestamp":"0x6702a8f0"}`), nil
	case "eth_chainId":
		return jsonRpcResult(nq, `"0x7b"`), nil
	}
	return nil, fmt.Errorf("unexpected method in test upstream: %s", method)
}

var _ common.Upstream = (*syncingShapeUpstream)(nil)

func jsonRpcResult(req *common.NormalizedRequest, raw string) *common.NormalizedResponse {
	jrr := common.MustNewJsonRpcResponseFromBytes([]byte("1"), []byte(raw), nil)
	return common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)
}

func newSyncingShapePoller(t *testing.T, up common.Upstream) *EvmStatePoller {
	t.Helper()
	appCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	logger := zerolog.Nop()
	tracker := health.NewTracker(&logger, "test", 2*time.Second)
	ssr, err := data.NewSharedStateRegistry(appCtx, &logger, &common.SharedStateConfig{
		Connector: &common.ConnectorConfig{
			Driver: common.DriverMemory,
			Memory: &common.MemoryConnectorConfig{MaxItems: 100_000, MaxTotalSize: "1GB"},
		},
	})
	require.NoError(t, err)
	return NewEvmStatePoller("test", appCtx, &logger, up, tracker, ssr)
}

func TestEvmStatePoller_UnrecognizedSyncingShapeDoesNotFailPoll(t *testing.T) {
	shapes := []string{
		`{"stage":"headers","progress":0.4}`, // a client we have never seen
		`"syncing"`,                          // string instead of bool/object
		`[]`,                                 // array
		`null`,                               // JSON null
		`123`,                                // number
	}

	for _, shape := range shapes {
		t.Run(shape, func(t *testing.T) {
			up := newSyncingShapeUpstream(shape)
			p := newSyncingShapePoller(t, up)

			// Well past the 10-consecutive-failure silencer threshold.
			for range 12 {
				require.NoError(t, p.Poll(context.Background()),
					"an eth_syncing shape we cannot interpret must not fail the poll")
			}

			assert.Equal(t, common.EvmSyncingStateUnknown, p.SyncingState(),
				"an uninterpretable answer is evidence of nothing")

			assert.False(t, p.GetDiagnostics().SkipSyncingCheck,
				"an unrecognized shape must not trip the failure silencer")
			assert.Equal(t, 0, syncingFailureCount(p),
				"an unrecognized shape is not a failure")
			assert.Equal(t, int64(12), up.syncingCalls.Load(),
				"the poller must keep asking — the shape may become recognizable")
		})
	}
}

func TestEvmStatePoller_RecognizedSyncingShapesUnchanged(t *testing.T) {
	cases := []struct {
		name   string
		result string
		polls  int
		expect common.EvmSyncingState
	}{
		{name: "BoolFalseNeedsFourConfirmations", result: `false`, polls: 3, expect: common.EvmSyncingStateUnknown},
		{name: "BoolFalseFullySyncedAfterFour", result: `false`, polls: 4, expect: common.EvmSyncingStateNotSyncing},
		{name: "BoolTrueIsSyncing", result: `true`, polls: 1, expect: common.EvmSyncingStateSyncing},
		{name: "ObjectWithCurrentBlockIsSyncing", result: `{"currentBlock":"0x1","highestBlock":"0x2"}`, polls: 1, expect: common.EvmSyncingStateSyncing},
		{name: "ObjectWithMsgCountIsSyncing", result: `{"msgCount":42}`, polls: 1, expect: common.EvmSyncingStateSyncing},
		{name: "ObjectOkFalseIsSyncing", result: `{"Ok":false}`, polls: 1, expect: common.EvmSyncingStateSyncing},
		{name: "ObjectOkTrueIsNotSyncingAfterFour", result: `{"ok":true}`, polls: 4, expect: common.EvmSyncingStateNotSyncing},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up := newSyncingShapeUpstream(tc.result)
			p := newSyncingShapePoller(t, up)

			for range tc.polls {
				require.NoError(t, p.Poll(context.Background()))
			}

			assert.Equal(t, tc.expect, p.SyncingState())
			assert.False(t, p.GetDiagnostics().SkipSyncingCheck)
		})
	}
}

// The pay-off of not counting unknown shapes as failures: a client that answers
// something we cannot read WHILE syncing, and a plain `false` once caught up, is
// still detected as synced. Under the old behavior the syncing check was
// permanently disabled after 10 polls and the upstream stayed Unknown forever.
func TestEvmStatePoller_UnrecognizedShapeThenRecognizedShapeIsDetected(t *testing.T) {
	up := newSyncingShapeUpstream(`{"stage":"headers","progress":0.4}`)
	p := newSyncingShapePoller(t, up)

	for range 15 {
		require.NoError(t, p.Poll(context.Background()))
	}
	require.Equal(t, common.EvmSyncingStateUnknown, p.SyncingState())

	up.setSyncingResult(`false`, nil)
	for range FullySyncedThreshold {
		require.NoError(t, p.Poll(context.Background()))
	}

	assert.Equal(t, common.EvmSyncingStateNotSyncing, p.SyncingState(),
		"the upstream must still be discoverable as synced after unreadable answers")
}

// Genuine failures keep their old semantics: they fail the poll and, with no
// prior success, permanently disable the check after 10 of them.
func TestEvmStatePoller_SyncingTransportErrorsStillTripSkip(t *testing.T) {
	up := newSyncingShapeUpstream(`false`)
	up.setSyncingResult("", errors.New("connection refused"))
	p := newSyncingShapePoller(t, up)

	for i := range 10 {
		err := p.Poll(context.Background())
		require.Error(t, err, "a transport failure must still fail the poll (attempt %d)", i+1)
		require.True(t, strings.Contains(err.Error(), "connection refused"))
	}

	diag := p.GetDiagnostics()
	assert.True(t, diag.SkipSyncingCheck,
		"10 consecutive genuine failures must still disable the syncing check")
	assert.Equal(t, common.EvmSyncingStateUnknown, p.SyncingState())

	// Once skipped, no further eth_syncing calls are made.
	calls := up.syncingCalls.Load()
	require.NoError(t, p.Poll(context.Background()))
	assert.Equal(t, calls, up.syncingCalls.Load())
}

// A JSON-RPC error object is a genuine failure, not an unreadable shape.
func TestEvmStatePoller_SyncingJsonRpcErrorStillFailsPoll(t *testing.T) {
	up := newSyncingShapeUpstream(`false`)
	up.jsonRpcError = `{"code":-32000,"message":"method handler crashed"}`
	p := newSyncingShapePoller(t, up)

	err := p.Poll(context.Background())
	require.Error(t, err, "a JSON-RPC error response must still fail the poll")
	assert.Equal(t, 1, syncingFailureCount(p))
}

// syncingFailureCount reads the consecutive-failure counter that drives the
// skip-after-10 silencer. Safe to read unlocked: Poll joins all its goroutines
// before returning, so no writer is live at call time.
func syncingFailureCount(p *EvmStatePoller) int {
	p.stateMu.RLock()
	defer p.stateMu.RUnlock()
	return p.syncingFailureCount
}
