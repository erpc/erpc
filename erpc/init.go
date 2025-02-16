package erpc

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/erpc/erpc/architecture/evm"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/util"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rs/zerolog"
)

func Init(
	appCtx context.Context,
	cfg *common.Config,
	logger zerolog.Logger,
) error {
	//
	// 1) Set the right log level depending on the configuration
	//
	level, err := zerolog.ParseLevel(cfg.LogLevel)
	if err != nil {
		logger.Warn().Msgf("invalid log level '%s', defaulting to 'debug': %s", cfg.LogLevel, err)
		level = zerolog.DebugLevel
	} else {
		logger = logger.Level(level)
	}

	if logger.GetLevel() <= zerolog.InfoLevel {
		finalCfgJson, err := common.SonicCfg.Marshal(cfg)
		if err != nil {
			logger.Warn().Msgf("failed to marshal final configuration for tracing: %v", err)
		} else {
			logger.Info().RawJSON("config", finalCfgJson).Msg("")
		}
	}

	//
	// 2) Initialize eRPC
	//
	logger.Info().Msg("initializing eRPC core")
	var evmJsonRpcCache *evm.EvmJsonRpcCache
	var sharedState data.SharedStateRegistry
	if cfg.Database != nil {
		if cfg.Database.EvmJsonRpcCache != nil {
			evmJsonRpcCache, err = evm.NewEvmJsonRpcCache(appCtx, &logger, cfg.Database.EvmJsonRpcCache)
			if err != nil {
				logger.Warn().Msgf("failed to initialize evm json rpc cache: %v", err)
			}
		}
		if cfg.Database.SharedState != nil {
			sharedState, err = data.NewSharedStateRegistry(appCtx, &logger, cfg.Database.SharedState)
			if err != nil {
				logger.Warn().Msgf("failed to initialize shared state registry: %v", err)
			}
		}
	}
	erpcInstance, err := NewERPC(appCtx, &logger, sharedState, evmJsonRpcCache, cfg)
	if err != nil {
		return err
	}

	//
	// 3) Expose Transports
	//
	logger.Info().Msg("initializing transports")
	if cfg.Server != nil {
		httpServer := NewHttpServer(appCtx, &logger, cfg.Server, cfg.Admin, erpcInstance)
		go func() {
			if err := httpServer.Start(&logger); err != nil {
				if err != http.ErrServerClosed {
					logger.Error().Msgf("failed to start http server: %v", err)
					util.OsExit(util.ExitCodeHttpServerFailed)
				}
			}
		}()
	}

	if cfg.Metrics != nil && cfg.Metrics.Enabled != nil && *cfg.Metrics.Enabled {
		if cfg.Metrics.Port == nil {
			return fmt.Errorf("metrics.port is not configured")
		}
		logger.Info().Msgf("starting metrics server on port: %d", *cfg.Metrics.Port)
		srv := &http.Server{
			BaseContext: func(ln net.Listener) context.Context {
				return appCtx
			},
			Addr:              fmt.Sprintf(":%d", *cfg.Metrics.Port),
			Handler:           promhttp.Handler(),
			ReadHeaderTimeout: 10 * time.Second,
		}
		go func() {
			if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				logger.Error().Msgf("error starting metrics server: %s", err)
				util.OsExit(util.ExitCodeHttpServerFailed)
			}
		}()
		go func() {
			<-appCtx.Done()
			logger.Info().Msg("shutting down metrics server...")
			shutdownCtx, cancel := context.WithTimeout(appCtx, 5*time.Second)
			defer cancel()
			if err := srv.Shutdown(shutdownCtx); err != nil {
				logger.Error().Msgf("metrics server forced to shutdown: %s", err)
			} else {
				logger.Info().Msg("metrics server stopped")
			}
		}()
	}

	return nil
}
