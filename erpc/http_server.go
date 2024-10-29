package erpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
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
)

type HttpServer struct {
	config *common.ServerConfig
	admin  *common.AdminConfig
	server *http.Server
	erpc   *ERPC
	logger *zerolog.Logger
}

func NewHttpServer(ctx context.Context, logger *zerolog.Logger, cfg *common.ServerConfig, admin *common.AdminConfig, erpc *ERPC) *HttpServer {
	reqMaxTimeout, err := time.ParseDuration(cfg.MaxTimeout)
	if err != nil {
		if cfg.MaxTimeout != "" {
			logger.Error().Err(err).Msgf("failed to parse max timeout duration using 30s default")
		}
		reqMaxTimeout = 30 * time.Second
	}

	srv := &HttpServer{
		config: cfg,
		admin:  admin,
		erpc:   erpc,
		logger: logger,
	}

	srv.server = &http.Server{
		Handler: http.TimeoutHandler(
			srv.createRequestHandler(ctx, reqMaxTimeout),
			reqMaxTimeout+1*time.Second,
			`{"jsonrpc":"2.0","error":{"code":-32603,"message":"request timeout before any upstream responded"}}`,
		),
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
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

func (s *HttpServer) createRequestHandler(mainCtx context.Context, reqMaxTimeout time.Duration) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				msg := fmt.Sprintf("unexpected server panic on top-level handler: %v -> %s", rec, debug.Stack())
				s.logger.Error().Msgf(msg)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusInternalServerError)
				fmt.Fprintf(w, `{"jsonrpc":"2.0","error":{"code":-32603,"message":"%s"}}`, msg)
			}
		}()

		encoder := common.SonicCfg.NewEncoder(w)
		encoder.SetEscapeHTML(false)

		projectId, architecture, chainId, isAdmin, isHealthCheck, err := s.parseUrlPath(r)
		if err != nil {
			handleErrorResponse(s.logger, nil, err, w, encoder)
			return
		}

		if isHealthCheck {
			s.handleHealthCheck(w, encoder)
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

		project, err := s.erpc.GetProject(projectId)
		if err != nil {
			handleErrorResponse(&lg, nil, err, w, encoder)
			return
		}

		if project != nil && project.Config.CORS != nil {
			if !s.handleCORS(w, r, project.Config.CORS) || r.Method == http.MethodOptions {
				return
			}
		}

		body, err := util.ReadAll(r.Body, 1024*1024, 10)
		if err != nil {
			handleErrorResponse(&lg, nil, err, w, encoder)
			return
		}

		lg.Debug().Msgf("received request with body: %s", body)

		var requests []json.RawMessage
		isBatch := len(body) > 0 && body[0] == '['
		if !isBatch {
			requests = []json.RawMessage{body}
		} else {
			err = common.SonicCfg.Unmarshal(body, &requests)
			if err != nil {
				handleErrorResponse(
					&lg,
					nil,
					common.NewErrJsonRpcRequestUnmarshal(err),
					w,
					encoder,
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
						msg := fmt.Sprintf("unexpected server panic on per-request handler: %v", rec)
						lg.Error().Msgf(msg)
						// Since we cannot write to w in a goroutine, collect the error in responses
						responses[index] = processErrorBody(&lg, nil, fmt.Errorf(msg))
					}
				}()

				defer wg.Done()

				requestCtx, cancel := context.WithTimeout(mainCtx, reqMaxTimeout)
				defer cancel()

				nq := common.NewNormalizedRequest(rawReq)
				nq.ApplyDirectivesFromHttp(headers, queryArgs)

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
					responses[index] = processErrorBody(&rlg, nq, err)
					return
				}

				if isAdmin {
					if err := s.erpc.AdminAuthenticate(requestCtx, nq, ap); err != nil {
						responses[index] = processErrorBody(&rlg, nq, err)
						return
					}
				} else {
					if err := project.AuthenticateConsumer(requestCtx, nq, ap); err != nil {
						responses[index] = processErrorBody(&rlg, nq, err)
						return
					}
				}

				if isAdmin {
					if s.admin != nil {
						resp, err := s.erpc.AdminHandleRequest(requestCtx, nq)
						if err != nil {
							responses[index] = processErrorBody(&rlg, nq, err)
							return
						}
						responses[index] = resp
						return
					} else {
						responses[index] = processErrorBody(
							&rlg,
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
						responses[index] = processErrorBody(&rlg, nq, common.NewErrInvalidRequest(err))
						return
					}
					if networkIdFromBody, ok := req["networkId"].(string); ok {
						networkId = networkIdFromBody
						parts := strings.Split(networkId, ":")
						if len(parts) != 2 {
							responses[index] = processErrorBody(&rlg, nq, common.NewErrInvalidRequest(fmt.Errorf(
								"networkId must follow this format: 'architecture:chainId' for example 'evm:42161'",
							)))
							return
						}
						architecture = parts[0]
						chainId = parts[1]
					} else {
						responses[index] = processErrorBody(&rlg, nq, common.NewErrInvalidRequest(fmt.Errorf(
							"networkId must follow this format: 'architecture:chainId' for example 'evm:42161'",
						)))
						return
					}
				} else {
					networkId = fmt.Sprintf("%s:%s", architecture, chainId)
				}

				nw, err := project.GetNetwork(networkId)
				if err != nil {
					responses[index] = processErrorBody(&rlg, nq, err)
					return
				}
				nq.SetNetwork(nw)

				resp, err := project.Forward(requestCtx, networkId, nq)
				if err != nil {
					responses[index] = processErrorBody(&rlg, nq, err)
					return
				}

				responses[index] = resp
			}(i, reqBody, headers, queryArgs)
		}

		wg.Wait()

		w.Header().Set("Content-Type", "application/json")

		if isBatch {
			w.WriteHeader(http.StatusOK)
			bw := NewBatchResponseWriter(responses)
			_, err = bw.WriteTo(w)

			// Release resources
			for _, resp := range responses {
				if r, ok := resp.(*common.NormalizedResponse); ok {
					r.Release()
				}
			}

			if err != nil {
				s.logger.Error().Err(err).Msg("failed to write batch response")
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusInternalServerError)
				fmt.Fprintf(w, `{"jsonrpc":"2.0","error":{"code":-32603,"message":"%s"}}`, err.Error())
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
				s.logger.Error().Err(err).Msg("failed to write response")
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusInternalServerError)
				fmt.Fprintf(w, `{"jsonrpc":"2.0","error":{"code":-32603,"message":"%s"}}`, err.Error())
				return
			}
		}
	})
}

func (s *HttpServer) parseUrlPath(r *http.Request) (
	projectId, architecture, chainId string,
	isAdmin bool,
	isHealthCheck bool,
	err error,
) {
	ps := path.Clean(r.URL.Path)
	segments := strings.Split(ps, "/")

	if len(segments) > 4 {
		return "", "", "", false, false, common.NewErrInvalidUrlPath(ps)
	}

	isPost := r.Method == http.MethodPost
	isOptions := r.Method == http.MethodOptions

	if (isPost || isOptions) && len(segments) == 4 {
		projectId = segments[1]
		architecture = segments[2]
		chainId = segments[3]
	} else if (isPost || isOptions) && len(segments) == 2 && segments[1] == "admin" {
		isAdmin = true
	} else if len(segments) == 2 && (segments[1] == "healthcheck" || segments[1] == "") {
		isHealthCheck = true
	} else {
		return "", "", "", false, false, common.NewErrInvalidUrlPath(ps)
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
		if common.WildcardMatch(allowedOrigin, origin) {
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

	if corsConfig.AllowCredentials {
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
			if err, ok := jrsp["cause"].(error); ok {
				if uer, ok := err.(*common.ErrUpstreamsExhausted); ok {
					rm = uer
				} else if uer, ok := err.(*common.ErrUpstreamRequest); ok {
					rm = uer
				}
			}
		} else if hjrsp, ok := res.(*HttpJsonRpcErrorResponse); ok {
			if err := hjrsp.Cause; err != nil {
				if uer, ok := err.(*common.ErrUpstreamRequest); ok {
					rm = uer
				}
			}
		}
	}
	if ok && rm != nil {
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

func processErrorBody(logger *zerolog.Logger, nq *common.NormalizedRequest, err error) interface{} {
	if !common.IsNull(err) {
		if nq != nil {
			nq.RLock()
		}
		if common.HasErrorCode(err, common.ErrCodeEndpointClientSideException, common.ErrCodeInvalidUrlPath) {
			logger.Debug().Err(err).Object("request", nq).Msgf("forward request errored with client-side exception")
		} else {
			if e, ok := err.(common.StandardError); ok {
				logger.Error().Err(err).Object("request", nq).Msgf("failed to forward request: %s", e.DeepestMessage())
			} else {
				logger.Error().Err(err).Object("request", nq).Msgf("failed to forward request: %s", err.Error())
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
		errObj := map[string]interface{}{
			"code":    jre.NormalizedCode(),
			"message": jre.Message,
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

func handleErrorResponse(logger *zerolog.Logger, nq *common.NormalizedRequest, err error, w http.ResponseWriter, encoder sonic.Encoder) {
	resp := processErrorBody(logger, nq, err)
	setResponseStatusCode(err, w)
	err = encoder.Encode(resp)
	if err != nil {
		logger.Error().Err(err).Msgf("failed to encode error response")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, `{"jsonrpc":"2.0","error":{"code":-32603,"message":"%s"}}`, err.Error())
	} else {
		w.Header().Set("Content-Type", "application/json")
	}
}

func (s *HttpServer) Start(logger *zerolog.Logger) error {
	addrV4 := fmt.Sprintf("%s:%d", s.config.HttpHostV4, s.config.HttpPort)
	addrV6 := fmt.Sprintf("%s:%d", s.config.HttpHostV6, s.config.HttpPort)

	var err error
	var ln net.Listener
	var ln4 net.Listener
	var ln6 net.Listener

	if s.config.HttpHostV4 != "" && s.config.ListenV4 {
		logger.Info().Msgf("starting http server on port: %d IPv4: %s", s.config.HttpPort, addrV4)
		ln4, err = net.Listen("tcp4", addrV4)
		if err != nil {
			return fmt.Errorf("error listening on IPv4: %w", err)
		}
	}
	if s.config.HttpHostV6 != "" && s.config.ListenV6 {
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

	return s.server.Serve(ln)
}

func (s *HttpServer) Shutdown(logger *zerolog.Logger) error {
	logger.Info().Msg("stopping http server...")
	return s.server.Shutdown(context.Background())
}
