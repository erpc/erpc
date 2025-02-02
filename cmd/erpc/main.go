package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/erpc"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/spf13/afero"
	"github.com/urfave/cli/v3"
)

func init() {
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnixMs
	zerolog.ErrorMarshalFunc = func(err error) interface{} {
		return err
	}

	if logWriter := os.Getenv("LOG_WRITER"); logWriter == "console" {
		log.Logger = zerolog.New(zerolog.NewConsoleWriter(func(w *zerolog.ConsoleWriter) {
			w.TimeFormat = "04:05.000ms"
		})).With().Timestamp().Logger()
	}

	if logLevel := os.Getenv("LOG_LEVEL"); logLevel != "" {
		level, err := zerolog.ParseLevel(logLevel)
		if err != nil {
			log.Warn().Msgf("invalid log level '%s', defaulting to 'debug': %s", logLevel, err)
		} else {
			zerolog.SetGlobalLevel(level)
		}
	}
}

func main() {
	logger := log.With().Logger()

	// Define the flag for the config file
	configFileFlag := &cli.StringFlag{
		Name:     "config",
		Usage:    "Config file to use (by default checking erpc.js, erpc.ts, erpc.yaml, erpc.yml)",
		Required: false,
	}
	requireConfigFlag := &cli.BoolFlag{
		Name:  "require-config",
		Usage: "Enforce passing a config file instead of using a default project and public endpoints",
	}

	// Define the validate command
	validateCmd := &cli.Command{
		Name:  "validate",
		Usage: "Validate the eRPC configuration",
		Action: baseCliAction(logger, func(cfg *common.Config) error {
			return erpc.AnalyseConfig(cfg, logger)
		}),
	}

	// Define the start command
	startCmd := &cli.Command{
		Name:  "start",
		Usage: "Start the eRPC service",
		Flags: []cli.Flag{
			requireConfigFlag,
		},
		Action: baseCliAction(logger, func(cfg *common.Config) error {
			return erpc.Init(
				context.Background(),
				cfg,
				logger,
			)
		}),
	}

	// Define the main command
	cmd := &cli.Command{
		Name:      "erpc",
		Usage:     "eRPC service, if no command is provided, it will start the service",
		ArgsUsage: "[config file]",
		Version:   common.ErpcVersion,
		Flags: []cli.Flag{
			configFileFlag,
			requireConfigFlag,
		},
		// Legacy action being the start one directly, to ensure we fetch the potential first arg as config file
		Action: baseCliAction(logger, func(cfg *common.Config) error {
			return erpc.Init(
				context.Background(),
				cfg,
				logger,
			)
		}),
		// sub command for start / validation
		Commands: []*cli.Command{
			startCmd,
			validateCmd,
		},
	}
	if err := cmd.Run(context.Background(), os.Args); err != nil {
		logger.Error().Msgf("failed to start erpc: %v", err)
		util.OsExit(util.ExitCodeERPCStartFailed)
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	recvSig := <-sig
	logger.Warn().Msgf("caught signal: %v", recvSig)
}

// Base cli action func with init log + config loading
func baseCliAction(
	logger zerolog.Logger,
	fn func(*common.Config) error,
) cli.ActionFunc {
	return func(ctx context.Context, cmd *cli.Command) error {
		logger.Info().
			Str("action", cmd.Name).
			Str("version", common.ErpcVersion).
			Str("commit", common.ErpcCommitSha).
			Msg("executing command")

		cfg, err := getConfig(logger, cmd)
		if err != nil {
			logger.Error().Err(err).Msg("failed to load configuration")
			return err
		}
		return fn(cfg)
	}
}

// Get the config object from the file system, validate it and return it
func getConfig(
	logger zerolog.Logger,
	cmd *cli.Command,
) (*common.Config, error) {
	fs := afero.NewOsFs()
	configPath := ""
	possibleConfigs := []string{"./erpc.js", "./erpc.ts", "./erpc.yaml", "./erpc.yml"}
	requireConfig := cmd.Bool("require-config")

	// Check for the config flag, if present, use that file
	if configFile := cmd.String("config"); len(configFile) > 1 {
		configPath = configFile
		requireConfig = true // Since a config path is provided, we enforce it
	} else if len(cmd.Args().Slice()) > 0 { // Check for positional arg, if present, use that file
		configPath = cmd.Args().First()
		requireConfig = true
	} else { // Check for defaults config paths
		for _, path := range possibleConfigs {
			if _, err := fs.Stat(path); err == nil {
				configPath = path
				break
			}
		}
	}

	cfg := &common.Config{}
	if requireConfig || configPath != "" {
		if configPath == "" {
			return nil, fmt.Errorf("no valid configuration file found in %v", possibleConfigs)
		}
		logger.Info().Msgf("resolved configuration file to: %s", configPath)
		var err error
		cfg, err = common.LoadConfig(fs, configPath)
		if err != nil {
			return nil, fmt.Errorf("failed to load configuration from %s: %v", configPath, err)
		}
	} else {
		if err := cfg.SetDefaults(); err != nil {
			return nil, fmt.Errorf("failed to set defaults for config: %v", err)
		}
	}

	if lvl := os.Getenv("LOG_LEVEL"); lvl != "" {
		// Allow overriding the log level from the environment variable
		level, err := zerolog.ParseLevel(lvl)
		if err != nil {
			logger.Warn().Msgf("invalid log level '%s', defaulting to 'debug': %s", lvl, err)
		} else {
			cfg.LogLevel = level.String()
		}
	}

	return cfg, nil
}
