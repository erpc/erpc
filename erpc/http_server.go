package erpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
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
	"github.com/valyala/fasthttp"
)

type HttpServer struct {
	config *common.ServerConfig
	server *fasthttp.Server
	erpc   *ERPC
	logger *zerolog.Logger
}

func NewHttpServer(ctx context.Context, logger *zerolog.Logger, cfg *common.ServerConfig, erpc *ERPC) *HttpServer {
	reqMaxTimeout, err := time.ParseDuration(cfg.MaxTimeout)
	if err != nil {
		if cfg.MaxTimeout != "" {
			logger.Error().Err(err).Msgf("failed to parse max timeout duration using 30s default")
		}
		reqMaxTimeout = 30 * time.Second
	}

	srv := &HttpServer{
		config: cfg,
		erpc:   erpc,
		logger: logger,
	}

	srv.server = &fasthttp.Server{
		Handler: fasthttp.TimeoutHandler(
			srv.createRequestHandler(ctx, reqMaxTimeout),
			// This is the last resort timeout if nothing could be done in time
			reqMaxTimeout+1*time.Second,
			`{"jsonrpc":"2.0","error":{"code":-32603,"message":"request timeout before any upstream responded"}}`,
		),
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
		Name:         fmt.Sprintf("erpc (%s/%s)", common.ErpcVersion, common.ErpcCommitSha),
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

func (s *HttpServer) createRequestHandler(mainCtx context.Context, reqMaxTimeout time.Duration) fasthttp.RequestHandler {
	return func(fastCtx *fasthttp.RequestCtx) {
		defer func() {
			defer func() { recover() }()
			if r := recover(); r != nil {
				msg := fmt.Sprintf("unexpected server panic on top-level handler: %v -> %s", r, util.Mem2Str(debug.Stack()))
				s.logger.Error().Msgf(msg)
				fastCtx.SetStatusCode(fasthttp.StatusInternalServerError)
				fastCtx.Response.Header.Set("Content-Type", "application/json")
				fastCtx.SetBodyString(fmt.Sprintf(`{"jsonrpc":"2.0","error":{"code":-32603,"message":"%s"}}`, msg))
			}
		}()

		encoder := common.SonicCfg.NewEncoder(fastCtx.Response.BodyWriter())
		encoder.SetEscapeHTML(false)

		projectId, architecture, chainId, isAdmin, isHealthCheck, err := s.parseUrlPath(fastCtx)
		if err != nil {
			handleErrorResponse(s.logger, nil, err, fastCtx, encoder)
			return
		}

		if isHealthCheck {
			s.handleHealthCheck(fastCtx, encoder)
			return
		}

		lg := s.logger.With().Str("projectId", projectId).Str("architecture", architecture).Str("chainId", chainId).Logger()

		project, err := s.erpc.GetProject(projectId)
		if err != nil {
			handleErrorResponse(&lg, nil, err, fastCtx, encoder)
			return
		}

		if project.Config.CORS != nil {
			if !s.handleCORS(fastCtx, project.Config.CORS) {
				return
			}

			if fastCtx.IsOptions() {
				return
			}
		}

		body := fastCtx.PostBody()

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
					fastCtx,
					encoder,
				)
				return
			}
		}

		responses := make([]interface{}, len(requests))
		var wg sync.WaitGroup

		var headersCopy fasthttp.RequestHeader
		var queryArgsCopy fasthttp.Args
		fastCtx.Request.Header.CopyTo(&headersCopy)
		fastCtx.QueryArgs().CopyTo(&queryArgsCopy)

		for i, reqBody := range requests {
			wg.Add(1)
			go func(index int, rawReq json.RawMessage, headersCopy *fasthttp.RequestHeader, queryArgsCopy *fasthttp.Args) {
				defer func() {
					defer func() { recover() }()
					if r := recover(); r != nil {
						msg := fmt.Sprintf("unexpected server panic on per-request handler: %v -> %s", r, util.Mem2Str(debug.Stack()))
						lg.Error().Msgf(msg)
						fastCtx.SetStatusCode(fasthttp.StatusInternalServerError)
						fastCtx.Response.Header.Set("Content-Type", "application/json")
						fastCtx.SetBodyString(fmt.Sprintf(`{"jsonrpc":"2.0","error":{"code":-32603,"message":"%s"}}`, msg))
					}
				}()

				defer wg.Done()

				requestCtx, cancel := context.WithTimeoutCause(mainCtx, reqMaxTimeout, common.NewErrRequestTimeout(reqMaxTimeout))
				defer cancel()

				nq := common.NewNormalizedRequest(rawReq)
				nq.ApplyDirectivesFromHttp(headersCopy, queryArgsCopy)

				m, _ := nq.Method()
				rlg := lg.With().Str("method", m).Logger()

				ap, err := auth.NewPayloadFromHttp(project.Config.Id, nq, headersCopy, queryArgsCopy)
				if err != nil {
					responses[index] = processErrorBody(&rlg, nq, err)
					return
				}

				if isAdmin {
					if err := project.AuthenticateAdmin(requestCtx, nq, ap); err != nil {
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
					if project.Config.Admin != nil {
						resp, err := project.HandleAdminRequest(requestCtx, nq)
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
			}(i, reqBody, &headersCopy, &queryArgsCopy)
		}

		wg.Wait()

		fastCtx.Response.Header.SetContentType("application/json")

		if isBatch {
			fastCtx.SetStatusCode(fasthttp.StatusOK)
			err = encoder.Encode(responses)
			for _, resp := range responses {
				if r, ok := resp.(*common.NormalizedResponse); ok {
					r.Release()
				}
			}
			if err != nil {
				fastCtx.SetStatusCode(fasthttp.StatusInternalServerError)
				fastCtx.Response.Header.Set("Content-Type", "application/json")
				fastCtx.SetBodyString(fmt.Sprintf(`{"jsonrpc":"2.0","error":{"code":-32603,"message":"%s"}}`, err.Error()))
				return
			}
		} else {
			res := responses[0]
			setResponseHeaders(res, fastCtx)
			setResponseStatusCode(res, fastCtx)
			if r, ok := res.(*common.NormalizedResponse); ok {
				rdr, err := r.GetReader()
				if err != nil {
					fastCtx.SetStatusCode(fasthttp.StatusInternalServerError)
					fastCtx.SetBodyString(fmt.Sprintf(`{"jsonrpc":"2.0","error":{"code":-32603,"message":"%s"}}`, err.Error()))
					return
				}
				fastCtx.Response.SetBodyStream(rdr, -1)
				r.Release()
			} else {
				err = encoder.Encode(res)
			}
			if err != nil {
				fastCtx.SetStatusCode(fasthttp.StatusInternalServerError)
				fastCtx.SetBodyString(fmt.Sprintf(`{"jsonrpc":"2.0","error":{"code":-32603,"message":"%s"}}`, err.Error()))
				return
			}
		}
	}
}

func (s *HttpServer) parseUrlPath(fctx *fasthttp.RequestCtx) (
	projectId, architecture, chainId string,
	isAdmin bool,
	isHealthCheck bool,
	err error,
) {
	isPost := fctx.IsPost()
	ps := path.Clean(util.Mem2Str(fctx.Path()))
	segments := strings.Split(ps, "/")

	if len(segments) != 2 && len(segments) != 3 && len(segments) != 4 {
		return "", "", "", false, false, common.NewErrInvalidUrlPath(ps)
	}

	if isPost && len(segments) == 4 {
		projectId = segments[1]
		architecture = segments[2]
		chainId = segments[3]
	} else if isPost && len(segments) == 3 && segments[2] == "admin" {
		projectId = segments[1]
		isAdmin = true
	} else if len(segments) == 2 && segments[1] == "healthcheck" {
		isHealthCheck = true
	} else {
		return "", "", "", false, false, common.NewErrInvalidUrlPath(ps)
	}

	return projectId, architecture, chainId, isAdmin, isHealthCheck, nil
}

func (s *HttpServer) handleCORS(ctx *fasthttp.RequestCtx, corsConfig *common.CORSConfig) bool {
	origin := util.Mem2Str(ctx.Request.Header.Peek("Origin"))
	if origin == "" {
		return true
	}

	health.MetricCORSRequestsTotal.WithLabelValues(util.Mem2Str(ctx.Path()), origin).Inc()

	allowed := false
	for _, allowedOrigin := range corsConfig.AllowedOrigins {
		if common.WildcardMatch(allowedOrigin, origin) {
			allowed = true
			break
		}
	}

	if !allowed {
		s.logger.Debug().Str("origin", origin).Msg("CORS request from disallowed origin")
		health.MetricCORSDisallowedOriginTotal.WithLabelValues(util.Mem2Str(ctx.Path()), origin).Inc()

		if ctx.IsOptions() {
			ctx.SetStatusCode(fasthttp.StatusNoContent)
		} else {
			ctx.Error("CORS request from disallowed origin", fasthttp.StatusForbidden)
		}
		return false
	}

	ctx.Response.Header.Set("Access-Control-Allow-Origin", origin)
	ctx.Response.Header.Set("Access-Control-Allow-Methods", strings.Join(corsConfig.AllowedMethods, ", "))
	ctx.Response.Header.Set("Access-Control-Allow-Headers", strings.Join(corsConfig.AllowedHeaders, ", "))
	ctx.Response.Header.Set("Access-Control-Expose-Headers", strings.Join(corsConfig.ExposedHeaders, ", "))

	if corsConfig.AllowCredentials {
		ctx.Response.Header.Set("Access-Control-Allow-Credentials", "true")
	}

	if corsConfig.MaxAge > 0 {
		ctx.Response.Header.Set("Access-Control-Max-Age", fmt.Sprintf("%d", corsConfig.MaxAge))
	}

	if ctx.IsOptions() {
		health.MetricCORSPreflightRequestsTotal.WithLabelValues(util.Mem2Str(ctx.Path()), origin).Inc()
		ctx.SetStatusCode(fasthttp.StatusNoContent)
		return false
	}

	return true
}

func setResponseHeaders(res interface{}, fastCtx *fasthttp.RequestCtx) {
	var rm common.ResponseMetadata
	var ok bool
	rm, ok = res.(common.ResponseMetadata)
	if !ok {
		var jrsp map[string]interface{}
		var hjrsp *HttpJsonRpcErrorResponse
		if jrsp, ok = res.(map[string]interface{}); ok {
			var err error
			if err, ok = jrsp["cause"].(error); ok {
				uer := &common.ErrUpstreamsExhausted{}
				if ok = errors.As(err, &uer); ok {
					rm = uer
				} else {
					uer := &common.ErrUpstreamRequest{}
					if ok = errors.As(err, &uer); ok {
						rm = uer
					}
				}
			}
		} else if hjrsp, ok = res.(*HttpJsonRpcErrorResponse); ok {
			if err := hjrsp.Cause; err != nil {
				uer := &common.ErrUpstreamRequest{}
				if ok = errors.As(err, &uer); ok {
					rm = uer
				}
			}
		}
	}
	if ok && rm != nil {
		if rm.FromCache() {
			fastCtx.Response.Header.Set("X-ERPC-Cache", "HIT")
		} else {
			fastCtx.Response.Header.Set("X-ERPC-Cache", "MISS")
		}
		if rm.UpstreamId() != "" {
			fastCtx.Response.Header.Set("X-ERPC-Upstream", rm.UpstreamId())
		}
		fastCtx.Response.Header.Set("X-ERPC-Attempts", fmt.Sprintf("%d", rm.Attempts()))
		fastCtx.Response.Header.Set("X-ERPC-Retries", fmt.Sprintf("%d", rm.Retries()))
		fastCtx.Response.Header.Set("X-ERPC-Hedges", fmt.Sprintf("%d", rm.Hedges()))
	}
}

func setResponseStatusCode(respOrErr interface{}, fastCtx *fasthttp.RequestCtx) {
	if err, ok := respOrErr.(error); ok {
		fastCtx.SetStatusCode(decideErrorStatusCode(err))
	} else if resp, ok := respOrErr.(map[string]interface{}); ok {
		if cause, ok := resp["cause"].(error); ok {
			fastCtx.SetStatusCode(decideErrorStatusCode(cause))
		} else {
			fastCtx.SetStatusCode(fasthttp.StatusOK)
		}
	} else if hjrsp, ok := respOrErr.(*HttpJsonRpcErrorResponse); ok {
		fastCtx.SetStatusCode(decideErrorStatusCode(hjrsp.Cause))
	} else {
		fastCtx.SetStatusCode(fasthttp.StatusOK)
	}
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
		if common.HasErrorCode(err, common.ErrCodeEndpointClientSideException) {
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

	// TODO extend this section to detect transport mode (besides json-rpc) when more modes are added.
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
	return fasthttp.StatusInternalServerError
}

func handleErrorResponse(logger *zerolog.Logger, nq *common.NormalizedRequest, err error, ctx *fasthttp.RequestCtx, encoder sonic.Encoder) {
	resp := processErrorBody(logger, nq, err)
	setResponseStatusCode(err, ctx)
	err = encoder.Encode(resp)
	if err != nil {
		logger.Error().Err(err).Msgf("failed to encode error response")
		ctx.SetStatusCode(fasthttp.StatusInternalServerError)
		ctx.Response.Header.Set("Content-Type", "application/json")
		ctx.SetBodyString(fmt.Sprintf(`{"jsonrpc":"2.0","error":{"code":-32603,"message":"%s"}}`, err.Error()))
	} else {
		ctx.Response.Header.Set("Content-Type", "application/json")
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
		return fmt.Errorf("you must configure at least one of server.httpPortV4 or server.httpPortV6")
	}

	return s.server.Serve(ln)
}

func (s *HttpServer) Shutdown(logger *zerolog.Logger) error {
	logger.Info().Msg("stopping http server...")
	return s.server.Shutdown()
}
