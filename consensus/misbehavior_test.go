package consensus

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// capturingExporter records every exported misbehavior line.
type capturingExporter struct {
	mu    sync.Mutex
	lines [][]byte
	meta  [][2]string
	err   error
}

func (c *capturingExporter) AppendWithMetadata(line []byte, method, networkId string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lines = append(c.lines, append([]byte(nil), line...))
	c.meta = append(c.meta, [2]string{method, networkId})
	return c.err
}

func (c *capturingExporter) Close() error { return nil }

func (c *capturingExporter) records(t *testing.T) []misbehaviorRecord {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]misbehaviorRecord, 0, len(c.lines))
	for _, l := range c.lines {
		var rec misbehaviorRecord
		require.NoError(t, json.Unmarshal(l, &rec))
		out = append(out, rec)
	}
	return out
}

func newPunishingExecutor(t *testing.T, cfg *config, exp misbehaviorExporter, level zerolog.Level) *executor {
	t.Helper()
	lg := zerolog.New(zerolog.NewTestWriter(t)).Level(level)
	return &executor{consensusPolicy: &consensusPolicy{
		config:                          cfg,
		logger:                          &lg,
		misbehavingUpstreamsLimiter:     &sync.Map{},
		misbehavingUpstreamsSitoutTimer: &sync.Map{},
		disputeLogLevel:                 zerolog.WarnLevel,
		exporter:                        exp,
	}}
}

func cordonState(u common.Upstream) (string, bool) {
	return u.(*common.FakeUpstream).CordonedReason()
}

func misbehaviorCount(u common.Upstream) int {
	return u.Tracker().(*common.FakeHealthTracker).MisbehaviorCount
}

func trackRound(t *testing.T, e *executor, labels metricsLabels, responses []*execResult) *slotResult {
	t.Helper()
	req := newTestRequest()
	ctx := context.WithValue(context.Background(), common.RequestContextKey, req)
	lg := zerolog.Nop()
	analysis := newConsensusAnalysis(&lg, ctx, e.config, responses)
	winner := e.determineWinner(ctx, &lg, analysis)
	e.trackAndPunishMisbehavingUpstreams(&lg, req, labels, winner, analysis)
	return winner
}

func TestMisbehavior_DissenterIsRecordedAndPunishedAfterThreshold(t *testing.T) {
	cfg := &config{
		maxParticipants:    3,
		agreementThreshold: 2,
		punishMisbehavior: &common.PunishMisbehaviorConfig{
			DisputeThreshold: 1,
			DisputeWindow:    common.Duration(time.Hour),
			SitOutPenalty:    common.Duration(time.Hour),
		},
	}
	e := newPunishingExecutor(t, cfg, nil, zerolog.Disabled)
	a := taggedUpstream("good-a")
	b := taggedUpstream("good-b")
	bad := taggedUpstream("bad-c")
	labels := metricsLabels{projectId: "p-punish", networkId: "evm:1", method: "eth_getBalance", category: "eth_getBalance",
		finalityStr: "unfinalized", finality: common.DataFinalityStateUnfinalized, userId: "u", agentName: "ag"}
	punished := telemetry.MetricConsensusUpstreamPunished.WithLabelValues("p-punish", "evm:1", "bad-c", "u", "ag")
	before := testutil.ToFloat64(punished)

	round := func() {
		w := trackRound(t, e, labels, []*execResult{
			resultFrom(t, a, "0xaa", 0),
			resultFrom(t, b, "0xaa", 1),
			resultFrom(t, bad, "0xbb", 2),
		})
		require.NoError(t, w.Error)
	}

	// First dispute consumes the single burst token: recorded, not punished.
	round()
	assert.Equal(t, 1, misbehaviorCount(bad))
	assert.Equal(t, 0, misbehaviorCount(a), "agreeing upstreams are never recorded")
	tr := bad.Tracker().(*common.FakeHealthTracker)
	assert.Equal(t, "eth_getBalance", tr.LastMisbehaviorMethod)
	assert.Equal(t, common.DataFinalityStateUnfinalized, tr.LastMisbehaviorFinality)
	_, cordoned := cordonState(bad)
	assert.False(t, cordoned, "within the dispute budget the upstream keeps serving")

	// Second dispute exceeds the budget: cordoned on every method.
	round()
	reason, cordoned := cordonState(bad)
	assert.True(t, cordoned)
	assert.Equal(t, "misbehaving in consensus", reason)
	assert.Equal(t, before+1, testutil.ToFloat64(punished))

	// Third dispute while sitting out does not punish again.
	round()
	assert.Equal(t, before+1, testutil.ToFloat64(punished), "an upstream already sitting out is not re-punished")
	assert.Equal(t, 3, misbehaviorCount(bad))
}

func TestMisbehavior_SitOutPenaltyExpiresAndUncordons(t *testing.T) {
	cfg := &config{
		maxParticipants:    3,
		agreementThreshold: 2,
		punishMisbehavior: &common.PunishMisbehaviorConfig{
			DisputeThreshold: 1,
			DisputeWindow:    common.Duration(time.Hour),
			SitOutPenalty:    common.Duration(20 * time.Millisecond),
		},
	}
	e := newPunishingExecutor(t, cfg, nil, zerolog.Disabled)
	a := taggedUpstream("good-a")
	b := taggedUpstream("good-b")
	bad := taggedUpstream("bad-c")
	for i := 0; i < 2; i++ {
		trackRound(t, e, metricsLabels{method: "eth_getBalance"}, []*execResult{
			resultFrom(t, a, "0xaa", 0),
			resultFrom(t, b, "0xaa", 1),
			resultFrom(t, bad, "0xbb", 2),
		})
	}
	_, cordoned := cordonState(bad)
	require.True(t, cordoned)

	require.Eventually(t, func() bool {
		_, c := cordonState(bad)
		_, stillSitting := e.misbehavingUpstreamsSitoutTimer.Load("bad-c")
		return !c && !stillSitting
	}, 5*time.Second, 5*time.Millisecond, "penalty must end with the upstream uncordoned and forgotten")
}

func TestMisbehavior_PunishmentGates(t *testing.T) {
	a := taggedUpstream("ups-a")
	b := taggedUpstream("ups-b")
	c := taggedUpstream("ups-c")
	d := taggedUpstream("ups-d")

	cases := []struct {
		name   string
		punish *common.PunishMisbehaviorConfig
		resps  func(t *testing.T) []*execResult
	}{
		{"punishment not configured", nil, nil},
		{"zero dispute threshold disables punishment",
			&common.PunishMisbehaviorConfig{DisputeThreshold: 0, DisputeWindow: common.Duration(time.Hour)}, nil},
		{"non-positive dispute window disables punishment",
			&common.PunishMisbehaviorConfig{DisputeThreshold: 1, DisputeWindow: 0}, nil},
		{"winner without a strict majority of valid participants is not enforced",
			&common.PunishMisbehaviorConfig{DisputeThreshold: 1, DisputeWindow: common.Duration(time.Hour), SitOutPenalty: common.Duration(time.Hour)},
			func(t *testing.T) []*execResult {
				return []*execResult{
					resultFrom(t, a, "0xaa", 0),
					resultFrom(t, b, "0xaa", 1),
					resultFrom(t, c, "0xbb", 2),
					resultFrom(t, d, "0xcc", 3),
				}
			}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config{maxParticipants: 4, agreementThreshold: 2, punishMisbehavior: tt.punish,
				disputeBehavior: common.ConsensusDisputeBehaviorAcceptMostCommonValidResult}
			e := newPunishingExecutor(t, cfg, nil, zerolog.Disabled)
			bad := taggedUpstream("bad")
			resps := tt.resps
			if resps == nil {
				resps = func(t *testing.T) []*execResult {
					return []*execResult{
						resultFrom(t, a, "0xaa", 0),
						resultFrom(t, b, "0xaa", 1),
						resultFrom(t, bad, "0xbb", 2),
					}
				}
			}
			for i := 0; i < 3; i++ {
				trackRound(t, e, metricsLabels{method: "eth_getBalance"}, resps(t))
			}
			for _, u := range []common.Upstream{bad, c, d} {
				_, cordoned := cordonState(u)
				assert.False(t, cordoned, "%s must not be cordoned", u.Id())
			}
			_, limiterCreated := e.misbehavingUpstreamsLimiter.Load("bad")
			assert.False(t, limiterCreated, "no dispute limiter is created while punishment is gated off")
		})
	}
}

func TestMisbehavior_ErrorsAreCountedNotPunished(t *testing.T) {
	cfg := &config{
		maxParticipants:    4,
		agreementThreshold: 2,
		punishMisbehavior: &common.PunishMisbehaviorConfig{
			DisputeThreshold: 1, DisputeWindow: common.Duration(time.Hour), SitOutPenalty: common.Duration(time.Hour),
		},
	}
	e := newPunishingExecutor(t, cfg, nil, zerolog.Disabled)
	a := taggedUpstream("ups-a")
	b := taggedUpstream("ups-b")
	reverting := taggedUpstream("ups-revert")
	flaky := taggedUpstream("ups-flaky")
	labels := metricsLabels{projectId: "p-errs", networkId: "evm:1", category: "eth_call", finalityStr: "unknown", method: "eth_call"}

	errCount := func(ups, rtype, code string) float64 {
		return testutil.ToFloat64(telemetry.MetricConsensusUpstreamErrors.WithLabelValues(
			"p-errs", "evm:1", ups, "eth_call", "unknown", rtype, code, "", ""))
	}
	revertBefore := errCount("ups-revert", "consensus_error", "ErrEndpointExecutionException")
	timeoutBefore := errCount("ups-flaky", "infrastructure_error", "timeout")

	for i := 0; i < 3; i++ {
		trackRound(t, e, labels, []*execResult{
			resultFrom(t, a, "0xaa", 0),
			resultFrom(t, b, "0xaa", 1),
			errorFrom(reverting, revertedErr(), 2),
			errorFrom(flaky, errors.New("i/o timeout"), 3),
		})
	}

	assert.Equal(t, revertBefore+3, errCount("ups-revert", "consensus_error", "ErrEndpointExecutionException"))
	assert.Equal(t, timeoutBefore+3, errCount("ups-flaky", "infrastructure_error", "timeout"))
	for _, u := range []common.Upstream{reverting, flaky} {
		assert.Equal(t, 0, misbehaviorCount(u), "errors are not data misbehavior")
		_, cordoned := cordonState(u)
		assert.False(t, cordoned)
	}
}

func TestMisbehavior_UpstreamErrorCodeLabels(t *testing.T) {
	ups := taggedUpstream("ups-x")
	cases := []struct {
		name string
		err  error
		code string
	}{
		{"missing data", missingDataFrom(ups, "header not found"), "ErrEndpointMissingData"},
		{"server side", common.NewErrEndpointServerSideException(errors.New("boom"), nil, 500), "ErrEndpointServerSideException"},
		{"other standard error", common.NewErrUpstreamsExhaustedWithCause(errors.New("x")), string(common.ErrCodeUpstreamsExhausted)},
		{"block not found text", errors.New("block not found"), "block_not_found"},
		{"opaque error fingerprint", errors.New("weird failure"), common.ErrorFingerprint(errors.New("weird failure"))},
	}
	for i, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			project := "p-codes-" + string(rune('a'+i))
			cfg := &config{maxParticipants: 3, agreementThreshold: 2}
			e := newPunishingExecutor(t, cfg, nil, zerolog.Disabled)
			labels := metricsLabels{projectId: project, networkId: "n", category: "m", finalityStr: "f", method: "m"}
			// response_type label depends on classification; sum both possible
			// values. Counters are process-global, so assert the delta.
			count := func() float64 {
				return testutil.ToFloat64(telemetry.MetricConsensusUpstreamErrors.WithLabelValues(project, "n", "ups-x", "m", "f",
					ResponseTypeConsensusError.String(), tt.code, "", "")) +
					testutil.ToFloat64(telemetry.MetricConsensusUpstreamErrors.WithLabelValues(project, "n", "ups-x", "m", "f",
						ResponseTypeInfrastructureError.String(), tt.code, "", ""))
			}
			before := count()
			trackRound(t, e, labels, []*execResult{
				resultFrom(t, taggedUpstream("ups-a"), "0xaa", 0),
				resultFrom(t, taggedUpstream("ups-b"), "0xaa", 1),
				errorFrom(ups, tt.err, 2),
			})
			assert.Equal(t, before+1, count(), "error must be labelled with code %q", tt.code)
		})
	}
}

func TestMisbehavior_AllInfrastructureErrorsSkipTracking(t *testing.T) {
	exp := &capturingExporter{}
	cfg := &config{maxParticipants: 2, agreementThreshold: 2}
	e := newPunishingExecutor(t, cfg, exp, zerolog.Disabled)
	a := taggedUpstream("ups-a")
	b := taggedUpstream("ups-b")
	trackRound(t, e, metricsLabels{projectId: "p-infra"}, []*execResult{
		errorFrom(a, errors.New("dial timeout"), 0),
		errorFrom(b, errors.New("connection refused"), 1),
	})
	assert.Empty(t, exp.records(t))
}

func TestMisbehavior_ExportsFullRecordForDisagreement(t *testing.T) {
	exp := &capturingExporter{}
	cfg := &config{maxParticipants: 4, agreementThreshold: 2, disputeBehavior: common.ConsensusDisputeBehaviorReturnError,
		ignoreFields: map[string][]string{"eth_getLogs": {"blockTimestamp"}}}
	// Debug logger so the dispute log path (warn level) is emitted too.
	e := newPunishingExecutor(t, cfg, exp, zerolog.DebugLevel)

	a := common.NewFakeUpstream("ups-a")
	a.Config().VendorName = "alchemy"
	b := taggedUpstream("ups-b")
	empty := taggedUpstream("ups-empty")
	failing := taggedUpstream("ups-fail")

	labels := metricsLabels{projectId: "p-export", networkId: "evm:1", method: "eth_getLogs", category: "eth_getLogs",
		finalityStr: "finalized", userId: "user-1"}
	winner := trackRound(t, e, labels, []*execResult{
		resultFrom(t, a, "0xaa", 0),
		resultFrom(t, b, "0xaa", 1),
		resultOf(t, empty, []interface{}{}, 2),
		errorFrom(failing, errors.New("dial timeout"), 3),
	})
	require.NoError(t, winner.Error)

	recs := exp.records(t)
	require.Len(t, recs, 1, "one record per round with misbehavior")
	assert.Equal(t, [2]string{"eth_getLogs", "evm:1"}, exp.meta[0])
	rec := recs[0]
	assert.Equal(t, "p-export", rec.ProjectID)
	assert.Equal(t, "user-1", rec.UserId)
	assert.Equal(t, "finalized", rec.Finality)
	assert.Equal(t, 4, rec.Policy.MaxParticipants)
	assert.Equal(t, 2, rec.Policy.AgreementThreshold)
	assert.Equal(t, common.ConsensusDisputeBehaviorReturnError, rec.Policy.DisputeBehavior)
	assert.Equal(t, []string{"blockTimestamp"}, rec.Policy.IgnoreFields["eth_getLogs"])
	assert.Contains(t, string(rec.Request), `"eth_getLogs"`)

	assert.Equal(t, "non_empty", rec.Winner.ResponseType)
	assert.NotEmpty(t, rec.Winner.Hash)
	assert.Positive(t, rec.Winner.Size)
	assert.Equal(t, 4, rec.Analysis.TotalParticipants)
	assert.Equal(t, 3, rec.Analysis.ValidParticipants)
	require.NotNil(t, rec.Analysis.BestByCount)
	assert.Equal(t, 2, rec.Analysis.BestByCount.Count)
	assert.Len(t, rec.Analysis.Groups, 3)

	byID := map[string]participantSnapshot{}
	for _, p := range rec.Participants {
		byID[p.UpstreamID] = p
	}
	require.Len(t, byID, 4)
	assert.Equal(t, "alchemy", byID["ups-a"].Vendor)
	assert.JSONEq(t, `"0xaa"`, string(byID["ups-a"].Response))
	assert.Equal(t, "empty", byID["ups-empty"].ResponseType)
	assert.JSONEq(t, `[]`, string(byID["ups-empty"].Response))
	assert.Equal(t, "infrastructure_error", byID["ups-fail"].ResponseType)
	assert.Contains(t, string(byID["ups-fail"].Response), "dial timeout",
		"errored participants carry the error as their body")
}

func TestMisbehavior_ExportRecordsErrorWinnerAndSurvivesExporterFailure(t *testing.T) {
	exp := &capturingExporter{err: errors.New("disk full")}
	cfg := &config{maxParticipants: 3, agreementThreshold: 2}
	e := newPunishingExecutor(t, cfg, exp, zerolog.Disabled)

	// Two empties agree; a non-empty dissents. Winner is the empty result.
	winner := trackRound(t, e, metricsLabels{method: "eth_getLogs"}, []*execResult{
		resultOf(t, taggedUpstream("ups-a"), []interface{}{}, 0),
		resultOf(t, taggedUpstream("ups-b"), []interface{}{}, 1),
		resultFrom(t, taggedUpstream("ups-c"), "0xdata", 2),
	})
	require.NoError(t, winner.Error)
	recs := exp.records(t)
	require.Len(t, recs, 1, "the record is attempted even when the exporter fails")
	assert.Equal(t, "empty", recs[0].Winner.ResponseType)

	// Synthesized error winner (no group holds it) uses the best data group as
	// the consensus reference and records the error as the winner.
	lg := zerolog.Nop()
	req := newTestRequest()
	ctx := context.WithValue(context.Background(), common.RequestContextKey, req)
	analysis := newConsensusAnalysis(&lg, ctx, cfg, []*execResult{
		resultFrom(t, taggedUpstream("ups-a"), "0xaa", 0),
		resultFrom(t, taggedUpstream("ups-b"), "0xaa", 1),
		resultFrom(t, taggedUpstream("ups-c"), "0xbb", 2),
	})
	errWinner := &slotResult{Error: common.NewErrConsensusDispute("forced", nil, nil)}
	e.trackAndPunishMisbehavingUpstreams(&lg, req, metricsLabels{method: "eth_getLogs"}, errWinner, analysis)
	recs = exp.records(t)
	require.Len(t, recs, 2)
	assert.Equal(t, "consensus_error", recs[1].Winner.ResponseType)
	assert.Equal(t, string(common.ErrCodeConsensusDispute), recs[1].Winner.Hash)
}

func TestFileMisbehaviorExporter_Lifecycle(t *testing.T) {
	lg := zerolog.Nop()

	t.Run("rejects empty and relative paths", func(t *testing.T) {
		_, err := newFileMisbehaviorExporter(nil, &lg, defaultPolicyName)
		require.Error(t, err)
		_, err = newFileMisbehaviorExporter(&common.MisbehaviorsDestinationConfig{}, &lg, defaultPolicyName)
		require.Error(t, err)
		_, err = newFileMisbehaviorExporter(&common.MisbehaviorsDestinationConfig{Path: "relative/dir"}, &lg, defaultPolicyName)
		require.ErrorContains(t, err, "must be absolute")
	})

	t.Run("fails when the base path cannot be created", func(t *testing.T) {
		blocker := filepath.Join(t.TempDir(), "file")
		require.NoError(t, os.WriteFile(blocker, []byte("x"), 0o600))
		_, err := newFileMisbehaviorExporter(&common.MisbehaviorsDestinationConfig{Path: filepath.Join(blocker, "sub")}, &lg, defaultPolicyName)
		require.Error(t, err)
	})

	t.Run("appends one line per record per resolved file and closes handles", func(t *testing.T) {
		dir := t.TempDir()
		exp, err := newFileMisbehaviorExporter(&common.MisbehaviorsDestinationConfig{
			Path: dir, FilePattern: "{method}-{networkId}",
		}, &lg, defaultPolicyName)
		require.NoError(t, err)
		require.NoError(t, exp.AppendWithMetadata([]byte(`{"n":1}`), "eth_call", "evm:1"))
		require.NoError(t, exp.AppendWithMetadata([]byte(`{"n":2}`), "eth_call", "evm:1"))
		require.NoError(t, exp.AppendWithMetadata([]byte(`{"n":3}`), "eth_getLogs", "evm:1"))
		require.NoError(t, exp.Close())
		assert.Empty(t, exp.hub.handles)

		readLines := func(name string) []string {
			f, err := os.Open(filepath.Join(dir, name))
			require.NoError(t, err)
			defer f.Close()
			var out []string
			s := bufio.NewScanner(f)
			for s.Scan() {
				out = append(out, s.Text())
			}
			return out
		}
		assert.Equal(t, []string{`{"n":1}`, `{"n":2}`}, readLines("eth_call-evm_1.jsonl"))
		assert.Equal(t, []string{`{"n":3}`}, readLines("eth_getLogs-evm_1.jsonl"))
	})

	t.Run("reports an error when the target file cannot be opened", func(t *testing.T) {
		dir := t.TempDir()
		exp, err := newFileMisbehaviorExporter(&common.MisbehaviorsDestinationConfig{Path: dir, FilePattern: "{method}"}, &lg, defaultPolicyName)
		require.NoError(t, err)
		// A directory squatting on the resolved file name makes open fail.
		require.NoError(t, os.Mkdir(filepath.Join(dir, "eth_call.jsonl"), 0o750))
		err = exp.AppendWithMetadata([]byte(`{}`), "eth_call", "evm:1")
		require.ErrorContains(t, err, "failed to open file")
	})
}

func TestCreateMisbehaviorExporter_SelectsImplementation(t *testing.T) {
	lg := zerolog.Nop()

	assert.Nil(t, createMisbehaviorExporter(nil, &lg, defaultPolicyName))
	assert.Nil(t, createMisbehaviorExporter(&common.MisbehaviorsDestinationConfig{}, &lg, defaultPolicyName))
	assert.Nil(t, NewMisbehaviorExporter(&common.MisbehaviorsDestinationConfig{
		Type: common.MisbehaviorsDestinationTypeFile, Path: "not-absolute",
	}, &lg), "a file exporter that fails to initialize is disabled")
	assert.Nil(t, NewMisbehaviorExporter(&common.MisbehaviorsDestinationConfig{
		Type: common.MisbehaviorsDestinationTypeS3, Path: "not-an-s3-uri",
	}, &lg), "an S3 exporter with an invalid URI is disabled")

	exp := NewMisbehaviorExporter(&common.MisbehaviorsDestinationConfig{Path: t.TempDir()}, &lg)
	require.NotNil(t, exp)
	_, isFile := exp.(*fileMisbehaviorExporter)
	assert.True(t, isFile, "untyped destinations default to the file exporter")
}

func TestParseS3Path(t *testing.T) {
	cases := []struct {
		in         string
		bucket     string
		prefix     string
		wantErrStr string
	}{
		{in: "s3://bucket", bucket: "bucket"},
		{in: "s3://bucket/", bucket: "bucket"},
		{in: "s3://bucket/a/b", bucket: "bucket", prefix: "a/b/"},
		{in: "s3://bucket/a/b/", bucket: "bucket", prefix: "a/b/"},
		{in: "s3://", wantErrStr: "no bucket"},
		{in: "/local/path", wantErrStr: "must start with s3://"},
	}
	for _, tt := range cases {
		t.Run(tt.in, func(t *testing.T) {
			bucket, prefix, err := parseS3Path(tt.in)
			if tt.wantErrStr != "" {
				require.ErrorContains(t, err, tt.wantErrStr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.bucket, bucket)
			assert.Equal(t, tt.prefix, prefix)
		})
	}
}

func TestNewConsensus_AppliesConfig(t *testing.T) {
	lg := zerolog.Nop()

	_, err := NewConsensus(nil, &lg, "")
	require.Error(t, err)

	yes := true
	dir := t.TempDir()
	c, err := NewConsensus(&common.ConsensusPolicyConfig{
		MaxParticipants:         3,
		AgreementThreshold:      2,
		DisputeBehavior:         common.ConsensusDisputeBehaviorAcceptMostCommonValidResult,
		LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorReturnError,
		PunishMisbehavior:       &common.PunishMisbehaviorConfig{DisputeThreshold: 3},
		DisputeLogLevel:         "info",
		IgnoreFields:            map[string][]string{"eth_getBlockByNumber": {"timestamp"}},
		PreferNonEmpty:          &yes,
		PreferLargerResponses:   &yes,
		PreferHighestValueFor:   map[string][]string{"eth_getTransactionCount": {"result"}},
		MisbehaviorsDestination: &common.MisbehaviorsDestinationConfig{Path: dir},
		RequiredParticipants:    []*common.ConsensusRequiredParticipant{{Tag: "t", MinParticipants: 1}},
	}, &lg, "")
	require.NoError(t, err)
	p := c.policy
	assert.Equal(t, 3, p.maxParticipants)
	assert.Equal(t, 2, p.agreementThreshold)
	assert.Equal(t, common.ConsensusDisputeBehaviorAcceptMostCommonValidResult, p.disputeBehavior)
	assert.Equal(t, common.ConsensusLowParticipantsBehaviorReturnError, p.lowParticipantsBehavior)
	assert.Equal(t, uint(3), p.punishMisbehavior.DisputeThreshold)
	assert.Equal(t, zerolog.InfoLevel, p.disputeLogLevel)
	assert.True(t, p.preferNonEmpty)
	assert.True(t, p.preferLargerResponses)
	assert.Contains(t, p.ignoreFields, "eth_getBlockByNumber")
	assert.Contains(t, p.preferHighestValueFor, "eth_getTransactionCount")
	assert.Len(t, p.requiredParticipants, 1)
	assert.NotNil(t, p.exporter, "a valid file destination enables export")

	t.Run("invalid dispute log level keeps the warn default", func(t *testing.T) {
		c, err := NewConsensus(&common.ConsensusPolicyConfig{MaxParticipants: 1, DisputeLogLevel: "loud"}, &lg, "")
		require.NoError(t, err)
		assert.Equal(t, zerolog.WarnLevel, c.policy.disputeLogLevel)
		assert.Nil(t, c.policy.exporter)
	})

	t.Run("nil consensus passes straight to the inner function", func(t *testing.T) {
		var nilC *Consensus
		want := validResponseWithValue("0xinner")
		got, err := nilC.Run(context.Background(), newTestRequest(),
			func(context.Context, *common.NormalizedRequest) (*common.NormalizedResponse, error) { return want, nil })
		require.NoError(t, err)
		assert.Same(t, want, got)
	})
}

// TestSitout_StaleExpiryDoesNotClearNewerPunishment pins Copilot's finding:
// Timer.Stop does not wait for an already-running expiry callback, so an old
// callback could clear a newer punishment. Expiry now CompareAndDeletes its
// own timer entry, so a callback whose entry was reset (admin uncordon) or
// replaced (new punishment) cannot tear down the current state.
func TestSitout_StaleExpiryDoesNotClearNewerPunishment(t *testing.T) {
	cfg := &config{
		maxParticipants:    3,
		agreementThreshold: 2,
		punishMisbehavior: &common.PunishMisbehaviorConfig{
			DisputeThreshold: 1,
			DisputeWindow:    common.Duration(time.Hour),
			SitOutPenalty:    common.Duration(time.Hour),
		},
	}
	e := newPunishingExecutor(t, cfg, nil, zerolog.Disabled)
	a := taggedUpstream("good-a")
	b := taggedUpstream("good-b")
	bad := taggedUpstream("bad-c")

	round := func() {
		trackRound(t, e, metricsLabels{method: "eth_getBalance"}, []*execResult{
			resultFrom(t, a, "0xaa", 0),
			resultFrom(t, b, "0xaa", 1),
			resultFrom(t, bad, "0xbb", 2),
		})
	}

	// Two disputes put bad-c into sit-out; capture the first entry.
	round()
	round()
	v, ok := e.misbehavingUpstreamsSitoutTimer.Load("bad-c")
	require.True(t, ok)
	first := v.(*sitoutEntry)

	// Admin uncordon resets the sit-out; a new punishment starts a second entry.
	e.misbehavingUpstreamsSitoutTimer.Delete("bad-c")
	round()
	v, ok = e.misbehavingUpstreamsSitoutTimer.Load("bad-c")
	require.True(t, ok)
	second := v.(*sitoutEntry)
	require.NotSame(t, first, second)

	// Fire the first entry's expiry callback manually (as if Stop lost the
	// race): CompareAndDelete on the stale entry fails, so the newer
	// punishment's entry survives.
	first.timer.Stop()
	if !e.misbehavingUpstreamsSitoutTimer.CompareAndDelete("bad-c", first) {
		_, stillThere := e.misbehavingUpstreamsSitoutTimer.Load("bad-c")
		assert.True(t, stillThere, "stale expiry must not delete the newer punishment's timer")
	}
	second.timer.Stop()
}
