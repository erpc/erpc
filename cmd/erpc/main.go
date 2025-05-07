package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/erpc"
	"github.com/erpc/erpc/util"
	"github.com/joho/godotenv"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/spf13/afero"
	"github.com/urfave/cli/v3"
)

func init() {
	// Load .env file if it exists
	if err := godotenv.Load(); err != nil {
		if !os.IsNotExist(err) {
			log.Error().Err(err).Msg("failed to load .env file")
		}
	}

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

	// Create a context that is cancelled on SIGINT/SIGTERM
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Define the flag for the config file
	configFileFlag := &cli.StringFlag{
		Name:     "config",
		Usage:    "Config file to use (by default checking erpc.js, erpc.ts, erpc.yaml, erpc.yml)",
		Required: false,
	}
	endpointFlag := &cli.StringSliceFlag{
		Name:    "endpoint",
		Aliases: []string{"e"},
		Usage:   "Endpoint URL to use when no config file is provided (can be specified multiple times)",
	}
	// setFlag := &cli.StringSliceFlag{
	// 	Name:    "set",
	// 	Aliases: []string{"s"},
	// 	Usage:   "Override a config value (can be specified multiple times)",
	// }
	requireConfigFlag := &cli.BoolFlag{
		Name:  "require-config",
		Usage: "Enforce passing a config file instead of using a default project and public endpoints",
	}

	// Define the validate command
	validateCmd := &cli.Command{
		Name:  "validate",
		Usage: "Validate the eRPC configuration",
		Action: baseCliAction(logger, func(ctx context.Context, cfg *common.Config) error {
			return erpc.AnalyseConfig(cfg, logger)
		}),
	}

	// Define the start command
	startCmd := &cli.Command{
		Name:  "start",
		Usage: "Start the eRPC service",
		Flags: []cli.Flag{
			endpointFlag,
			// setFlag,
			requireConfigFlag,
		},
		Action: baseCliAction(logger, func(ctx context.Context, cfg *common.Config) error {
			return erpc.Init(
				ctx,
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
			endpointFlag,
			// setFlag,
			requireConfigFlag,
		},
		// Legacy action being the start one directly, to ensure we fetch the potential first arg as config file
		Action: baseCliAction(logger, func(ctx context.Context, cfg *common.Config) error {
			return erpc.Init(
				ctx,
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
	if err := cmd.Run(ctx, os.Args); err != nil {
		logger.Error().Msgf("failed to start erpc: %v", err)
		util.OsExit(util.ExitCodeERPCStartFailed)
	}
}

// Base cli action func with init log + config loading
func baseCliAction(
	logger zerolog.Logger,
	fn func(ctx context.Context, cfg *common.Config) error,
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
		return fn(ctx, cfg)
	}
}

// Get the config object from the file system, validate it and return it
func getConfig(
	logger zerolog.Logger,
	cmd *cli.Command,
) (*common.Config, error) {
	fs := afero.NewOsFs()
	configPath := ""
	possibleConfigs := []string{
		"./erpc.yaml",
		"./erpc.yml",
		"./erpc.ts",
		"./erpc.js",

		"/erpc.yaml",
		"/erpc.yml",
		"/erpc.ts",
		"/erpc.js",

		"/root/erpc.yaml",
		"/root/erpc.yml",
		"/root/erpc.ts",
		"/root/erpc.js",
	}
	requireConfig := cmd.Bool("require-config")
	endpoints := cmd.StringSlice("endpoint")
	// configOverrides := cmd.StringMap("set")

	// Check for the config flag, if present, use that file
	if configFile := cmd.String("config"); len(configFile) > 1 {
		configPath = configFile
		requireConfig = true // Since a config path is provided, we enforce it
	} else if len(cmd.Args().Slice()) > 0 { // Check for positional arg, if present, use that file
		configPath = cmd.Args().First()
		requireConfig = true
	} else { // Check for defaults config paths
		currentDir, err := os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("failed to get current directory: %v", err)
		}
		for _, path := range possibleConfigs {
			fullPath := path
			if !strings.HasPrefix(path, "/") {
				fullPath = filepath.Join(currentDir, path)
			}
			logger.Info().Msgf("looking for config file: %s", fullPath)
			if _, err := fs.Stat(fullPath); err == nil {
				configPath = path
				break
			}
		}
	}

	cfg := &common.Config{}
	opts := &common.DefaultOptions{}

	// If endpoints are provided via command line, use them
	if len(endpoints) > 0 {
		logger.Info().Msgf("using %d endpoints provided via command line", len(endpoints))
		opts.Endpoints = endpoints
	}

	if requireConfig || configPath != "" {
		if configPath == "" {
			return nil, fmt.Errorf("no valid configuration file found in %v", possibleConfigs)
		}
		logger.Info().Msgf("resolved configuration file to: %s", configPath)
		var err error
		cfg, err = common.LoadConfig(fs, configPath, opts)
		if err != nil {
			return nil, fmt.Errorf("failed to load configuration from %s: %v", configPath, err)
		}
	} else {
		if err := cfg.SetDefaults(opts); err != nil {
			return nil, fmt.Errorf("failed to set defaults for config: %v", err)
		}
	}

	// for keyPath, value := range configOverrides {
	// 	// TODO walk the map based on KeyPath and set the value
	// }

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
