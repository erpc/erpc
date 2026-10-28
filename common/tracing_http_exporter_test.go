package common

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// Every endpoint form the config accepts must reach the receiver; the
// documented "http://host:port" form and the HTTP default once exported
// nothing (erpc/erpc#1276).
func TestTracingHTTPExporter_EndpointForms(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	hostPort := strings.TrimPrefix(srv.URL, "http://")

	cases := []struct {
		endpoint string
		wantPath string
	}{
		{hostPort, "/v1/traces"},
		{srv.URL, "/v1/traces"},
		{srv.URL + "/", "/v1/traces"},
		{srv.URL + "/otlp/v1/traces", "/otlp/v1/traces"},
	}
	for _, tc := range cases {
		t.Run(tc.endpoint, func(t *testing.T) {
			mu.Lock()
			paths = nil
			mu.Unlock()

			ctx := context.Background()
			exp, err := createTracingHTTPExporter(ctx, &TracingConfig{Endpoint: tc.endpoint})
			require.NoError(t, err)
			tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
			_, span := tp.Tracer("test").Start(ctx, "op")
			span.End()
			require.NoError(t, tp.Shutdown(ctx))

			mu.Lock()
			defer mu.Unlock()
			require.Equal(t, []string{tc.wantPath}, paths)
		})
	}

	for _, bad := range []string{"ftp://x:1", "http://", "http://x:1"} {
		cfg := &TracingConfig{Endpoint: bad}
		if bad == "http://x:1" {
			cfg.TLS = &TLSConfig{Enabled: true}
		}
		_, err := createTracingHTTPExporter(context.Background(), cfg)
		require.Error(t, err, bad)
	}
}
