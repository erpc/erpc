package erpc

import (
	"compress/gzip"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bytedance/sonic"
	"github.com/erpc/erpc/auth"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

type HttpServer struct {
	appCtx                  context.Context
	serverCfg               *common.ServerConfig
	healthCheckCfg          *common.HealthCheckConfig
	adminCfg                *common.AdminConfig
	server                  *http.Server
	erpc                    *ERPC
	logger                  *zerolog.Logger
	healthCheckAuthRegistry *auth.AuthRegistry
	draining                *atomic.Bool
}

func NewHttpServer(
	ctx context.Context,
	logger *zerolog.Logger,
	cfg *common.ServerConfig,
	healthCheckCfg *common.HealthCheckConfig,
	adminCfg *common.AdminConfig,
	erpc *ERPC,
) (*HttpServer, error) {
	reqMaxTimeout := 150 * time.Second
	if cfg.MaxTimeout != nil {
		reqMaxTimeout = cfg.MaxTimeout.Duration()
	}
	readTimeout := 30 * time.Second
	if cfg.ReadTimeout != nil {
		readTimeout = cfg.ReadTimeout.Duration()
	}
	writeTimeout := 120 * time.Second
	if cfg.WriteTimeout != nil {
		writeTimeout = cfg.WriteTimeout.Duration()
	}

	draining := atomic.Bool{}
	go func() {
		<-ctx.Done()
		draining.Store(true)
		log.Info().Msg("entering draining mode → healthcheck will fail")
	}()

	srv := &HttpServer{
		logger:         logger,
		appCtx:         ctx,
		serverCfg:      cfg,
		healthCheckCfg: healthCheckCfg,
		adminCfg:       adminCfg,
		erpc:           erpc,
		draining:       &draining,
	}

	h := srv.createRequestHandler()
	if cfg.EnableGzip != nil && *cfg.EnableGzip {
		h = gzipHandler(h)
	}
	srv.server = &http.Server{
		Handler: TimeoutHandler(
			h,
			reqMaxTimeout,
		),
		ReadTimeout:  readTimeout,
		WriteTimeout: writeTimeout,
	}

	if healthCheckCfg != nil && healthCheckCfg.Auth != nil {
		var err error
		srv.healthCheckAuthRegistry, err = auth.NewAuthRegistry(logger, "healthcheck", healthCheckCfg.Auth, nil)
		if err != nil {
			return nil, fmt.Errorf("failed to create healthcheck auth registry: %w", err)
		}
	}

	go func() {
		<-ctx.Done()
		// wait for readiness probe to mark the pod NotReady
		// ideally (period_seconds * failure_threshold) + safety margin (1s)
		if srv.serverCfg.WaitBeforeShutdown != nil {
			time.Sleep(srv.serverCfg.WaitBeforeShutdown.Duration())
		}
		if err := srv.Shutdown(logger); err != nil {
			logger.Error().Msgf("http server forced to shutdown: %s", err)
		} else {
			logger.Info().Msg("http server stopped")
		}
	}()

	return srv, nil
}

func (s *HttpServer) createRequestHandler() http.Handler {
	handleRequest := func(httpCtx context.Context, r *http.Request, w http.ResponseWriter, writeFatalError func(ctx context.Context, statusCode int, body error)) {
		startedAt := time.Now()
		encoder := common.SonicCfg.NewEncoder(w)
		encoder.SetEscapeHTML(false)

		// Get host without port number
		host := r.Host
		if colonIndex := strings.Index(host, ":"); colonIndex != -1 {
			host = host[:colonIndex]
		}

		var projectId, architecture, chainId string
		var isAdmin, isHealthCheck bool
		var err error

		// Check aliasing rules
		if s.serverCfg.Aliasing != nil {
			for _, rule := range s.serverCfg.Aliasing.Rules {
				matched, err := common.WildcardMatch(rule.MatchDomain, host)
				if err != nil {
					s.logger.Error().Err(err).Interface("rule", rule).Msg("failed to match aliasing rule")
					continue
				}
				if matched {
					projectId = rule.ServeProject
					architecture = rule.ServeArchitecture
					chainId = rule.ServeChain
					break
				}
			}
		}

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-ERPC-Version", common.ErpcVersion)
		w.Header().Set("X-ERPC-Commit", common.ErpcCommitSha)

		projectId, architecture, chainId, isAdmin, isHealthCheck, err = s.parseUrlPath(r, projectId, architecture, chainId)
		if err != nil {
			handleErrorResponse(
				httpCtx,
				s.logger,
				&startedAt,
				nil,
				err,
				w,
				encoder,
				writeFatalError,
				&common.TRUE,
			)
			return
		}

		if isHealthCheck {
			s.handleHealthCheck(httpCtx, w, r, &startedAt, projectId, architecture, chainId, encoder, writeFatalError)
			return
		}

		if isAdmin {
			if s.adminCfg != nil && s.adminCfg.CORS != nil {
				if !s.handleCORS(httpCtx, w, r, s.adminCfg.CORS) || r.Method == http.MethodOptions {
					return
				}
			}
		}

		var lg zerolog.Logger
		if isAdmin {
			lg = s.logger.With().Str("component", "admin").Logger()
		} else {
			lg = s.logger.With().Str("component", "proxy").Str("projectId", projectId).Str("networkId", fmt.Sprintf("%s:%s", architecture, chainId)).Logger()
		}

		if projectId == "" && !isAdmin {
			handleErrorResponse(
				httpCtx,
				&lg,
				&startedAt,
				nil,
				common.NewErrInvalidRequest(fmt.Errorf("projectId is required in path or must be aliased")),
				w,
				encoder,
				writeFatalError,
				s.serverCfg.IncludeErrorDetails,
			)
			return
		}

		project, err := s.erpc.GetProject(projectId)
		if err != nil {
			handleErrorResponse(
				httpCtx,
				&lg,
				&startedAt,
				nil,
				err,
				w,
				encoder,
				writeFatalError,
				&common.TRUE,
			)
			return
		}

		if project != nil && project.Config.CORS != nil {
			if !s.handleCORS(httpCtx, w, r, project.Config.CORS) || r.Method == http.MethodOptions {
				return
			}
		}

		// Handle gzipped request bodies
		var bodyReader io.Reader = r.Body
		if r.Header.Get("Content-Encoding") == "gzip" {
			gzReader, err := gzip.NewReader(r.Body)
			if err != nil {
				handleErrorResponse(
					httpCtx,
					&lg,
					&startedAt,
					nil,
					common.NewErrInvalidRequest(fmt.Errorf("invalid gzip body: %w", err)),
					w,
					encoder,
					writeFatalError,
					&common.TRUE,
				)
				return
			}
			defer gzReader.Close()
			bodyReader = gzReader
		}

		// Replace the existing body read with our potentially decompressed reader
		_, readBodySpan := common.StartDetailSpan(httpCtx, "Http.ReadBody")
		body, err := util.ReadAll(bodyReader, 1024*1024, 512)
		readBodySpan.End()
		if err != nil {
			common.SetTraceSpanError(readBodySpan, err)
			handleErrorResponse(
				httpCtx,
				&lg,
				&startedAt,
				nil,
				err,
				w,
				encoder,
				writeFatalError,
				&common.TRUE,
			)
			return
		}

		_, parseRequestsSpan := common.StartDetailSpan(httpCtx, "Http.ParseRequests")
		lg.Info().RawJSON("body", body).Msgf("received http request")

		var requests []json.RawMessage
		isBatch := len(body) > 0 && body[0] == '['
		if !isBatch {
			requests = []json.RawMessage{body}
		} else {
			err = common.SonicCfg.Unmarshal(body, &requests)
			if err != nil {
				handleErrorResponse(
					httpCtx,
					&lg,
					&startedAt,
					nil,
					common.NewErrJsonRpcRequestUnmarshal(err, body),
					w,
					encoder,
					writeFatalError,
					&common.TRUE,
				)
				common.SetTraceSpanError(parseRequestsSpan, err)
				parseRequestsSpan.End()
				return
			}
		}

		responses := make([]interface{}, len(requests))
		var wg sync.WaitGroup

		headers := r.Header
		queryArgs := r.URL.Query()

		parseRequestsSpan.End()

		for i, reqBody := range requests {
			wg.Add(1)
			go func(index int, rawReq json.RawMessage, headers http.Header, queryArgs map[string][]string) {
				defer func() {
					defer wg.Done()
					if rec := recover(); rec != nil {
						telemetry.MetricUnexpectedPanicTotal.WithLabelValues(
							"request-handler",
							fmt.Sprintf("project:%s network:%s", architecture, chainId),
							common.ErrorFingerprint(rec),
						).Inc()
						lg.Error().
							Interface("panic", rec).
							Str("stack", string(debug.Stack())).
							Msgf("unexpected server panic on per-request handler")
						err := fmt.Errorf("unexpected server panic on per-request handler: %v stack: %s", rec, string(debug.Stack()))
						responses[index] = processErrorBody(&lg, &startedAt, nil, err, s.serverCfg.IncludeErrorDetails)
					}
				}()

				nq := common.NewNormalizedRequest(rawReq)
				requestCtx := common.StartRequestSpan(httpCtx, nq)

				// Validate the raw JSON-RPC payload early
				if err := nq.Validate(); err != nil {
					responses[index] = processErrorBody(&lg, &startedAt, nq, err, &common.TRUE)
					common.EndRequestSpan(requestCtx, nil, responses[index])
					return
				}

				method, _ := nq.Method()
				rlg := lg.With().Str("method", method).Logger()

				var ap *auth.AuthPayload
				var err error

				if project != nil {
					ap, err = auth.NewPayloadFromHttp(method, r.RemoteAddr, headers, queryArgs)
				} else if isAdmin {
					ap, err = auth.NewPayloadFromHttp(method, r.RemoteAddr, headers, queryArgs)
				}
				if err != nil {
					responses[index] = processErrorBody(&rlg, &startedAt, nq, err, &common.TRUE)
					common.EndRequestSpan(requestCtx, nil, err)
					return
				}

				if isAdmin {
					if err := s.erpc.AdminAuthenticate(requestCtx, method, ap); err != nil {
						responses[index] = processErrorBody(&rlg, &startedAt, nq, err, &common.TRUE)
						common.EndRequestSpan(requestCtx, nil, err)
						return
					}
				} else {
					if err := project.AuthenticateConsumer(requestCtx, method, ap); err != nil {
						responses[index] = processErrorBody(&rlg, &startedAt, nq, err, &common.TRUE)
						common.EndRequestSpan(requestCtx, nil, err)
						return
					}
				}

				if isAdmin {
					if s.adminCfg != nil {
						resp, err := s.erpc.AdminHandleRequest(requestCtx, nq)
						if err != nil {
							responses[index] = processErrorBody(&rlg, &startedAt, nq, err, &common.TRUE)
							common.EndRequestSpan(requestCtx, nil, err)
							return
						}
						responses[index] = resp
						common.EndRequestSpan(requestCtx, resp, nil)
						return
					} else {
						responses[index] = processErrorBody(
							&rlg,
							&startedAt,
							nq,
							common.NewErrAuthUnauthorized(
								"",
								"admin is not enabled for this project",
							),
							s.serverCfg.IncludeErrorDetails,
						)
						common.EndRequestSpan(requestCtx, nil, err)
						return
					}
				}

				var networkId string

				if architecture == "" || chainId == "" {
					var req map[string]interface{}
					if err := common.SonicCfg.Unmarshal(rawReq, &req); err != nil {
						responses[index] = processErrorBody(&rlg, &startedAt, nq, common.NewErrInvalidRequest(err), &common.TRUE)
						common.EndRequestSpan(requestCtx, nil, err)
						return
					}
					if networkIdFromBody, ok := req["networkId"].(string); ok {
						networkId = networkIdFromBody
						parts := strings.Split(networkId, ":")
						if len(parts) == 2 {
							architecture = parts[0]
							chainId = parts[1]
						}
					}
				} else {
					networkId = fmt.Sprintf("%s:%s", architecture, chainId)
				}

				if architecture == "" || chainId == "" {
					responses[index] = processErrorBody(&rlg, &startedAt, nq, common.NewErrInvalidRequest(fmt.Errorf(
						"architecture and chain must be provided in URL (for example /<project>/evm/42161) or in request body (for example \"networkId\":\"evm:42161\") or configureed via domain aliasing",
					)), s.serverCfg.IncludeErrorDetails)
					common.EndRequestSpan(requestCtx, nil, err)
					return
				}

				nw, err := project.GetNetwork(networkId)
				if err != nil {
					responses[index] = processErrorBody(&rlg, &startedAt, nq, err, s.serverCfg.IncludeErrorDetails)
					common.EndRequestSpan(requestCtx, nil, err)
					return
				}
				nq.SetNetwork(nw)

				nq.ApplyDirectiveDefaults(nw.Config().DirectiveDefaults)
				nq.ApplyDirectivesFromHttp(headers, queryArgs)
				rlg.Trace().Interface("directives", nq.Directives()).Msgf("applied request directives")

				resp, err := project.Forward(requestCtx, networkId, nq)
				if err != nil {
					responses[index] = processErrorBody(&rlg, &startedAt, nq, err, s.serverCfg.IncludeErrorDetails)
					common.EndRequestSpan(requestCtx, nil, err)
					return
				}

				responses[index] = resp
				common.EndRequestSpan(requestCtx, resp, nil)
			}(i, reqBody, headers, queryArgs)
		}

		wg.Wait()

		httpCtx, writeResponseSpan := common.StartDetailSpan(httpCtx, "HttpServer.WriteResponse")
		defer writeResponseSpan.End()

		if err := httpCtx.Err(); err != nil {
			if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				cause := context.Cause(httpCtx)
				if cause != nil {
					err = cause
				}
				s.logger.Trace().Err(err).Msg("request premature context error")
				writeFatalError(httpCtx, http.StatusInternalServerError, err)
			}
			return
		}

		common.InjectHTTPResponseTraceContext(httpCtx, w)

		if isBatch {
			statusSet := false
			for _, resp := range responses {
				statusCode := determineResponseStatusCode(resp)
				if statusCode != http.StatusOK {
					if !statusSet {
						w.WriteHeader(statusCode)
						statusSet = true
					}
				}
			}
			if !statusSet {
				w.WriteHeader(http.StatusOK)
			}

			bw := NewBatchResponseWriter(responses)
			_, err = bw.WriteTo(w)
			for _, resp := range responses {
				if r, ok := resp.(*common.NormalizedResponse); ok {
					go r.Release()
				}
			}

			if err != nil && !errors.Is(err, http.ErrHandlerTimeout) {
				s.logger.Error().Err(err).Msg("failed to write batch response")
				writeFatalError(httpCtx, http.StatusInternalServerError, err)
				return
			}

			common.EnrichHTTPServerSpan(httpCtx, http.StatusOK, nil)
		} else {
			res := responses[0]
			setResponseHeaders(httpCtx, res, w)
			statusCode := determineResponseStatusCode(res)
			w.WriteHeader(statusCode)

			switch v := res.(type) {
			case *common.NormalizedResponse:
				_, err = v.WriteTo(w)
				go v.Release()
			case *HttpJsonRpcErrorResponse:
				_, err = writeJsonRpcError(w, v)
			default:
				err = common.SonicCfg.NewEncoder(w).Encode(res)
			}

			if err != nil {
				writeFatalError(httpCtx, statusCode, err)
				return
			} else {
				common.EnrichHTTPServerSpan(httpCtx, statusCode, nil)
			}
		}
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Extract trace context from request headers and start a new span
		httpCtx, span := common.StartHTTPServerSpan(r.Context(), r)
		defer span.End()

		finalErrorOnce := &sync.Once{}
		writeFatalError := func(httpCtx context.Context, statusCode int, err error) {
			finalErrorOnce.Do(func() {
				defer func() {
					if rec := recover(); rec != nil {
						telemetry.MetricUnexpectedPanicTotal.WithLabelValues(
							"final-error-writer",
							fmt.Sprintf("statusCode:%d", statusCode),
							common.ErrorFingerprint(rec)+" via err:"+common.ErrorFingerprint(err),
						).Inc()
						s.logger.Error().
							Interface("panic", rec).
							Str("stack", string(debug.Stack())).
							Msgf("unexpected server panic on final error writer")
					}
				}()
				w.WriteHeader(statusCode)

				msg, err := common.SonicCfg.Marshal(err.Error())
				if err != nil {
					msg, _ = common.SonicCfg.Marshal(err.Error())
				}
				body := fmt.Sprintf(`{"jsonrpc":"2.0","error":{"code":-32603,"message":%s}}`, msg)

				fmt.Fprint(w, body)
				common.EnrichHTTPServerSpan(httpCtx, statusCode, err)
			})
		}

		defer func() {
			if rec := recover(); rec != nil {
				telemetry.MetricUnexpectedPanicTotal.WithLabelValues(
					"top-level-handler",
					"",
					common.ErrorFingerprint(rec),
				).Inc()
				s.logger.Error().
					Interface("panic", rec).
					Str("stack", string(debug.Stack())).
					Msgf("unexpected panic on top-level handler")
				writeFatalError(
					httpCtx,
					http.StatusInternalServerError,
					fmt.Errorf(`unexpected server panic on top-level handler: %s`, rec),
				)
			}
		}()

		handleRequest(httpCtx, r, w, writeFatalError)
	})
}

func (s *HttpServer) parseUrlPath(
	r *http.Request,
	preSelectedProjectId,
	preSelectedArchitecture,
	preSelectedChainId string,
) (
	projectId, architecture, chainId string,
	isAdmin bool,
	isHealthCheck bool,
	err error,
) {
	ps := path.Clean(r.URL.Path)
	segments := strings.Split(ps, "/")

	// Remove empty first segment from leading slash
	segments = segments[1:]

	isPost := r.Method == http.MethodPost
	isOptions := r.Method == http.MethodOptions

	// Initialize with preselected values
	projectId = preSelectedProjectId
	architecture = preSelectedArchitecture
	chainId = preSelectedChainId

	// Special case for admin endpoint
	if len(segments) == 1 && segments[0] == "admin" && (isPost || isOptions) {
		return "", "", "", true, false, nil
	}

	// Handle healthcheck variations
	if len(segments) > 0 && segments[len(segments)-1] == "healthcheck" {
		isHealthCheck = true
		segments = segments[:len(segments)-1] // Remove healthcheck segment
	} else if len(segments) == 0 || (len(segments) == 1 && segments[0] == "") {
		if !(isPost || isOptions) {
			isHealthCheck = true
			segments = nil
		}
	}

	// Parse remaining segments based on what's preselected
	switch {
	case projectId == "" && architecture == "" && chainId == "":
		// Case: Nothing preselected
		switch len(segments) {
		case 1:
			projectId = segments[0]
		case 2:
			projectId = segments[0]
			// Check if second segment is a network alias
			if project, err := s.erpc.GetProject(projectId); err == nil {
				architecture, chainId = project.networksRegistry.ResolveAlias(segments[1])
			}
			if architecture == "" {
				architecture = segments[1]
			}
		case 3:
			projectId = segments[0]
			architecture = segments[1]
			chainId = segments[2]
		case 0:
			if !isHealthCheck {
				return "", "", "", false, false, common.NewErrInvalidUrlPath("must provide /<project>/<architecture>/<chainId>", ps)
			}
		default:
			return "", "", "", false, false, common.NewErrInvalidUrlPath("must only provide /<project>/<architecture>/<chainId>", ps)
		}

	case projectId != "" && architecture == "" && chainId == "":
		// Case: Only projectId preselected
		switch len(segments) {
		case 1:
			// Check if segment is a network alias
			if project, err := s.erpc.GetProject(projectId); err == nil {
				architecture, chainId = project.networksRegistry.ResolveAlias(segments[0])
			}
			if architecture == "" {
				architecture = segments[0]
			}
		case 2:
			architecture = segments[0]
			chainId = segments[1]
		case 3:
			// Still allow explicitly specifying projectId
			projectId = segments[0]
			architecture = segments[1]
			chainId = segments[2]
		case 0:
			if !isHealthCheck {
				return "", "", "", false, false, common.NewErrInvalidUrlPath("for project-only alias must provide /<architecture>/<chainId>", ps)
			}
		default:
			return "", "", "", false, false, common.NewErrInvalidUrlPath("for project-only alias must only provide /<architecture>/<chainId>", ps)
		}

	case projectId != "" && architecture != "" && chainId == "":
		// Case: ProjectId and architecture preselected
		switch len(segments) {
		case 1:
			// Check if segment is a network alias
			if project, err := s.erpc.GetProject(projectId); err == nil {
				ar, ch := project.networksRegistry.ResolveAlias(segments[0])
				if ar != "" && ch != "" {
					architecture = ar
					chainId = ch
				}
			}
			if chainId == "" {
				chainId = segments[0]
			}
			if chainId == "" && !isHealthCheck {
				return "", "", "", false, false, common.NewErrInvalidUrlPath("for project-and-architecture alias must provide /<chainId>", ps)
			}
		case 3:
			// Still allow explicitly specifying projectId+architecture
			projectId = segments[0]
			architecture = segments[1]
			chainId = segments[2]
		case 0:
			if !isHealthCheck {
				return "", "", "", false, false, common.NewErrInvalidUrlPath("for project-and-architecture alias must provide /<chainId>", ps)
			}
		default:
			return "", "", "", false, false, common.NewErrInvalidUrlPath("for project-and-architecture alias must only provide /<chainId>", ps)
		}

	case projectId != "" && architecture != "" && chainId != "":
		// Case: All values preselected
		if len(segments) > 1 && !isHealthCheck {
			return "", "", "", false, false, common.NewErrInvalidUrlPath("must not provide anything on the path and only use /", ps)
		}

	case projectId == "" && architecture != "" && chainId != "":
		switch len(segments) {
		case 1:
			projectId = segments[0]
			if projectId == "" && !isHealthCheck {
				return "", "", "", false, false, common.NewErrInvalidUrlPath("for architecture-and-chain alias must provide /<project>", ps)
			}
		case 0:
			if !isHealthCheck {
				return "", "", "", false, false, common.NewErrInvalidUrlPath("for architecture-and-chain alias must provide /<project>", ps)
			}
		case 3:
			// Still allow explicitly specifying project+architecture+chain
			projectId = segments[0]
			architecture = segments[1]
			chainId = segments[2]
		default:
			return "", "", "", false, false, common.NewErrInvalidUrlPath("for architecture-and-chain alias must only provide /<project>", ps)
		}

	case projectId == "" && architecture != "" && chainId == "":
		switch len(segments) {
		case 1:
			projectId = segments[0]
		case 2:
			projectId = segments[0]
			chainId = segments[1]
		case 3:
			// Still allow explicitly specifying project+architecture+chain
			projectId = segments[0]
			architecture = segments[1]
			chainId = segments[2]
		default:
			return "", "", "", false, false, common.NewErrInvalidUrlPath("for architecture-only alias must only provide /<project>", ps)
		}

	default:
		if projectId != "" && architecture == "" && chainId != "" {
			return "", "", "", false, false, common.NewErrInvalidUrlPath("it is not possible to alias for project and chain WITHOUT architecture", ps)
		}
		// Invalid combination of preselected values
		return "", "", "", false, false, common.NewErrInvalidUrlPath("invalid combination of path elements and/or aliasing rules", ps)
	}

	if projectId == "" && !isHealthCheck {
		return "", "", "", false, false, common.NewErrInvalidUrlPath("project is required either in path or via domain aliasing", ps)
	}

	if (chainId != "" || architecture != "") && !common.IsValidArchitecture(architecture) {
		return "", "", "", false, false, common.NewErrInvalidUrlPath("architecture is not valid (must be 'evm')", ps)
	}

	if !isPost && !isOptions {
		isHealthCheck = true
	}

	return projectId, architecture, chainId, isAdmin, isHealthCheck, nil
}

func (s *HttpServer) handleCORS(httpCtx context.Context, w http.ResponseWriter, r *http.Request, corsConfig *common.CORSConfig) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		// When no Origin is provided, we allow the request as there's no point in enforcing CORS.
		// For example if client is a custom code (not mainstream browser) there's no point in enforcing CORS.
		// Besides, eRPC is not relying on cookies so CORS is not a big concern (i.e. session hijacking is irrelevant).
		// Bad actors can just build a custom proxy and spoof headers to bypass it.
		// Therefore in the context of eRPC, even using "*" (allowed origins) is not a big concern, in this context
		// CORS is just useful to prevent normies from putting your eRPC URL in their frontend code for example.
		return true
	}

	telemetry.MetricCORSRequestsTotal.WithLabelValues(r.URL.Path, origin).Inc()

	allowed := false
	for _, allowedOrigin := range corsConfig.AllowedOrigins {
		match, err := common.WildcardMatch(allowedOrigin, origin)
		if err != nil {
			s.logger.Error().Err(err).Msgf("failed to match CORS origin")
			continue
		}
		if match {
			allowed = true
			break
		}
	}

	// If disallowed origin, we can continue without CORS headers
	if !allowed {
		s.logger.Debug().Str("origin", origin).Msg("CORS request from disallowed origin, continuing without CORS headers")
		telemetry.MetricCORSDisallowedOriginTotal.WithLabelValues(r.URL.Path, origin).Inc()

		// If it's a preflight OPTIONS request, we can send a basic 204 with no CORS.
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			common.EnrichHTTPServerSpan(httpCtx, http.StatusNoContent, nil)
			return false
		}

		// Otherwise, continue the request without any Access-Control-Allow-* headers.
		// The browser will block it if it *requires* CORS, but non-browser clients or
		// extensions can still proceed.
		return true
	}

	// We get here if the origin is allowed, so we can set CORS headers
	w.Header().Set("Access-Control-Allow-Origin", origin)
	w.Header().Set("Access-Control-Allow-Methods", strings.Join(corsConfig.AllowedMethods, ", "))
	w.Header().Set("Access-Control-Allow-Headers", strings.Join(corsConfig.AllowedHeaders, ", "))
	w.Header().Set("Access-Control-Expose-Headers", strings.Join(corsConfig.ExposedHeaders, ", "))

	if corsConfig.AllowCredentials != nil && *corsConfig.AllowCredentials {
		w.Header().Set("Access-Control-Allow-Credentials", "true")
	}

	if corsConfig.MaxAge > 0 {
		w.Header().Set("Access-Control-Max-Age", fmt.Sprintf("%d", corsConfig.MaxAge))
	}

	// If it's a preflight request, return a 204 and don't continue to main handler.
	if r.Method == http.MethodOptions {
		telemetry.MetricCORSPreflightRequestsTotal.WithLabelValues(r.URL.Path, origin).Inc()
		w.WriteHeader(http.StatusNoContent)
		common.EnrichHTTPServerSpan(httpCtx, http.StatusNoContent, nil)
		return false
	}

	// Otherwise, allow the request to proceed
	return true
}

func setResponseHeaders(ctx context.Context, res interface{}, w http.ResponseWriter) {
	var rm common.ResponseMetadata
	var ok bool
	rm, ok = res.(common.ResponseMetadata)
	if !ok {
		if jrsp, ok := res.(map[string]interface{}); ok {
			if err, ok := jrsp["cause"]; ok {
				if ser, ok := err.(error); ok {
					rm = common.LookupResponseMetadata(ser)
				}
			}
		} else if hjrsp, ok := res.(*HttpJsonRpcErrorResponse); ok {
			rm = common.LookupResponseMetadata(hjrsp.Cause)
		}
	}
	if rm != nil && !rm.IsObjectNull(ctx) {
		if rm.FromCache() {
			w.Header().Set("X-ERPC-Cache", "HIT")
		} else {
			w.Header().Set("X-ERPC-Cache", "MISS")
		}
		if ups := rm.UpstreamId(); ups != "" {
			w.Header().Set("X-ERPC-Upstream", ups)
		}
		w.Header().Set("X-ERPC-Attempts", fmt.Sprintf("%d", rm.Attempts()))
		w.Header().Set("X-ERPC-Retries", fmt.Sprintf("%d", rm.Retries()))
		w.Header().Set("X-ERPC-Hedges", fmt.Sprintf("%d", rm.Hedges()))
	}
}

func determineResponseStatusCode(respOrErr interface{}) int {
	statusCode := http.StatusOK
	if err, ok := respOrErr.(error); ok {
		statusCode = decideErrorStatusCode(err)
	} else if resp, ok := respOrErr.(map[string]interface{}); ok {
		if cause, ok := resp["cause"].(error); ok {
			statusCode = decideErrorStatusCode(cause)
		}
	} else if hjrsp, ok := respOrErr.(*HttpJsonRpcErrorResponse); ok {
		statusCode = decideErrorStatusCode(hjrsp.Cause)
	} else if resp, ok := respOrErr.(*common.NormalizedResponse); ok && resp.IsObjectNull() {
		statusCode = http.StatusInternalServerError
	} else if respOrErr == nil {
		statusCode = http.StatusInternalServerError
	}
	return statusCode
}

type HttpJsonRpcErrorResponse struct {
	Jsonrpc string      `json:"jsonrpc"`
	Id      interface{} `json:"id"`
	Error   interface{} `json:"error"`
	Cause   error       `json:"-"`
}

func processErrorBody(logger *zerolog.Logger, startedAt *time.Time, nq *common.NormalizedRequest, origErr error, includeErrorDetails *bool) interface{} {
	err := origErr
	if !common.IsNull(err) {
		if nq != nil {
			nq.RLock()
		}
		if common.HasErrorCode(
			err,
			common.ErrCodeEndpointExecutionException,
			common.ErrCodeEndpointClientSideException,
			common.ErrCodeInvalidUrlPath,
			common.ErrCodeInvalidRequest,
			common.ErrCodeAuthUnauthorized,
			common.ErrCodeJsonRpcRequestUnmarshal,
			common.ErrCodeProjectNotFound,
		) {
			logger.Debug().Err(err).Object("request", nq).Msgf("forward request errored with client-side exception")
		} else if errors.Is(err, context.Canceled) {
			logger.Debug().Err(err).Object("request", nq).Msgf("forward request errored with context cancellation")
		} else if errors.Is(err, context.DeadlineExceeded) {
			logger.Debug().Err(err).Object("request", nq).Msgf("forward request errored with deadline exceeded")
		} else {
			if e, ok := err.(common.StandardError); ok {
				logger.Warn().Err(err).Object("request", nq).Dur("durationMs", time.Since(*startedAt)).Msgf("failed to forward request: %s", e.DeepestMessage())
			} else {
				logger.Warn().Err(err).Object("request", nq).Dur("durationMs", time.Since(*startedAt)).Msgf("failed to forward request: %s", err.Error())
			}
		}
		if nq != nil {
			nq.RUnlock()
		}
	}

	// This is a special attempt to extract execution errors first (e.g. execution reverted):
	exe := &common.ErrEndpointExecutionException{}
	if errors.As(err, &exe) {
		err = exe
	}

	// To simplify client's life if there's only one upstream error, we can use that as the error
	// instead of Exhausted error which obfuscates underlying errors.
	if ex, ok := err.(*common.ErrUpstreamsExhausted); ok {
		if len(ex.Errors()) == 1 {
			err = ex.Errors()[0]
		}
	}

	err = common.TranslateToJsonRpcException(err)
	var jsonrpcVersion string = "2.0"
	var reqId interface{} = nil
	var method string = ""
	if nq != nil {
		jrr, _ := nq.JsonRpcRequest()
		if jrr != nil {
			jsonrpcVersion = jrr.JSONRPC
			reqId = jrr.ID
			method = jrr.Method
		}
	}
	jre := &common.ErrJsonRpcExceptionInternal{}
	if errors.As(err, &jre) {
		message := jre.Message
		errObj := map[string]interface{}{
			"code":    jre.NormalizedCode(),
			"message": message,
		}
		// Append "data" field, ref: https://www.jsonrpc.org/specification#:~:text=A%20Primitive%20or%20Structured%20value%20that%20contains%20additional%20information%20about%20the%20error.
		if jre.Details["data"] != nil {
			errObj["data"] = jre.Details["data"]
		} else if includeErrorDetails != nil && *includeErrorDetails {
			if method != "eth_call" {
				// For eth_calls clients expect "data" to be string for revert reason.
				// TODO Move this logic to "evm" package.
				errObj["data"] = origErr
			}
		}

		return &HttpJsonRpcErrorResponse{
			Jsonrpc: jsonrpcVersion,
			Id:      reqId,
			Error:   errObj,
			Cause:   err,
		}
	}

	if _, ok := err.(*common.BaseError); ok {
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

// statusCodeOrderPreference is a list of status code ranges that are preferred in the order of preference.
// This is used when multiple upstreams return different status codes for various reasons.
// We try to pick the "most likely relevant" one based on the status code ranges.
var statusCodeOrderPreference = []struct {
	min int
	max int
}{
	{200, 299}, // Successful response from at least one upstream.
	{429, 429}, // Too Many Requests (rate limiting).
	{500, 599}, // Upstream/server errors.
	{405, 428}, // Method/headers/pre-condition related client errors.
	{430, 499}, // Remaining 4xx client errors.
	{400, 400}, // Bad Request – generic validation failure.
	{401, 404}, // Unauthorized / Payment Required / Not Found.
	{300, 399}, // Redirection responses (should rarely occur).
}

// preferredStatusCode returns the code that should win between current and cand according to
// statusCodeOrderPreference.  Lower preference index wins; if both codes fall in the same
// preference bucket, the numerically smaller code wins (e.g. 200 beats 204, 400 beats 422, etc.).
// The comparison is allocation-free and intended for hot-path usage.
func preferredStatusCode(current, cand int) int {
	if current == cand {
		return current
	}

	prefIdx := func(code int) int {
		for idx, rng := range statusCodeOrderPreference {
			if code >= rng.min && code <= rng.max {
				return idx
			}
		}
		// If somehow outside all ranges, treat as lowest priority (after redirects).
		return len(statusCodeOrderPreference)
	}

	idxCur := prefIdx(current)
	idxNew := prefIdx(cand)

	if idxNew < idxCur {
		return cand
	}
	if idxNew > idxCur {
		return current
	}
	// Same bucket – choose smaller numerical code.
	if cand < current {
		return cand
	}
	return current
}

func decideErrorStatusCode(err interface{}) int {
	if se, ok := err.(common.StandardError); ok {
		return se.ErrorStatusCode()
	}

	// TODO refactor the logic so we can eliminate this code path.
	// this is needed because in some scenarios one or more UpstreamsExhausted errors are wrapped
	// in another UpstreamsExhausted error (e.g. getLogs splits where 1 or more sub-requests fail).
	// In such case "err" will be an UpstreamsExhausted which carries multiple status codes.
	// Probably best place to resolve this is in TranslateToJsonRpcException so that
	// nested UpstreamsExhausted errors are resolved to 1 "most significant" error.
	if ue, ok := err.(interface{ Unwrap() []error }); ok {
		bestCode := http.StatusServiceUnavailable // sensible default / fallback

		for _, innerErr := range ue.Unwrap() {
			se, ok := innerErr.(common.StandardError)
			if !ok {
				continue
			}
			bestCode = preferredStatusCode(bestCode, se.ErrorStatusCode())
			// Early exit: cannot get better than 2xx in first bucket.
			if bestCode >= 200 && bestCode <= 299 {
				return bestCode
			}
		}

		return bestCode
	}

	return http.StatusInternalServerError
}

func handleErrorResponse(
	httpCtx context.Context,
	logger *zerolog.Logger,
	startedAt *time.Time,
	nq *common.NormalizedRequest,
	err error,
	w http.ResponseWriter,
	encoder sonic.Encoder,
	writeFatalError func(ctx context.Context, statusCode int, body error),
	includeErrorDetails *bool,
) {
	resp := processErrorBody(logger, startedAt, nq, err, includeErrorDetails)
	statusCode := determineResponseStatusCode(err)
	w.WriteHeader(statusCode)
	span := trace.SpanFromContext(httpCtx)
	span.AddEvent("http.response_write_start")
	err = encoder.Encode(resp)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			logger.Debug().Err(err).Interface("response", resp).Msgf("skipping error response encoding due to context cancellation")
		} else {
			logger.Error().Err(err).Interface("response", resp).Msgf("failed to encode error response")
		}
		writeFatalError(httpCtx, http.StatusInternalServerError, err)
	} else {
		common.EnrichHTTPServerSpan(httpCtx, statusCode, nil)
	}
	span.AddEvent("http.response_write_end",
		trace.WithAttributes(
			attribute.Int("response_status", statusCode),
		),
	)
}

func (s *HttpServer) Start(logger *zerolog.Logger) error {
	var err error
	var ln net.Listener
	var ln4 net.Listener
	var ln6 net.Listener

	if s.serverCfg.HttpPort == nil {
		return fmt.Errorf("server.httpPort is not configured")
	}

	if s.serverCfg.HttpHostV4 != nil && s.serverCfg.ListenV4 != nil && *s.serverCfg.ListenV4 {
		addrV4 := fmt.Sprintf("%s:%d", *s.serverCfg.HttpHostV4, *s.serverCfg.HttpPort)
		logger.Info().Msgf("starting http server on port: %d IPv4: %s", *s.serverCfg.HttpPort, addrV4)
		ln4, err = net.Listen("tcp4", addrV4)
		if err != nil {
			return fmt.Errorf("error listening on IPv4: %w", err)
		}
	}
	if s.serverCfg.HttpHostV6 != nil && s.serverCfg.ListenV6 != nil && *s.serverCfg.ListenV6 {
		addrV6 := fmt.Sprintf("%s:%d", *s.serverCfg.HttpHostV6, *s.serverCfg.HttpPort)
		logger.Info().Msgf("starting http server on port: %d IPv6: %s", s.serverCfg.HttpPort, addrV6)
		ln6, err = net.Listen("tcp6", addrV6)
		if err != nil {
			if ln4 != nil {
				err := ln4.Close()
				if err != nil {
					logger.Error().Err(err).Msgf("failed to close IPv4 listener")
				}
			}
			return fmt.Errorf("error listening on IPv6: %w", err)
		}
	}

	if ln4 != nil && ln6 != nil {
		ln = &dualStackListener{ln4, ln6}
	} else if ln4 != nil {
		ln = ln4
	} else if ln6 != nil {
		ln = ln6
	}

	if ln == nil {
		return fmt.Errorf("you must configure at least one of server.httpHostV4 or server.httpHostV6")
	}

	// Handle TLS configuration if enabled
	if s.serverCfg.TLS != nil && s.serverCfg.TLS.Enabled {
		tlsConfig := &tls.Config{
			MinVersion: tls.VersionTLS12,
		}

		// Load certificate and key
		cert, err := tls.LoadX509KeyPair(s.serverCfg.TLS.CertFile, s.serverCfg.TLS.KeyFile)
		if err != nil {
			return fmt.Errorf("failed to load TLS certificate and key: %w", err)
		}
		tlsConfig.Certificates = []tls.Certificate{cert}

		// Load CA if specified
		if s.serverCfg.TLS.CAFile != "" {
			caCert, err := os.ReadFile(s.serverCfg.TLS.CAFile)
			if err != nil {
				return fmt.Errorf("failed to read CA file: %w", err)
			}
			caCertPool := x509.NewCertPool()
			if !caCertPool.AppendCertsFromPEM(caCert) {
				return fmt.Errorf("failed to parse CA certificate")
			}
			tlsConfig.ClientCAs = caCertPool
			tlsConfig.ClientAuth = tls.RequireAndVerifyClientCert
		}

		tlsConfig.InsecureSkipVerify = s.serverCfg.TLS.InsecureSkipVerify

		// Wrap the listener with TLS
		ln = tls.NewListener(ln, tlsConfig)
		logger.Info().Msg("TLS enabled for HTTP server")
	}

	return s.server.Serve(ln)
}

func (s *HttpServer) Shutdown(logger *zerolog.Logger) error {
	logger.Info().Msg("stopping http server...")
	return s.server.Shutdown(context.Background())
}

type gzipResponseWriter struct {
	http.ResponseWriter
	gzipWriter *gzip.Writer
}

func (w *gzipResponseWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		err := w.gzipWriter.Flush()
		if err != nil {
			log.Error().Err(err).Msg("failed to flush gzip writer")
		}
		flusher.Flush()
	}
}

func (w *gzipResponseWriter) Write(b []byte) (int, error) {
	return w.gzipWriter.Write(b)
}

func gzipHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Check if client accepts gzip encoding
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			next.ServeHTTP(w, r)
			return
		}

		// Initialize gzip writer
		gz := gzip.NewWriter(w)
		defer gz.Close()

		// Create gzip response writer
		gzw := &gzipResponseWriter{
			ResponseWriter: w,
			gzipWriter:     gz,
		}

		// Remove Content-Length header as it will no longer be valid
		w.Header().Del("Content-Length")

		// Set required headers
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Vary", "Accept-Encoding")

		// Call the next handler with our gzip response writer
		next.ServeHTTP(gzw, r)
	})
}
