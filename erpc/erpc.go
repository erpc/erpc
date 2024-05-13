package erpc

import (
	"fmt"
	"net/http"

	"github.com/flair-sdk/erpc/config"
	"github.com/flair-sdk/erpc/proxy"
	"github.com/flair-sdk/erpc/server"
	"github.com/flair-sdk/erpc/upstream"
	"github.com/flair-sdk/erpc/util"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rs/zerolog/log"
)

func Bootstrap(cfg *config.Config) (func(), error) {
	upstreamOrchestrator := upstream.NewUpstreamOrchestrator(cfg)
	err := upstreamOrchestrator.Bootstrap()
	if err != nil {
		return nil, err
	}

	rateLimitersHub := proxy.NewRateLimitersHub(cfg.RateLimiters)
	rateLimitersHub.Bootstrap()

	proxyCore := proxy.NewProxyCore(upstreamOrchestrator, rateLimitersHub)

	// Create a new HTTP server
	httpServer := server.NewHttpServer(cfg, proxyCore)
	go func() {
		if err := httpServer.Start(); err != nil {
			log.Error().Msgf("failed to start httpServer: %v", err)
			util.OsExit(util.ExitCodeHttpServerFailed)
		}
	}()

	if cfg.Metrics != nil && cfg.Metrics.Enabled {
		addr := fmt.Sprintf("%s:%d", cfg.Metrics.Host, cfg.Metrics.Port)
		log.Info().Msgf("starting metrics server on addr: %s", addr)
		go func() {
			if err := http.ListenAndServe(addr, promhttp.Handler()); err != nil {
				log.Error().Msgf("error starting metrics server: %s", err)
				util.OsExit(util.ExitCodeHttpServerFailed)
			}
		}()
	}

	// Return a shutdown function
	return func() {
		log.Info().Msg("shutting down eRPC...")
	}, nil
}
