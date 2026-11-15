package erpc

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// callHealthCheck runs the handler once and returns the status and body.
func callHealthCheck(t *testing.T, s *HttpServer, projectId string) (int, string) {
	t.Helper()
	w := httptest.NewRecorder()
	startedAt := time.Now()
	encoder := common.SonicCfg.NewEncoder(w)
	s.handleHealthCheck(
		context.Background(), w,
		&http.Request{Method: "GET", URL: &url.URL{Path: "/healthcheck"}},
		&startedAt,
		projectId, "", "",
		encoder,
		func(ctx context.Context, statusCode int, body error) {
			w.WriteHeader(statusCode)
			_ = encoder.Encode(map[string]string{"error": body.Error()})
		},
	)
	resp := w.Result()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, string(body)
}

// A load balancer reads the status code. An instance with no project loaded
// cannot serve, so it must not answer 200.
func TestHealthCheck_NoProjectsIsUnavailable(t *testing.T) {
	s := &HttpServer{
		logger: &log.Logger,
		erpc: &ERPC{
			projectsRegistry: &ProjectsRegistry{preparedProjects: map[string]*PreparedProject{}},
		},
		serverCfg:      &common.ServerConfig{IncludeErrorDetails: &common.TRUE},
		healthCheckCfg: &common.HealthCheckConfig{Mode: common.HealthCheckModeSimple},
		draining:       &atomic.Bool{},
	}

	code, body := callHealthCheck(t, s, "")

	assert.Equal(t, http.StatusServiceUnavailable, code)
	assert.Contains(t, body, "no projects found")
}

// An instance where eRPC never initialized cannot serve, so it must not
// answer 200.
func TestHealthCheck_ErpcNotInitializedIsUnavailable(t *testing.T) {
	s := &HttpServer{
		logger:         &log.Logger,
		serverCfg:      &common.ServerConfig{IncludeErrorDetails: &common.TRUE},
		healthCheckCfg: &common.HealthCheckConfig{Mode: common.HealthCheckModeSimple},
		draining:       &atomic.Bool{},
	}

	code, body := callHealthCheck(t, s, "")

	assert.Equal(t, http.StatusServiceUnavailable, code)
	assert.Contains(t, body, "eRPC is not initialized")
}
