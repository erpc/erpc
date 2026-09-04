package erpc

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/erpc/erpc/architecture/evm"
	"github.com/erpc/erpc/architecture/svm"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/util"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rs/zerolog"
)

// metricsOptions maps the metrics config onto the telemetry manager's options.
// telemetry cannot import common (common imports telemetry), so the translation
// lives here. A nil config yields nil, which Configure reads as "no metrics
// section".
func metricsOptions(cfg *common.MetricsConfig) *telemetry.Options {
	if cfg == nil {
		return nil
	}
	o := &telemetry.Options{
		HistogramBuckets:        cfg.HistogramBuckets,
		HistogramDropLabels:     cfg.HistogramDropLabels,
		HistogramLabelOverrides: cfg.HistogramLabelOverrides,
		CounterDropLabels:       cfg.CounterDropLabels,
		CounterLabelOverrides:   cfg.CounterLabelOverrides,
		ExposeMetrics:           cfg.ExposeMetrics,
		DropMetrics:             cfg.DropMetrics,
	}
	if cfg.CounterIdleEvictionAfter != nil {
		d := cfg.CounterIdleEvictionAfter.Duration()
		o.CounterIdleEvictionAfter = &d
	}
	return o
}

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
	// 2) Apply the metrics configuration and register the exposed metrics
	//
	// Metrics are defined unregistered at package init and registered here:
	// Prometheus freezes a family's label-set hash for the life of the registry,
	// so the label filters have to be installed before the first registration,
	// and a family excluded by exposeMetrics/dropMetrics is simply never
	// registered.
	if err := telemetry.Configure(metricsOptions(cfg.Metrics)); err != nil {
		logger.Warn().Err(err).Msg("failed to apply metrics configuration, using defaults where possible")
	}
	if cfg.Metrics != nil && (len(cfg.Metrics.ExposeMetrics) > 0 || len(cfg.Metrics.DropMetrics) > 0) {
		exposed, total := telemetry.ExposedFamilyCount()
		logger.Info().
			Int("exposed", exposed).
			Int("total", total).
			Strs("exposeMetrics", cfg.Metrics.ExposeMetrics).
			Strs("dropMetrics", cfg.Metrics.DropMetrics).
			Msg("metric exposure filter applied")
		// An entry that matches nothing does nothing, so a typo'd metric name is
		// otherwise invisible until someone notices the series never appeared.
		if unmatched := telemetry.UnmatchedExposureEntries(); len(unmatched) > 0 {
			logger.Warn().
				Strs("entries", unmatched).
				Msg("metrics.exposeMetrics/dropMetrics entries match no known metric family; check for typos")
		}
	}

	// Install a global networkId -> alias resolver so network-labeled metrics from
	// components that only know the raw networkId (e.g. the gRPC cache connector,
	// which discovers networks by chainId) use the same alias as every other metric.
	if cfg != nil {
		aliasByNetworkId := make(map[string]string)
		for _, p := range cfg.Projects {
			if p == nil {
				continue
			}
			for _, n := range p.Networks {
				if n != nil && n.Evm != nil && n.Evm.ChainId != 0 && n.Alias != "" {
					aliasByNetworkId[util.EvmNetworkId(n.Evm.ChainId)] = n.Alias
				}
			}
		}
		if len(aliasByNetworkId) > 0 {
			common.SetNetworkAliasResolver(func(networkId string) string { return aliasByNetworkId[networkId] })
		}
	}

	//
	// 3) Initialize eRPC
	//
	logger.Info().Msg("initializing eRPC core")
	var evmJsonRpcCache *evm.EvmJsonRpcCache
	var svmJsonRpcCache *svm.SvmJsonRpcCache
	var sharedState data.SharedStateRegistry
	if cfg.Database != nil {
		if cfg.Database.EvmJsonRpcCache != nil {
			evmJsonRpcCache, err = evm.NewEvmJsonRpcCache(appCtx, &logger, cfg.Database.EvmJsonRpcCache)
			if err != nil {
				logger.Warn().Msgf("failed to initialize evm json rpc cache: %v", err)
			}
		}
		if cfg.Database.SvmJsonRpcCache != nil {
			svmJsonRpcCache, err = svm.NewSvmJsonRpcCache(appCtx, &logger, cfg.Database.SvmJsonRpcCache)
			if err != nil {
				logger.Warn().Msgf("failed to initialize svm json rpc cache: %v", err)
			}
		}
		if cfg.Database.SharedState != nil {
			sharedState, err = data.NewSharedStateRegistry(appCtx, &logger, cfg.Database.SharedState)
			if err != nil {
				logger.Warn().Msgf("failed to initialize shared state registry: %v", err)
			}
		}
	}
	erpcInstance, err := NewERPC(appCtx, &logger, sharedState, evmJsonRpcCache, svmJsonRpcCache, cfg)
	if err != nil {
		return err
	}

	// Bootstrap core before starting servers so routes are ready
	erpcInstance.Bootstrap(appCtx)

	//
	// 4) Expose Transports
	//
	logger.Info().Msg("initializing transports")
	if cfg.Server != nil {
		httpServer, err := NewHttpServer(appCtx, &logger, cfg.Server, cfg.HealthCheck, cfg.Admin, erpcInstance)
		if err != nil {
			return err
		}
		go func() {
			if err := httpServer.Start(&logger); err != nil {
				if err != http.ErrServerClosed {
					logger.Error().Msgf("failed to start http server: %v", err)
					util.OsExit(util.ExitCodeHttpServerFailed)
				}
			}
		}()
	}
	if cfg.Server != nil && cfg.Server.GrpcEnabled != nil && *cfg.Server.GrpcEnabled && !grpcSharesHttpV4(cfg.Server) {
		grpcServer, err := NewGrpcServer(appCtx, &logger, cfg.Server, erpcInstance)
		if err != nil {
			return err
		}
		go func() {
			if err := grpcServer.Start(&logger); err != nil {
				logger.Error().Msgf("failed to start gRPC server: %v", err)
				util.OsExit(util.ExitCodeHttpServerFailed)
			}
		}()
	}
	if cfg.Metrics != nil && cfg.Metrics.Enabled != nil && *cfg.Metrics.Enabled {
		if cfg.Metrics.ErrorLabelMode != "" {
			common.SetErrorLabelMode(cfg.Metrics.ErrorLabelMode)
		}
		if cfg.Metrics.Port == nil {
			return fmt.Errorf("metrics.port is not configured")
		}
		logger.Info().Msgf("starting metrics server on port: %d", *cfg.Metrics.Port)
		srv := &http.Server{
			BaseContext: func(ln net.Listener) context.Context {
				return appCtx
			},
			Addr: fmt.Sprintf(":%d", *cfg.Metrics.Port),
			// promhttp.Handler() with the gatherer wrapped, so exposeMetrics /
			// dropMetrics also govern the collectors the manager does not own.
			Handler: promhttp.InstrumentMetricHandler(
				prometheus.DefaultRegisterer,
				promhttp.HandlerFor(telemetry.Gatherer(prometheus.DefaultGatherer), promhttp.HandlerOpts{}),
			),
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

	// Wait until the context is cancelled, then give the http server some time to finish draining.
	<-appCtx.Done()
	logger.Info().Msg("shutting down gracefully...")
	// Flush buffered integrity forensics before the process goes away; the S3
	// exporter otherwise loses everything written since its last interval.
	evm.CloseIntegrityExporters()
	if cfg.Server != nil && cfg.Server.WaitAfterShutdown != nil {
		time.Sleep(cfg.Server.WaitAfterShutdown.Duration())
	}

	return nil
}
