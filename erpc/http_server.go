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

// Only compress responses larger than 1KB to save CPU on small responses
const compressionThreshold = 1024

type HttpServer struct {
	appCtx                  context.Context
	serverCfg               *common.ServerConfig
	healthCheckCfg          *common.HealthCheckConfig
	adminCfg                *common.AdminConfig
	serverV4                *http.Server
	serverV6                *http.Server
	erpc                    *ERPC
	logger                  *zerolog.Logger
	healthCheckAuthRegistry *auth.AuthRegistry
	draining                *atomic.Bool
	gzipPool                *util.GzipReaderPool
	trustedForwarderNets    []net.IPNet
	trustedForwarderIPs     map[string]struct{}
	trustedIPHeaders        []string
	resolvedResponseHeaders map[string]string
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

	gzipPool := util.NewGzipReaderPool()

	srv := &HttpServer{
		logger:         logger,
		appCtx:         ctx,
		serverCfg:      cfg,
		healthCheckCfg: healthCheckCfg,
		adminCfg:       adminCfg,
		erpc:           erpc,
		draining:       &draining,
		gzipPool:       gzipPool,
	}

	if cfg != nil {
		srv.trustedForwarderIPs = make(map[string]struct{}, len(cfg.TrustedIPForwarders))
		var forwarders []string
		forwarders = append(forwarders, cfg.TrustedIPForwarders...)
		for _, entry := range forwarders {
			val := strings.TrimSpace(entry)
			if val == "" {
				continue
			}
			if strings.Contains(val, "/") {
				if _, ipnet, err := net.ParseCIDR(val); err == nil && ipnet != nil {
					srv.trustedForwarderNets = append(srv.trustedForwarderNets, *ipnet)
				} else {
					logger.Warn().Str("trustedForwarder", val).Msg("invalid CIDR in trusted forwarders; ignoring")
				}
			} else {
				if ip := net.ParseIP(val); ip != nil {
					srv.trustedForwarderIPs[ip.String()] = struct{}{}
				} else {
					logger.Warn().Str("trustedForwarder", val).Msg("invalid IP in trusted forwarders; ignoring")
				}
			}
		}
	}

	if cfg != nil {
		var hdrs []string
		hdrs = append(hdrs, cfg.TrustedIPHeaders...)
		for _, h := range hdrs {
			h = strings.TrimSpace(h)
			if h == "" {
				continue
			}
			srv.trustedIPHeaders = append(srv.trustedIPHeaders, h)
		}
	}

	// Resolve custom response headers (expand env vars at startup)
	if cfg != nil && len(cfg.ResponseHeaders) > 0 {
		srv.resolvedResponseHeaders = make(map[string]string, len(cfg.ResponseHeaders))
		for key, value := range cfg.ResponseHeaders {
			// Expand env vars (handles both ${VAR} and $VAR syntax)
			resolved := os.ExpandEnv(value)
			if resolved != "" {
				srv.resolvedResponseHeaders[key] = resolved
				logger.Info().Str("header", key).Str("value", resolved).Msg("custom response header configured")
			} else {
				logger.Debug().Str("header", key).Msg("custom response header skipped (empty value after env expansion)")
			}
		}
	}

	h := srv.createRequestHandler()
	if cfg.EnableGzip != nil && *cfg.EnableGzip {
		h = gzipHandler(h)
	}

	// Create handler with timeout
	handlerWithTimeout := TimeoutHandler(h, reqMaxTimeout)

	// Create IPv4 server if configured
	if cfg.ListenV4 != nil && *cfg.ListenV4 {
		srv.serverV4 = &http.Server{
			Handler:        handlerWithTimeout,
			ReadTimeout:    readTimeout,
			WriteTimeout:   writeTimeout,
			IdleTimeout:    300 * time.Second,
			MaxHeaderBytes: 1 << 20, // 1MB
		}
	}

	// Create IPv6 server if configured
	if cfg.ListenV6 != nil && *cfg.ListenV6 {
		srv.serverV6 = &http.Server{
			Handler:      handlerWithTimeout,
			ReadTimeout:  readTimeout,
			WriteTimeout: writeTimeout,
		}
	}

	if healthCheckCfg != nil && healthCheckCfg.Auth != nil {
		var err error
		srv.healthCheckAuthRegistry, err = auth.NewAuthRegistry(ctx, logger, "healthcheck", healthCheckCfg.Auth, nil)
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

		// Add custom response headers (resolved at startup with env var expansion)
		for key, value := range s.resolvedResponseHeaders {
			w.Header().Set(key, value)
		}

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

		// Set network in context for force-trace matching in child spans (uses existing parsed values)
		if !isAdmin && !isHealthCheck && architecture != "" && chainId != "" {
			httpCtx = common.SetForceTraceNetwork(httpCtx, architecture+":"+chainId)
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
			gzReader, err := s.gzipPool.GetReset(r.Body)
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
			defer s.gzipPool.Put(gzReader)
			bodyReader = gzReader
		}

		// Replace the existing body read with our potentially decompressed reader
		_, readBodySpan := common.StartDetailSpan(httpCtx, "Http.ReadBody")
		body, cleanup, err := util.ReadAll(bodyReader, 2048)
		// Clean up buffer after parsing the request
		if cleanup != nil {
			defer cleanup()
		}
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
		if len(body) > 0 {
			lg.Info().RawJSON("body", body).Msgf("received http request")
		} else {
			lg.Info().Msgf("received http request with empty body")
		}

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

		// We no longer need the top-level body; drop reference early to free its backing array
		body = nil

		batchHandled := false
		if isBatch && !isAdmin && !isHealthCheck {
			batchInfo, detectErr := detectEthCallBatchInfo(requests, architecture, chainId)
			if detectErr != nil {
				lg.Info().Err(detectErr).
					Int("requestCount", len(requests)).
					Msg("eth_call batch detection failed, processing individually")
			}
			if batchInfo != nil && isMulticall3AggregationEnabled(project, batchInfo.networkId) {
				batchHandled = s.handleEthCallBatchAggregation(
					httpCtx,
					&startedAt,
					r,
					project,
					lg,
					batchInfo,
					requests,
					headers,
					queryArgs,
					responses,
				)
			}
		}

		if !batchHandled {
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
					// Help GC: drop reference to the rawReq slice copy in the parent slice as soon as possible
					rawReq = nil
					requestCtx := common.StartRequestSpan(httpCtx, nq)

					// Resolve and set real client IP before any rate limiting/auth checks
					clientIP := s.resolveRealClientIP(r)
					nq.SetClientIP(clientIP)

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
						_, err := s.erpc.AdminAuthenticate(requestCtx, nq, method, ap)
						if err != nil {
							responses[index] = processErrorBody(&rlg, &startedAt, nq, err, &common.TRUE)
							common.EndRequestSpan(requestCtx, nil, err)
							return
						}
					} else {
						user, err := project.AuthenticateConsumer(requestCtx, nq, method, ap)
						if err != nil {
							responses[index] = processErrorBody(&rlg, &startedAt, nq, err, s.serverCfg.IncludeErrorDetails)
							common.EndRequestSpan(requestCtx, nil, err)
							return
						}
						if user != nil {
							rlg = rlg.With().Str("userId", user.Id).Logger()
						}
						nq.SetUser(user)
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

					nw, err := project.GetNetwork(httpCtx, networkId)
					if err != nil {
						responses[index] = processErrorBody(&rlg, &startedAt, nq, err, s.serverCfg.IncludeErrorDetails)
						common.EndRequestSpan(requestCtx, nil, err)
						return
					}
					nq.SetNetwork(nw)

					nq.ApplyDirectiveDefaults(nw.Config().DirectiveDefaults)
					// Configure how to store User-Agent (raw vs simplified) based on project config
					uaMode := common.UserAgentTrackingModeSimplified
					if project != nil && project.Config.UserAgentMode != "" {
						uaMode = project.Config.UserAgentMode
					}
					nq.EnrichFromHttp(headers, queryArgs, uaMode)
					rlg.Trace().Interface("directives", nq.Directives()).Msgf("applied request directives")

					resp, err := project.Forward(requestCtx, networkId, nq)
					if err != nil {
						// If an error occurred but a response was produced (e.g., lastValidResponse),
						// release it now since we are not going to write it.
						if resp != nil {
							go resp.Release()
						}
						responses[index] = processErrorBody(&rlg, &startedAt, nq, err, s.serverCfg.IncludeErrorDetails)
						common.EndRequestSpan(requestCtx, nil, err)
						return
					}

					responses[index] = resp
					common.EndRequestSpan(requestCtx, resp, nil)
				}(i, reqBody, headers, queryArgs)
			}

			wg.Wait()
		}

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
			// Ensure we do not retain responses when the request context is done
			for _, resp := range responses {
				if v, ok := resp.(*common.NormalizedResponse); ok && v != nil {
					go v.Release()
				}
			}
			return
		}

		common.InjectHTTPResponseTraceContext(httpCtx, w)

		if isBatch {
			// JSON-RPC 2.0 over HTTP should always return 200 OK at transport level
			w.WriteHeader(http.StatusOK)

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
			// Determine HTTP status code - defaults to 200 for JSON-RPC responses,
			// but transport-level errors (auth, rate limit, etc.) get appropriate status codes
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
				// Keep transport 200 for JSON-RPC POST requests per spec
				if r.Method == http.MethodPost {
					statusCode = http.StatusOK
				}
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
		case 0:
			// Allow global healthcheck when only architecture is aliased
			// (e.g., host preselects architecture like "evm" and path is just /healthcheck or /)
			if !isHealthCheck {
				return "", "", "", false, false, common.NewErrInvalidUrlPath("for architecture-only alias must provide /<project>", ps)
			}
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
	if resp, ok := res.(*common.NormalizedResponse); ok {
		w.Header().Set("X-ERPC-Duration", fmt.Sprintf("%d", resp.Duration().Milliseconds()))
	}
}

// determineResponseStatusCode extracts any error from a response and determines
// the appropriate HTTP status code. Defaults to 200 for JSON-RPC responses,
// but transport-level errors (auth, rate limit, not found) get appropriate status codes.
func determineResponseStatusCode(res interface{}) int {
	var err error
	switch v := res.(type) {
	case *HttpJsonRpcErrorResponse:
		err = v.Cause
	case error:
		err = v
	default:
		return http.StatusOK
	}

	if err == nil {
		return http.StatusOK
	}

	// Transport-level errors get appropriate HTTP status codes
	switch {
	// 400 Bad Request - malformed requests
	case common.HasErrorCode(err, common.ErrCodeInvalidUrlPath, common.ErrCodeJsonRpcRequestUnmarshal, common.ErrCodeInvalidRequest):
		return http.StatusBadRequest
	// 401 Unauthorized - authentication failures
	case common.HasErrorCode(err, common.ErrCodeAuthUnauthorized, common.ErrCodeEndpointUnauthorized):
		return http.StatusUnauthorized
	// 404 Not Found - resource not found
	case common.HasErrorCode(err, common.ErrCodeProjectNotFound, common.ErrCodeNetworkNotFound):
		return http.StatusNotFound
	// 413 Request Entity Too Large
	case common.HasErrorCode(err, common.ErrCodeEndpointRequestTooLarge):
		return http.StatusRequestEntityTooLarge
	// 429 Too Many Requests - rate limiting
	case common.HasErrorCode(err,
		common.ErrCodeAuthRateLimitRuleExceeded,
		common.ErrCodeProjectRateLimitRuleExceeded,
		common.ErrCodeNetworkRateLimitRuleExceeded,
		common.ErrCodeEndpointCapacityExceeded):
		return http.StatusTooManyRequests
	}

	// All other errors (JSON-RPC application errors) return 200
	return http.StatusOK
}

type HttpJsonRpcErrorResponse struct {
	Jsonrpc string      `json:"jsonrpc"`
	Id      interface{} `json:"id"`
	Error   interface{} `json:"error"`
	Cause   error       `json:"-"`
}

func (r *HttpJsonRpcErrorResponse) MarshalZerologObject(e *zerolog.Event) {
	if r == nil {
		return
	}
	e.Str("jsonrpc", r.Jsonrpc)
	e.Interface("id", r.Id)
	if r.Error != nil {
		switch v := r.Error.(type) {
		case *common.ErrJsonRpcExceptionExternal:
			e.Object("error", v)
		case map[string]interface{}:
			e.Interface("error", v)
		}
	}
}

func processErrorBody(logger *zerolog.Logger, startedAt *time.Time, nq *common.NormalizedRequest, origErr error, includeErrorDetails *bool) interface{} {
	err := origErr

	// Build the response first, then log with it
	resp := buildErrorResponseBody(nq, err, origErr, includeErrorDetails)

	// Log the error with the response
	if !common.IsNull(err) {
		if nq != nil {
			nq.RLock()
		}
		respMarshaler, _ := resp.(zerolog.LogObjectMarshaler)
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
			logger.Debug().Err(err).Object("request", nq).Object("response", respMarshaler).Msg("forward request errored with client-side exception")
		} else if errors.Is(err, context.Canceled) {
			logger.Debug().Err(err).Object("request", nq).Object("response", respMarshaler).Msg("forward request errored with context cancellation")
		} else if errors.Is(err, context.DeadlineExceeded) {
			logger.Debug().Err(err).Object("request", nq).Object("response", respMarshaler).Msg("forward request errored with deadline exceeded")
		} else {
			if e, ok := err.(common.StandardError); ok {
				logger.Warn().Err(err).Object("request", nq).Object("response", respMarshaler).Dur("durationMs", time.Since(*startedAt)).Msgf("failed to forward request: %s", e.DeepestMessage())
			} else {
				logger.Warn().Err(err).Object("request", nq).Object("response", respMarshaler).Dur("durationMs", time.Since(*startedAt)).Msgf("failed to forward request: %s", err.Error())
			}
		}
		if nq != nil {
			nq.RUnlock()
		}
	}

	return resp
}

// buildErrorResponseBody constructs the error response without logging
func buildErrorResponseBody(nq *common.NormalizedRequest, err, origErr error, includeErrorDetails *bool) interface{} {
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
	// Transport defaults to 200 for JSON-RPC, with limited exceptions.
	// Non-200 codes are reserved for transport/infrastructure level issues,
	// not JSON-RPC application errors (which stay 200 with error in body).
	statusCode := http.StatusOK
	switch {
	// 400 Bad Request - malformed requests (not valid JSON-RPC)
	case common.HasErrorCode(err, common.ErrCodeInvalidUrlPath, common.ErrCodeJsonRpcRequestUnmarshal, common.ErrCodeInvalidRequest):
		statusCode = http.StatusBadRequest
	// 401 Unauthorized - authentication failures
	case common.HasErrorCode(err, common.ErrCodeAuthUnauthorized, common.ErrCodeEndpointUnauthorized):
		statusCode = http.StatusUnauthorized
	// 404 Not Found - resource not found at HTTP level
	case common.HasErrorCode(err, common.ErrCodeProjectNotFound, common.ErrCodeNetworkNotFound):
		statusCode = http.StatusNotFound
	// 413 Request Entity Too Large
	case common.HasErrorCode(err, common.ErrCodeEndpointRequestTooLarge):
		statusCode = http.StatusRequestEntityTooLarge
	// 429 Too Many Requests - rate limiting (critical for client retry logic)
	case common.HasErrorCode(err,
		common.ErrCodeAuthRateLimitRuleExceeded,
		common.ErrCodeProjectRateLimitRuleExceeded,
		common.ErrCodeNetworkRateLimitRuleExceeded,
		common.ErrCodeEndpointCapacityExceeded):
		statusCode = http.StatusTooManyRequests
	}
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
	// Validate that at least one server is configured
	if s.serverV4 == nil && s.serverV6 == nil {
		return fmt.Errorf("you must configure at least one of server.listenV4 or server.listenV6")
	}

	// Channel to collect errors from server goroutines
	errChan := make(chan error, 2)
	serversStarted := 0

	// Start IPv4 server if configured
	if s.serverV4 != nil && s.serverCfg.ListenV4 != nil && *s.serverCfg.ListenV4 {
		if s.serverCfg.HttpPortV4 == nil {
			return fmt.Errorf("server.httpPortV4 is not configured")
		}
		addrV4 := fmt.Sprintf("%s:%d", *s.serverCfg.HttpHostV4, *s.serverCfg.HttpPortV4)
		logger.Info().Msgf("starting IPv4 HTTP server on %s", addrV4)

		// Handle TLS for IPv4 server
		if s.serverCfg.TLS != nil && s.serverCfg.TLS.Enabled {
			tlsConfig, err := s.createTLSConfig()
			if err != nil {
				return fmt.Errorf("failed to create TLS config: %w", err)
			}
			s.serverV4.TLSConfig = tlsConfig
			logger.Info().Msg("TLS enabled for IPv4 HTTP server")
		}

		serversStarted++
		go func() {
			var err error
			s.serverV4.Addr = addrV4
			if s.serverCfg.TLS != nil && s.serverCfg.TLS.Enabled {
				err = s.serverV4.ListenAndServeTLS(s.serverCfg.TLS.CertFile, s.serverCfg.TLS.KeyFile)
			} else {
				err = s.serverV4.ListenAndServe()
			}
			if err != nil && err != http.ErrServerClosed {
				errChan <- fmt.Errorf("IPv4 server error: %w", err)
			} else {
				errChan <- nil
			}
		}()
	}

	// Start IPv6 server if configured
	if s.serverV6 != nil && s.serverCfg.ListenV6 != nil && *s.serverCfg.ListenV6 {
		if s.serverCfg.HttpPortV6 == nil {
			return fmt.Errorf("server.httpPortV6 is not configured")
		}
		addrV6 := fmt.Sprintf("%s:%d", *s.serverCfg.HttpHostV6, *s.serverCfg.HttpPortV6)
		logger.Info().Msgf("starting IPv6 HTTP server on %s", addrV6)

		// Handle TLS for IPv6 server
		if s.serverCfg.TLS != nil && s.serverCfg.TLS.Enabled {
			tlsConfig, err := s.createTLSConfig()
			if err != nil {
				return fmt.Errorf("failed to create TLS config: %w", err)
			}
			s.serverV6.TLSConfig = tlsConfig
			logger.Info().Msg("TLS enabled for IPv6 HTTP server")
		}

		serversStarted++
		go func() {
			var err error
			s.serverV6.Addr = addrV6
			if s.serverCfg.TLS != nil && s.serverCfg.TLS.Enabled {
				err = s.serverV6.ListenAndServeTLS(s.serverCfg.TLS.CertFile, s.serverCfg.TLS.KeyFile)
			} else {
				err = s.serverV6.ListenAndServe()
			}
			if err != nil && err != http.ErrServerClosed {
				errChan <- fmt.Errorf("IPv6 server error: %w", err)
			} else {
				errChan <- nil
			}
		}()
	}

	// Wait for the first error or all servers to finish
	for i := 0; i < serversStarted; i++ {
		if err := <-errChan; err != nil {
			return err
		}
	}

	return nil
}

// createTLSConfig creates a TLS configuration from server config
func (s *HttpServer) createTLSConfig() (*tls.Config, error) {
	tlsConfig := &tls.Config{
		MinVersion: tls.VersionTLS12,
	}

	// Load certificate and key
	cert, err := tls.LoadX509KeyPair(s.serverCfg.TLS.CertFile, s.serverCfg.TLS.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("failed to load TLS certificate and key: %w", err)
	}
	tlsConfig.Certificates = []tls.Certificate{cert}

	// Load CA if specified
	if s.serverCfg.TLS.CAFile != "" {
		caCert, err := os.ReadFile(s.serverCfg.TLS.CAFile)
		if err != nil {
			return nil, fmt.Errorf("failed to read CA file: %w", err)
		}
		caCertPool := x509.NewCertPool()
		if !caCertPool.AppendCertsFromPEM(caCert) {
			return nil, fmt.Errorf("failed to parse CA certificate")
		}
		tlsConfig.ClientCAs = caCertPool
		tlsConfig.ClientAuth = tls.RequireAndVerifyClientCert
	}

	tlsConfig.InsecureSkipVerify = s.serverCfg.TLS.InsecureSkipVerify
	return tlsConfig, nil
}

func (s *HttpServer) Shutdown(logger *zerolog.Logger) error {
	logger.Info().Msg("stopping http servers...")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	errChan := make(chan error, 2)
	serversToShutdown := 0

	// Shutdown IPv4 server if running
	if s.serverV4 != nil {
		serversToShutdown++
		go func() {
			if err := s.serverV4.Shutdown(ctx); err != nil {
				errChan <- fmt.Errorf("IPv4 server shutdown error: %w", err)
			} else {
				logger.Info().Msg("IPv4 HTTP server stopped")
				errChan <- nil
			}
		}()
	}

	// Shutdown IPv6 server if running
	if s.serverV6 != nil {
		serversToShutdown++
		go func() {
			if err := s.serverV6.Shutdown(ctx); err != nil {
				errChan <- fmt.Errorf("IPv6 server shutdown error: %w", err)
			} else {
				logger.Info().Msg("IPv6 HTTP server stopped")
				errChan <- nil
			}
		}()
	}

	// Wait for all servers to shutdown
	var lastErr error
	for i := 0; i < serversToShutdown; i++ {
		if err := <-errChan; err != nil {
			lastErr = err
		}
	}

	return lastErr
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

// conditionalGzipWriter wraps ResponseWriter and decides whether to compress
// based on the first write size. This avoids buffering while still allowing
// us to skip compression for small responses.
type conditionalGzipWriter struct {
	http.ResponseWriter
	gzipWriter  *gzip.Writer
	pool        *util.GzipWriterPool
	decided     bool
	compressing bool
}

// Compile-time check that conditionalGzipWriter implements http.Flusher
var _ http.Flusher = (*conditionalGzipWriter)(nil)

func (w *conditionalGzipWriter) Write(b []byte) (int, error) {
	// If we've already decided, just pass through
	if w.decided {
		if w.compressing {
			return w.gzipWriter.Write(b)
		}
		return w.ResponseWriter.Write(b)
	}

	// First write - decide based on size
	w.decided = true

	// If the first chunk is small, assume the whole response is small
	// This works well for RPC responses which are typically sent in one write
	if len(b) < compressionThreshold {
		// Skip compression for small responses
		w.compressing = false
		return w.ResponseWriter.Write(b)
	}

	// Large response, enable compression
	w.compressing = true

	// Set compression headers
	w.ResponseWriter.Header().Del("Content-Length")
	w.ResponseWriter.Header().Set("Content-Encoding", "gzip")
	w.ResponseWriter.Header().Set("Vary", "Accept-Encoding")
	if ct := w.ResponseWriter.Header().Get("Content-Type"); ct == "" {
		w.ResponseWriter.Header().Set("Content-Type", "application/json")
	}

	// Initialize gzip writer
	w.gzipWriter = w.pool.Get(w.ResponseWriter)

	// Write the data
	return w.gzipWriter.Write(b)
}

// Flush implements http.Flusher interface to support streaming responses
func (w *conditionalGzipWriter) Flush() {
	if w.compressing && w.gzipWriter != nil {
		_ = w.gzipWriter.Flush()
	}
	// Also flush underlying ResponseWriter if it supports it
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *conditionalGzipWriter) Close() error {
	if w.compressing && w.gzipWriter != nil {
		err := w.gzipWriter.Close()
		w.pool.Put(w.gzipWriter)
		return err
	}
	return nil
}

func gzipHandler(next http.Handler) http.Handler {
	// Pool writers across responses for better performance
	var gzPool = util.NewGzipWriterPool()

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Check if client accepts gzip encoding
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			next.ServeHTTP(w, r)
			return
		}

		// Create conditional gzip response writer that decides on first write
		gzw := &conditionalGzipWriter{
			ResponseWriter: w,
			pool:           gzPool,
		}

		// Call the next handler with our conditional gzip response writer
		next.ServeHTTP(gzw, r)

		// Ensure proper cleanup
		_ = gzw.Close()
	})
}

// resolveRealClientIP determines the originating client IP, honoring standard forwarding headers
// only when the immediate peer is a trusted forwarder. Falls back to remote address.
func (s *HttpServer) resolveRealClientIP(r *http.Request) string {
	remoteIP := parseRemoteIP(r.RemoteAddr)
	if remoteIP == nil {
		return "n/a"
	}

	if !s.isTrustedForwarder(remoteIP) {
		return remoteIP.String()
	}

	// Iterate over configured trusted IP headers in order and parse as XFF-like list
	for _, hdr := range s.trustedIPHeaders {
		if hdr == "" {
			continue
		}
		if v := strings.TrimSpace(r.Header.Get(hdr)); v != "" {
			ips := parseXForwardedFor(v)
			if ip := trimRightTrustedAndPick(ips, s.isTrustedForwarder); ip != nil {
				return ip.String()
			}
		}
	}

	return remoteIP.String()
}

func (s *HttpServer) isTrustedForwarder(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if s.trustedForwarderIPs != nil {
		if _, ok := s.trustedForwarderIPs[ip.String()]; ok {
			return true
		}
	}
	for i := range s.trustedForwarderNets {
		if s.trustedForwarderNets[i].Contains(ip) {
			return true
		}
	}
	return false
}

// no header allowlist; headers are explicitly configured in server.trustedIPHeaders

func parseRemoteIP(remoteAddr string) net.IP {
	host := remoteAddr
	if h, _, err := net.SplitHostPort(remoteAddr); err == nil {
		host = h
	}
	// Remove IPv6 brackets if any
	host = stripAddrDecorations(host)
	return net.ParseIP(host)
}

func parseXForwardedFor(xff string) []net.IP {
	parts := strings.Split(xff, ",")
	ips := make([]net.IP, 0, len(parts))
	for _, p := range parts {
		v := strings.TrimSpace(p)
		v = stripAddrDecorations(v)
		// Some implementations might include host:port; strip port if present
		if h, _, err := net.SplitHostPort(v); err == nil {
			v = h
		}
		if ip := net.ParseIP(v); ip != nil {
			ips = append(ips, ip)
		}
	}
	return ips
}

// parseForwardedFor parses RFC 7239 Forwarded header and extracts the sequence of for= IPs
func parseForwardedFor(fwd string) []net.IP {
	// Split elements by comma, then params by ';', pick for= value
	items := strings.Split(fwd, ",")
	ips := make([]net.IP, 0, len(items))
	for _, it := range items {
		params := strings.Split(it, ";")
		for _, p := range params {
			p = strings.TrimSpace(p)
			if len(p) >= 4 && strings.HasPrefix(strings.ToLower(p), "for=") {
				v := strings.TrimSpace(p[4:])
				// Remove optional quotes
				v = strings.Trim(v, "\"")
				v = stripAddrDecorations(v)
				// Strip optional :port if present
				if h, _, err := net.SplitHostPort(v); err == nil {
					v = h
				}
				if ip := net.ParseIP(v); ip != nil {
					ips = append(ips, ip)
				}
			}
		}
	}
	return ips
}

// trimRightTrustedAndPick removes trailing trusted proxy IPs and returns the nearest untrusted IP (client)
func trimRightTrustedAndPick(ips []net.IP, isTrusted func(net.IP) bool) net.IP {
	if len(ips) == 0 {
		return nil
	}
	end := len(ips) - 1
	for end >= 0 && isTrusted(ips[end]) {
		end--
	}
	if end >= 0 {
		return ips[end]
	}
	return nil
}

// stripAddrDecorations removes brackets and spaces commonly seen around IPv6 or quoted values
func stripAddrDecorations(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]") {
		return s[1 : len(s)-1]
	}
	return s
}

// isMulticall3AggregationEnabled checks if multicall3 aggregation is enabled for a given network.
// Returns true (default) if no explicit config is set, or if the config is explicitly set to enabled.
func isMulticall3AggregationEnabled(project *PreparedProject, networkId string) bool {
	if project == nil || project.Config == nil {
		return true // Default to enabled
	}

	project.cfgMu.RLock()
	defer project.cfgMu.RUnlock()

	for _, nwCfg := range project.Config.Networks {
		if nwCfg != nil && nwCfg.NetworkId() == networkId {
			if nwCfg.Evm != nil && nwCfg.Evm.Multicall3Aggregation != nil {
				return nwCfg.Evm.Multicall3Aggregation.Enabled
			}
			break
		}
	}

	return true // Default to enabled
}
