package erpc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/bytedance/sonic"
	"github.com/erpc/erpc/auth"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/health"
	"github.com/rs/zerolog"
	"github.com/valyala/fasthttp"
)

type HttpServer struct {
	config *common.ServerConfig
	server *fasthttp.Server
	erpc   *ERPC
	logger *zerolog.Logger
}

var bufPool = sync.Pool{
	New: func() interface{} {
		return new(bytes.Buffer)
	},
}

func NewHttpServer(ctx context.Context, logger *zerolog.Logger, cfg *common.ServerConfig, erpc *ERPC) *HttpServer {
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

	srv.server = &fasthttp.Server{
		Handler:      srv.handleRequest(timeOutDur),
		ReadTimeout:  timeOutDur,
		WriteTimeout: timeOutDur,
	}

	go func() {
		<-ctx.Done()
		logger.Info().Msg("shutting down http server...")
		if err := srv.Shutdown(logger); err != nil {
			logger.Error().Msgf("http server forced to shutdown: %s", err)
		} else {
			logger.Info().Msg("http server stopped")
		}
	}()

	return srv
}

func (s *HttpServer) handleRequest(timeOutDur time.Duration) fasthttp.RequestHandler {
	return func(ctx *fasthttp.RequestCtx) {
		buf := bufPool.Get().(*bytes.Buffer)
		defer bufPool.Put(buf)
		buf.Reset()
		encoder := json.NewEncoder(buf)

		segments := strings.Split(string(ctx.Path()), "/")
		if len(segments) != 2 && len(segments) != 3 && len(segments) != 4 {
			handleErrorResponse(s.logger, nil, common.NewErrInvalidUrlPath(string(ctx.Path())), ctx)
			return
		}

		projectId := segments[1]
		architecture, chainId := "", ""
		isAdmin := false

		if len(segments) == 4 {
			architecture = segments[2]
			chainId = segments[3]
		} else if len(segments) == 3 {
			if segments[2] == "admin" {
				isAdmin = true
			} else {
				handleErrorResponse(s.logger, nil, common.NewErrInvalidUrlPath(string(ctx.Path())), ctx)
				return
			}
		}

		project, err := s.erpc.GetProject(projectId)
		if err != nil {
			handleErrorResponse(s.logger, nil, err, ctx)
			return
		}

		if project.Config.CORS != nil {
			if !s.handleCORS(ctx, project.Config.CORS) {
				return
			}

			if ctx.IsOptions() {
				return
			}
		}

		requestCtx, cancel := context.WithTimeoutCause(ctx, timeOutDur, common.NewErrRequestTimeOut(timeOutDur))
		defer cancel()

		body := ctx.PostBody()

		s.logger.Debug().Msgf("received request for projectId: %s, architecture: %s with body: %s", projectId, architecture, body)

		var requests []json.RawMessage
		err = sonic.Unmarshal(body, &requests)
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

				nq := common.NewNormalizedRequest(rawReq)
				nq.ApplyDirectivesFromHttpHeaders(&ctx.Request.Header)

				ap, err := auth.NewPayloadFromHttp(project.Config.Id, nq, ctx)
				if err != nil {
					responses[index] = processErrorBody(s.logger, nq, err)
					return
				}

				if isAdmin {
					if err := project.AuthenticateAdmin(requestCtx, nq, ap); err != nil {
						responses[index] = processErrorBody(s.logger, nq, err)
						return
					}
				} else {
					if err := project.AuthenticateConsumer(requestCtx, nq, ap); err != nil {
						responses[index] = processErrorBody(s.logger, nq, err)
						return
					}
				}

				if isAdmin {
					if project.Config.Admin != nil {
						resp, err := project.HandleAdminRequest(requestCtx, nq)
						if err != nil {
							responses[index] = processErrorBody(s.logger, nq, err)
							return
						}
						responses[index] = resp
						return
					} else {
						responses[index] = processErrorBody(
							s.logger,
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
					if err := sonic.Unmarshal(rawReq, &req); err != nil {
						responses[index] = processErrorBody(s.logger, nq, common.NewErrInvalidRequest(err))
						return
					}
					if networkIdFromBody, ok := req["networkId"].(string); ok {
						networkId = networkIdFromBody
						parts := strings.Split(networkId, ":")
						if len(parts) != 2 {
							errorMsg := "Invalid networkId format. Expected format: 'architecture:chainId'. " +
								"For example, 'evm:42161'."
							responses[index] = processErrorBody(s.logger, nq, common.NewErrInvalidRequest(fmt.Errorf(errorMsg)))
							return
						}
						architecture = parts[0]
						chainId = parts[1]
					} else {
						errorMsg := "networkId not provided in request body. Please provide in the format 'architecture:chainId'. " +
							"For example, 'evm:42161'."
						responses[index] = processErrorBody(s.logger, nq, common.NewErrInvalidRequest(fmt.Errorf(errorMsg)))
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

				resp, err := project.Forward(requestCtx, networkId, nq)
				if err != nil {
					responses[index] = processErrorBody(s.logger, nq, err)
					return
				}

				responses[index] = resp
			}(i, reqBody)
		}

		wg.Wait()

		ctx.Response.Header.SetContentType("application/json")

		if isBatch {
			ctx.SetStatusCode(fasthttp.StatusOK)
			encoder.Encode(responses)
		} else {
			res := responses[0]

			var rm common.ResponseMetadata
			var ok bool
			rm, ok = res.(common.ResponseMetadata)
			if !ok {
				var jrsp, errObj map[string]interface{}
				if jrsp, ok = res.(map[string]interface{}); ok {
					if errObj, ok = jrsp["error"].(map[string]interface{}); ok {
						if err, ok = errObj["cause"].(error); ok {
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
					}
				}
			}

			if ok {
				if rm.FromCache() {
					ctx.Response.Header.Set("X-ERPC-Cache", "HIT")
				} else {
					ctx.Response.Header.Set("X-ERPC-Cache", "MISS")
				}
				if rm.UpstreamId() != "" {
					ctx.Response.Header.Set("X-ERPC-Upstream", rm.UpstreamId())
				}
				ctx.Response.Header.Set("X-ERPC-Attempts", fmt.Sprintf("%d", rm.Attempts()))
				ctx.Response.Header.Set("X-ERPC-Retries", fmt.Sprintf("%d", rm.Retries()))
				ctx.Response.Header.Set("X-ERPC-Hedges", fmt.Sprintf("%d", rm.Hedges()))
			}

			if err, ok := res.(error); ok {
				ctx.SetStatusCode(processErrorStatusCode(err))
			} else {
				ctx.SetStatusCode(fasthttp.StatusOK)
			}
			encoder.Encode(res)
		}

		ctx.SetBody(buf.Bytes())
	}
}

func (s *HttpServer) handleCORS(ctx *fasthttp.RequestCtx, corsConfig *common.CORSConfig) bool {
	origin := string(ctx.Request.Header.Peek("Origin"))
	if origin == "" {
		return true
	}

	health.MetricCORSRequestsTotal.WithLabelValues(string(ctx.Path()), origin).Inc()

	allowed := false
	for _, allowedOrigin := range corsConfig.AllowedOrigins {
		if common.WildcardMatch(allowedOrigin, origin) {
			allowed = true
			break
		}
	}

	if !allowed {
		s.logger.Debug().Str("origin", origin).Msg("CORS request from disallowed origin")
		health.MetricCORSDisallowedOriginTotal.WithLabelValues(string(ctx.Path()), origin).Inc()

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
		health.MetricCORSPreflightRequestsTotal.WithLabelValues(string(ctx.Path()), origin).Inc()
		ctx.SetStatusCode(fasthttp.StatusNoContent)
		return false
	}

	return true
}

func processErrorBody(logger *zerolog.Logger, nq *common.NormalizedRequest, err error) interface{} {
	if !common.IsNull(err) {
		if common.HasErrorCode(err, common.ErrCodeEndpointClientSideException) {
			logger.Debug().Err(err).Msgf("forward request errored with client-side exception")
		} else {
			logger.Error().Err(err).Msgf("failed to forward request")
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
	return fasthttp.StatusInternalServerError
}

func handleErrorResponse(logger *zerolog.Logger, nq *common.NormalizedRequest, err error, ctx *fasthttp.RequestCtx) {
	if !common.IsNull(err) {
		if common.HasErrorCode(err, common.ErrCodeEndpointClientSideException) {
			logger.Debug().Err(err).Msgf("forward request errored with client-side exception")
		} else {
			logger.Error().Err(err).Msgf("failed to forward request")
		}
	}

	ctx.Response.Header.SetContentType("application/json")
	var httpErr common.ErrorWithStatusCode
	if errors.As(err, &httpErr) {
		ctx.SetStatusCode(httpErr.ErrorStatusCode())
	} else {
		ctx.SetStatusCode(fasthttp.StatusInternalServerError)
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
		writeErr := json.NewEncoder(ctx).Encode(map[string]interface{}{
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

	var bodyErr common.ErrorWithBody
	var writeErr error

	if errors.As(err, &bodyErr) {
		writeErr = json.NewEncoder(ctx).Encode(bodyErr.ErrorResponseBody())
	} else if _, ok := err.(*common.BaseError); ok {
		writeErr = json.NewEncoder(ctx).Encode(err)
	} else {
		if serr, ok := err.(common.StandardError); ok {
			writeErr = json.NewEncoder(ctx).Encode(serr)
		} else {
			writeErr = json.NewEncoder(ctx).Encode(
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
	logger.Info().Msgf("starting http server on %s:%d", s.config.HttpHost, s.config.HttpPort)
	return s.server.ListenAndServe(fmt.Sprintf("%s:%d", s.config.HttpHost, s.config.HttpPort))
}

func (s *HttpServer) Shutdown(logger *zerolog.Logger) error {
	logger.Info().Msg("shutting down http server")
	return s.server.Shutdown()
}
