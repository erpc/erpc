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

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/upstream"
	"github.com/rs/zerolog"
)

type HttpServer struct {
	config *common.ServerConfig
	server *http.Server
}

func NewHttpServer(ctx context.Context, logger *zerolog.Logger, cfg *common.ServerConfig, erpc *ERPC) *HttpServer {
	addr := fmt.Sprintf("%s:%d", cfg.HttpHost, cfg.HttpPort)

	handler := http.NewServeMux()
	handler.HandleFunc("/", func(hrw http.ResponseWriter, r *http.Request) {
		var resp common.NormalizedResponse
		var err error

		logger.Debug().Msgf("received request on path: %s with body length: %d", r.URL.Path, r.ContentLength)

		// Split the URL path into segments
		segments := strings.Split(r.URL.Path, "/")

		// Check if the URL path has at least three segments ("/main/evm/1")
		if len(segments) != 4 {
			http.NotFound(hrw, r)
			return
		}

		projectId := segments[1]
		networkId := fmt.Sprintf("%s:%s", segments[2], segments[3])

		body, err := io.ReadAll(r.Body)
		if err != nil {
			logger.Error().Err(err).Msgf("failed to read request body")

			hrw.Header().Set("Content-Type", "application/json")
			hrw.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(hrw).Encode(err)
			return
		}

		logger.Debug().Msgf("received request for projectId: %s, networkId: %s with body: %s", projectId, networkId, body)

		project, err := erpc.GetProject(projectId)
		if err == nil {
			nw, err := erpc.GetNetwork(projectId, networkId)
			if err != nil {
				logger.Error().Err(err).Msgf("failed to get network %s for project %s", networkId, projectId)
				handleErrorResponse(logger, err, hrw)
				return
			}
			nq := upstream.NewNormalizedRequest(body)
			nq.SetNetwork(nw)
			nq.ApplyDirectivesFromHttpHeaders(r.Header)

			resp, err = project.Forward(r.Context(), networkId, nq)
			if err == nil && resp != nil {
				hrw.Header().Set("Content-Type", "application/json")
				hrw.WriteHeader(http.StatusOK)
				hrw.Write(resp.Body())
				logger.Debug().Msgf("request forwarded successfully for projectId: %s, networkId: %s", projectId, networkId)
			} else {
				handleErrorResponse(logger, err, hrw)
			}
		} else {
			handleErrorResponse(logger, err, hrw)
		}
	})

	srv := &HttpServer{
		config: cfg,
		server: &http.Server{
			Addr:    addr,
			Handler: handler,
		},
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

func handleErrorResponse(logger *zerolog.Logger, err error, hrw http.ResponseWriter) {
	logger.Error().Err(err).Msgf("failed to forward request")

	hrw.Header().Set("Content-Type", "application/json")
	var httpErr common.ErrorWithStatusCode
	if errors.As(err, &httpErr) {
		hrw.WriteHeader(httpErr.ErrorStatusCode())
	} else {
		hrw.WriteHeader(http.StatusInternalServerError)
	}

	jre := &common.ErrJsonRpcExceptionInternal{}
	if errors.As(err, &jre) {
		json.NewEncoder(hrw).Encode(map[string]interface{}{
			"code":    jre.NormalizedCode(),
			"message": jre.Message,
			"cause":   err,
		})
		return
	}

	var bodyErr common.ErrorWithBody
	var writeErr error

	if errors.As(err, &bodyErr) {
		writeErr = json.NewEncoder(hrw).Encode(bodyErr.ErrorResponseBody())
	} else if _, ok := err.(*common.BaseError); ok {
		writeErr = json.NewEncoder(hrw).Encode(err)
	} else {
		writeErr = json.NewEncoder(hrw).Encode(
			common.BaseError{
				Code:    "ErrUnknown",
				Message: "unexpected server error",
				Cause:   err,
			},
		)
	}

	if writeErr != nil {
		logger.Error().Err(writeErr).Msgf("failed to encode error response body")
		hrw.WriteHeader(http.StatusInternalServerError)

		var cause interface{}
		if be, ok := writeErr.(*common.BaseError); ok {
			cause = be
		} else {
			cause = writeErr.Error()
		}

		json.NewEncoder(hrw).Encode(map[string]interface{}{
			"code":    common.JsonRpcErrorServerSideException,
			"message": "unexpected server error",
			"cause":   cause,
		})
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
