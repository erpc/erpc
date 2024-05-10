package server

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/flair-sdk/erpc/config"
	"github.com/flair-sdk/erpc/proxy"
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

		proxyCore.Forward(project, network, w, r)
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
