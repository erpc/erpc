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
	"time"

	"github.com/bytedance/sonic"
	"github.com/erpc/erpc/auth"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

type HttpServer struct {
	appCtx context.Context
	config *common.ServerConfig
	admin  *common.AdminConfig
	server *http.Server
	erpc   *ERPC
	logger *zerolog.Logger
}

func NewHttpServer(ctx context.Context, logger *zerolog.Logger, cfg *common.ServerConfig, admin *common.AdminConfig, erpc *ERPC) *HttpServer {
	var err error
	reqMaxTimeout := 150 * time.Second
	if cfg.MaxTimeout != nil {
		reqMaxTimeout, err = time.ParseDuration(*cfg.MaxTimeout)
		if err != nil {
			logger.Error().Err(err).Msgf("failed to parse max timeout duration using 150s default")
		}
	}

	readTimeout := 30 * time.Second
	if cfg.ReadTimeout != nil {
		readTimeout, err = time.ParseDuration(*cfg.ReadTimeout)
		if err != nil {
			logger.Error().Err(err).Msgf("failed to parse read timeout duration using 30s default")
		}
	}

	writeTimeout := 120 * time.Second
	if cfg.WriteTimeout != nil {
		writeTimeout, err = time.ParseDuration(*cfg.WriteTimeout)
		if err != nil {
			logger.Error().Err(err).Msgf("failed to parse write timeout duration using 120s default")
		}
	}

	srv := &HttpServer{
		appCtx: ctx,
		config: cfg,
		admin:  admin,
		erpc:   erpc,
		logger: logger,
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

	go func() {
		<-ctx.Done()
		if err := srv.Shutdown(logger); err != nil {
			logger.Error().Msgf("http server forced to shutdown: %s", err)
		} else {
			logger.Info().Msg("http server stopped")
		}
	}()

	return srv
}

func (s *HttpServer) createRequestHandler() http.Handler {
	handleRequest := func(r *http.Request, w http.ResponseWriter, writeFatalError func(statusCode int, body error)) {
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
		if s.config.Aliasing != nil {
			for _, rule := range s.config.Aliasing.Rules {
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

		projectId, architecture, chainId, isAdmin, isHealthCheck, err = s.parseUrlPath(r, projectId, architecture, chainId)
		if err != nil {
			handleErrorResponse(s.logger, &startedAt, nil, err, w, encoder, writeFatalError)
			return
		}

		if isHealthCheck {
			s.handleHealthCheck(w, &startedAt, projectId, encoder, writeFatalError)
			return
		}

		if isAdmin {
			if s.admin != nil && s.admin.CORS != nil {
				if !s.handleCORS(w, r, s.admin.CORS) || r.Method == http.MethodOptions {
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
			handleErrorResponse(&lg, &startedAt, nil, common.NewErrInvalidRequest(fmt.Errorf("projectId is required in path or must be aliased")), w, encoder, writeFatalError)
			return
		}

		project, err := s.erpc.GetProject(projectId)
		if err != nil {
			handleErrorResponse(&lg, &startedAt, nil, err, w, encoder, writeFatalError)
			return
		}

		if project != nil && project.Config.CORS != nil {
			if !s.handleCORS(w, r, project.Config.CORS) || r.Method == http.MethodOptions {
				return
			}
		}

		// Handle gzipped request bodies
		var bodyReader io.Reader = r.Body
		if r.Header.Get("Content-Encoding") == "gzip" {
			gzReader, err := gzip.NewReader(r.Body)
			if err != nil {
				handleErrorResponse(&lg, &startedAt, nil, common.NewErrInvalidRequest(fmt.Errorf("invalid gzip body: %w", err)), w, encoder, writeFatalError)
				return
			}
			defer gzReader.Close()
			bodyReader = gzReader
		}

		// Replace the existing body read with our potentially decompressed reader
		body, err := util.ReadAll(bodyReader, 1024*1024, 512)
		if err != nil {
			handleErrorResponse(&lg, &startedAt, nil, err, w, encoder, writeFatalError)
			return
		}

		lg.Info().RawJSON("body", body).Msgf("received http request")

		var requests []json.RawMessage
		isBatch := len(body) > 0 && body[0] == '['
		if !isBatch {
			requests = []json.RawMessage{body}
		} else {
			err = common.SonicCfg.Unmarshal(body, &requests)
			if err != nil {
				handleErrorResponse(
					&lg,
					&startedAt,
					nil,
					common.NewErrJsonRpcRequestUnmarshal(err),
					w,
					encoder,
					writeFatalError,
				)
				return
			}
		}

		responses := make([]interface{}, len(requests))
		var wg sync.WaitGroup

		headers := r.Header
		queryArgs := r.URL.Query()

		for i, reqBody := range requests {
			wg.Add(1)
			go func(index int, rawReq json.RawMessage, headers http.Header, queryArgs map[string][]string) {
				defer func() {
					if rec := recover(); rec != nil {
						msg := fmt.Sprintf("unexpected server panic on per-request handler: %v stack: %s", rec, string(debug.Stack()))
						lg.Error().Msgf(msg)
						responses[index] = processErrorBody(&lg, &startedAt, nil, fmt.Errorf(msg))
					}
				}()

				defer wg.Done()

				requestCtx := r.Context()

				nq := common.NewNormalizedRequest(rawReq)
				nq.ApplyDirectivesFromHttp(headers, queryArgs)

				if err := nq.Validate(); err != nil {
					responses[index] = processErrorBody(&lg, &startedAt, nq, err)
					return
				}

				m, _ := nq.Method()
				rlg := lg.With().Str("method", m).Logger()

				rlg.Trace().Interface("directives", nq.Directives()).Msgf("applied request directives")

				var ap *auth.AuthPayload
				var err error

				if project != nil {
					ap, err = auth.NewPayloadFromHttp(project.Config.Id, nq, headers, queryArgs)
				} else if isAdmin {
					ap, err = auth.NewPayloadFromHttp("admin", nq, headers, queryArgs)
				}
				if err != nil {
					responses[index] = processErrorBody(&rlg, &startedAt, nq, err)
					return
				}

				if isAdmin {
					if err := s.erpc.AdminAuthenticate(requestCtx, nq, ap); err != nil {
						responses[index] = processErrorBody(&rlg, &startedAt, nq, err)
						return
					}
				} else {
					if err := project.AuthenticateConsumer(requestCtx, nq, ap); err != nil {
						responses[index] = processErrorBody(&rlg, &startedAt, nq, err)
						return
					}
				}

				if isAdmin {
					if s.admin != nil {
						resp, err := s.erpc.AdminHandleRequest(requestCtx, nq)
						if err != nil {
							responses[index] = processErrorBody(&rlg, &startedAt, nq, err)
							return
						}
						responses[index] = resp
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
						)
						return
					}
				}

				var networkId string

				if architecture == "" || chainId == "" {
					var req map[string]interface{}
					if err := common.SonicCfg.Unmarshal(rawReq, &req); err != nil {
						responses[index] = processErrorBody(&rlg, &startedAt, nq, common.NewErrInvalidRequest(err))
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
					)))
					return
				}

				nw, err := project.GetNetwork(networkId)
				if err != nil {
					responses[index] = processErrorBody(&rlg, &startedAt, nq, err)
					return
				}
				nq.SetNetwork(nw)

				resp, err := project.Forward(requestCtx, networkId, nq)
				if err != nil {
					responses[index] = processErrorBody(&rlg, &startedAt, nq, err)
					return
				}

				responses[index] = resp
			}(i, reqBody, headers, queryArgs)
		}

		wg.Wait()
		requestCtx := r.Context()

		if err := requestCtx.Err(); err != nil {
			if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				cause := context.Cause(requestCtx)
				if cause != nil {
					err = cause
				}
				s.logger.Trace().Err(err).Msg("request premature context error")
				writeFatalError(http.StatusInternalServerError, err)
			}
			return
		}

		if isBatch {
			w.WriteHeader(http.StatusOK)
			bw := NewBatchResponseWriter(responses)
			_, err = bw.WriteTo(w)

			for _, resp := range responses {
				if r, ok := resp.(*common.NormalizedResponse); ok {
					r.Release()
				}
			}

			if err != nil && !errors.Is(err, http.ErrHandlerTimeout) {
				s.logger.Error().Err(err).Msg("failed to write batch response")
				writeFatalError(http.StatusInternalServerError, err)
				return
			}
		} else {
			res := responses[0]
			setResponseHeaders(res, w)
			setResponseStatusCode(res, w)

			switch v := res.(type) {
			case *common.NormalizedResponse:
				_, err = v.WriteTo(w)
				v.Release()
			case *HttpJsonRpcErrorResponse:
				_, err = writeJsonRpcError(w, v)
			default:
				err = common.SonicCfg.NewEncoder(w).Encode(res)
			}

			if err != nil {
				writeFatalError(http.StatusInternalServerError, err)
				return
			}
		}
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		finalErrorOnce := &sync.Once{}
		writeFatalError := func(statusCode int, err error) {
			finalErrorOnce.Do(func() {
				defer func() {
					if rec := recover(); rec != nil {
						s.logger.Error().Msgf("unexpected server panic on final error writer: %v -> %s", rec, debug.Stack())
					}
				}()
				w.WriteHeader(statusCode)

				msg, err := common.SonicCfg.Marshal(err.Error())
				if err != nil {
					msg, _ = common.SonicCfg.Marshal(err.Error())
				}
				body := fmt.Sprintf(`{"jsonrpc":"2.0","error":{"code":-32603,"message":%s}}`, msg)

				fmt.Fprint(w, body)
			})
		}

		defer func() {
			if rec := recover(); rec != nil {
				msg := fmt.Sprintf("unexpected server panic on top-level handler: %v -> %s", rec, debug.Stack())
				s.logger.Error().Msgf(msg)
				writeFatalError(
					http.StatusInternalServerError,
					fmt.Errorf(`unexpected server panic on top-level handler: %s`, rec),
				)
			}
		}()

		handleRequest(r, w, writeFatalError)
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
			architecture = segments[1]
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
			architecture = segments[0]
		case 2:
			architecture = segments[0]
			chainId = segments[1]
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
			chainId = segments[0]
			if chainId == "" && !isHealthCheck {
				return "", "", "", false, false, common.NewErrInvalidUrlPath("for project-and-architecture alias must provide /<chainId>", ps)
			}
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

	return projectId, architecture, chainId, isAdmin, isHealthCheck, nil
}

func (s *HttpServer) handleCORS(w http.ResponseWriter, r *http.Request, corsConfig *common.CORSConfig) bool {
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

	health.MetricCORSRequestsTotal.WithLabelValues(r.URL.Path, origin).Inc()

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

	if !allowed {
		s.logger.Debug().Str("origin", origin).Msg("CORS request from disallowed origin")
		health.MetricCORSDisallowedOriginTotal.WithLabelValues(r.URL.Path, origin).Inc()

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
		} else {
			http.Error(w, "CORS request from disallowed origin", http.StatusForbidden)
		}
		return false
	}

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

	if r.Method == http.MethodOptions {
		health.MetricCORSPreflightRequestsTotal.WithLabelValues(r.URL.Path, origin).Inc()
		w.WriteHeader(http.StatusNoContent)
		return false
	}

	return true
}

func setResponseHeaders(res interface{}, w http.ResponseWriter) {
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
	if rm != nil && !rm.IsObjectNull() {
		if rm.FromCache() {
			w.Header().Set("X-ERPC-Cache", "HIT")
		} else {
			w.Header().Set("X-ERPC-Cache", "MISS")
		}
		if rm.UpstreamId() != "" {
			w.Header().Set("X-ERPC-Upstream", rm.UpstreamId())
		}
		w.Header().Set("X-ERPC-Attempts", fmt.Sprintf("%d", rm.Attempts()))
		w.Header().Set("X-ERPC-Retries", fmt.Sprintf("%d", rm.Retries()))
		w.Header().Set("X-ERPC-Hedges", fmt.Sprintf("%d", rm.Hedges()))
	}
}

func setResponseStatusCode(respOrErr interface{}, w http.ResponseWriter) {
	statusCode := http.StatusOK
	if err, ok := respOrErr.(error); ok {
		statusCode = decideErrorStatusCode(err)
	} else if resp, ok := respOrErr.(map[string]interface{}); ok {
		if cause, ok := resp["cause"].(error); ok {
			statusCode = decideErrorStatusCode(cause)
		}
	} else if hjrsp, ok := respOrErr.(*HttpJsonRpcErrorResponse); ok {
		statusCode = decideErrorStatusCode(hjrsp.Cause)
	}
	w.WriteHeader(statusCode)
}

type HttpJsonRpcErrorResponse struct {
	Jsonrpc string      `json:"jsonrpc"`
	Id      interface{} `json:"id"`
	Error   interface{} `json:"error"`
	Cause   error       `json:"-"`
}

func processErrorBody(logger *zerolog.Logger, startedAt *time.Time, nq *common.NormalizedRequest, err error) interface{} {
	if !common.IsNull(err) {
		if nq != nil {
			nq.RLock()
		}
		if common.HasErrorCode(
			err,
			common.ErrCodeEndpointClientSideException,
			common.ErrCodeInvalidUrlPath,
			common.ErrCodeInvalidRequest,
		) {
			logger.Debug().Err(err).Object("request", nq).Msgf("forward request errored with client-side exception")
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
		message := jre.Message
		deepestMessage := jre.DeepestMessage()
		if message != deepestMessage {
			message = fmt.Sprintf("%s: %s", message, deepestMessage)
		}
		errObj := map[string]interface{}{
			"code":    jre.NormalizedCode(),
			"message": message,
		}
		if jre.Details["data"] != nil {
			errObj["data"] = jre.Details["data"]
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

func decideErrorStatusCode(err error) int {
	if e, ok := err.(common.StandardError); ok {
		return e.ErrorStatusCode()
	}
	return http.StatusInternalServerError
}

func handleErrorResponse(
	logger *zerolog.Logger,
	startedAt *time.Time,
	nq *common.NormalizedRequest,
	err error,
	w http.ResponseWriter,
	encoder sonic.Encoder,
	writeFatalError func(statusCode int, body error),
) {
	resp := processErrorBody(logger, startedAt, nq, err)
	setResponseStatusCode(err, w)
	err = encoder.Encode(resp)
	if err != nil {
		logger.Error().Err(err).Msgf("failed to encode error response")
		writeFatalError(http.StatusInternalServerError, err)
	}
}

func (s *HttpServer) Start(logger *zerolog.Logger) error {
	var err error
	var ln net.Listener
	var ln4 net.Listener
	var ln6 net.Listener

	if s.config.HttpPort == nil {
		return fmt.Errorf("server.httpPort is not configured")
	}

	if s.config.HttpHostV4 != nil && s.config.ListenV4 != nil && *s.config.ListenV4 {
		addrV4 := fmt.Sprintf("%s:%d", *s.config.HttpHostV4, *s.config.HttpPort)
		logger.Info().Msgf("starting http server on port: %d IPv4: %s", *s.config.HttpPort, addrV4)
		ln4, err = net.Listen("tcp4", addrV4)
		if err != nil {
			return fmt.Errorf("error listening on IPv4: %w", err)
		}
	}
	if s.config.HttpHostV6 != nil && s.config.ListenV6 != nil && *s.config.ListenV6 {
		addrV6 := fmt.Sprintf("%s:%d", *s.config.HttpHostV6, *s.config.HttpPort)
		logger.Info().Msgf("starting http server on port: %d IPv6: %s", s.config.HttpPort, addrV6)
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
	if s.config.TLS != nil && s.config.TLS.Enabled {
		tlsConfig := &tls.Config{
			MinVersion: tls.VersionTLS12,
		}

		// Load certificate and key
		cert, err := tls.LoadX509KeyPair(s.config.TLS.CertFile, s.config.TLS.KeyFile)
		if err != nil {
			return fmt.Errorf("failed to load TLS certificate and key: %w", err)
		}
		tlsConfig.Certificates = []tls.Certificate{cert}

		// Load CA if specified
		if s.config.TLS.CAFile != "" {
			caCert, err := os.ReadFile(s.config.TLS.CAFile)
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

		tlsConfig.InsecureSkipVerify = s.config.TLS.InsecureSkipVerify

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
