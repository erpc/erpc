package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/flair-sdk/erpc/common"
	"github.com/flair-sdk/erpc/config"
	"github.com/flair-sdk/erpc/proxy"
	"github.com/flair-sdk/erpc/upstream"
	"github.com/rs/zerolog/log"
)

type HttpServer struct {
	config *config.Config
	server *http.Server
}

func NewHttpServer(cfg *config.Config, proxyCore *proxy.ProxyCore) *HttpServer {
	addr := fmt.Sprintf("%s:%s", cfg.Server.HttpHost, cfg.Server.HttpPort)

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

		project := segments[1]
		network := segments[2]

		body, err := io.ReadAll(r.Body)
		if err != nil {
			log.Error().Err(err).Msgf("failed to read request body")

			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(err)
			return
		}

		normalizedReq := upstream.NewNormalizedRequest(body)
		err = proxyCore.Forward(project, network, normalizedReq, w)

		if err != nil {
			log.Error().Err(err).Msgf("failed to forward request to upstream")

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
