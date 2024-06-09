package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/flair-sdk/erpc/common"
	"github.com/flair-sdk/erpc/config"
	"github.com/flair-sdk/erpc/erpc"
	"github.com/flair-sdk/erpc/evm"
	"github.com/flair-sdk/erpc/upstream"
	"github.com/rs/zerolog/log"
)

type HttpServer struct {
	config *config.ServerConfig
	server *http.Server
}

func NewHttpServer(cfg *config.ServerConfig, erpc *erpc.ERPC) *HttpServer {
	addr := fmt.Sprintf("%s:%s", cfg.HttpHost, cfg.HttpPort)

	handler := http.NewServeMux()
	handler.HandleFunc("/", func(hrw http.ResponseWriter, r *http.Request) {
		log.Debug().Msgf("received request on path: %s with body length: %d", r.URL.Path, r.ContentLength)

		// Split the URL path into segments
		segments := strings.Split(r.URL.Path, "/")

		// Check if the URL path has at least three segments ("/main/evm/1")
		if len(segments) != 4 {
			http.NotFound(hrw, r)
			return
		}

		projectId := segments[1]
		architecture := segments[2]
		var networkId string
		switch architecture {
		case upstream.ArchitectureEvm:
			networkId = evm.EIP155(segments[3])
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			log.Error().Err(err).Msgf("failed to read request body")

			hrw.Header().Set("Content-Type", "application/json")
			hrw.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(hrw).Encode(err)
			return
		}

		log.Debug().Msgf("received request for projectId: %s, networkId: %s with body: %s", projectId, networkId, body)

		nq := common.NewNormalizedRequest(networkId, body)
		project, err := erpc.GetProject(projectId)
		if err == nil {
			cwr := common.NewHttpCompositeResponseWriter(hrw)
			err = project.Forward(r.Context(), networkId, nq, cwr)
		}

		if err != nil {
			log.Error().Err(err).Msgf("failed to forward request")

			hrw.Header().Set("Content-Type", "application/json")
			var httpErr common.ErrorWithStatusCode
			if errors.As(err, &httpErr) {
				hrw.WriteHeader(httpErr.ErrorStatusCode())
			} else {
				hrw.WriteHeader(http.StatusInternalServerError)
			}

			var bodyErr common.ErrorWithBody
			if errors.As(err, &bodyErr) {
				json.NewEncoder(hrw).Encode(bodyErr.ErrorResponseBody())
			} else {
				json.NewEncoder(hrw).Encode(err)
			}
		} else {
			log.Debug().Msgf("request forwarded successfully for projectId: %s, networkId: %s", projectId, networkId)
		}
	})

	return &HttpServer{
		config: cfg,
		server: &http.Server{
			Addr:    addr,
			Handler: handler,
		},
	}
}

func (s *HttpServer) Start() error {
	log.Info().Msgf("starting http server on %s", s.server.Addr)
	return s.server.ListenAndServe()
}

func (s *HttpServer) Shutdown() error {
	log.Info().Msg("shutting down http server")
	return s.server.Shutdown(context.Background())
}
