package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/flair-sdk/erpc/common"
	"github.com/flair-sdk/erpc/config"
	"github.com/flair-sdk/erpc/erpc"
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
	handler.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		log.Debug().Msgf("received request on path: %s with body length: %d", r.URL.Path, r.ContentLength)

		// Split the URL path into segments
		segments := strings.Split(r.URL.Path, "/")

		// Check if the URL path has at least three segments ("/main/1")
		if len(segments) != 3 {
			http.NotFound(w, r)
			return
		}

		projectId := segments[1]
		networkId := segments[2]

		body, err := io.ReadAll(r.Body)
		if err != nil {
			log.Error().Err(err).Msgf("failed to read request body")

			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(err)
			return
		}

		log.Debug().Msgf("received request for projectId: %s, networkId: %s with body: %s", projectId, networkId, body)

		nq := upstream.NewNormalizedRequest(body)
		project, err := erpc.GetProject(projectId)
		if err == nil {
			// This mutex is used when multiple upstreams are tried in parallel (e.g. when Hedge failsafe policy is used)
			writeMu := &sync.Mutex{}
			err = project.Forward(r.Context(), networkId, nq, w, writeMu)
		}

		if err != nil {
			log.Error().Err(err).Msgf("failed to forward request")

			w.Header().Set("Content-Type", "application/json")
			var httpErr common.ErrorWithStatusCode
			if errors.As(err, &httpErr) {
				w.WriteHeader(httpErr.ErrorStatusCode())
			} else {
				w.WriteHeader(http.StatusInternalServerError)
			}

			var bodyErr common.ErrorWithBody
			if errors.As(err, &bodyErr) {
				json.NewEncoder(w).Encode(bodyErr.ErrorResponseBody())
			} else {
				json.NewEncoder(w).Encode(err)
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
