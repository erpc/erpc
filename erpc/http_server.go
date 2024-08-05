package erpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
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
		if len(segments) < 2 || len(segments) > 4 {
			handleErrorResponse(s.logger, nil, common.NewErrInvalidUrlPath(r.URL.Path), w)
			return
		}

		projectId := segments[1]
		var architecture, chainId string

		if len(segments) == 4 {
			// URL format: /main/evm/111
			architecture = segments[2]
			chainId = segments[3]
		} else {
			// URL format: /main
			// networkId will be provided in the request body
			architecture = ""
			chainId = ""
		}

		project, err := s.erpc.GetProject(projectId)
		if err != nil {
			handleErrorResponse(s.logger, nil, err, w)
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

		body, err := io.ReadAll(r.Body)
		if err != nil {
			handleErrorResponse(s.logger, nil, common.NewErrInvalidRequest(err), w)
			return
		}

		s.logger.Debug().Msgf("received request for projectId: %s, architecture: %s with body: %s", projectId, architecture, body)

		var requests []json.RawMessage
		err = json.Unmarshal(body, &requests)
		isBatch := err == nil

		if !isBatch {
			requests = []json.RawMessage{body}
		}

		responses := make([]interface{}, len(requests))
		var wg sync.WaitGroup
		wg.Add(len(requests))

		for i, reqBody := range requests {
			go func(index int, rawReq json.RawMessage) {
				defer wg.Done()

				nq := upstream.NewNormalizedRequest(rawReq)
				nq.ApplyDirectivesFromHttpHeaders(r.Header)

				var networkId string

				if architecture == "" || chainId == "" {
					// Extract networkId from the request body
					var req map[string]interface{}
					if err := json.Unmarshal(rawReq, &req); err != nil {
						responses[index] = processErrorBody(s.logger, nq, common.NewErrInvalidRequest(err))
						return
					}
					if networkIdFromBody, ok := req["networkId"].(string); ok {
						networkId = networkIdFromBody
						parts := strings.Split(networkId, ":")
						if len(parts) != 2 {
							responses[index] = processErrorBody(s.logger, nq, common.NewErrInvalidRequest(fmt.Errorf("invalid networkId format")))
							return
						}
						architecture = parts[0]
						chainId = parts[1]
					} else {
						responses[index] = processErrorBody(s.logger, nq, common.NewErrInvalidRequest(fmt.Errorf("networkId not provided in request body")))
						return
					}
				} else {
					networkId = fmt.Sprintf("%s:%s", architecture, chainId)
				}

				nw, err := project.GetNetwork(networkId)
				if err != nil {
					responses[index] = processErrorBody(s.logger, nq, err)
					return
				}
				nq.SetNetwork(nw)

				ap, err := auth.NewPayloadFromHttp(project.Config, nq, r)
				if err != nil {
					responses[index] = processErrorBody(s.logger, nq, err)
					return
				}

				if err := project.Authenticate(requestCtx, nq, ap); err != nil {
					responses[index] = processErrorBody(s.logger, nq, err)
					return
				}

				resp, err := project.Forward(requestCtx, networkId, nq)
				if err != nil {
					responses[index] = processErrorBody(s.logger, nq, err)
					return
				}

				responses[index] = resp
			}(i, reqBody)
		}

		wg.Wait()

		w.Header().Set("Content-Type", "application/json")

		if isBatch {
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(responses)
		} else {
			if _, ok := responses[0].(error); ok {
				w.WriteHeader(processErrorStatusCode(responses[0].(error)))
			} else {
				w.WriteHeader(http.StatusOK)
			}

			json.NewEncoder(w).Encode(responses[0])
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

func processErrorBody(logger *zerolog.Logger, nq *upstream.NormalizedRequest, err error) interface{} {
	if !common.IsNull(err) {
		if common.HasErrorCode(err, common.ErrCodeEndpointClientSideException) {
			logger.Debug().Err(err).Msgf("forward request errored with client-side exception")
		} else {
			logger.Error().Err(err).Msgf("failed to forward request")
		}
	}

	err = common.TranslateToJsonRpcException(err)

	var jsonrpcVersion string = "2.0"
	var reqId interface{} = nil
	if nq != nil {
		jrr, _ := nq.JsonRpcRequest()
		if jrr != nil {
			jsonrpcVersion = jrr.JSONRPC
			reqId = jrr.ID
		}
	}

	jre := &common.ErrJsonRpcExceptionInternal{}
	if errors.As(err, &jre) {
		return map[string]interface{}{
			"jsonrpc": jsonrpcVersion,
			"id":      reqId,
			"error": map[string]interface{}{
				"code":    jre.NormalizedCode(),
				"message": jre.Message,
				"data":    jre.Details["data"],
				"cause":   err,
			},
		}
	}

	var bodyErr common.ErrorWithBody
	if errors.As(err, &bodyErr) {
		return bodyErr.ErrorResponseBody()
	} else if _, ok := err.(*common.BaseError); ok {
		return err
	} else if serr, ok := err.(common.StandardError); ok {
		return serr
	}

	return common.BaseError{
		Code:    "ErrUnknown",
		Message: "unexpected server error",
		Cause:   err,
	}
}

func processErrorStatusCode(err error) int {
	var httpErr common.ErrorWithStatusCode
	if errors.As(err, &httpErr) {
		return httpErr.ErrorStatusCode()
	}
	return http.StatusInternalServerError
}

func handleErrorResponse(logger *zerolog.Logger, nq *upstream.NormalizedRequest, err error, hrw http.ResponseWriter) {
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

	var jsonrpcVersion string = "2.0"
	var reqId interface{} = nil
	if nq != nil {
		jrr, _ := nq.JsonRpcRequest()
		if jrr != nil {
			jsonrpcVersion = jrr.JSONRPC
			reqId = jrr.ID
		}
	}

	jre := &common.ErrJsonRpcExceptionInternal{}
	if errors.As(err, &jre) {
		writeErr := json.NewEncoder(hrw).Encode(map[string]interface{}{
			"jsonrpc": jsonrpcVersion,
			"id":      reqId,
			"error": map[string]interface{}{
				"code":    jre.NormalizedCode(),
				"message": jre.Message,
				"data":    jre.Details["data"],
				"cause":   err,
			},
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
