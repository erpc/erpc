package common

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// Tracing state is process-global; none of these tests run in parallel.

func TestCovMiscTracing_DisabledIsNoop(t *testing.T) {
	covMiscDisableTracing(t)
	ctx := context.Background()

	gotCtx, span := StartSpan(ctx, "x")
	assert.Equal(t, ctx, gotCtx)
	assert.False(t, span.IsRecording())
	assert.Equal(t, trace.SpanContext{}, span.SpanContext())
	assert.Nil(t, span.TracerProvider())
	// noop methods must be callable
	span.SetName("n")
	span.SetAttributes(attribute.String("a", "b"))
	span.AddEvent("e")
	span.RecordError(errors.New("boom"))
	span.SetStatus(codes.Error, "x")
	span.End()

	_, dspan := StartDetailSpan(ctx, "x")
	assert.False(t, dspan.IsRecording())

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("traceparent", "00-0af7651916cd43dd8448eb211c80319c-b7ad567c75a3d1e5-01")
	assert.False(t, trace.SpanContextFromContext(ExtractHTTPRequestTraceContext(r)).IsValid())

	w := httptest.NewRecorder()
	InjectHTTPResponseTraceContext(ctx, w)
	assert.Empty(t, w.Header().Get("traceparent"))

	sctx, sspan := StartHTTPServerSpan(ctx, r)
	assert.Equal(t, ctx, sctx)
	assert.False(t, sspan.IsRecording())

	assert.Equal(t, ctx, StartRequestSpan(ctx, NewNormalizedRequest([]byte(`{"method":"eth_call"}`))))
	EndRequestSpan(ctx, nil, nil) // no-op
	assert.Equal(t, ctx, SetForceTraceNetwork(ctx, "evm:1"))
	assert.Equal(t, "", GetForceTraceNetwork(ctx))
	require.NoError(t, ForceFlushTraces(ctx))
}

func TestCovMiscTracing_ShutdownWithoutProviderIsNil(t *testing.T) {
	prev := tracerProvider
	tracerProvider = nil
	t.Cleanup(func() { tracerProvider = prev })
	require.NoError(t, ShutdownTracing(context.Background()))
}

func TestCovMiscTracing_StartSpanRecordsAndDetailGate(t *testing.T) {
	sr := covMiscRecordSpans(t, 1, false, nil)
	ctx := context.Background()

	_, s := StartSpan(ctx, "Cache.Get", trace.WithAttributes(attribute.String("k", "v")))
	assert.True(t, s.IsRecording())
	s.End()
	_, d := StartDetailSpan(ctx, "Detail.Op")
	assert.False(t, d.IsRecording(), "detail spans require IsTracingDetailed")
	d.End()

	sp := covMiscOnlySpan(t, sr)
	assert.Equal(t, "Cache.Get", sp.Name())
	assert.Equal(t, "v", covMiscAttrs(sp.Attributes())["k"].AsString())

	IsTracingDetailed = true
	_, d = StartDetailSpan(ctx, "Detail.Op")
	d.End()
	require.Len(t, sr.Ended(), 2)
	assert.Equal(t, "Detail.Op", sr.Ended()[1].Name())
}

func TestCovMiscTracing_SampleRateZeroDropsUnlessForced(t *testing.T) {
	sr := covMiscRecordSpans(t, 0, false, nil)

	r := httptest.NewRequest(http.MethodPost, "http://erpc.local/main/evm/1", nil)
	_, span := StartHTTPServerSpan(context.Background(), r)
	assert.False(t, span.IsRecording())
	span.End()
	assert.Empty(t, sr.Ended())

	r = httptest.NewRequest(http.MethodPost, "http://erpc.local/main/evm/1?force-trace=true", nil)
	_, span = StartHTTPServerSpan(context.Background(), r)
	assert.True(t, span.IsRecording())
	span.End()
	sp := covMiscOnlySpan(t, sr)
	attrs := covMiscAttrs(sp.Attributes())
	assert.Equal(t, "header_or_query", attrs["erpc.forced_trace_reason"].AsString())
	assert.True(t, attrs[forceTraceAttributeKey].AsBool())
	assert.Equal(t, trace.SpanKindServer, sp.SpanKind())
}

func TestCovMiscTracing_ShouldForceTraceHTTP(t *testing.T) {
	cases := []struct {
		name   string
		header string
		query  string
		want   bool
	}{
		{"none", "", "", false},
		{"header true", "true", "", true},
		{"header 1", "1", "", true},
		{"header yes", "yes", "", true},
		{"header false wins over query", "no", "true", false},
		{"query 1", "", "1", true},
		{"query yes", "", "yes", true},
		{"query other", "", "nah", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := "http://x/"
			if tc.query != "" {
				u += "?" + ForceTraceQueryParam + "=" + tc.query
			}
			r := httptest.NewRequest(http.MethodGet, u, nil)
			if tc.header != "" {
				r.Header.Set(ForceTraceHeader, tc.header)
			}
			assert.Equal(t, tc.want, shouldForceTrace(r))
		})
	}
}

func TestCovMiscTracing_HTTPServerSpanAttributesAndPropagation(t *testing.T) {
	sr := covMiscRecordSpans(t, 1, false, nil)

	r := httptest.NewRequest(http.MethodPost, "http://erpc.local/p/evm/1", nil)
	r.Header.Set("User-Agent", "cov-agent")
	r.Header.Set("traceparent", "00-0af7651916cd43dd8448eb211c80319c-b7ad567c75a3d1e5-01")

	extracted := ExtractHTTPRequestTraceContext(r)
	assert.Equal(t, "0af7651916cd43dd8448eb211c80319c", trace.SpanContextFromContext(extracted).TraceID().String())

	ctx, span := StartHTTPServerSpan(context.Background(), r)
	assert.Equal(t, "0af7651916cd43dd8448eb211c80319c", span.SpanContext().TraceID().String(), "remote parent continues trace")

	w := httptest.NewRecorder()
	InjectHTTPResponseTraceContext(ctx, w)
	assert.Contains(t, w.Header().Get("traceparent"), span.SpanContext().SpanID().String())

	EnrichHTTPServerSpan(ctx, 200, nil)
	span.End()

	sp := covMiscOnlySpan(t, sr)
	attrs := covMiscAttrs(sp.Attributes())
	assert.Equal(t, "POST", attrs["http.method"].AsString())
	assert.Equal(t, "cov-agent", attrs["http.user_agent"].AsString())
	assert.Equal(t, int64(200), attrs["http.status_code"].AsInt64())
	assert.Equal(t, codes.Ok, sp.Status().Code)
	assert.Equal(t, "b7ad567c75a3d1e5", sp.Parent().SpanID().String())
}

func TestCovMiscTracing_EnrichHTTPServerSpanStatus(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		err      error
		wantCode codes.Code
		wantDesc string
		wantErrC string
	}{
		{"client error status", 429, nil, codes.Error, "Too Many Requests", ""},
		{"standard error", 500, NewErrInvalidConfig("bad"), codes.Error, "ErrInvalidConfig", "ErrInvalidConfig"},
		{"plain error", 500, errors.New("plain"), codes.Error, "plain", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sr := covMiscRecordSpans(t, 1, false, nil)
			ctx, span := StartSpan(context.Background(), "s")
			EnrichHTTPServerSpan(ctx, tc.status, tc.err)
			span.End()
			sp := covMiscOnlySpan(t, sr)
			assert.Equal(t, tc.wantCode, sp.Status().Code)
			assert.Contains(t, sp.Status().Description, tc.wantDesc)
			attrs := covMiscAttrs(sp.Attributes())
			if tc.wantErrC != "" {
				assert.Equal(t, tc.wantErrC, attrs["error.code"].AsString())
			}
			if tc.err != nil {
				require.Len(t, sp.Events(), 1)
				assert.Equal(t, "exception", sp.Events()[0].Name)
			}
		})
	}

	t.Run("non-recording span untouched", func(t *testing.T) {
		covMiscRecordSpans(t, 1, false, nil)
		EnrichHTTPServerSpan(context.Background(), 500, errors.New("x")) // no span in ctx: must not panic
	})
}

func TestCovMiscTracing_SetTraceSpanErrorIgnoresNonErrorsAndNilSpans(t *testing.T) {
	sr := covMiscRecordSpans(t, 1, false, nil)
	SetTraceSpanError(nil, errors.New("x"))
	_, span := StartSpan(context.Background(), "s")
	SetTraceSpanError(span, "a string is not an error")
	span.End()
	SetTraceSpanError(span, errors.New("after end: not recording"))
	sp := covMiscOnlySpan(t, sr)
	assert.Equal(t, codes.Unset, sp.Status().Code)
	assert.Empty(t, sp.Events())
}

func TestCovMiscTracing_ForceTraceMatchers(t *testing.T) {
	matchers := []*ForceTraceMatcher{
		nil,
		{}, // empty: never matches
		{Network: "evm:137", Method: "eth_call"},
		{Network: "evm:42161"},
		{Method: "debug_*|trace_*"},
	}
	covMiscRecordSpans(t, 0, false, matchers)

	cases := []struct {
		network, method string
		want            bool
		reason          string
	}{
		{"evm:137", "eth_call", true, "network:evm:137,method:eth_call"},
		{"evm:137", "eth_getBalance", false, ""},
		{"evm:42161", "anything", true, "network:evm:42161"},
		{"evm:1", "trace_block", true, "method:trace_block"},
		{"evm:1", "eth_call", false, ""},
	}
	for _, tc := range cases {
		got, reason := ShouldForceTrace(tc.network, tc.method)
		assert.Equal(t, tc.want, got, "%s %s", tc.network, tc.method)
		assert.Equal(t, tc.reason, reason)
	}

	forceTraceMatchers = nil
	got, reason := ShouldForceTrace("evm:137", "eth_call")
	assert.False(t, got)
	assert.Empty(t, reason)
}

func TestCovMiscTracing_ForceTraceNetworkContext(t *testing.T) {
	covMiscRecordSpans(t, 1, false, nil)
	ctx := context.Background()
	assert.Equal(t, ctx, SetForceTraceNetwork(ctx, ""), "empty network is not stored")
	ctx = SetForceTraceNetwork(ctx, "evm:10")
	assert.Equal(t, "evm:10", GetForceTraceNetwork(ctx))
}

func TestCovMiscTracing_RequestSpanForcedByNetworkMatcher(t *testing.T) {
	sr := covMiscRecordSpans(t, 0, true, []*ForceTraceMatcher{{Network: "evm:10"}})

	jrq := NewJsonRpcRequest("eth_getBalance", []interface{}{"0xabc", "latest"})
	jrq.ID = 7
	req := NewNormalizedRequestFromJsonRpcRequest(jrq)

	// unmatched network: sampled out
	ctx := StartRequestSpan(context.Background(), req)
	EndRequestSpan(ctx, nil, nil)
	assert.Empty(t, sr.Ended())

	ctx = SetForceTraceNetwork(context.Background(), "evm:10")
	ctx = StartRequestSpan(ctx, req)
	EndRequestSpan(ctx, nil, NewErrInvalidConfig("nope"))

	sp := covMiscOnlySpan(t, sr)
	assert.Equal(t, "Request.Handle", sp.Name())
	attrs := covMiscAttrs(sp.Attributes())
	assert.Equal(t, "eth_getBalance", attrs["request.method"].AsString())
	assert.Equal(t, "network:evm:10", attrs["erpc.forced_trace_reason"].AsString())
	assert.Equal(t, "7", attrs["request.id"].AsString())
	assert.Equal(t, `["0xabc","latest"]`, attrs["request.jsonrpc.params"].AsString())
	assert.Equal(t, codes.Error, sp.Status().Code)
	assert.Equal(t, "ErrInvalidConfig", attrs["error.code"].AsString())
}

func TestCovMiscTracing_EndRequestSpanSuccessDetailed(t *testing.T) {
	sr := covMiscRecordSpans(t, 1, true, nil)

	ntw := &covMiscNetwork{id: "evm:1", finality: DataFinalityStateFinalized}
	req := NewNormalizedRequestFromJsonRpcRequest(NewJsonRpcRequest("eth_blockNumber", nil))
	req.SetNetwork(ntw)
	req.SetUser(&User{Id: "alice"})

	jrr, err := NewJsonRpcResponse(1, "0x10", nil)
	require.NoError(t, err)
	resp := NewNormalizedResponse().
		WithRequest(req).
		WithJsonRpcResponse(jrr).
		SetFromCache(true).
		SetUpstream(NewFakeUpstream("up-a")).
		SetAttempts(3).
		SetRetries(2).
		SetHedges(1)

	ctx := StartRequestSpan(context.Background(), req)
	EndRequestSpan(ctx, resp, nil)

	sp := covMiscOnlySpan(t, sr)
	attrs := covMiscAttrs(sp.Attributes())
	assert.Equal(t, codes.Ok, sp.Status().Code)
	assert.True(t, attrs["cache.hit"].AsBool())
	assert.Equal(t, "up-a", attrs["upstream.id"].AsString())
	assert.Equal(t, int64(len(`"0x10"`)), attrs["response.result_size"].AsInt64())
	assert.Equal(t, "evm:1", attrs["network.id"].AsString())
	assert.Equal(t, "alice", attrs["user.id"].AsString())
	assert.Equal(t, "finalized", attrs["request.finality"].AsString())
	assert.Equal(t, "finalized", attrs["response.finality"].AsString())
	assert.Equal(t, int64(3), attrs["execution.attempts"].AsInt64())
	assert.Equal(t, int64(2), attrs["execution.retries"].AsInt64())
	assert.Equal(t, int64(1), attrs["execution.hedges"].AsInt64())
	_, hasParams := attrs["request.jsonrpc.params"]
	assert.False(t, hasParams, "nil params are not recorded")
}

func TestCovMiscTracing_EndRequestSpanNonRecordingContext(t *testing.T) {
	sr := covMiscRecordSpans(t, 1, false, nil)
	EndRequestSpan(context.Background(), NewNormalizedResponse(), nil)
	assert.Empty(t, sr.Ended())
}

func TestCovMiscTracing_ForceFlush(t *testing.T) {
	sr := covMiscRecordSpans(t, 1, false, nil)
	_, s := StartSpan(context.Background(), "f")
	s.End()
	require.NoError(t, ForceFlushTraces(context.Background()))
	assert.Len(t, sr.Ended(), 1)
}

func TestCovMiscTracing_SamplerDecisions(t *testing.T) {
	traceID := trace.TraceID{1}
	forced := []attribute.KeyValue{attribute.Bool(forceTraceAttributeKey, true)}
	notForced := []attribute.KeyValue{attribute.Bool(forceTraceAttributeKey, false), attribute.String("x", "y")}

	cases := []struct {
		rate     float64
		attrs    []attribute.KeyValue
		want     sdktrace.SamplingDecision
		descPart string
	}{
		{0, nil, sdktrace.Drop, "AlwaysOffSampler"},
		{0, notForced, sdktrace.Drop, "AlwaysOffSampler"},
		{0, forced, sdktrace.RecordAndSample, "AlwaysOffSampler"},
		{1, nil, sdktrace.RecordAndSample, "AlwaysOnSampler"},
		{0.5, nil, sdktrace.RecordAndSample, "ParentBased"}, // TraceID{1,0..} is below the ratio bound
	}
	for _, tc := range cases {
		s := createTracingSampler(&TracingConfig{SampleRate: tc.rate})
		res := s.ShouldSample(sdktrace.SamplingParameters{ParentContext: context.Background(), TraceID: traceID, Attributes: tc.attrs})
		assert.Equal(t, tc.want, res.Decision, "rate=%v attrs=%v", tc.rate, tc.attrs)
		assert.True(t, strings.HasPrefix(s.Description(), "ForceTraceSampler{"))
		assert.Contains(t, s.Description(), tc.descPart)
	}
}

func TestCovMiscTracing_HTTPEndpointOptions(t *testing.T) {
	cases := []struct {
		endpoint string
		tls      bool
		wantN    int
		wantErr  string
	}{
		{"collector:4318", false, 2, ""},
		{"collector:4318", true, 1, ""},
		{"https://collector:4318", true, 1, ""},
		{"https://collector:4318/custom", false, 2, ""},
		{"http://collector:4318/", false, 2, ""},
		{"http://collector:4318", true, 0, "tls.enabled needs an https://"},
		{"ftp://collector:1", false, 0, "want http(s)://"},
		{"http://%zz", false, 0, "invalid tracing endpoint"},
	}
	for _, tc := range cases {
		opts, err := tracingHTTPEndpointOptions(tc.endpoint, tc.tls)
		if tc.wantErr != "" {
			require.ErrorContains(t, err, tc.wantErr, tc.endpoint)
			continue
		}
		require.NoError(t, err, tc.endpoint)
		assert.Len(t, opts, tc.wantN, tc.endpoint)
	}
}

func TestCovMiscTracing_ExportersWithTLSAndHeaders(t *testing.T) {
	certFile, keyFile := covMiscWriteCert(t)
	ctx := context.Background()

	goodTLS := &TLSConfig{Enabled: true, CertFile: certFile, KeyFile: keyFile, CAFile: certFile}
	badTLS := &TLSConfig{Enabled: true, CAFile: certFile + ".missing"}

	g, err := createTracingGRPCExporter(ctx, &TracingConfig{Endpoint: "127.0.0.1:1", TLS: goodTLS, Headers: map[string]string{"x-k": "v"}})
	require.NoError(t, err)
	require.NoError(t, g.Shutdown(ctx))
	g, err = createTracingGRPCExporter(ctx, &TracingConfig{Endpoint: "127.0.0.1:1"})
	require.NoError(t, err)
	require.NoError(t, g.Shutdown(ctx))
	_, err = createTracingGRPCExporter(ctx, &TracingConfig{Endpoint: "127.0.0.1:1", TLS: badTLS})
	require.ErrorContains(t, err, "failed to read CA file")

	h, err := createTracingHTTPExporter(ctx, &TracingConfig{Endpoint: "https://127.0.0.1:1", TLS: goodTLS, Headers: map[string]string{"x-k": "v"}})
	require.NoError(t, err)
	require.NoError(t, h.Shutdown(ctx))
	_, err = createTracingHTTPExporter(ctx, &TracingConfig{Endpoint: "127.0.0.1:1", TLS: badTLS})
	require.ErrorContains(t, err, "failed to read CA file")
}

// covMiscResetTracingInit gives InitializeTracing a fresh sync.Once and
// restores every global it may touch.
func covMiscResetTracingInit(t *testing.T) {
	t.Helper()
	prevEnabled, prevDetailed := IsTracingEnabled, IsTracingDetailed
	prevTracer, prevProvider, prevMatchers := tracer, tracerProvider, forceTraceMatchers
	prevGlobalTP := otel.GetTracerProvider()
	prevProp := otel.GetTextMapPropagator()
	prevHandler := otel.GetErrorHandler()
	initOnce = sync.Once{}
	t.Cleanup(func() {
		if tracerProvider != nil && tracerProvider != prevProvider {
			_ = tracerProvider.Shutdown(context.Background())
		}
		IsTracingEnabled, IsTracingDetailed = prevEnabled, prevDetailed
		tracer, tracerProvider, forceTraceMatchers = prevTracer, prevProvider, prevMatchers
		otel.SetTracerProvider(prevGlobalTP)
		otel.SetTextMapPropagator(prevProp)
		otel.SetErrorHandler(prevHandler)
		initOnce = sync.Once{}
	})
}

func TestCovMiscTracing_InitializeDisabled(t *testing.T) {
	covMiscResetTracingInit(t)
	IsTracingEnabled, IsTracingDetailed = true, true
	logger := zerolog.Nop()

	require.NoError(t, InitializeTracing(context.Background(), &logger, nil))
	assert.False(t, IsTracingEnabled)
	assert.False(t, IsTracingDetailed)
	_, span := StartSpan(context.Background(), "after-disabled-init")
	assert.False(t, span.IsRecording())

	// second call is a no-op thanks to sync.Once, even with an enabled config
	require.NoError(t, InitializeTracing(context.Background(), &logger, &TracingConfig{Enabled: true, Protocol: "bogus"}))
	assert.False(t, IsTracingEnabled)
}

func TestCovMiscTracing_InitializeUnsupportedProtocol(t *testing.T) {
	covMiscResetTracingInit(t)
	IsTracingEnabled = false
	logger := zerolog.Nop()
	err := InitializeTracing(context.Background(), &logger, &TracingConfig{Enabled: true, Protocol: "carrier-pigeon"})
	require.ErrorContains(t, err, "unsupported tracing protocol: carrier-pigeon")
	assert.False(t, IsTracingEnabled)
}

func TestCovMiscTracing_InitializeExporterError(t *testing.T) {
	covMiscResetTracingInit(t)
	IsTracingEnabled = false
	logger := zerolog.Nop()
	err := InitializeTracing(context.Background(), &logger, &TracingConfig{Enabled: true, Protocol: TracingProtocolHttp, Endpoint: "ftp://x:1"})
	require.ErrorContains(t, err, "invalid tracing endpoint")
	assert.False(t, IsTracingEnabled)
}

func TestCovMiscTracing_InitializeHTTPExportsSpans(t *testing.T) {
	covMiscResetTracingInit(t)

	var mu sync.Mutex
	var bodies [][]byte
	got := make(chan struct{}, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b bytes.Buffer
		_, _ = b.ReadFrom(r.Body)
		mu.Lock()
		bodies = append(bodies, b.Bytes())
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		got <- struct{}{}
	}))
	defer srv.Close()

	// Debug level exercises the otel-logger wiring branch (global test level
	// is disabled, so nothing is written).
	logger := zerolog.New(io.Discard).Level(zerolog.DebugLevel)
	cfg := &TracingConfig{
		Enabled:            true,
		Protocol:           TracingProtocolHttp,
		Endpoint:           srv.URL,
		SampleRate:         1,
		Detailed:           true,
		ServiceName:        "cov-misc-svc",
		ResourceAttributes: map[string]string{"deployment.env": "test", "skipped": ""},
		ForceTraceMatchers: []*ForceTraceMatcher{{Network: "evm:1", Method: "eth_call"}},
	}
	require.NoError(t, InitializeTracing(context.Background(), &logger, cfg))
	assert.True(t, IsTracingEnabled)
	assert.True(t, IsTracingDetailed)
	forced, reason := ShouldForceTrace("evm:1", "eth_call")
	assert.True(t, forced)
	assert.Equal(t, "network:evm:1,method:eth_call", reason)

	// The global propagator now carries W3C trace context.
	ctxWithSpan, parent := StartSpan(context.Background(), "Parent")
	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctxWithSpan, carrier)
	assert.Contains(t, carrier.Get("traceparent"), parent.SpanContext().TraceID().String())
	parent.End()

	_, span := StartSpan(context.Background(), "Export.Me")
	span.End()
	require.NoError(t, ForceFlushTraces(context.Background()))
	select {
	case <-got:
	case <-time.After(10 * time.Second):
		t.Fatal("collector received no export")
	}
	mu.Lock()
	payload := string(bytes.Join(bodies, nil))
	mu.Unlock()
	assert.Contains(t, payload, "Export.Me")
	assert.Contains(t, payload, "cov-misc-svc")
	assert.Contains(t, payload, "deployment.env")

	require.NoError(t, ShutdownTracing(context.Background()))
	tracerProvider = nil
}

func TestCovMiscTracing_InitializeGRPC(t *testing.T) {
	covMiscResetTracingInit(t)
	logger := zerolog.Nop()
	require.NoError(t, InitializeTracing(context.Background(), &logger, &TracingConfig{
		Enabled: true, Protocol: TracingProtocolGrpc, Endpoint: "127.0.0.1:1", SampleRate: 0.25,
	}))
	assert.True(t, IsTracingEnabled)
	assert.False(t, IsTracingDetailed)
	// Fractional rates are parent-based: a remote parent's decision wins.
	sampledParent := trace.NewSpanContext(trace.SpanContextConfig{TraceID: trace.TraceID{9}, SpanID: trace.SpanID{1}, TraceFlags: trace.FlagsSampled, Remote: true})
	droppedParent := trace.NewSpanContext(trace.SpanContextConfig{TraceID: trace.TraceID{9}, SpanID: trace.SpanID{2}, Remote: true})
	_, s1 := StartSpan(trace.ContextWithRemoteSpanContext(context.Background(), sampledParent), "child")
	_, s2 := StartSpan(trace.ContextWithRemoteSpanContext(context.Background(), droppedParent), "child")
	assert.True(t, s1.IsRecording())
	assert.False(t, s2.IsRecording())
	s1.End()
	s2.End()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = ShutdownTracing(ctx) // export to a closed port may error; shutdown must return
	tracerProvider = nil
}
