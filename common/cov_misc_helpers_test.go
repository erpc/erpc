package common

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"iter"
	"math/big"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// covMiscNetwork is a minimal Network for exercising helpers that read a
// network's id/config/metrics/finality.
type covMiscNetwork struct {
	id       string
	label    string
	cfg      *NetworkConfig
	metrics  map[string]TrackedMetrics
	finality DataFinalityState
}

func (n *covMiscNetwork) Id() string                        { return n.id }
func (n *covMiscNetwork) Label() string                     { return n.label }
func (n *covMiscNetwork) ProjectId() string                 { return "cov-misc-project" }
func (n *covMiscNetwork) Architecture() NetworkArchitecture { return ArchitectureEvm }
func (n *covMiscNetwork) Config() *NetworkConfig            { return n.cfg }
func (n *covMiscNetwork) Logger() *zerolog.Logger           { l := zerolog.Nop(); return &l }
func (n *covMiscNetwork) GetMethodMetrics(method string) TrackedMetrics {
	if m, ok := n.metrics[method]; ok {
		return m
	}
	return nil
}
func (n *covMiscNetwork) Forward(context.Context, *NormalizedRequest) (*NormalizedResponse, error) {
	return nil, nil
}
func (n *covMiscNetwork) GetFinality(context.Context, *NormalizedRequest, *NormalizedResponse) DataFinalityState {
	return n.finality
}

// covMiscEvmNetwork adds the EVM view and the routable-upstreams surface.
type covMiscEvmNetwork struct {
	*covMiscNetwork
	latest, finalized int64
	leader            Upstream
	refreshed         atomic.Int32
	routable          []Upstream
}

func (n *covMiscEvmNetwork) EvmHead(context.Context, *NormalizedRequest) EvmHead {
	return EvmHead{Latest: n.latest, Finalized: n.finalized, Leader: n.leader}
}
func (n *covMiscEvmNetwork) EvmRefreshLatestBlock(context.Context)                { n.refreshed.Add(1) }
func (n *covMiscEvmNetwork) EvmRoutableUpstreams(context.Context) iter.Seq[Upstream] {
	return func(yield func(Upstream) bool) {
		for _, u := range n.routable {
			if !yield(u) {
				return
			}
		}
	}
}

type covMiscMetrics struct{ q QuantileTracker }

func (m *covMiscMetrics) ErrorRate() float64                    { return 0 }
func (m *covMiscMetrics) GetResponseQuantiles() QuantileTracker { return m.q }

// covMiscSyncedUpstream overrides the fake's fixed syncing state.
type covMiscSyncedUpstream struct {
	*FakeUpstream
	state EvmSyncingState
}

func (u *covMiscSyncedUpstream) EvmSyncingState() EvmSyncingState { return u.state }

func covMiscNewSyncedUpstream(id string, state EvmSyncingState) *covMiscSyncedUpstream {
	return &covMiscSyncedUpstream{FakeUpstream: NewFakeUpstream(id).(*FakeUpstream), state: state}
}

// covMiscWriteCert writes a self-signed ECDSA cert/key pair to a temp dir and
// returns (certFile, keyFile).
func covMiscWriteCert(t *testing.T) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "cov-misc.local"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	keyDer, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)

	dir := t.TempDir()
	certFile := filepath.Join(dir, "cert.pem")
	keyFile := filepath.Join(dir, "key.pem")
	require.NoError(t, os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600))
	require.NoError(t, os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDer}), 0o600))
	return certFile, keyFile
}

// covMiscRecordSpans swaps the package tracer for one backed by an in-memory
// span recorder and restores every tracing global on cleanup.
func covMiscRecordSpans(t *testing.T, sampleRate float64, detailed bool, matchers []*ForceTraceMatcher) *tracetest.SpanRecorder {
	t.Helper()
	prevEnabled, prevDetailed := IsTracingEnabled, IsTracingDetailed
	prevTracer, prevProvider, prevMatchers := tracer, tracerProvider, forceTraceMatchers

	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(createTracingSampler(&TracingConfig{SampleRate: sampleRate})),
		sdktrace.WithSpanProcessor(sr),
	)
	tracer = tp.Tracer(instrumentationName)
	tracerProvider = tp
	IsTracingEnabled = true
	IsTracingDetailed = detailed
	forceTraceMatchers = matchers

	t.Cleanup(func() {
		_ = tp.Shutdown(context.Background())
		IsTracingEnabled, IsTracingDetailed = prevEnabled, prevDetailed
		tracer, tracerProvider, forceTraceMatchers = prevTracer, prevProvider, prevMatchers
	})
	return sr
}

// covMiscDisableTracing forces the disabled path and restores on cleanup.
func covMiscDisableTracing(t *testing.T) {
	t.Helper()
	prevEnabled, prevDetailed, prevProvider := IsTracingEnabled, IsTracingDetailed, tracerProvider
	IsTracingEnabled, IsTracingDetailed = false, false
	t.Cleanup(func() {
		IsTracingEnabled, IsTracingDetailed, tracerProvider = prevEnabled, prevDetailed, prevProvider
	})
}

func covMiscAttrs(kvs []attribute.KeyValue) map[string]attribute.Value {
	out := make(map[string]attribute.Value, len(kvs))
	for _, kv := range kvs {
		out[string(kv.Key)] = kv.Value
	}
	return out
}

func covMiscOnlySpan(t *testing.T, sr *tracetest.SpanRecorder) sdktrace.ReadOnlySpan {
	t.Helper()
	ended := sr.Ended()
	require.Len(t, ended, 1)
	return ended[0]
}

var _ trace.Span = defaultNoopSpan
