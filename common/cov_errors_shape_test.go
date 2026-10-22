package common

import (
	"bytes"
	"context"
	"errors"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// covErrorsEmptyMulti is a multi-error wrapper whose Unwrap yields no children;
// errors.Join cannot produce one (it returns nil instead).
type covErrorsEmptyMulti struct{}

func (covErrorsEmptyMulti) Error() string   { return "empty multi" }
func (covErrorsEmptyMulti) Unwrap() []error { return nil }

func covErrorsSetLabelMode(t *testing.T, mode LabelMode) {
	t.Helper()
	SetErrorLabelMode(mode)
	t.Cleanup(func() { SetErrorLabelMode(ErrorLabelModeVerbose) })
}

func covErrorsUnmarshalObj(t *testing.T, v interface{}) map[string]interface{} {
	t.Helper()
	raw, err := SonicCfg.Marshal(v)
	require.NoError(t, err)
	var m map[string]interface{}
	require.NoError(t, SonicCfg.Unmarshal(raw, &m), string(raw))
	return m
}

func covErrorsZerolog(t *testing.T, obj zerolog.LogObjectMarshaler) map[string]interface{} {
	t.Helper()
	// Test init disables zerolog globally; enable it for this capture only.
	prev := zerolog.GlobalLevel()
	zerolog.SetGlobalLevel(zerolog.TraceLevel)
	t.Cleanup(func() { zerolog.SetGlobalLevel(prev) })
	var buf bytes.Buffer
	lg := zerolog.New(&buf)
	lg.Info().Object("err", obj).Send()
	var m map[string]interface{}
	require.NoError(t, SonicCfg.Unmarshal(buf.Bytes(), &m), buf.String())
	inner, _ := m["err"].(map[string]interface{})
	return inner
}

func TestCovErrors_SetErrorLabelMode_IgnoresUnknown(t *testing.T) {
	covErrorsSetLabelMode(t, ErrorLabelModeCompact)
	SetErrorLabelMode("bogus")
	assert.Equal(t, "ErrUnknown", strings.Split(ErrorSummary(&BaseError{Code: "ErrUnknown"}), "/")[0])
	assert.Equal(t, "GenericError", ErrorSummary(errors.New("x")), "still compact after an invalid mode")
}

func TestCovErrors_IsNull(t *testing.T) {
	assert.True(t, IsNull(nil))
	assert.True(t, IsNull(""))
	assert.True(t, IsNull(&BaseError{}))
	assert.False(t, IsNull(&BaseError{Code: "ErrX"}))
	assert.False(t, IsNull(NewErrUpstreamSyncing("u")))
	assert.False(t, IsNull(errors.New("plain")))
}

func TestCovErrors_ErrorSummary_Compact(t *testing.T) {
	covErrorsSetLabelMode(t, ErrorLabelModeCompact)

	jre := NewErrJsonRpcExceptionInternal(-32000, JsonRpcErrorCallException, "call exception", nil, nil)
	cases := []struct {
		name string
		in   interface{}
		want string
	}{
		{"Nil", nil, ""},
		{"UpstreamRequestWithJsonRpcCause", NewErrUpstreamRequest(jre, nil, "evm:1", "eth_call", 0, 1, 0, 0), "ErrUpstreamRequest/-32000"},
		{"SkippedWithStdCause", NewErrUpstreamRequestSkipped(NewErrUpstreamSyncing("u"), "u"), "ErrUpstreamRequestSkipped/ErrUpstreamSyncing"},
		{"OtherWithCauseNoCompound", NewErrEndpointUnsupported(NewErrUpstreamSyncing("u")), "ErrEndpointUnsupported"},
		{"JoinSingle", errors.Join(NewErrUpstreamSyncing("u")), "ErrUpstreamSyncing"},
		{"JoinMany", errors.Join(NewErrUpstreamSyncing("u"), errors.New("raw failure")), "ErrUpstreamSyncing, raw failure"},
		{"EmptyMulti", covErrorsEmptyMulti{}, "UnknownMultipleErrors"},
		{"Deadline", covErrorsWrap(context.DeadlineExceeded), "ContextDeadlineExceeded"},
		{"Canceled", covErrorsWrap(context.Canceled), "ContextCanceled"},
		{"Generic", errors.New("x"), "GenericError"},
		{"String", "some text", "StringError"},
		{"Other", 42, "UnknownError"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, ErrorSummary(tc.in))
		})
	}
}

func TestCovErrors_ErrorSummary_Verbose(t *testing.T) {
	covErrorsSetLabelMode(t, ErrorLabelModeVerbose)

	jre := NewErrJsonRpcExceptionInternal(-32000, JsonRpcErrorCallException, "execution reverted: 0xdeadbeefcafe", nil, nil)
	cases := []struct {
		name string
		in   interface{}
		want string
	}{
		{"StandardChain", NewErrUpstreamRequest(jre, nil, "evm:1", "eth_call", 0, 1, 0, 0), "ErrUpstreamRequest <- -32000 <- ErrJsonRpcExceptionInternal: execution reverted"},
		{"PlainCleaned", errors.New("dial 10.0.0.1 failed\n after 1234 ms"), "dial X.X.X.X failed after XX ms"},
		{"StringCleaned", "trie node abcdef0123 missing", "trie node 0xREDACTED missing"},
		{"OtherCleaned", 12345, "XX"},
		{"TxHash", errors.New("transaction abc123 not found"), "transaction 0xREDACTED not found"},
		{"LongAlnum", errors.New("key " + strings.Repeat("a", 40)), "key XX"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, ErrorSummary(tc.in))
		})
	}

	long := ErrorSummary(errors.New(strings.Repeat("ab ", 300)))
	assert.Len(t, long, 512, "messages are capped at 512 chars")
}

func TestCovErrors_ErrorFingerprint(t *testing.T) {
	covErrorsSetLabelMode(t, ErrorLabelModeVerbose)
	assert.Equal(t, "ErrUpstreamSyncing upstream is syncing and should not serve requests", ErrorFingerprint(NewErrUpstreamSyncing("u")))
	fp := ErrorFingerprint(errors.New(strings.Repeat("ab ", 200)))
	assert.Len(t, fp, 256)
	assert.Equal(t, "a b c", ErrorFingerprint(errors.New("a!@#b$%^c")))
}

func TestCovErrors_IsClientDisconnect(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{context.Canceled, true},
		{covErrorsWrap(context.DeadlineExceeded), true},
		{errors.New("use of closed network connection"), true},
		{errors.New("write tcp: use of closed network connection"), true},
		{errors.New("write: broken pipe"), true},
		{errors.New("read: connection reset by peer"), true},
		{errors.New("ECONNRESET"), true},
		{errors.New("EPIPE"), true},
		{errors.New("server returned 500"), false},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, IsClientDisconnect(tc.err), "%v", tc.err)
	}
}

func TestCovErrors_BaseError_ErrorString(t *testing.T) {
	assert.Equal(t, "ErrX: m", (&BaseError{Code: "ErrX", Message: "m"}).Error())
	assert.Equal(t, `ErrX: m -> ({"a":1})`, (&BaseError{Code: "ErrX", Message: "m", Details: map[string]interface{}{"a": 1}}).Error())
	assert.Equal(t, "ErrX: m \ncaused by: c", (&BaseError{Code: "ErrX", Message: "m", Cause: errors.New("c")}).Error())
	assert.Equal(t, "ErrX: m -> ({\"a\":1}) \ncaused by: c", (&BaseError{Code: "ErrX", Message: "m", Cause: errors.New("c"), Details: map[string]interface{}{"a": 1}}).Error())

	unmarshalable := (&BaseError{Code: "ErrX", Message: "m", Details: map[string]interface{}{"fn": func() {}}}).Error()
	assert.True(t, strings.HasPrefix(unmarshalable, "ErrX: m -> (map[fn:"), unmarshalable)
}

func TestCovErrors_BaseError_ChainAndSearch(t *testing.T) {
	leaf := &BaseError{Code: "ErrLeaf", Message: "leaf msg", Details: map[string]interface{}{"k": "leaf"}}
	mid := &BaseError{Code: "ErrMid", Message: "mid msg", Cause: leaf}
	top := &BaseError{Code: "ErrTop", Message: "top msg", Cause: mid, Details: map[string]interface{}{"own": 1}}

	assert.Equal(t, "ErrTop <- ErrMid <- ErrLeaf", top.CodeChain())
	assert.Equal(t, "leaf msg", top.DeepestMessage())
	assert.Equal(t, "plain", (&BaseError{Code: "E", Message: "m", Cause: errors.New("plain")}).DeepestMessage())
	assert.Equal(t, "E", (&BaseError{Code: "E", Cause: errors.New("plain")}).CodeChain())

	assert.Equal(t, 1, top.DeepSearch("own"))
	assert.Equal(t, "leaf", top.DeepSearch("k"))
	assert.Nil(t, top.DeepSearch("missing"))
	assert.Nil(t, (&BaseError{Code: "E", Cause: errors.New("plain")}).DeepSearch("k"))

	multi := &BaseError{Code: "ErrMulti", Cause: errors.Join(errors.New("plain"), &BaseError{Code: "ErrA"}, leaf)}
	assert.Equal(t, "leaf", multi.DeepSearch("k"), "first standard child that has the key wins")
	assert.Nil(t, multi.DeepSearch("missing"))
	assert.True(t, multi.HasCode("ErrLeaf"))
	assert.False(t, multi.HasCode("ErrNope"))

	assert.True(t, errors.Is(top, leaf))
	assert.Same(t, mid, top.Unwrap())
	assert.Same(t, mid, top.GetCause())
	assert.Equal(t, ErrorCode("ErrTop"), top.GetCode())
	assert.Same(t, top, top.Base())
}

func TestCovErrors_BaseError_FlagSetters(t *testing.T) {
	var nilBase *BaseError
	assert.NotPanics(t, func() { nilBase.WithRetryableTowardNetwork(false) })
	assert.Nil(t, nilBase.WithPermanentMissingData(true))

	b := &BaseError{Code: ErrCodeEndpointMissingData}
	b.WithPermanentMissingData(true)
	assert.Equal(t, true, b.Details["permanentMissingData"])
	assert.True(t, IsPermanentlyMissingData(b))

	r := &BaseError{Code: "ErrR"}
	r.WithRetryableTowardNetwork(false)
	assert.False(t, IsRetryableTowardNetwork(r))
}

func TestCovErrors_BaseError_MarshalJSONShapes(t *testing.T) {
	t.Run("NoCodeNoCause_IsJustMessage", func(t *testing.T) {
		raw, err := SonicCfg.Marshal(&BaseError{Message: "just text"})
		require.NoError(t, err)
		assert.Equal(t, `"just text"`, string(raw))
	})
	t.Run("GenericCodeWithCause_CauseIsString", func(t *testing.T) {
		m := covErrorsUnmarshalObj(t, &BaseError{Code: "ErrGeneric", Message: "m", Cause: errors.New("inner")})
		assert.Equal(t, "inner", m["cause"])
		assert.Equal(t, "m", m["message"])
	})
	t.Run("JoinedCauses_Array", func(t *testing.T) {
		m := covErrorsUnmarshalObj(t, &BaseError{Code: "ErrTop", Message: "m", Cause: errors.Join(NewErrUpstreamSyncing("u1"), errors.New("raw"))})
		causes, ok := m["cause"].([]interface{})
		require.True(t, ok, "%v", m)
		require.Len(t, causes, 2)
		assert.Equal(t, "ErrUpstreamSyncing", causes[0].(map[string]interface{})["code"])
		assert.Equal(t, "raw", causes[1])
	})
	t.Run("StandardCause_Nested", func(t *testing.T) {
		m := covErrorsUnmarshalObj(t, &BaseError{Code: "ErrTop", Message: "m", Cause: NewErrUpstreamSyncing("u1")})
		c := m["cause"].(map[string]interface{})
		assert.Equal(t, "ErrUpstreamSyncing", c["code"])
		assert.Equal(t, "u1", c["details"].(map[string]interface{})["upstreamId"])
	})
	t.Run("PlainCause_WrappedAsGenericMessage", func(t *testing.T) {
		// A plain cause is wrapped as ErrGeneric, which itself marshals as its bare message.
		m := covErrorsUnmarshalObj(t, &BaseError{Code: "ErrTop", Message: "m", Cause: errors.New("raw")})
		assert.Equal(t, "raw", m["cause"])
	})
	t.Run("NoCause_OmitsCause", func(t *testing.T) {
		m := covErrorsUnmarshalObj(t, &BaseError{Code: "ErrTop", Message: "m", Details: map[string]interface{}{"a": "b"}})
		assert.NotContains(t, m, "cause")
		assert.Equal(t, map[string]interface{}{"a": "b"}, m["details"])
	})
}

func TestCovErrors_BaseError_Zerolog(t *testing.T) {
	var nilBase *BaseError
	assert.Empty(t, covErrorsZerolog(t, nilBase))

	m := covErrorsZerolog(t, &BaseError{Code: "ErrTop", Message: "m", Details: map[string]interface{}{"a": "b"}, Cause: NewErrUpstreamSyncing("u")})
	assert.Equal(t, "ErrTop", m["code"])
	assert.Equal(t, "m", m["message"])
	assert.Equal(t, map[string]interface{}{"a": "b"}, m["details"])
	assert.Equal(t, "ErrUpstreamSyncing", m["cause"].(map[string]interface{})["code"])

	plain := covErrorsZerolog(t, &BaseError{Code: "ErrTop", Cause: errors.New("raw")})
	assert.Equal(t, "raw", plain["cause"])
	assert.NotContains(t, plain, "details")

	multi := covErrorsZerolog(t, &BaseError{Code: "ErrTop", Cause: errors.Join(NewErrUpstreamSyncing("u"), NewErrUpstreamShadowing("v"))})
	causes, ok := multi["cause"].([]interface{})
	require.True(t, ok, "%v", multi)
	assert.Len(t, causes, 2)
}

func TestCovErrors_TaskFatalError(t *testing.T) {
	var nilFatal *TaskFatalError
	assert.Equal(t, "fatal task error", nilFatal.Error())
	assert.Equal(t, "fatal task error", (&TaskFatalError{}).Error())
	err := NewTaskFatal(covErrorsSentinel)
	assert.Equal(t, covErrorsSentinel.Error(), err.Error())
	assert.True(t, errors.Is(err, covErrorsSentinel))
	var tf interface{ IsTaskFatal() bool }
	require.True(t, errors.As(covErrorsWrap(err), &tf))
	assert.True(t, tf.IsTaskFatal())
}

func covErrorsAllCategoryCauses(t *testing.T) []error {
	t.Helper()
	u, err := url.Parse("https://rpc.example.com")
	require.NoError(t, err)
	return []error{
		NewErrEndpointUnsupported(nil),
		NewErrEndpointMissingData(nil, nil),
		context.DeadlineExceeded,
		NewErrEndpointRequestTimeout(time.Second, nil),
		NewErrNetworkRequestTimeout(time.Second, nil),
		NewErrFailsafeTimeoutExceeded(ScopeUpstream, nil, nil),
		NewErrEndpointServerSideException(nil, nil, 0),
		NewErrEndpointCapacityExceeded(nil),
		NewErrUpstreamRateLimitRuleExceeded("u", "b", "r"),
		NewErrFailsafeCircuitBreakerOpen(ScopeUpstream, nil, nil),
		NewErrEndpointBillingIssue(nil),
		NewErrEndpointTransportFailure(u, errors.New("refused")),
		NewErrUpstreamExcludedByPolicy("u"),
		NewErrUpstreamMethodIgnored("m", "u"),
		NewErrUpstreamRequestSkipped(nil, "u"),
		NewErrEndpointRequestTooLarge(nil, EvmBlockRangeTooLarge),
		NewErrGetLogsExceededMaxAllowedRange(2, 1),
		NewErrGetLogsExceededMaxAllowedAddresses(2, 1),
		NewErrGetLogsExceededMaxAllowedTopics(2, 1),
		NewErrEndpointUnauthorized(nil),
		NewErrUpstreamSyncing("u"),
		NewErrUpstreamBlockUnavailable("u", 1, 0, 0),
		NewErrEndpointContentValidation(nil, nil),
		errors.New("mystery"),
		NewErrUpstreamNodeTypeMismatch(nil, EvmNodeTypeArchive, EvmNodeTypeFull),
		NewErrUpstreamHedgeCancelled("u", nil),
		NewErrEndpointClientSideException(nil),
		NewErrJsonRpcRequestUnmarshal(nil, nil),
		NewErrInvalidRequest(nil),
		NewErrInvalidUrlPath("r", "/p"),
	}
}

func TestCovErrors_UpstreamsExhausted_SummarizesEveryCategory(t *testing.T) {
	ers := &sync.Map{}
	for i, e := range covErrorsAllCategoryCauses(t) {
		ers.Store(string(rune('a'+i)), e)
	}
	ers.Store("zz-nil", nil)
	ers.Store("zz-not-error", "just a string")

	err := NewErrUpstreamsExhausted(nil, ers, "main", "evm:1", "eth_call", 2*time.Second, 5, 4, 1, 30)
	ex := err.(*ErrUpstreamsExhausted)

	want := "1 upstream unsupported method, 1 upstream missing data, 4 upstream timeout, 1 upstream server errors, " +
		"2 upstream rate limited, 1 upstream circuit breaker open, 1 upstream billing issues, 1 upstream transport errors, " +
		"1 upstream excluded by policy, 1 upstream method ignored, 1 upstream skipped, 4 upstream too large complaints, " +
		"1 upstream unauthorized, 2 upstream not synced, 1 upstream validation mismatch, 1 upstream unknown errors, " +
		"1 node type mismatches, 1 hedges cancelled, 4 user errors"
	assert.Equal(t, want, ex.SummarizeCauses())
	assert.Equal(t, "all upstream attempts failed ("+want+")", ex.Message)
	assert.Equal(t, want, ex.DeepestMessage())
	assert.Equal(t, "ErrUpstreamsExhausted", ex.CodeChain())
	assert.Len(t, ex.Errors(), 30, "nil and non-error entries are dropped")
	assert.Equal(t, 5, ex.Attempts())
	assert.Equal(t, 4, ex.Retries())
	assert.Equal(t, 1, ex.Hedges())
	assert.Equal(t, int64(2000), ex.Details["durationMs"])
	assert.Equal(t, 30, ex.Details["upstreams"])
	assert.False(t, ex.FromCache())
	assert.False(t, ex.IsObjectNull())
	assert.Nil(t, ex.Request())
	assert.True(t, HasErrorCode(err, ErrCodeUpstreamsExhausted, ErrCodeEndpointBillingIssue))
	assert.True(t, errors.Is(err, context.DeadlineExceeded), "joined causes are reachable via errors.Is")

	m := covErrorsUnmarshalObj(t, err)
	causes, ok := m["cause"].([]interface{})
	require.True(t, ok)
	assert.Len(t, causes, 30)
}

func TestCovErrors_UpstreamsExhausted_OrderIsDeterministic(t *testing.T) {
	upB := NewFakeUpstream("b-up")
	upA := NewFakeUpstream("a-up")
	upC := NewFakeUpstream("c-up")

	ers := &sync.Map{}
	// Upstream keys (sorted by Id), string keys, and other keys falling back to the error.
	ers.Store(upB, NewErrUpstreamRequest(NewErrEndpointServerSideException(nil, nil, 0), upB, "evm:1", "eth_call", 0, 1, 0, 0))
	ers.Store(upA, NewErrUpstreamRequest(NewErrEndpointServerSideException(nil, nil, 0), upA, "evm:1", "eth_call", 0, 1, 0, 0))
	// Non-retryable sorts after every retryable cause, despite the lowest key.
	ers.Store("0-terminal", NewErrEndpointExecutionException(nil))
	// Unkeyed (int key): id taken from the upstream carried on the error.
	ers.Store(1, NewErrUpstreamRequest(NewErrEndpointServerSideException(nil, nil, 0), upC, "evm:1", "eth_call", 0, 1, 0, 0))
	// Unkeyed with no upstream: falls back to error text; ties broken by text.
	ers.Store(2, errors.New("zz plain 2"))
	ers.Store(3, errors.New("zz plain 1"))

	for range 5 {
		err := NewErrUpstreamsExhausted(nil, ers, "main", "evm:1", "eth_call", 0, 1, 0, 0, 6).(*ErrUpstreamsExhausted)
		children := err.Errors()
		require.Len(t, children, 6)
		assert.Equal(t, "a-up", children[0].(*ErrUpstreamRequest).UpstreamId())
		assert.Equal(t, "b-up", children[1].(*ErrUpstreamRequest).UpstreamId())
		assert.Equal(t, "c-up", children[2].(*ErrUpstreamRequest).UpstreamId())
		assert.Equal(t, "zz plain 1", children[3].Error())
		assert.Equal(t, "zz plain 2", children[4].Error())
		assert.True(t, HasErrorCode(children[5], ErrCodeEndpointExecutionException))

		ups := err.Upstreams()
		require.Len(t, ups, 3)
		assert.Equal(t, []string{"a-up", "b-up", "c-up"}, []string{ups[0].Id(), ups[1].Id(), ups[2].Id()})
		assert.Equal(t, "a-up", err.UpstreamId(), "first upstreamId found in children")
	}
	assert.Nil(t, orderCauses(nil))
}

func TestCovErrors_UpstreamsExhausted_EdgeShapes(t *testing.T) {
	var nilEx *ErrUpstreamsExhausted
	assert.Nil(t, nilEx.Upstreams())
	assert.Nil(t, nilEx.Errors())
	assert.True(t, nilEx.IsObjectNull())

	empty := NewErrUpstreamsExhausted(nil, &sync.Map{}, "p", "n", "m", 0, 0, 0, 0, 0).(*ErrUpstreamsExhausted)
	assert.Equal(t, "all upstream attempts failed", empty.Message, "no causes: no summary suffix")
	assert.Nil(t, empty.Cause)
	assert.Equal(t, empty.Message, empty.DeepestMessage())
	assert.Equal(t, "ErrUpstreamsExhausted", empty.CodeChain())
	assert.Empty(t, empty.Upstreams())
	assert.Equal(t, "", empty.UpstreamId())
	assert.False(t, IsRetryableTowardNetwork(empty))
	assert.False(t, IsRetryableTowardsUpstream(empty))

	plainCause := NewErrUpstreamsExhaustedWithCause(errors.New("raw")).(*ErrUpstreamsExhausted)
	assert.Equal(t, "", plainCause.SummarizeCauses())
	assert.Equal(t, "", plainCause.DeepestMessage())
	assert.Nil(t, plainCause.Errors())
	assert.Equal(t, "ErrUpstreamsExhausted", plainCause.CodeChain())
	assert.Equal(t, 0, plainCause.Attempts())
	assert.Equal(t, 0, plainCause.Retries())
	assert.Equal(t, 0, plainCause.Hedges())

	emptyMulti := NewErrUpstreamsExhaustedWithCause(covErrorsEmptyMulti{}).(*ErrUpstreamsExhausted)
	assert.Equal(t, "", emptyMulti.DeepestMessage())
	assert.Empty(t, emptyMulti.Errors())

	wrongTypes := &ErrUpstreamsExhausted{BaseError: BaseError{Details: map[string]interface{}{"attempts": "1", "retries": "1", "hedges": "1", "upstreamId": 3}}}
	assert.Equal(t, 0, wrongTypes.Attempts())
	assert.Equal(t, 0, wrongTypes.Retries())
	assert.Equal(t, 0, wrongTypes.Hedges())
	assert.Equal(t, "", wrongTypes.UpstreamId())
	assert.True(t, wrongTypes.IsObjectNull())
}

func TestCovErrors_UpstreamsExhausted_Retryability(t *testing.T) {
	build := func(children ...error) error {
		ers := &sync.Map{}
		for i, c := range children {
			ers.Store(string(rune('a'+i)), c)
		}
		return NewErrUpstreamsExhausted(nil, ers, "p", "n", "m", 0, 0, 0, 0, len(children))
	}

	onlyVerdicts := build(NewErrEndpointUnsupported(nil), NewErrUpstreamMethodIgnored("m", "u"))
	assert.False(t, IsRetryableTowardNetwork(onlyVerdicts), "every upstream said it does not serve the method")
	assert.False(t, IsRetryableTowardsUpstream(onlyVerdicts))

	mixed := build(NewErrEndpointUnsupported(nil), NewErrEndpointServerSideException(nil, nil, 0))
	assert.True(t, IsRetryableTowardNetwork(mixed))
	assert.True(t, IsRetryableTowardsUpstream(mixed))

	wrappedVerdict := build(NewErrUpstreamRequest(NewErrEndpointUnsupported(nil), nil, "n", "m", 0, 1, 0, 0))
	assert.False(t, IsRetryableTowardNetwork(wrappedVerdict), "verdict is found through single-cause wrappers")

	// A verdict check never descends into a nested multi-error.
	nestedMulti := build(&BaseError{Code: "ErrWrap", Cause: errors.Join(NewErrEndpointUnsupported(nil))})
	assert.False(t, IsRetryableTowardNetwork(nestedMulti), "nested child is non-retryable verdict-only bundle")

	nonTerminalNested := build(&BaseError{Code: "ErrWrap", Cause: errors.Join(NewErrEndpointServerSideException(nil, nil, 0))})
	assert.True(t, IsRetryableTowardNetwork(nonTerminalNested))

	assert.True(t, IsRetryableTowardNetwork(errors.New("plain")))
	assert.False(t, isUpstreamMethodVerdict(errors.New("plain")))
	assert.False(t, isUpstreamMethodVerdict(&BaseError{Code: "ErrWrap", Cause: errors.Join(NewErrEndpointUnsupported(nil))}))
	assert.False(t, isUpstreamMethodVerdict(nil))

	// Flag below a multi-error wrapper is not consulted by the linear walk.
	flagged := &BaseError{Code: "ErrWrap", Cause: errors.Join(
		NewErrEndpointExecutionException(nil),
		NewErrEndpointServerSideException(nil, nil, 0),
	)}
	assert.True(t, IsRetryableTowardNetwork(flagged))
	assert.False(t, IsPermanentlyMissingData(&BaseError{Code: "ErrWrap", Cause: errors.Join(
		(&BaseError{Code: ErrCodeEndpointMissingData}).WithPermanentMissingData(true),
	)}))
	assert.True(t, IsPermanentlyMissingData(NewErrUpstreamRequest(
		(&BaseError{Code: ErrCodeEndpointMissingData}).WithPermanentMissingData(true), nil, "n", "m", 0, 0, 0, 0)))
}

func TestCovErrors_NoUpstreamsLeftToSelect_States(t *testing.T) {
	req := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":[]}`))
	u1, u2, u3, u4 := NewFakeUpstream("u1"), NewFakeUpstream("u2"), NewFakeUpstream("u3"), NewFakeUpstream("u4")
	req.SetUpstreams([]Upstream{u1, u2, u3, u4})
	req.ErrorsByUpstream.Store(u1, NewErrEndpointMissingData(nil, u1))
	req.ErrorsByUpstream.Store(u2, NewErrEndpointServerSideException(nil, nil, 0))
	req.ConsumedUpstreams.Store(u3, true)

	err := NewErrNoUpstreamsLeftToSelect(req, "all consumed")
	be := err.(*ErrNoUpstreamsLeftToSelect)
	assert.Equal(t, ErrCodeNoUpstreamsLeftToSelect, be.Code)
	assert.Equal(t, "no upstreams left to select, all consumed", be.Message)
	assert.Equal(t, map[string]interface{}{
		"method": "eth_getBalance",
		"u1":     "empty_response",
		"u2":     "errored",
		"u3":     "consumed",
		"u4":     "available",
	}, be.Details)
}

func TestCovErrors_JsonRpcExceptionInternal_Codes(t *testing.T) {
	e := NewErrJsonRpcExceptionInternal(-32005, JsonRpcErrorCapacityExceeded, "limited", nil, nil)
	assert.Equal(t, JsonRpcErrorCapacityExceeded, e.NormalizedCode())
	assert.Equal(t, -32005, e.OriginalCode())
	assert.Equal(t, "-32005 <- ErrJsonRpcExceptionInternal", e.CodeChain())

	bare := NewErrJsonRpcExceptionInternal(0, 0, "none", nil, nil)
	assert.NotContains(t, bare.Details, "originalCode")
	assert.NotContains(t, bare.Details, "normalizedCode")
	assert.Equal(t, JsonRpcErrorNumber(0), bare.NormalizedCode())
	assert.Equal(t, 0, bare.OriginalCode())

	fromJSON := NewErrJsonRpcExceptionInternal(0, 0, "f", nil, map[string]interface{}{"originalCode": float64(-32000)})
	assert.Equal(t, -32000, fromJSON.OriginalCode(), "float64 from decoded JSON is accepted")
	weird := NewErrJsonRpcExceptionInternal(0, 0, "w", nil, map[string]interface{}{"originalCode": "x"})
	assert.Equal(t, 0, weird.OriginalCode())
}

func TestCovErrors_JsonRpcExceptionExternal(t *testing.T) {
	e := NewErrJsonRpcExceptionExternal(-32000, "execution reverted", "0x08c379a0")
	assert.Equal(t, "-32000: execution reverted", e.Error())
	assert.Equal(t, "-32000", e.CodeChain())
	assert.False(t, e.HasCode("-32000"))
	assert.Equal(t, "execution reverted", e.DeepestMessage())
	assert.Nil(t, e.GetCause())

	m := covErrorsUnmarshalObj(t, e)
	assert.Equal(t, map[string]interface{}{"code": float64(-32000), "message": "execution reverted", "data": "0x08c379a0"}, m)

	z := covErrorsZerolog(t, e)
	assert.Equal(t, float64(-32000), z["code"])
	assert.Equal(t, "0x08c379a0", z["data"])

	noData := covErrorsZerolog(t, &ErrJsonRpcExceptionExternal{Code: 1, Message: "m"})
	assert.NotContains(t, noData, "data")

	var nilExt *ErrJsonRpcExceptionExternal
	assert.Empty(t, covErrorsZerolog(t, nilExt))
}

func TestCovErrors_ConsensusErrors(t *testing.T) {
	type multi interface {
		Errors() []error
	}
	causes := []error{NewErrUpstreamSyncing("a"), errors.New("b")}
	for name, ctor := range map[string]func(string, []ParticipantInfo, []error) error{
		"Dispute":            NewErrConsensusDispute,
		"CompositionDispute": NewErrConsensusCompositionDispute,
		"LowParticipants":    NewErrConsensusLowParticipants,
	} {
		t.Run(name, func(t *testing.T) {
			withCauses := ctor("msg", nil, causes).(multi)
			assert.Len(t, withCauses.Errors(), 2)
			assert.True(t, HasErrorCode(withCauses.(error), ErrCodeUpstreamSyncing))
			assert.Nil(t, ctor("msg", nil, nil).(multi).Errors(), "no causes: nil")

			var se StandardError
			require.True(t, errors.As(withCauses.(error), &se))
			se.Base().Cause = errors.New("single")
			assert.Nil(t, withCauses.Errors(), "non-multi cause: nil")
		})
	}

	participants := []ParticipantInfo{
		{Upstream: "a", ResultHash: "h1"},
		{Upstream: "b"},
		{},
		{Upstream: "c", ErrSummary: "ErrTimeout"},
		{ErrSummary: "orphan"},
	}
	low := NewErrConsensusLowParticipants("not enough", participants, nil).(*ErrConsensusLowParticipants)
	assert.Equal(t, "[a = h1, b = NoResult, c = ErrTimeout, orphan]", low.SummarizeParticipants())
	assert.Equal(t, "ErrConsensusLowParticipants: not enough: [a = h1, b = NoResult, c = ErrTimeout, orphan]", low.DeepestMessage())

	assert.Equal(t, "", (&ErrConsensusLowParticipants{}).SummarizeParticipants())
	wrong := &ErrConsensusLowParticipants{BaseError{Details: map[string]interface{}{"participants": "x"}}}
	assert.Equal(t, "", wrong.SummarizeParticipants())

	dispute := NewErrConsensusDispute("disagree", nil, []error{NewErrEndpointExecutionException(nil)})
	assert.False(t, IsRetryableTowardNetwork(dispute), "all-terminal dispute children are not retryable")
}

func TestCovErrors_HasErrorCode_Shapes(t *testing.T) {
	assert.False(t, HasErrorCode(nil, ErrCodeUpstreamSyncing))
	assert.True(t, HasErrorCode(errors.Join(errors.New("x"), NewErrUpstreamSyncing("u")), ErrCodeUpstreamSyncing))
	assert.False(t, HasErrorCode(errors.Join(errors.New("x")), ErrCodeUpstreamSyncing))
	assert.False(t, HasErrorCode(errors.New("x"), ErrCodeUpstreamSyncing))
	assert.False(t, HasErrorCode(covErrorsWrap(NewErrUpstreamSyncing("u")), ErrCodeUpstreamSyncing), "fmt %w wrappers are not traversed")
}

func TestCovErrors_OrderCauses_KeyTieFallsBackToText(t *testing.T) {
	up := NewFakeUpstream("same-id")
	ers := &sync.Map{}
	// An Upstream key and a string key resolve to the same sort key; text breaks the tie.
	ers.Store(up, errors.New("zz second"))
	ers.Store("same-id", errors.New("aa first"))
	got := orderCauses(ers)
	require.Len(t, got, 2)
	assert.Equal(t, "aa first", got[0].Error())
	assert.Equal(t, "zz second", got[1].Error())
}

func TestCovErrors_IsRetryableTowardNetwork_EmptyMultiCause(t *testing.T) {
	// A multi-error cause with no children is not a verdict bundle; the linear
	// walk stops at it, so only the outer flag counts.
	outer := &BaseError{Code: "ErrWrap", Cause: covErrorsEmptyMulti{}}
	assert.True(t, IsRetryableTowardNetwork(outer))
	outer.WithRetryableTowardNetwork(false)
	assert.False(t, IsRetryableTowardNetwork(outer))
}
