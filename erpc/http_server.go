package erpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/erpc/erpc/auth"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/upstream"
	"github.com/rs/zerolog"
)

type HttpServer struct {
	config *common.ServerConfig
	server *http.Server
	erpc   *ERPC
	logger *zerolog.Logger
}

func NewHttpServer(ctx context.Context, logger *zerolog.Logger, cfg *common.ServerConfig, erpc *ERPC) *HttpServer {
	addr := fmt.Sprintf("%s:%d", cfg.HttpHost, cfg.HttpPort)

	timeOutDur, err := time.ParseDuration(cfg.MaxTimeout)
	if err != nil {
		if cfg.MaxTimeout != "" {
			logger.Error().Err(err).Msgf("failed to parse max timeout duration using 30s default")
		}
		timeOutDur = 30 * time.Second
	}

	srv := &HttpServer{
		config: cfg,
		erpc:   erpc,
		logger: logger,
	}

	handler := http.NewServeMux()
	handler.HandleFunc("/", srv.handleRequest(timeOutDur))

	srv.server = &http.Server{
		Addr:    addr,
		Handler: handler,
	}

	go func() {
		<-ctx.Done()
		logger.Info().Msg("shutting down http server...")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx, logger); err != nil {
			logger.Error().Msgf("http server forced to shutdown: %s", err)
		} else {
			logger.Info().Msg("http server stopped")
		}
	}()

	return srv
}

func (s *HttpServer) handleRequest(timeOutDur time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		segments := strings.Split(r.URL.Path, "/")
		if len(segments) != 4 {
			handleErrorResponse(s.logger, common.NewErrInvalidUrlPath(r.URL.Path), w)
			return
		}

		projectId := segments[1]
		networkId := fmt.Sprintf("%s:%s", segments[2], segments[3])

		project, err := s.erpc.GetProject(projectId)
		if err != nil {
			handleErrorResponse(s.logger, err, w)
			return
		}

		// Apply CORS if configured
		if project.Config.CORS != nil {
			if !s.handleCORS(w, r, project.Config.CORS) {
				return
			}

			if r.Method == http.MethodOptions {
				return // CORS preflight request handled
			}
		}

		requestCtx, cancel := context.WithTimeout(r.Context(), timeOutDur)
		defer cancel()

		resultChan := make(chan common.NormalizedResponse, 1)
		errChan := make(chan error, 1)

		go func() {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				errChan <- common.NewErrInvalidRequest(err)
				return
			}

			s.logger.Debug().Msgf("received request for projectId: %s, networkId: %s with body: %s", projectId, networkId, body)

			nw, err := s.erpc.GetNetwork(projectId, networkId)
			if err != nil {
				errChan <- err
				return
			}

			nq := upstream.NewNormalizedRequest(body)
			nq.SetNetwork(nw)
			nq.ApplyDirectivesFromHttpHeaders(r.Header)

			ap, err := auth.NewPayloadFromHttp(project.Config, nq, r)
			if err != nil {
				errChan <- err
				return
			}

			if err := project.Authenticate(requestCtx, nq, ap); err != nil {
				errChan <- err
				return
			}

			resp, err := project.Forward(requestCtx, networkId, nq)
			if err != nil {
				errChan <- err
				return
			}

			s.logger.Debug().Msgf("request forwarded successfully for projectId: %s, networkId: %s", projectId, networkId)
			resultChan <- resp
		}()

		select {
		case resp := <-resultChan:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write(resp.Body())
		case err := <-errChan:
			handleErrorResponse(s.logger, err, w)
		case <-requestCtx.Done():
			handleErrorResponse(s.logger, common.NewErrRequestTimeOut(timeOutDur), w)
		}
	}
}
func (s *HttpServer) handleCORS(w http.ResponseWriter, r *http.Request, corsConfig *common.CORSConfig) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true // Not a CORS request
	}

	// Count all CORS requests
	health.MetricCORSRequestsTotal.WithLabelValues(r.URL.Path, origin).Inc()

	// Check if the origin is allowed
	allowed := false
	for _, allowedOrigin := range corsConfig.AllowedOrigins {
		if common.WildcardMatch(allowedOrigin, origin) {
			allowed = true
			break
		}
	}

	if !allowed {
		s.logger.Debug().Str("origin", origin).Msg("CORS request from disallowed origin")
		health.MetricCORSDisallowedOriginTotal.WithLabelValues(r.URL.Path, origin).Inc()
		
		if r.Method == http.MethodOptions {
			// For preflight requests, return 204 No Content
			w.WriteHeader(http.StatusNoContent)
		} else {
			// For actual requests, return 403 Forbidden
			http.Error(w, "CORS request from disallowed origin", http.StatusForbidden)
		}
		return false
	}

	// Set CORS headers
	w.Header().Set("Access-Control-Allow-Origin", origin)
	w.Header().Set("Access-Control-Allow-Methods", strings.Join(corsConfig.AllowedMethods, ", "))
	w.Header().Set("Access-Control-Allow-Headers", strings.Join(corsConfig.AllowedHeaders, ", "))
	w.Header().Set("Access-Control-Expose-Headers", strings.Join(corsConfig.ExposedHeaders, ", "))

	if corsConfig.AllowCredentials {
		w.Header().Set("Access-Control-Allow-Credentials", "true")
	}

	if corsConfig.MaxAge > 0 {
		w.Header().Set("Access-Control-Max-Age", fmt.Sprintf("%d", corsConfig.MaxAge))
	}

	// Handle preflight request
	if r.Method == http.MethodOptions {
		health.MetricCORSPreflightRequestsTotal.WithLabelValues(r.URL.Path, origin).Inc()
		w.WriteHeader(http.StatusNoContent)
		return false
	}

	return true
}

func handleErrorResponse(logger *zerolog.Logger, err error, hrw http.ResponseWriter) {
	if !common.IsNull(err) {
		if common.HasErrorCode(err, common.ErrCodeEndpointClientSideException) {
			logger.Debug().Err(err).Msgf("forward request errored with client-side exception")
		} else {
			logger.Error().Err(err).Msgf("failed to forward request")
		}
	}

	//
	// 1) Decide on the http status code
	//
	hrw.Header().Set("Content-Type", "application/json")
	var httpErr common.ErrorWithStatusCode
	if errors.As(err, &httpErr) {
		hrw.WriteHeader(httpErr.ErrorStatusCode())
	} else {
		hrw.WriteHeader(http.StatusInternalServerError)
	}

	//
	// 2) Write the body based on the transport type
	//

	// TODO when the second transport is implemented we need to detect which transport is used and translate the error accordingly
	err = common.TranslateToJsonRpcException(err)

	jre := &common.ErrJsonRpcExceptionInternal{}
	if errors.As(err, &jre) {
		writeErr := json.NewEncoder(hrw).Encode(map[string]interface{}{
			"code":    jre.NormalizedCode(),
			"message": jre.Message,
			"data":    jre.Details["data"],
			"cause":   err,
		})
		if writeErr != nil {
			logger.Error().Err(writeErr).Msgf("failed to encode error response body")
		}
		return
	}

	//
	// 3) Final fallback if an unexpected error happens, this code path is very unlikely to happen
	//
	var bodyErr common.ErrorWithBody
	var writeErr error

	if errors.As(err, &bodyErr) {
		writeErr = json.NewEncoder(hrw).Encode(bodyErr.ErrorResponseBody())
	} else if _, ok := err.(*common.BaseError); ok {
		writeErr = json.NewEncoder(hrw).Encode(err)
	} else {
		if serr, ok := err.(common.StandardError); ok {
			writeErr = json.NewEncoder(hrw).Encode(serr)
		} else {
			writeErr = json.NewEncoder(hrw).Encode(
				common.BaseError{
					Code:    "ErrUnknown",
					Message: "unexpected server error",
					Cause:   err,
				},
			)
		}
	}

	if writeErr != nil {
		logger.Error().Err(writeErr).Msgf("failed to encode error response body")
	}
}

func (s *HttpServer) Start(logger *zerolog.Logger) error {
	logger.Info().Msgf("starting http server on %s", s.server.Addr)
	return s.server.ListenAndServe()
}

func (s *HttpServer) Shutdown(ctx context.Context, logger *zerolog.Logger) error {
	logger.Info().Msg("shutting down http server")
	return s.server.Shutdown(ctx)
}
