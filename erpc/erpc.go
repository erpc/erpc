package erpc

import (
	"github.com/flair-sdk/erpc/config"
	"github.com/flair-sdk/erpc/proxy"
	"github.com/flair-sdk/erpc/server"
	"github.com/flair-sdk/erpc/upstream"
	"github.com/rs/zerolog/log"
)

func Bootstrap(cfg *config.Config) (func(), error) {
	upstreamOrchestrator := upstream.NewUpstreamOrchestrator(cfg)
	upstreamOrchestrator.Bootstrap()

	proxyCore := proxy.NewProxyCore(upstreamOrchestrator)

	// Create a new HTTP server
	httpServer := server.NewHttpServer(cfg, proxyCore)
	go func() {
		if err := httpServer.Start(); err != nil {
			log.Fatal().Msgf("failed to start httpServer: %v", err)
		}
	}()

	// Return a shutdown function
	return func() {
		log.Info().Msg("shutting down eRPC...")
	}, nil
}
