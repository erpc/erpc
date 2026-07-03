// Command erpc-simulator serves a local browser-based playground for
// designing and testing eRPC selection policies, failsafe stacks, and
// upstream behaviour under synthetic traffic.
//
// Architecture (browser drives traffic, backend executes real eRPC):
//
//   ┌──────────────────────────────────────────────────────────────────────┐
//   │  erpc-simulator (Go)                                                 │
//   │                                                                      │
//   │   ┌─────────────────────┐    ┌──────────────────────────┐            │
//   │   │ static asset server │    │  WebSocket /ws            │            │
//   │   │ /index.html, .css,  │    │  per-conn Session:        │            │
//   │   │ .jsx, .js (embed.FS)│    │   - reads send-batch      │            │
//   │   └─────────────────────┘    │   - executes via          │            │
//   │             ↑                │     Orchestrator.Execute  │            │
//   │     HTTP    │                │   - flushes stats + traces│            │
//   │             ↓                └────────────┬──────────────┘            │
//   │                                           ↓                           │
//   │                              ┌────────────────────────────┐           │
//   │                              │  Orchestrator               │          │
//   │                              │   ├─ real *erpc.ERPC        │          │
//   │                              │   ├─ real *erpc.Network     │          │
//   │                              │   │     ↓ Forward(ctx,req)  │          │
//   │                              │   ├─ UpstreamHub (fakes)    │          │
//   │                              │   │     ↑ HTTP loopback     │          │
//   │                              │   └─ Rolling counters       │          │
//   │                              │       + scenario loop       │          │
//   │                              └────────────────────────────┘           │
//   └──────────────────────────────────────────────────────────────────────┘
//                                       ↑ WebSocket (JSON frames)
//   ┌──────────────────────────────────────────────────────────────────────┐
//   │  Browser tab                                                         │
//   │   - React + Babel UI                                                 │
//   │   - simulator.js: traffic generator (poisson/constant/bursty),       │
//   │     method sampler, WS shim. Sends `send-batch` frames per tick.     │
//   │   - flow stage, charts, policy editor, knob panel, log/drawer.       │
//   └──────────────────────────────────────────────────────────────────────┘
//
// Usage:
//
//	go run ./cmd/erpc-simulator
//	make build && ./bin/erpc-simulator -addr :8080
package main

import (
	"context"
	"embed"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/common/legacy"
	"github.com/erpc/erpc/internal/simulator"
	"github.com/erpc/erpc/upstream"
	"github.com/rs/zerolog"
)

//go:embed all:web
var webFS embed.FS

func init() {
	// Mirror cmd/erpc's legacy-config migration wiring so the
	// simulator's eRPC accepts old-style YAML payloads on
	// apply-config.
	common.LegacyTranslateFn = legacy.TranslateFromConfig

	// Capture the FULL per-attempt error chain. Production caps this
	// at 200 chars to keep request-log volume small; the simulator's
	// lifecycle drawer wants to render the whole `caused by` tree.
	// Set to 0 to disable truncation entirely.
	upstream.AttemptErrorDetailMaxLen = 0
}

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "address for the simulator UI + WebSocket")
	logLevel := flag.String("log-level", "warn", "zerolog level for the in-process eRPC instance")
	// `-web-dir` serves assets from disk instead of the embedded fs.
	// Useful when iterating on the JSX / CSS — without this every change
	// requires a Go rebuild (the `//go:embed all:web` directive captures
	// files at compile time). Point it at the absolute path of
	// `cmd/erpc-simulator/web/`.
	webDir := flag.String("web-dir", "", "serve UI assets from this directory instead of the embedded fs (dev iteration)")
	// `-dump-file` writes a chronological JSONL record of every observable
	// event to the given path. Boot config, knob/policy/config changes,
	// scenarios, paused state, AND every request lifecycle (with the full
	// per-attempt log, selection trail, response body, and error chain).
	// Companion `*.AGENTS.md` is written next to it explaining the schema
	// and idiomatic queries — so an AI agent investigating the dump after
	// the fact has everything it needs in one place.
	dumpFile := flag.String("dump-file", "", "path to write a JSONL dump of every simulator event (boot, knob/policy/config changes, requests). Empty disables.")
	// `-preset svm` boots the Solana seed (svm:mainnet-beta network +
	// SVM upstreams advancing 400ms synthetic slots) instead of the
	// default eth-mainnet one. Knobs, policy editor, failsafe, charts
	// all work identically.
	preset := flag.String("preset", "evm", "seed config preset: evm | svm")
	// `-seed-file` boots from an operator-provided YAML instead of a
	// built-in preset — e.g. a copy of a production topology. Endpoints
	// are still rewritten to the synthetic loopback hub; the file only
	// shapes topology (upstream ids/vendors/tags, network, failsafe).
	seedFile := flag.String("seed-file", "", "path to a seed config YAML (overrides -preset)")
	flag.Parse()

	level, err := zerolog.ParseLevel(*logLevel)
	if err != nil {
		log.Fatalf("erpc-simulator: bad log level: %v", err)
	}
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnixMs
	logger := zerolog.New(os.Stderr).Level(level).With().Timestamp().Logger()

	common.LegacyTranslateLogger = func(w string) {
		logger.Warn().Str("source", "config-migration").Msg(w)
	}

	rootCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var dumper *simulator.Dumper
	if *dumpFile != "" {
		d, derr := simulator.NewDumper(*dumpFile)
		if derr != nil {
			logger.Fatal().Err(derr).Msg("simulator: NewDumper failed")
		}
		dumper = d
		fmt.Fprintf(os.Stderr, "erpc-simulator: dumping events to %s\n", *dumpFile)
	}

	seedYAML := simulator.SeedYAMLExpanded
	switch *preset {
	case "evm":
	case "svm":
		seedYAML = simulator.SeedYAMLSvmExpanded
	default:
		logger.Fatal().Str("preset", *preset).Msg("erpc-simulator: unknown -preset (want evm or svm)")
	}
	if *seedFile != "" {
		s, serr := simulator.SeedFromFile(*seedFile)
		if serr != nil {
			logger.Fatal().Err(serr).Str("path", *seedFile).Msg("erpc-simulator: -seed-file load failed")
		}
		seedYAML = s
	}

	o, err := simulator.New(simulator.Options{
		Logger: logger,
		// Use the placeholder-EXPANDED seed (built once at init() in
		// internal/simulator/config.go). The raw `simulator.SeedYAML`
		// const keeps the `{SELECTION_POLICY_FUNC}` placeholder for the
		// frontend's "↺ default" button on the YAML editor, but the
		// orchestrator needs a fully-formed eRPC config to boot.
		SeedYAML:        seedYAML,
		UpstreamHubBind: "127.0.0.1:0",
		Dumper:          dumper,
	})
	if err != nil {
		logger.Fatal().Err(err).Msg("simulator: New failed")
	}
	if err := o.Start(rootCtx); err != nil {
		logger.Fatal().Err(err).Msg("simulator: Start failed")
	}
	defer o.Stop()

	var assetFS http.FileSystem
	if *webDir != "" {
		fmt.Fprintf(os.Stderr, "erpc-simulator: serving UI from disk: %s\n", *webDir)
		assetFS = http.Dir(*webDir)
	} else {
		sub, err := fs.Sub(webFS, "web")
		if err != nil {
			logger.Fatal().Err(err).Msg("simulator: embed.FS sub failed")
		}
		assetFS = http.FS(sub)
	}

	mux := http.NewServeMux()
	mux.Handle("/", noCacheHTML(http.FileServer(assetFS)))
	mux.Handle("/ws", simulator.WSHandler(o))

	srv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		fmt.Fprintf(os.Stderr, "erpc-simulator: listening on http://%s (ws at /ws)\n", *addr)
		fmt.Fprintf(os.Stderr, "erpc-simulator: fake upstreams on http://%s\n", o.Hub().Addr())
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Fatal().Err(err).Msg("simulator: ListenAndServe")
		}
	}()

	<-rootCtx.Done()
	fmt.Fprintln(os.Stderr, "erpc-simulator: shutting down…")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error().Err(err).Msg("simulator: HTTP shutdown")
	}
}

// noCacheHTML disables caching for every asset the simulator serves.
// Local dev — rebuilds happen constantly; cached .jsx files cause
// confusing "the bug isn't fixed!" moments. Trade slightly more
// bandwidth for a sane reload-and-see-it loop.
func noCacheHTML(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store, must-revalidate")
		w.Header().Set("Pragma", "no-cache")
		w.Header().Set("Expires", "0")
		h.ServeHTTP(w, r)
	})
}

// (The simulator's seed config is now `simulator.SeedYAML` — the full
// eRPC YAML the editor opens with. The orchestrator parses it, rewrites
// endpoints to the loopback hub, and synthesizes per-upstream knobs
// with reasonable defaults the operator can re-tune live.)
