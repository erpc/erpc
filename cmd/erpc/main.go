package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/flair-sdk/erpc/config"
	"github.com/flair-sdk/erpc/erpc"
	"github.com/flair-sdk/erpc/server"
	"github.com/flair-sdk/erpc/util"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/spf13/afero"
)

func main() {
	logger := log.With().Logger()

	shutdown, err := Init(context.Background(), &logger, afero.NewOsFs(), os.Args)
	defer shutdown()

	if err != nil {
		logger.Error().Msgf("failed to start eRPC: %v", err)
		util.OsExit(util.ExitCodeERPCStartFailed)
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	recvSig := <-sig
	logger.Warn().Msgf("caught signal: %v", recvSig)
}

func Init(
	ctx context.Context,
	logger *zerolog.Logger,
	fs afero.Fs,
	args []string,
) (func() error, error) {
	//
	// 1) Load configuration
	//
	logger.Info().Msg("loading eRPC configuration")
	configPath := "./erpc.yaml"
	if len(args) > 1 {
		configPath = args[1]
	}
	if _, err := fs.Stat(configPath); errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("config file '%s' does not exist", configPath)
	}
	logger.Info().Msgf("resolved configuration file to: %s", configPath)
	cfg, err := config.LoadConfig(fs, configPath)
	if err != nil {
		return nil, fmt.Errorf("failed to load configuration from %s: %v", configPath, err)
	}
	if level, err := zerolog.ParseLevel(cfg.LogLevel); err != nil {
		logger.Warn().Msgf("invalid log level '%s', defaulting to 'debug': %s", cfg.LogLevel, err)
		zerolog.SetGlobalLevel(zerolog.DebugLevel)
	} else {
		logger.Level(level)
	}

	//
	// 2) Initialize eRPC
	//
	logger.Info().Msg("bootstrapping eRPC")
	var evmJsonRpcCache *erpc.EvmJsonRpcCache
	if cfg.Database != nil {
		if cfg.Database.EvmJsonRpcCache != nil {
			evmJsonRpcCache, err = erpc.NewEvmJsonRpcCache(ctx, cfg.Database.EvmJsonRpcCache)
			if err != nil {
				return nil, err
			}
		}
	}
	erpcInstance, err := erpc.NewERPC(logger, evmJsonRpcCache, cfg)
	if err != nil {
		return nil, err
	}

	//
	// 3) Expose Transports
	//
	logger.Info().Msg("bootstrapping transports")
	var httpServer *server.HttpServer
	if cfg.Server != nil {
		httpServer = server.NewHttpServer(cfg.Server, erpcInstance)
		go func() {
			if err := httpServer.Start(); err != nil {
				if err != http.ErrServerClosed {
					logger.Error().Msgf("failed to start http server: %v", err)
					util.OsExit(util.ExitCodeHttpServerFailed)
				}
			}
		}()
	}

	if cfg.Metrics != nil && cfg.Metrics.Enabled {
		addr := fmt.Sprintf("%s:%d", cfg.Metrics.Host, cfg.Metrics.Port)
		logger.Info().Msgf("starting metrics server on addr: %s", addr)
		go func() {
			if err := http.ListenAndServe(addr, promhttp.Handler()); err != nil {
				logger.Error().Msgf("error starting metrics server: %s", err)
				util.OsExit(util.ExitCodeHttpServerFailed)
			}
		}()
	}

	return func() error {
		logger.Info().Msg("shutting down eRPC...")
		err := erpcInstance.Shutdown()
		if err != nil {
			return err
		}
		if httpServer != nil {
			err = httpServer.Shutdown()
			if err != nil {
				return err
			}
		}
		return nil
	}, nil
}
